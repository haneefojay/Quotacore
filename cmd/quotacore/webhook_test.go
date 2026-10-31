package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/webhook"
)

// webhookPath is a tenant's webhook route.
func webhookPath(tenantID string) string { return "/v1/admin/tenants/" + tenantID + "/webhook" }

// putWebhook configures a tenant's webhook and returns the response body.
func (s *stack) putWebhook(tenantID, body string, want int) map[string]any {
	s.t.Helper()
	return mustStatus(s.t, s.adminDo(http.MethodPut, webhookPath(tenantID), body), want)
}

// webhookAudit is one audit row whose subject is a webhook, oldest first.
type webhookAudit struct {
	Action, Before, After, Reason string
}

func (s *stack) webhookAudits() []webhookAudit {
	s.t.Helper()
	rows, err := s.seedDB.QueryContext(context.Background(), `
SELECT action, coalesce(before::text, ''), coalesce(after::text, ''), coalesce(reason, '')
  FROM audit_log WHERE subject_type = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []webhookAudit
	for rows.Next() {
		var a webhookAudit
		if err := rows.Scan(&a.Action, &a.Before, &a.After, &a.Reason); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// TestT20EveryWebhookChangeIsAuditedWithBeforeAndAfter is IP-21 DoD 1
// (DR-041, DR-055): a create, a replace, a rotation and a removal each write
// one audit row naming what changed, none holds a secret, and a tenant's soft
// delete removes its webhook with a row of its own.
func TestT20EveryWebhookChangeIsAuditedWithBeforeAndAfter(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	created := s.putWebhook(tn.ID, `{"url":"https://hooks.example.com/a","subscriptions":[{"type":"threshold_crossing","feature_key":"`+f+`","value":80},{"type":"absolute_threshold","feature_key":"`+f+`","value":10}]}`, http.StatusCreated)
	first := str(created, "secret")
	if !strings.HasPrefix(first, webhook.SecretPrefix) || num(t, created, "secret_generation") != 1 {
		t.Fatalf("a create: %v", created)
	}
	got := mustStatus(t, s.adminDo(http.MethodGet, webhookPath(tn.External), ""), http.StatusOK)
	if _, has := got["secret"]; has || str(got, "url") != "https://hooks.example.com/a" || len(got["subscriptions"].([]any)) != 2 {
		t.Fatalf("a read: %v", got)
	}
	replaced := s.putWebhook(tn.ID, `{"url":"https://hooks.example.com/b","subscriptions":[{"type":"plan_change"},{"type":"cycle_rollover"}]}`, http.StatusOK)
	if _, has := replaced["secret"]; has || num(t, replaced, "secret_generation") != 1 {
		t.Fatalf("a replace returned a secret or moved the generation: %v", replaced)
	}
	rotated := mustStatus(t, s.adminDo(http.MethodPost, webhookPath(tn.ID)+"/rotate-secret", ""), http.StatusOK)
	second := str(rotated, "secret")
	if second == first || !strings.HasPrefix(second, webhook.SecretPrefix) || num(t, rotated, "secret_generation") != 2 {
		t.Fatalf("a rotation: %v", rotated)
	}
	mustStatus(t, s.adminDo(http.MethodDelete, webhookPath(tn.ID), ""), http.StatusNoContent)
	if r := s.adminDo(http.MethodGet, webhookPath(tn.ID), ""); r.code(t) != "webhook_not_found" {
		t.Fatalf("a read after the removal: %d %s", r.Status, r.Body)
	}

	audits := s.webhookAudits()
	want := []string{"webhook.create", "webhook.update", "webhook.rotate_secret", "webhook.delete"}
	if len(audits) != len(want) {
		t.Fatalf("%d webhook audit rows, want %d: %+v", len(audits), len(want), audits)
	}
	for i, a := range audits {
		if a.Action != want[i] {
			t.Errorf("row %d is %s, want %s", i, a.Action, want[i])
		}
		if strings.Contains(a.Before+a.After, first) || strings.Contains(a.Before+a.After, second) {
			t.Errorf("%s holds a secret", a.Action)
		}
	}
	if audits[0].Before != "" || !strings.Contains(audits[0].After, "hooks.example.com/a") {
		t.Errorf("the create's before and after: %+v", audits[0])
	}
	if !strings.Contains(audits[1].Before, "hooks.example.com/a") || !strings.Contains(audits[1].After, "hooks.example.com/b") {
		t.Errorf("the replace's before and after: %+v", audits[1])
	}
	if !strings.Contains(audits[2].Before, `"secret_generation": 1`) && !strings.Contains(audits[2].Before, `"secret_generation":1`) {
		t.Errorf("the rotation's before does not name generation 1: %s", audits[2].Before)
	}
	if !strings.Contains(audits[2].After, `"secret_generation": 2`) && !strings.Contains(audits[2].After, `"secret_generation":2`) {
		t.Errorf("the rotation's after does not name generation 2: %s", audits[2].After)
	}
	if audits[3].After != "" || !strings.Contains(audits[3].Before, "hooks.example.com/b") {
		t.Errorf("the removal's before and after: %+v", audits[3])
	}

	// A soft delete takes the webhook with it, in the same transaction.
	s.putWebhook(tn.ID, `{"url":"https://hooks.example.com/c","subscriptions":[{"type":"plan_change"}]}`, http.StatusCreated)
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+tn.ID, `{"reason":"churned"}`), http.StatusOK)
	if n := s.rows(`SELECT count(*) FROM webhooks`); n != 0 {
		t.Errorf("%d webhook rows survive the tenant's delete", n)
	}
	audits = s.webhookAudits()
	last := audits[len(audits)-1]
	if last.Action != "webhook.delete" || last.Reason != "tenant deleted" {
		t.Errorf("the delete's webhook row: %+v", last)
	}
	if r := s.adminDo(http.MethodPut, webhookPath(tn.ID), `{"url":"https://hooks.example.com/d","subscriptions":[{"type":"plan_change"}]}`); r.code(t) != "tenant_deleted" {
		t.Errorf("a webhook on a deleted tenant: %d %s", r.Status, r.Body)
	}
}

// TestAWebhookConfigurationIsValidated: the endpoint is absolute https with
// no credentials, and every subscription entry is checked and named (DR-055).
func TestAWebhookConfigurationIsValidated(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	bf, rf := uniq("wbf"), uniq("wrf")
	s.createBooleanFeature(bf)
	s.createRollingFeature(rf)
	ok := `[{"type":"plan_change"}]`
	cases := map[string]struct{ body, field string }{
		"plain http":       {`{"url":"http://hooks.example.com/","subscriptions":` + ok + `}`, "url"},
		"credentials":      {`{"url":"https://u:p@hooks.example.com/","subscriptions":` + ok + `}`, "url"},
		"a fragment":       {`{"url":"https://hooks.example.com/#x","subscriptions":` + ok + `}`, "url"},
		"a relative URL":   {`{"url":"/hooks","subscriptions":` + ok + `}`, "url"},
		"no subscriptions": {`{"url":"https://hooks.example.com/","subscriptions":[]}`, "subscriptions"},
		"an unknown type":  {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"everything"}]}`, "subscriptions[0].type"},
		"a percent above 100": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"threshold_crossing","feature_key":"` + f + `","value":101}]}`,
			"subscriptions[0].value"},
		"a threshold with no feature": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"absolute_threshold","value":1}]}`,
			"subscriptions[0].feature_key"},
		"a value on a plan change": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"plan_change","value":1}]}`,
			"subscriptions[0].value"},
		"a repeated entry": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"plan_change"},{"type":"plan_change"}]}`,
			"subscriptions[1]"},
		"a feature that does not exist": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"absolute_threshold","feature_key":"nothing.here","value":1}]}`,
			"subscriptions[0].feature_key"},
		"a boolean feature": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"absolute_threshold","feature_key":"` + bf + `","value":1}]}`,
			"subscriptions[0].feature_key"},
		"a rolling feature": {`{"url":"https://hooks.example.com/","subscriptions":[{"type":"threshold_crossing","feature_key":"` + rf + `","value":50}]}`,
			"subscriptions[0].feature_key"},
		"a secret field": {`{"url":"https://hooks.example.com/","subscriptions":` + ok + `,"secret":"mine"}`, "secret"},
	}
	for name, c := range cases {
		r := s.adminDo(http.MethodPut, webhookPath(tn.ID), c.body)
		if r.code(t) != "validation_failed" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
			continue
		}
		if !strings.Contains(string(r.Body), `"field":"`+c.field+`"`) {
			t.Errorf("%s does not name %s: %s", name, c.field, r.Body)
		}
	}
	if n := s.rows(`SELECT count(*) FROM webhooks`); n != 0 {
		t.Errorf("a refused configuration wrote %d rows", n)
	}
}

// TestT20APayloadCarriesTheLedgersEventID is IP-21 DoD 3 (INV-W1, INV-U1): a
// consume that crosses a subscribed threshold becomes a payload whose
// event_id, the ledger row's id and the Quotacore-Delivery header are one
// value, signed with the secret the create returned.
func TestT20APayloadCarriesTheLedgersEventID(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(10)
	created := s.putWebhook(tn.ID, `{"url":"https://hooks.example.com/","subscriptions":[{"type":"threshold_crossing","feature_key":"`+f+`","value":50},{"type":"absolute_threshold","feature_key":"`+f+`","value":5}]}`, http.StatusCreated)
	secret := str(created, "secret")
	var subs []webhook.Subscription
	raw, _ := json.Marshal(created["subscriptions"])
	if err := json.Unmarshal(raw, &subs); err != nil {
		t.Fatal(err)
	}

	if r := s.consume(s.runtime, f, 6, tn.External, newKey()); r.Status != http.StatusOK {
		t.Fatalf("consume: %d %s", r.Status, r.Body)
	}
	// The event is read back from the ledger, with the id the sink minted
	// when it accepted it, which is the identity a delivery reuses (ADR-0016).
	rows := s.ledger(tn)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows", len(rows))
	}
	id, err := uuid.Parse(rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	ev := dataplane.Event{ID: id, FeatureKey: rows[0].Feature, Type: rows[0].Type, Delta: rows[0].Delta,
		Balance: rows[0].Balance, OccurredAt: rows[0].OccurredAt}
	p, matched := webhook.Match(subs, ev, tn.External, 10)
	if !matched || len(p.Crossings) != 2 {
		t.Fatalf("10 to 4 crosses 50%% used and 5 left: %v %+v", matched, p)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	h := webhook.Headers(secret, p.EventID, body, at)
	if p.EventID != rows[0].ID || h.Get(webhook.HeaderDelivery) != rows[0].ID {
		t.Errorf("payload %s, header %s, ledger %s: not one event_id", p.EventID, h.Get(webhook.HeaderDelivery), rows[0].ID)
	}
	if err := webhook.Verify(secret, h, body, at); err != nil {
		t.Errorf("the delivery does not verify with the secret the create returned: %v", err)
	}
	// A rotation ends the old secret at once.
	rotated := mustStatus(t, s.adminDo(http.MethodPost, webhookPath(tn.ID)+"/rotate-secret", ""), http.StatusOK)
	if err := webhook.Verify(str(rotated, "secret"), h, body, at); err == nil {
		t.Error("a delivery signed with the old secret verifies under the new one")
	}
	// The secret is derived from the signing key, the webhook and its
	// generation, so the stack's key reproduces what the route returned.
	var hookID string
	if err := s.seedDB.QueryRow(`SELECT id::text FROM webhooks`).Scan(&hookID); err != nil {
		t.Fatal(err)
	}
	if webhook.Secret([]byte(testWebhookKey), hookID, 2) != str(rotated, "secret") {
		t.Error("the rotated secret is not the derivation of generation 2")
	}
}

// testWebhookKey is the signing key every stack is configured with unless a
// test turns webhooks off. It is a fixture, not a secret.
const testWebhookKey = "test-only-webhook-signing-key-not-a-secret"

// TestT20WebhooksAreNotImplementedWithoutASigningKey is IP-21 DoD 7 (Q-42):
// an instance whose operator has not set QUOTACORE_WEBHOOK_SIGNING_KEY
// answers every webhook route 501 not_implemented, after authentication and
// scope, and writes nothing.
func TestT20WebhooksAreNotImplementedWithoutASigningKey(t *testing.T) {
	s := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_WEBHOOK_SIGNING_KEY"] = "" })
	_, _, tn := s.provisioned(10)
	routes := []struct{ method, path, body string }{
		{http.MethodPut, webhookPath(tn.ID), `{"url":"https://hooks.example.com/","subscriptions":[{"type":"plan_change"}]}`},
		{http.MethodGet, webhookPath(tn.ID), ""},
		{http.MethodDelete, webhookPath(tn.ID), ""},
		{http.MethodPost, webhookPath(tn.ID) + "/rotate-secret", ""},
	}
	for _, rt := range routes {
		if r := s.adminDo(rt.method, rt.path, rt.body); r.code(t) != "not_implemented" || r.Status != http.StatusNotImplemented {
			t.Errorf("%s %s: %d %s", rt.method, rt.path, r.Status, r.Body)
		}
		if r := s.do(rt.method, rt.path, s.runtime, rt.body); r.code(t) != "insufficient_scope" {
			t.Errorf("%s %s with a runtime key: %d %s", rt.method, rt.path, r.Status, r.Body)
		}
		if r := s.do(rt.method, rt.path, "", rt.body); r.code(t) != "unauthorized" {
			t.Errorf("%s %s without a key: %d %s", rt.method, rt.path, r.Status, r.Body)
		}
	}
	if n := s.rows(`SELECT count(*) FROM webhooks`) + s.rows(`SELECT count(*) FROM audit_log WHERE subject_type = 'webhook'`); n != 0 {
		t.Errorf("a disabled instance wrote %d webhook rows", n)
	}
}

// TestAShortSigningKeyRefusesStart: a signing key under 32 bytes would make
// every secret weak, so the service refuses to start, without echoing it.
func TestAShortSigningKeyRefusesStart(t *testing.T) {
	_, err := Load(func(k string) string {
		return map[string]string{
			"QUOTACORE_DATABASE_URL":        "postgres://x/y",
			"QUOTACORE_REDIS_URL":           "redis://x",
			"QUOTACORE_WEBHOOK_SIGNING_KEY": "too-short-fixture",
		}[k]
	})
	if err == nil || !strings.Contains(err.Error(), "QUOTACORE_WEBHOOK_SIGNING_KEY") || strings.Contains(err.Error(), "too-short-fixture") {
		t.Errorf("a short signing key: %v", err)
	}
}

// TestTheCLIConfiguresAWebhookAndPrintsItsSecretOnce: tenant webhook set
// builds the subscription set from --subscribe, and the secret is printed on
// stdout once with the warning on stderr.
func TestTheCLIConfiguresAWebhookAndPrintsItsSecretOnce(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(10)
	out := s.mustCLI("tenant", "webhook", "set", tn.External, "--endpoint", "https://hooks.example.com/cli",
		"--subscribe", "plan_change", "--subscribe", "threshold_crossing:"+f+":80", "--subscribe", "absolute_threshold:"+f+":2")
	if !strings.Contains(out.out, `"secret": "qcwh_`) || !strings.Contains(out.errOut, "shown once") {
		t.Fatalf("set: %+v", out)
	}
	again := s.mustCLI("tenant", "webhook", "set", tn.External, "--endpoint", "https://hooks.example.com/cli2", "--subscribe", "cycle_rollover")
	if strings.Contains(again.out, "qcwh_") || strings.Contains(again.errOut, "shown once") {
		t.Errorf("a replace printed a secret: %+v", again)
	}
	got := s.mustCLI("tenant", "webhook", "get", tn.External)
	if strings.Contains(got.out, "qcwh_") || !strings.Contains(got.out, "cli2") {
		t.Errorf("get: %+v", got)
	}
	rot := s.mustCLI("tenant", "webhook", "rotate-secret", tn.External)
	if !strings.Contains(rot.out, `"secret": "qcwh_`) || !strings.Contains(rot.errOut, "shown once") {
		t.Errorf("rotate-secret: %+v", rot)
	}
	s.mustCLI("tenant", "webhook", "delete", tn.External)
	if r := s.cli("tenant", "webhook", "get", tn.External); r.code != 5 {
		t.Errorf("get after delete exits %d, want 5: %+v", r.code, r)
	}
}
