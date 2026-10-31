package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/webhook"
)

const (
	webhookURLMax    = 2048
	subscriptionsMax = 20
	thresholdPercent = 100
	webhookSubsField = "subscriptions"
	meteredKind      = "metered"
)

// webhooks refuses every webhook route with 501 not_implemented when the
// operator has not set QUOTACORE_WEBHOOK_SIGNING_KEY: an instance that has not
// opted into egress answers as one without the surface (ADR-0019, Q-42). The
// router has already authenticated the caller and checked its scope.
func (s *Service) webhooks(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.webhookKey) == 0 {
			envelope.Write(w, r, envelope.Error(contract.ErrorCodeNotImplemented,
				"webhooks are not enabled on this instance"))
			return
		}
		h(w, r)
	}
}

// withSecret renders a webhook with the secret its id and generation derive
// to. Only a create and a rotation call it (Q-40).
func (s *Service) withSecret(sw StoredWebhook) *contract.WebhookWithSecret {
	return &contract.WebhookWithSecret{
		TenantExternalID: sw.Webhook.TenantExternalID,
		URL:              sw.Webhook.URL,
		Subscriptions:    sw.Webhook.Subscriptions,
		SecretGeneration: sw.Webhook.SecretGeneration,
		CreatedAt:        sw.Webhook.CreatedAt,
		UpdatedAt:        sw.Webhook.UpdatedAt,
		Secret:           webhook.Secret(s.webhookKey, sw.ID, sw.Webhook.SecretGeneration),
	}
}

// webhookURLIssue names what is wrong with an endpoint, or "" when it is an
// absolute https URL with a host, no user information and no fragment. Which
// addresses it may reach is IP-23's policy, not this check's.
func webhookURLIssue(raw string) string {
	if utf8.RuneCountInString(raw) > webhookURLMax {
		return fmt.Sprintf("must be at most %d characters", webhookURLMax)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || !u.IsAbs():
		return "must be an absolute URL"
	case u.Scheme != "https":
		return "must use https"
	case u.Host == "" || u.Hostname() == "":
		return "must name a host"
	case u.User != nil:
		return "must not carry user information"
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "must not carry a fragment"
	}
	return ""
}

// subscriptions reads and checks the subscription set, naming each offending
// entry by its index (DR-055). It returns the entries whose feature the route
// must still look up.
func (s *Service) subscriptions(b *body) ([]contract.WebhookSubscription, []int) {
	raw, ok := b.fields[webhookSubsField]
	if !ok {
		b.add(webhookSubsField, "is required")
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		b.add(webhookSubsField, "must be an array")
		return nil, nil
	}
	if len(entries) < 1 || len(entries) > subscriptionsMax {
		b.add(webhookSubsField, fmt.Sprintf("must hold 1 to %d entries", subscriptionsMax))
		return nil, nil
	}
	var (
		out     []contract.WebhookSubscription
		lookups []int
		seen    = map[string]bool{}
	)
	for i, e := range entries {
		field := fmt.Sprintf("%s[%d]", webhookSubsField, i)
		var fields map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(e))
		dec.UseNumber()
		if err := dec.Decode(&fields); err != nil || fields == nil {
			b.add(field, "must be an object")
			continue
		}
		entry := &body{fields: fields}
		for name := range fields {
			if name != "type" && name != "feature_key" && name != "value" {
				entry.add(name, "is not a field of a subscription")
			}
		}
		typ, _ := entry.str("type", true, nil, 0, 0)
		var sub contract.WebhookSubscription
		sub.Type = contract.WebhookSubscriptionType(typ)
		switch typ {
		case webhook.ThresholdCrossing, webhook.AbsoluteThreshold:
			key, _ := entry.str("feature_key", true, featureKeyShape, 1, 64)
			lo, hi := int64(0), int64(limitCeiling)
			if typ == webhook.ThresholdCrossing {
				lo, hi = 1, thresholdPercent
			}
			v, set := entry.integer("value", true, lo, hi)
			if key != "" && set {
				sub.FeatureKey = contract.NewOptFeatureKey(contract.FeatureKey(key))
				sub.Value = contract.NewOptInt64(v)
			}
		case webhook.PlanChange, webhook.CycleRollover:
			if entry.has("feature_key") {
				entry.add("feature_key", "is for a threshold only")
			}
			if entry.has("value") {
				entry.add("value", "is for a threshold only")
			}
		case "":
		default:
			entry.add("type", "must be threshold_crossing, absolute_threshold, plan_change or cycle_rollover")
		}
		for _, is := range entry.issues {
			b.add(field+"."+is.Field, is.Issue)
		}
		if len(entry.issues) > 0 {
			continue
		}
		id := fmt.Sprintf("%s|%s|%d", sub.Type, sub.FeatureKey.Value, sub.Value.Value)
		if seen[id] {
			b.add(field, "repeats an earlier entry")
			continue
		}
		seen[id] = true
		if sub.FeatureKey.Set {
			lookups = append(lookups, len(out))
		}
		out = append(out, sub)
	}
	return out, lookups
}

// putWebhook creates or replaces the tenant's webhook (DR-055). A create
// answers 201 with the secret, once; a replace answers 200 without it and
// keeps the secret.
func (s *Service) putWebhook(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "url", webhookSubsField)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	endpoint, set := b.str("url", true, nil, 1, 0)
	if set {
		if problem := webhookURLIssue(endpoint); problem != "" {
			b.add("url", problem)
		}
	}
	subs, lookups := s.subscriptions(b)
	if len(b.issues) == 0 {
		// A threshold counts down a cycle balance, which only a metered
		// feature has: a boolean one has no counter and a rolling one no
		// cycle (DR-050, DR-052).
		for _, i := range lookups {
			key := string(subs[i].FeatureKey.Value)
			f, err := s.store.GetFeature(r.Context(), key)
			field := fmt.Sprintf("%s[%d].feature_key", webhookSubsField, i)
			if err != nil {
				var c Coded
				if errors.As(err, &c) && c.CatalogueCode() == contract.ErrorCodeFeatureNotFound {
					b.add(field, "names no feature")
					continue
				}
				s.fail(w, r, err)
				return
			}
			if string(f.Kind) != meteredKind {
				b.add(field, "must name a metered feature; a threshold counts down a cycle balance")
			}
		}
	}
	if p := b.check(&contract.WebhookPut{}); p != nil {
		problem(w, r, p)
		return
	}
	sw, created, err := s.store.PutWebhook(r.Context(), actor(r), id, WebhookInput{URL: endpoint, Subscriptions: subs})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created {
		respond(w, r, http.StatusCreated, s.withSecret(sw))
		return
	}
	respond(w, r, http.StatusOK, &sw.Webhook)
}

func (s *Service) getWebhook(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	sw, err := s.store.GetWebhook(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &sw.Webhook)
}

func (s *Service) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	if err := s.store.DeleteWebhook(r.Context(), actor(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	noContent(w, r)
}

// rotateWebhookSecret moves the webhook to its next generation and returns
// the new secret, once; the old one stops verifying at once.
func (s *Service) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	sw, err := s.store.RotateWebhookSecret(r.Context(), actor(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, s.withSecret(sw))
}
