package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/quotacore/quotacore/internal/observability"
)

// Checkpoint is how far the pass has read (data-model.md section 2.13).
type Checkpoint struct {
	EventsSeq int64
	GapsID    int64
}

// Changed is a tenant-feature whose rows or gaps arrived since the checkpoint,
// and the earliest instant among them, from which the pass reads.
type Changed struct {
	Subject
	Since time.Time
}

// Duplicate is one unresolved duplicate dead letter: a second mutation under
// one idempotency key that the store applied and the ledger refused (FS-22).
type Duplicate struct {
	EventID        string
	TenantID       string
	TenantExternal string
	TenantDeleted  bool
	Feature        string
	Type           string // consumed or refunded
	Amount         int64
	ClientKey      string
	At             time.Time
}

// The outcomes of a reversal's refund.
const (
	RefundApplied  = "applied"
	RefundReplayed = "replayed"
	RefundRefused  = "refused"
)

// Refunded is what the reversal's refund did in the store.
type Refunded struct {
	Outcome    string
	Balance    int64
	CycleIndex int64
	CycleStart time.Time
	// Rolled is the cycle_rolled_over the refund's own transition produced,
	// which the ledger needs before the reversal row; nil when nothing
	// rolled.
	Rolled *RolledOver
	At     time.Time
}

// RolledOver is the opening a transition wrote.
type RolledOver struct {
	Delta, Balance, CycleIndex int64
	CycleStart                 time.Time
}

// Reversal is a duplicate and the refund that reversed it, for the one
// transaction that records both (IP-11 D4).
type Reversal struct {
	Duplicate
	Refunded
	// Key is the store-side idempotency key the refund ran under.
	Key string
	// RequestID is the reversal row's request_id and the audit row's.
	RequestID string
}

// ErrNotReversible is a refund the Refunder cannot issue at all: the feature
// is no longer the tenant's, so there is no window to refund into.
var ErrNotReversible = errors.New("reconcile: the duplicate cannot be refunded")

// Ledger is the Postgres side of the pass; internal/db implements it.
type Ledger interface {
	// Lock takes the pass's advisory lock for one pass, so one instance runs
	// it at a time; ok is false when another instance holds it.
	Lock(ctx context.Context) (release func(), ok bool, err error)
	Checkpoint(ctx context.Context) (Checkpoint, error)
	SaveCheckpoint(ctx context.Context, c Checkpoint) error
	// Changed lists the tenant-features with a row created before settled
	// whose seq is past the checkpoint, or a gap past it, up to limit of
	// them, and the checkpoint that reading them reaches.
	Changed(ctx context.Context, from Checkpoint, settled time.Time, limit int) ([]Changed, Checkpoint, error)
	// Rows is a tenant-feature's rows from since on, in (created_at, seq)
	// order, with its unreversed duplicate dead letters in their place, and
	// the row before since; or every row, when since is zero.
	Rows(ctx context.Context, sub Subject, since time.Time) (rows []Row, prev *Row, err error)
	// Duplicates lists up to limit unresolved duplicate dead letters, oldest
	// first.
	Duplicates(ctx context.Context, limit int) ([]Duplicate, error)
	// Reverse commits a reversal: the duplicate appended with its key moved
	// to reconciles_key, the rollover if any, the reversal row, the audit
	// row, and the dead letter resolved. It reports false when the dead
	// letter was already resolved, which commits nothing.
	Reverse(ctx context.Context, r Reversal) (bool, error)
	// Resolve closes a dead letter without a reversal.
	Resolve(ctx context.Context, eventID, resolution string) error
	// Opened appends the cycle_rolled_over a refused refund's transition
	// wrote, which the store applied whatever the refund's own decision.
	Opened(ctx context.Context, d Duplicate, requestID string, ro RolledOver, at time.Time) error
	// Record inserts a finding and reports whether it is new.
	Record(ctx context.Context, f Finding, foundBy string) (bool, error)
}

// Balances reads a hash; internal/store implements it.
type Balances interface {
	Hash(ctx context.Context, sub Subject) (Hash, error)
}

// Refunder issues the reversal's refund; internal/dataplane implements it. It
// hands nothing to the ledger's sink: the pass writes the rows itself, in the
// transaction that resolves the dead letter (S7).
type Refunder interface {
	Refund(ctx context.Context, d Duplicate, key string) (Refunded, error)
}

// ReversalKey is the store-side idempotency key a duplicate's refund runs
// under. It is outside the header's grammar (api-conventions.md section 3),
// so no client can collide with it, and it is the same on every attempt, so
// a pass retried after a crash replays the refund rather than issuing a
// second one (IP-11 D4).
func ReversalKey(eventID string) string { return "#reversal/" + eventID }

// Resolutions of a duplicate dead letter (data-model.md section 2.12).
const (
	ResolvedReversed      = "reversed"
	ResolvedTenantDeleted = "tenant_deleted"
	ResolvedReferred      = "referred"
)

// PassOptions configure the in-service pass.
type PassOptions struct {
	Ledger   Ledger
	Balances Balances
	Refunder Refunder
	Metrics  *observability.Reconcile
	Logger   *slog.Logger
	// Settle is how old a row must be before the pass reads it, and how long
	// it waits before reading a disagreement a second time (IP-11 D3).
	Settle time.Duration
	// Batch bounds the duplicates reversed and the tenant-features verified
	// in one pass. Zero means 1 000.
	Batch int
	// Paused reports whether the instance is read-only, when the pass does
	// not run (IP-11 D5); nil is never.
	Paused func() bool
	// Now is the pass's clock.
	Now func() time.Time
}

// Pass is the in-service reconciliation (IP-11 D2): it reverses detected
// duplicates and verifies what changed since it last ran. It never corrects
// a balance (observability.md section 5).
type Pass struct{ o PassOptions }

// NewPass builds a pass.
func NewPass(o PassOptions) *Pass {
	if o.Batch <= 0 {
		o.Batch = 1000
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Paused == nil {
		o.Paused = func() bool { return false }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Pass{o: o}
}

// Ran is what one pass did.
type Ran struct {
	Ran       bool // false when another instance held the lock, or read-only
	Reversed  int
	Referred  int
	Verified  int
	Suspects  int
	Confirmed []Finding
	Recorded  int
}

// Run runs a pass after first, then every interval, until ctx ends.
func (p *Pass) Run(ctx context.Context, first, every time.Duration) {
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r, err := p.Once(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			p.o.Logger.Error("the reconciliation pass failed; the next one tries again", "error", err)
		case r.Ran:
			p.o.Logger.Info("reconciliation pass", "reversed", r.Reversed, "referred", r.Referred,
				"verified", r.Verified, "findings", r.Recorded)
		}
		timer.Reset(every)
	}
}

// Once runs one pass.
func (p *Pass) Once(ctx context.Context) (Ran, error) {
	var r Ran
	if p.o.Paused() {
		return r, nil
	}
	release, ok, err := p.o.Ledger.Lock(ctx)
	if err != nil || !ok {
		return r, err
	}
	defer release()
	r.Ran = true
	if err := p.reverse(ctx, &r); err != nil {
		return r, err
	}
	return r, p.verify(ctx, &r)
}

// reverse issues the reversal of every outstanding duplicate, oldest first
// and bounded (DR-049, IP-11 D4).
func (p *Pass) reverse(ctx context.Context, r *Ran) error {
	dups, err := p.o.Ledger.Duplicates(ctx, p.o.Batch)
	if err != nil {
		return err
	}
	for _, d := range dups {
		if p.o.Paused() {
			return nil
		}
		sub := Subject{TenantID: d.TenantID, Feature: d.Feature}
		switch {
		case d.TenantDeleted:
			if err := p.o.Ledger.Resolve(ctx, d.EventID, ResolvedTenantDeleted); err != nil {
				return err
			}
			continue
		case d.Type != "consumed":
			// DR-049 reverses a deduction. A second refund under one key
			// went to the customer's favour, and a person decides it.
			if err := p.refer(ctx, r, d, Finding{Subject: sub, Kind: KindDuplicateRefund, Row: d.EventID,
				Actual: ptr(d.Amount), Detail: "a second refund under idempotency key " + d.ClientKey}); err != nil {
				return err
			}
			continue
		}
		key := ReversalKey(d.EventID)
		out, err := p.o.Refunder.Refund(ctx, d, key)
		if errors.Is(err, ErrNotReversible) || (err == nil && out.Outcome == RefundRefused) {
			why := "the refund would raise the balance above limit plus bonus (DR-021); grant instead"
			if err != nil {
				why = err.Error()
			} else if out.Rolled != nil {
				if err := p.o.Ledger.Opened(ctx, d, "reversal-"+d.EventID, *out.Rolled, out.At); err != nil {
					return err
				}
			}
			if err := p.refer(ctx, r, d, Finding{Subject: sub, Kind: KindReversalRefused, Row: d.EventID,
				Actual: ptr(d.Amount), Detail: "the reversal of a second deduction under idempotency key " +
					d.ClientKey + " was refused: " + why}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			// The store did not answer; the next pass tries again under
			// the same key.
			p.o.Logger.Warn("could not refund a detected duplicate; the next pass tries again",
				"event_id", d.EventID, "error", err)
			continue
		}
		done, err := p.o.Ledger.Reverse(ctx, Reversal{Duplicate: d, Refunded: out, Key: key,
			RequestID: "reversal-" + d.EventID})
		if err != nil {
			return err
		}
		if done {
			r.Reversed++
			if p.o.Metrics != nil {
				p.o.Metrics.DuplicateReversal.Inc()
			}
			p.o.Logger.Warn("reversed a duplicate deduction", "event_id", d.EventID, "tenant_id", d.TenantID,
				"feature_key", d.Feature, "amount", d.Amount, "refund", out.Outcome)
		}
	}
	return nil
}

func (p *Pass) refer(ctx context.Context, r *Ran, d Duplicate, f Finding) error {
	if _, err := p.record(ctx, f, "pass"); err != nil {
		return err
	}
	r.Referred++
	return p.o.Ledger.Resolve(ctx, d.EventID, ResolvedReferred)
}

// record inserts a finding, logs it, and counts it once (IP-11 D3, S2).
func (p *Pass) record(ctx context.Context, f Finding, by string) (bool, error) {
	created, err := p.o.Ledger.Record(ctx, f, by)
	if err != nil || !created {
		return created, err
	}
	if Counted(f.Kind) && p.o.Metrics != nil {
		p.o.Metrics.Mismatch.Inc()
	}
	p.o.Logger.Error("reconciliation found a disagreement; a person decides, nothing is corrected",
		"kind", f.Kind, "tenant_id", f.TenantID, "feature_key", f.Feature, "event_id", f.Row, "detail", f.Detail)
	return true, nil
}

type read struct {
	since time.Time
	hash  Hash
	found map[string]Finding
}

// verify checks the tenant-features that changed since the checkpoint. A
// disagreement is a suspect until a second read, one settle period later,
// finds the same hash and the same disagreement, so an event still in another
// instance's queue is never reported (IP-11 D3).
func (p *Pass) verify(ctx context.Context, r *Ran) error {
	from, err := p.o.Ledger.Checkpoint(ctx)
	if err != nil {
		return err
	}
	changed, next, err := p.o.Ledger.Changed(ctx, from, p.o.Now().Add(-p.o.Settle), p.o.Batch)
	if err != nil {
		return err
	}
	suspects := map[Subject]read{}
	for _, c := range changed {
		got, err := p.read(ctx, c.Subject, c.Since)
		if err != nil {
			return err
		}
		r.Verified++
		if len(got.found) > 0 {
			suspects[c.Subject] = got
		}
	}
	r.Suspects = len(suspects)
	if len(suspects) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.o.Settle):
		}
		for sub, first := range suspects {
			again, err := p.read(ctx, sub, first.since)
			if err != nil {
				return err
			}
			if again.hash != first.hash {
				// The balance moved: whatever moved it will be a row, and
				// the next pass reads it.
				continue
			}
			for fp, f := range again.found {
				if _, same := first.found[fp]; !same {
					continue
				}
				r.Confirmed = append(r.Confirmed, f)
				created, err := p.record(ctx, f, "pass")
				if err != nil {
					return err
				}
				if created {
					r.Recorded++
				}
			}
		}
	}
	return p.o.Ledger.SaveCheckpoint(ctx, next)
}

func (p *Pass) read(ctx context.Context, sub Subject, since time.Time) (read, error) {
	rows, prev, err := p.o.Ledger.Rows(ctx, sub, since)
	if err != nil {
		return read{}, err
	}
	hash, err := p.o.Balances.Hash(ctx, sub)
	if err != nil {
		return read{}, err
	}
	v := Verify(sub, rows, prev, &hash)
	out := read{since: since, hash: hash, found: map[string]Finding{}}
	for _, f := range v.Findings {
		out.found[f.Fingerprint()] = f
	}
	return out, nil
}
