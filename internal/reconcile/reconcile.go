// Package reconcile is the control plane's half of the idempotency claim: it
// notices when the balance hashes and the ledger disagree, rather than
// preventing it again (ADR-0004, DR-030, DR-042). It decides; it does no I/O
// of its own. What it reads and writes it reaches through the interfaces in
// pass.go and rebuild.go, which internal/db, internal/store and
// internal/dataplane implement, and source_test.go holds it off every store,
// script and SQL package, so nothing in here can move a balance except the
// one refund the reversal hands to its Refunder (IP-11 D1, D10).
//
// The arithmetic is one rule, read across a tenant-feature's whole history:
// every balance move is a row, a move of zero writes none (Q-22), and every
// row carries its own delta and balance_after, so a row's balance_after less
// its delta is the balance_after of the row before it. Inside a cycle that is
// the reconciliation identity of DR-042; across a boundary it is the chain from
// a closed cycle to the opening of the next, which is how an event dropped at
// the end of a cycle is found. The hash is then compared with the last row.
package reconcile

import (
	"fmt"
	"strconv"
	"time"
)

// The kinds of finding (data-model.md section 2.13).
const (
	// KindIdentity is a row inside a cycle that does not follow from the row
	// before it: an event between them is missing, or a row was altered.
	KindIdentity = "identity"
	// KindChain is the first row of a cycle that does not follow from the
	// last row of the cycle before: an event at the end of the closed cycle
	// is missing.
	KindChain = "chain"
	// KindHashBalance is a hash whose balance is not the last row's
	// balance_after, in the same cycle.
	KindHashBalance = "hash_balance"
	// KindHashCycle is a hash in a cycle the ledger cannot have led to.
	KindHashCycle = "hash_cycle"
	// KindHashMissing is a tenant-feature with ledger rows and no hash. It is
	// reported, and never counted as a mismatch: balance_key_missing_total is
	// its alert (S3).
	KindHashMissing = "hash_missing"
	// KindNoCycleStart is a last row written before migration 00010, which
	// names no window, so the hash cannot be compared with it.
	KindNoCycleStart = "no_cycle_start"
	// KindReversalRefused is a duplicate whose reversal the ceiling refused
	// (DR-021); a person grants instead.
	KindReversalRefused = "reversal_refused"
	// KindDuplicateRefund is a second refund under one key, which DR-049 does
	// not reverse.
	KindDuplicateRefund = "duplicate_refund"
	// KindUnreconcilable is a tenant-feature a rebuild would not write.
	KindUnreconcilable = "unreconcilable"
)

// Counted reports whether a finding of this kind raises
// quotacore_event_reconcile_mismatch_total, which counts disagreements
// between the hash and the ledger (observability.md section 1.5).
func Counted(kind string) bool {
	switch kind {
	case KindIdentity, KindChain, KindHashBalance, KindHashCycle:
		return true
	}
	return false
}

// openings are the event types whose balance_after opens or reopens a cycle
// (data-model.md section 2.7).
var openings = map[string]bool{"cycle_rolled_over": true, "plan_changed": true, "force_rolled_over": true}

// Row is one ledger row, or one duplicate dead letter, which the store applied
// and the table refused by design until it is reversed (IP-11 D1).
type Row struct {
	ID         string
	Seq        int64 // zero for a dead letter
	Type       string
	Delta      int64
	After      int64
	CycleIndex int64
	// CycleStart is nil for a row written before migration 00010.
	CycleStart *time.Time
	At         time.Time
}

// Before is the balance the row's mutation started from.
func (r Row) Before() int64 { return r.After - r.Delta }

// Cycle is the cycle a row or a hash is in.
type Cycle struct {
	Start time.Time `json:"cycle_start"`
	Index int64     `json:"cycle_index"`
}

func (c Cycle) String() string {
	return c.Start.UTC().Format(time.RFC3339) + "#" + strconv.FormatInt(c.Index, 10)
}

// after reports whether c is later than d: a later window, or the same window
// at a higher index.
func (c Cycle) after(d Cycle) bool {
	if !c.Start.Equal(d.Start) {
		return c.Start.After(d.Start)
	}
	return c.Index > d.Index
}

func (r Row) cycle() (Cycle, bool) {
	if r.CycleStart == nil {
		return Cycle{}, false
	}
	return Cycle{Start: r.CycleStart.UTC(), Index: r.CycleIndex}, true
}

// Hash is a balance hash as the store holds it.
type Hash struct {
	Present bool
	Balance int64
	Limit   int64
	Bonus   int64
	Cycle   Cycle
}

// Subject names a tenant-feature.
type Subject struct {
	TenantID string `json:"tenant_id"`
	Feature  string `json:"feature_key"`
}

// Finding is one disagreement, with what was expected and what was found.
type Finding struct {
	Subject
	Kind     string `json:"kind"`
	Cycle    *Cycle `json:"cycle,omitempty"`
	Expected *int64 `json:"expected,omitempty"`
	Actual   *int64 `json:"actual,omitempty"`
	// Row is the event_id the finding is about, when there is one.
	Row    string `json:"event_id,omitempty"`
	Detail string `json:"detail"`
}

// Fingerprint names the disagreement, so a pass that finds it again records
// nothing new (data-model.md section 2.13).
func (f Finding) Fingerprint() string {
	s := f.Kind + "|" + f.TenantID + "|" + f.Feature + "|" + f.Row
	if f.Cycle != nil {
		s += "|" + f.Cycle.String()
	}
	if f.Actual != nil {
		s += "|" + strconv.FormatInt(*f.Actual, 10)
	}
	return s
}

func ptr(n int64) *int64 { return &n }

// Verified is what Verify read: the findings, and the balance the ledger ends
// at, which a rebuild offers as its estimate.
type Verified struct {
	Findings []Finding
	// Rows is how many rows were read.
	Rows int
	// End is the last row's balance_after and the cycle it is in; HasEnd is
	// false when there were no rows.
	End      int64
	EndCycle *Cycle
	HasEnd   bool
}

// Verify checks one tenant-feature's rows, in the order they were applied as
// near as the ledger records it (created_at, then seq), against each other
// and against the hash. prev, when not nil, is the row before the first one
// given, so a pass can verify only what is new. hash is nil when the caller
// has no hash to compare with.
func Verify(sub Subject, rows []Row, prev *Row, hash *Hash) Verified {
	out := Verified{Rows: len(rows)}
	var cur *int64
	var curCycle *Cycle
	if prev != nil {
		cur = ptr(prev.After)
		if c, ok := prev.cycle(); ok {
			curCycle = &c
		}
	}
	for i := 0; i < len(rows); {
		// One cycle's run of rows, as they are ordered.
		j := i + 1
		c, named := rows[i].cycle()
		for j < len(rows) {
			d, ok := rows[j].cycle()
			if ok != named || (named && d != c) {
				break
			}
			j++
		}
		group := rows[i:j]
		end, broke := chain(group, cur)
		if broke != nil {
			kind := KindIdentity
			if broke.index == 0 && cur != nil && curCycle != nil && named && *curCycle != c {
				kind = KindChain
			}
			r := group[broke.index]
			f := Finding{Subject: sub, Kind: kind, Row: r.ID, Expected: ptr(broke.want), Actual: ptr(r.Before()),
				Detail: fmt.Sprintf("%s %s starts from %d, and the row before it ends at %d", r.Type, r.ID, r.Before(), broke.want)}
			if named {
				cc := c
				f.Cycle = &cc
			}
			out.Findings = append(out.Findings, f)
		}
		cur = &end
		if named {
			cc := c
			curCycle = &cc
		} else {
			curCycle = nil
		}
		i = j
	}
	if len(rows) > 0 {
		out.HasEnd = true
		out.End = *cur
		out.EndCycle = curCycle
	}
	if hash == nil {
		return out
	}
	if !hash.Present {
		if len(rows) > 0 {
			out.Findings = append(out.Findings, Finding{Subject: sub, Kind: KindHashMissing, Row: rows[len(rows)-1].ID,
				Detail: "the ledger has rows and the store has no balance hash"})
		}
		return out
	}
	if len(rows) == 0 {
		return out
	}
	last := rows[len(rows)-1]
	hc := hash.Cycle
	switch {
	case curCycle == nil:
		out.Findings = append(out.Findings, Finding{Subject: sub, Kind: KindNoCycleStart, Row: last.ID,
			Detail: "the last row was written before migration 00010 and names no window"})
	case *curCycle == hc:
		if hash.Balance != out.End {
			out.Findings = append(out.Findings, Finding{Subject: sub, Kind: KindHashBalance, Cycle: &hc, Row: last.ID,
				Expected: ptr(out.End), Actual: ptr(hash.Balance),
				Detail: fmt.Sprintf("the hash holds %d and the ledger ends at %d in the same cycle", hash.Balance, out.End)})
		}
	case hc.after(*curCycle):
		// A later cycle with no rows opened at its limit and has not moved:
		// its opening wrote no row only because the balance it closed
		// already equalled that limit (Q-22).
		if hash.Balance != hash.Limit || hash.Bonus != 0 || out.End != hash.Limit {
			out.Findings = append(out.Findings, Finding{Subject: sub, Kind: KindHashCycle, Cycle: &hc, Row: last.ID,
				Expected: ptr(hash.Limit), Actual: ptr(hash.Balance),
				Detail: fmt.Sprintf("the hash is in cycle %s, after the ledger's last cycle %s, and holds %d of a limit of %d with the ledger ending at %d",
					hc, *curCycle, hash.Balance, hash.Limit, out.End)})
		}
	default:
		out.Findings = append(out.Findings, Finding{Subject: sub, Kind: KindHashCycle, Cycle: &hc, Row: last.ID,
			Actual: ptr(hash.Balance),
			Detail: fmt.Sprintf("the hash is in cycle %s, before the ledger's last cycle %s", hc, *curCycle)})
	}
	return out
}

type breakAt struct {
	index int
	want  int64
}

// chain walks one cycle's rows from start (nil: the first row's own
// starting balance) and returns where the cycle ends and, if a row does not
// follow, the first such row in the order given. Two instances' clocks can
// order two rows a millisecond apart the wrong way, so a run that breaks in
// the order given is chained again by balance before it is called broken: a
// greedy walk that always takes the earliest row starting from the current
// balance (IP-11 D1).
func chain(rows []Row, start *int64) (int64, *breakAt) {
	end, b := inOrder(rows, start)
	if b == nil {
		return end, nil
	}
	if end, ok := byBalance(rows, start); ok {
		return end, nil
	}
	return lastAfter(rows), b
}

func inOrder(rows []Row, start *int64) (int64, *breakAt) {
	cur := rows[0].Before()
	if start != nil {
		cur = *start
	}
	for i, r := range rows {
		if r.Before() != cur {
			return 0, &breakAt{index: i, want: cur}
		}
		cur = r.After
	}
	return cur, nil
}

func byBalance(rows []Row, start *int64) (int64, bool) {
	starts := []int64{}
	if start != nil {
		starts = append(starts, *start)
	} else {
		// With nothing before them, the run starts from a balance that is no
		// other row's balance_after; try each such row's start.
		ends := map[int64]int{}
		for _, r := range rows {
			ends[r.After]++
		}
		seen := map[int64]bool{}
		for _, r := range rows {
			b := r.Before()
			if ends[b] == 0 && !seen[b] {
				seen[b] = true
				starts = append(starts, b)
			}
		}
		if len(starts) == 0 {
			starts = append(starts, rows[0].Before())
		}
	}
	for _, s := range starts {
		used := make([]bool, len(rows))
		cur := s
		ok := true
		for range rows {
			next := -1
			for i, r := range rows {
				if !used[i] && r.Before() == cur {
					next = i
					break
				}
			}
			if next < 0 {
				ok = false
				break
			}
			used[next] = true
			cur = rows[next].After
		}
		if ok {
			return cur, true
		}
	}
	return 0, false
}

// lastAfter is where a broken run ends: the balance_after of its last row in
// the order given, which is what the store most likely holds.
func lastAfter(rows []Row) int64 { return rows[len(rows)-1].After }

// Opening reports whether a row type opens a cycle.
func Opening(eventType string) bool { return openings[eventType] }
