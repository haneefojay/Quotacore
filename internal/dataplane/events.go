package dataplane

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/script"
)

// Event is what an applied consume or refund hands the ledger (request-lifecycle.md
// section 1, step 9), and, since IP-10, what an admin route that moved a
// balance hands it: a grant, a set, a force-rollover, or the discard a plan
// change or an apply-now makes; and, since IP-12, the cycle_rolled_over a
// script's transition produced, handed over before the request's own event.
type Event struct {
	// ID is the event_id a consumer deduplicates on (INV-W1's ledger
	// reading). The sink mints it once, as a UUIDv7, when it accepts the
	// event, and every retry and replay carries the same one; a caller leaves
	// it zero.
	ID         uuid.UUID
	RequestID  string
	TenantID   string
	FeatureKey string
	// Type is consumed or refunded from the data plane; granted, set,
	// force_rolled_over or plan_changed from the admin routes; and
	// cycle_rolled_over from whichever caller's script rolled.
	Type string
	// Source is api, admin, or system for a rollover (data-model.md section
	// 2.1).
	Source string
	Amount int64
	// Delta is the signed change to the balance, never zero: a zero change
	// is no event (DR-023, Q-22).
	Delta      int64
	Balance    int64 // after the operation
	CycleIndex int64
	// CycleStart is the start of the window the hash was in, from the same
	// script reply as CycleIndex, so the two name one cycle even across a
	// re-anchor (data-model.md section 2.7).
	CycleStart time.Time
	ClientKey  string // the Idempotency-Key the caller sent
	Metadata   json.RawMessage
	OccurredAt time.Time
}

// EventSink takes events off the request path. Enqueue must not block and must
// not fail the request: the response has already been written when it is
// called, and the ledger is best effort and at-least-once (ADR-0016).
type EventSink interface {
	Enqueue(Event)
}

// NopSink discards every event. The service ships internal/ledger's sink;
// this one is for unit tests that have no ledger to write to.
type NopSink struct{}

func (NopSink) Enqueue(Event) {}

// Rollover is the cycle_rolled_over event for a script reply whose transition
// re-granted the allowance (IP-12 D3, U-6): the closed cycle's balance becomes
// the new window's limit, attributed to the system and to the request whose
// script rolled. A caller hands it to the sink before its own event, so the
// ledger reads in the order the store changed. It reports false when nothing
// rolled, on a replay, and when the closing balance already equalled the new
// limit, which is no change and so no event (Q-22).
func Rollover(requestID, tenantID, featureKey string, out script.Outcome, at time.Time) (Event, bool) {
	if !out.Rolled() || out.Closed == nil {
		return Event{}, false
	}
	delta := out.Limit - *out.Closed
	if delta == 0 {
		return Event{}, false
	}
	return Event{
		RequestID:  requestID,
		TenantID:   tenantID,
		FeatureKey: featureKey,
		Type:       "cycle_rolled_over",
		Source:     "system",
		Amount:     max(delta, -delta),
		Delta:      delta,
		Balance:    out.Limit,
		CycleIndex: out.Index,
		CycleStart: out.Start,
		OccurredAt: at,
	}, true
}
