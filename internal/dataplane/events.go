package dataplane

import (
	"encoding/json"
	"time"
)

// Event is what an applied consume or refund hands the ledger (request-lifecycle.md
// section 1, step 9). The ledger's own row, its event_id and its queue are
// IP-12's; this is the call site and the facts the request knows.
type Event struct {
	RequestID  string
	TenantID   string
	FeatureKey string
	Type       string // consumed or refunded
	Amount     int64
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
