// Package webhook is the part of v0.3's outbound notification that needs no
// network (IP-21): the signing secret, the signature over a delivery, its
// verification, the payload a ledger event becomes, and the rule that decides
// whether a tenant's subscription set is told about an event (DR-055). It opens
// no connection and imports nothing that does; delivery is IP-22's.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/quotacore/quotacore/internal/dataplane"
)

// The three headers every delivery carries (webhooks.md section 2).
const (
	HeaderDelivery  = "Quotacore-Delivery"
	HeaderTimestamp = "Quotacore-Timestamp"
	HeaderSignature = "Quotacore-Signature"
)

// ReplayWindow is how far a delivery's timestamp may be from the receiver's
// clock, either way, before the receiver refuses it (Q-41). The receiver's
// record of the event_ids it has processed covers the window itself.
const ReplayWindow = 300 * time.Second

// SecretPrefix starts every webhook secret, so one pasted in the wrong place
// is recognisable, as an API key is by qc_ (ADR-0018).
const SecretPrefix = "qcwh_"

// MinSigningKey is the shortest QUOTACORE_WEBHOOK_SIGNING_KEY accepted: the
// HMAC-SHA256 block of entropy every derived secret rests on.
const MinSigningKey = 32

// signatureVersion names the scheme, so a later one can be sent beside it.
const signatureVersion = "v1"

// Secret derives a webhook's signing secret from the operator's signing key,
// the webhook's id and its generation (Q-40). Nothing stores it: the same
// three inputs give the same secret, a rotation is a new generation, and a new
// webhook is a new id. It is the bytes a receiver keys its HMAC with, as they
// are.
func Secret(signingKey []byte, webhookID string, generation int64) string {
	m := hmac.New(sha256.New, signingKey)
	m.Write([]byte("quotacore webhook secret v1\x00"))
	m.Write([]byte(webhookID))
	var g [8]byte
	binary.BigEndian.PutUint64(g[:], uint64(generation))
	m.Write(g[:])
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// signed is the string a signature covers: the timestamp, the event_id and
// the body, so none of the three can be changed or moved to another delivery
// without the signature failing (IP-21 DoD 5).
func signed(ts int64, eventID string, body []byte) []byte {
	out := make([]byte, 0, 32+len(eventID)+len(body))
	out = strconv.AppendInt(out, ts, 10)
	out = append(out, '.')
	out = append(out, eventID...)
	out = append(out, '.')
	return append(out, body...)
}

// Sign is the Quotacore-Signature value of a delivery at ts.
func Sign(secret string, ts time.Time, eventID string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(signed(ts.Unix(), eventID, body))
	return signatureVersion + "=" + hex.EncodeToString(m.Sum(nil))
}

// Headers are the headers a delivery of body for eventID is sent with at at.
func Headers(secret, eventID string, body []byte, at time.Time) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(HeaderDelivery, eventID)
	h.Set(HeaderTimestamp, strconv.FormatInt(at.Unix(), 10))
	h.Set(HeaderSignature, Sign(secret, at, eventID, body))
	return h
}

// The reasons Verify refuses a delivery.
var (
	ErrMissingHeader     = errors.New("webhook: a signature header is missing or malformed")
	ErrOutsideWindow     = errors.New("webhook: the timestamp is outside the replay window")
	ErrSignatureMismatch = errors.New("webhook: no signature matches")
)

// Verify is the receiver's check, which the recipe in webhooks.md performs
// without this package: the timestamp is within ReplayWindow of now, and one
// of the v1 signatures is the HMAC of the signed string under secret, compared
// in constant time. It does not deduplicate; that is the receiver's record of
// event_ids (INV-W1).
func Verify(secret string, h http.Header, body []byte, now time.Time) error {
	eventID, rawTS, rawSig := h.Get(HeaderDelivery), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if eventID == "" || rawTS == "" || rawSig == "" {
		return ErrMissingHeader
	}
	ts, err := strconv.ParseInt(rawTS, 10, 64)
	if err != nil {
		return ErrMissingHeader
	}
	if d := now.Sub(time.Unix(ts, 0)); d > ReplayWindow || d < -ReplayWindow {
		return ErrOutsideWindow
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(signed(ts, eventID, body))
	want := m.Sum(nil)
	for _, part := range strings.Split(rawSig, ",") {
		version, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || version != signatureVersion {
			continue
		}
		got, err := hex.DecodeString(value)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

// The subscription types (DR-055).
const (
	ThresholdCrossing = "threshold_crossing"
	AbsoluteThreshold = "absolute_threshold"
	PlanChange        = "plan_change"
	CycleRollover     = "cycle_rollover"
)

// Subscription is one entry of a tenant's subscription set, as stored and as
// the API renders it.
type Subscription struct {
	Type       string `json:"type"`
	FeatureKey string `json:"feature_key,omitempty"`
	Value      *int64 `json:"value,omitempty"`
}

// Crossing is one threshold an event crossed.
type Crossing struct {
	Type  string `json:"type"`
	Value int64  `json:"value"`
}

// Payload is a delivery's body: one per ledger event, carrying that event's
// id (INV-W1), and every subscribed threshold it crossed.
type Payload struct {
	EventID          string     `json:"event_id"`
	EventType        string     `json:"event_type"`
	OccurredAt       string     `json:"occurred_at"`
	TenantExternalID string     `json:"tenant_external_id"`
	FeatureKey       string     `json:"feature_key"`
	Delta            int64      `json:"delta"`
	Balance          int64      `json:"balance"`
	Crossings        []Crossing `json:"crossings"`
}

// Crosses reports whether a balance falling from before to after crosses a
// threshold of limit (DR-055). A threshold_crossing of p percent is crossed
// when the balance falls from above (100 - p)% of the limit to at or below it,
// an absolute_threshold of n when it falls from above n to at or below n. Only
// a fall crosses; a refund or a grant re-arms it by rising back above. The
// percentage is compared in integers, which is exact for any balance below
// 2^53 (DR-022).
func Crosses(s Subscription, before, after, limit int64) bool {
	if s.Value == nil || after >= before {
		return false
	}
	v := *s.Value
	switch s.Type {
	case ThresholdCrossing:
		if limit <= 0 {
			return false
		}
		line := (100 - v) * limit
		return 100*before > line && 100*after <= line
	case AbsoluteThreshold:
		return before > v && after <= v
	}
	return false
}

// Match decides whether a tenant with the subscription set subs is told about
// ev, and builds the one payload it would be: a plan change or a rollover the
// set subscribes to, or any subscribed threshold on ev's feature that ev
// crossed, all listed together. limit is the feature's limit in the cycle ev
// belongs to; ev.ID must be the id the ledger minted.
func Match(subs []Subscription, ev dataplane.Event, tenantExternalID string, limit int64) (Payload, bool) {
	p := Payload{
		EventID:          ev.ID.String(),
		EventType:        ev.Type,
		OccurredAt:       ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		TenantExternalID: tenantExternalID,
		FeatureKey:       ev.FeatureKey,
		Delta:            ev.Delta,
		Balance:          ev.Balance,
		Crossings:        []Crossing{},
	}
	matched := false
	before := ev.Balance - ev.Delta
	for _, s := range subs {
		switch s.Type {
		case PlanChange:
			matched = matched || ev.Type == "plan_changed"
		case CycleRollover:
			matched = matched || ev.Type == "cycle_rolled_over" || ev.Type == "force_rolled_over"
		case ThresholdCrossing, AbsoluteThreshold:
			if s.FeatureKey == ev.FeatureKey && Crosses(s, before, ev.Balance, limit) {
				p.Crossings = append(p.Crossings, Crossing{Type: s.Type, Value: *s.Value})
				matched = true
			}
		}
	}
	return p, matched
}
