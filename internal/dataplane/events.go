package dataplane

import (
	"encoding/json"
	"time"
)

// Event is what an applied consume or refund hands the ledger (request-lifecycle.md
// section 1, step 9), and, since IP-10, what an admin route that moved a
// balance hands it: a grant, a set, a force-rollover, or the discard a plan
// change or an apply-now makes. The ledger's own row, its event_id and its
// queue are IP-12's; this is the call site and the facts the request knows.
type Event struct {
	RequestID  string
	TenantID   string
	FeatureKey string
	// Type is consumed or refunded from the data plane; granted, set,
	// force_rolled_over or plan_changed from the admin routes.
	Type string
	// Source is api or admin (data-model.md section 2.1).
	Source string
	Amount int64
	// Delta is the signed change to the balance, never zero: a zero change
	// is no event (DR-023, Q-22).
	Delta      int64
	Balance    int64 // after the operation
	CycleIndex int64
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

// NopSink discards every event. It is the sink until IP-12 provides the queue
// and the writer, and it is documented as such rather than pretending to be a
// ledger.
type NopSink struct{}

func (NopSink) Enqueue(Event) {}
