package reconcile

import (
	"fmt"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
)

// The classes a rebuild puts an absent hash in (IP-11 D7). Only an exact one
// is written without an operator; the others stay absent and keep answering
// 503 (DR-045) until an operator accepts the estimate or sets a value.
const (
	// ClassExact is a hash the ledger determines in all six fields.
	ClassExact = "exact"
	// ClassUnverified reconciles, but the ledger may have lost an event that
	// would change it: a recorded gap or crashed writer covers the cycle, or
	// the operator restored Postgres while it was open.
	ClassUnverified = "unverified"
	// ClassUnreconcilable is a hash the ledger cannot determine.
	ClassUnreconcilable = "unreconcilable"
	// ClassPresent is a hash the store still holds. It is verified and never
	// rewritten.
	ClassPresent = "present"
)

// Window is a cycle window: its index under the tenant's anchor, its start,
// and its end, which is nil for an entitlement that never resets.
type Window struct {
	Index int64      `json:"cycle_index"`
	Start time.Time  `json:"window_start"`
	End   *time.Time `json:"window_end"`
}

// State is the six fields of a balance hash.
type State struct {
	Balance int64  `json:"balance"`
	Limit   int64  `json:"limit"`
	Bonus   int64  `json:"bonus"`
	Window  Window `json:"window"`
}

// Target is one tenant-feature the rebuild enumerates, with what the
// configuration says about it now: the effective limit and the cadence it is
// anchored on (DR-018, DR-002).
type Target struct {
	Subject
	ExternalID string
	Anchor     time.Time
	Interval   cycle.Interval
	Location   *time.Location
	Limit      int64
	Now        time.Time
	// Horizon is the oldest instant event retention still keeps; rows
	// created before it may have been deleted. Zero keeps everything.
	Horizon time.Time
	// ConfigChanged is the last instant the tenant's or its plan's
	// configuration changed after its creation, or zero.
	ConfigChanged time.Time
}

// Loss is something the ledger records it may have lost (IP-11 D8): a gap
// the writer tallied, a crashed writer's session, or the instant Postgres was
// restored to. A loss with no tenant covers every tenant-feature.
type Loss struct {
	TenantID string
	Feature  string
	Reason   string
	From     time.Time
	To       time.Time
}

func (l Loss) covers(sub Subject, from time.Time) bool {
	if l.TenantID != "" && (l.TenantID != sub.TenantID || (l.Feature != "" && l.Feature != sub.Feature)) {
		return false
	}
	return !l.To.Before(from)
}

// Evidence is what the rebuild read about one tenant-feature.
type Evidence struct {
	Rows []Row
	Hash Hash
	// Losses are the gaps and crashed sessions the ledger records.
	Losses []Loss
	// Rejected counts the unresolved rejected dead letters that name it: an
	// event the store applied and the ledger refused.
	Rejected int
}

// Rebuilt is the rebuild's answer for one tenant-feature, which is also its
// entry in the report.
type Rebuilt struct {
	Subject
	ExternalID string `json:"external_id"`
	Class      string `json:"class"`
	Reason     string `json:"reason"`
	// Estimate is the hash the ledger implies, which an exact rebuild writes
	// and --accept writes for any other. Nil when the ledger implies none.
	Estimate *State `json:"estimate,omitempty"`
	// Current is the window the cycle engine computes for now.
	Current  Window    `json:"window"`
	Rows     int       `json:"rows"`
	Findings []Finding `json:"findings,omitempty"`
}

func (t Target) window(index int64) Window {
	if t.Interval == cycle.Never {
		return Window{Index: 0, Start: t.Anchor}
	}
	end := cycle.Add(t.Anchor, t.Interval, t.Location, index+1)
	return Window{Index: index, Start: cycle.Add(t.Anchor, t.Interval, t.Location, index), End: &end}
}

func (t Target) current() Window {
	w := cycle.CurrentWindow(t.Anchor, t.Interval, t.Location, t.Now)
	return Window{Index: w.Index, Start: w.Start, End: w.End}
}

func sameStart(a, b time.Time) bool { return a.UnixMilli() == b.UnixMilli() }

// Classify decides what a rebuild may write for one tenant-feature (IP-11
// D7). It reads the ledger's last cycle back into the six fields: the
// balance is where the rows end; the limit is what the cycle opened at,
// moved by every relimit since; the bonus is every grant since the opening;
// and the window is the last row's, unless the hash has since rolled into a
// later window at the limit with no row to show it (Q-22), which is also
// what the next request's transition would leave.
func Classify(t Target, ev Evidence) Rebuilt {
	out := Rebuilt{Subject: t.Subject, ExternalID: t.ExternalID, Current: t.current(), Rows: len(ev.Rows)}
	if ev.Hash.Present {
		v := Verify(t.Subject, ev.Rows, nil, &ev.Hash)
		out.Class, out.Findings = ClassPresent, v.Findings
		out.Reason = "the store holds the hash; it is verified and never rewritten"
		return out
	}
	v := Verify(t.Subject, ev.Rows, nil, nil)
	out.Findings = v.Findings
	if len(ev.Rows) == 0 {
		return t.noRows(out, ev)
	}
	est, opened, reason := t.estimate(ev.Rows, v)
	out.Estimate = est
	switch {
	case len(v.Findings) > 0:
		return unreconcilable(out, fmt.Sprintf("the ledger does not reconcile: %s", v.Findings[0].Detail))
	case ev.Rejected > 0:
		return unreconcilable(out, fmt.Sprintf("%d rejected dead letters name it: the store applied events the ledger refused", ev.Rejected))
	case reason != "":
		return unreconcilable(out, reason)
	case !t.Horizon.IsZero() && opened.Before(t.Horizon):
		return unreconcilable(out, fmt.Sprintf("its cycle opened at %s, before the retention horizon %s, with no "+
			"opening row, so rows it needs may have been deleted; keep event retention above the longest interval in use "+
			"(deployment.md section 4.4)", opened.UTC().Format(time.RFC3339), t.Horizon.UTC().Format(time.RFC3339)))
	}
	// From the start of the ledger's last cycle, not the window written: an
	// event lost at the end of that cycle is what a roll to the limit would
	// otherwise hide.
	if l, ok := t.lost(ev.Losses, v.EndCycle.Start); ok {
		out.Class = ClassUnverified
		out.Reason = fmt.Sprintf("the ledger records a possible loss (%s, %s to %s) inside the cycle", l.Reason,
			l.From.UTC().Format(time.RFC3339), l.To.UTC().Format(time.RFC3339))
		return out
	}
	out.Class, out.Reason = ClassExact, "the ledger determines all six fields"
	return out
}

func unreconcilable(out Rebuilt, reason string) Rebuilt {
	out.Class, out.Reason = ClassUnreconcilable, reason
	return out
}

func (t Target) lost(losses []Loss, from time.Time) (Loss, bool) {
	for _, l := range losses {
		if l.covers(t.Subject, from) {
			return l, true
		}
	}
	return Loss{}, false
}

// noRows is a tenant-feature with no ledger row at all: provisioned at its
// limit and never moved, or moved only by rollovers that changed nothing.
func (t Target) noRows(out Rebuilt, ev Evidence) Rebuilt {
	cur := out.Current
	out.Estimate = &State{Balance: t.Limit, Limit: t.Limit, Window: cur}
	switch {
	case ev.Rejected > 0:
		return unreconcilable(out, fmt.Sprintf("%d rejected dead letters name it: the store applied events the ledger refused", ev.Rejected))
	case !t.Horizon.IsZero() && cur.Start.Before(t.Horizon):
		return unreconcilable(out, fmt.Sprintf("it has no rows and its cycle opened at %s, before the retention horizon "+
			"%s, so its rows may have been deleted; keep event retention above the longest interval in use "+
			"(deployment.md section 4.4)", cur.Start.UTC().Format(time.RFC3339), t.Horizon.UTC().Format(time.RFC3339)))
	}
	// With no row to say which cycle the hash is in, a loss at any time
	// retention keeps may be the one event that moved it.
	if l, ok := t.lost(ev.Losses, t.Horizon); ok {
		out.Class = ClassUnverified
		out.Reason = fmt.Sprintf("it has no rows and the ledger records a possible loss (%s, %s to %s) inside the cycle",
			l.Reason, l.From.UTC().Format(time.RFC3339), l.To.UTC().Format(time.RFC3339))
		return out
	}
	if !t.ConfigChanged.IsZero() && !t.ConfigChanged.Before(cur.Start) {
		out.Class = ClassUnverified
		out.Reason = "it has no rows and its configuration changed inside the cycle, so the limit it was provisioned at is not the effective one"
		return out
	}
	out.Class, out.Reason = ClassExact, "it has no rows: the hash opened at its limit and has not moved"
	return out
}

// estimate reads the six fields from the last cycle's rows. It returns the
// instant the hash entered the cycle, which is the cycle's start when no
// opening row is there to name it, and a reason when the ledger cannot name
// a field.
func (t Target) estimate(rows []Row, v Verified) (*State, time.Time, string) {
	if v.EndCycle == nil {
		return nil, time.Time{}, "its last row was written before migration 00010 and names no window"
	}
	c := *v.EndCycle
	first := len(rows)
	for first > 0 {
		pc, ok := rows[first-1].cycle()
		if !ok || pc != c {
			break
		}
		first--
	}
	group := rows[first:]
	// The cycle opens at its opening row, or, with none, at the balance its
	// first row started from, which was the limit: a provisioning or a
	// rollover that moved nothing.
	limit, bonus := group[0].Before(), int64(0)
	opened, known := c.Start, true
	if openings[group[0].Type] {
		opened = group[0].At
	}
	for i, r := range group {
		switch {
		case r.Type == "granted":
			bonus += r.Delta
		case r.Type == "cycle_rolled_over" || r.Type == "force_rolled_over" || (r.Type == "plan_changed" && i == 0):
			// A rollover, a force-rollover or a re-anchor opens at the limit
			// with no bonus; a relimit that is the cycle's first row moved a
			// balance that was its limit, so it opens there too.
			limit, bonus = r.After, 0
		case r.Type == "plan_changed":
			// A relimit moves the balance by the change in the limit,
			// floored at zero; floored, the change is unknown.
			if r.After == 0 {
				known = false
			}
			limit += r.Delta
		}
	}
	est := &State{Balance: v.End, Limit: limit, Bonus: bonus, Window: Window{Index: c.Index, Start: c.Start}}
	if !known {
		return est, opened, "a relimit in its cycle emptied the balance, so the limit it set is unknown"
	}
	cur := t.current()
	w := t.window(c.Index)
	switch {
	case sameStart(c.Start, cur.Start) && c.Index == cur.Index:
		est.Window = cur
	case !sameStart(w.Start, c.Start):
		return est, opened, fmt.Sprintf("its last cycle %s is not a window of the tenant's anchor", c)
	case cur.Start.After(c.Start) || (sameStart(cur.Start, c.Start) && cur.Index > c.Index):
		if v.End == t.Limit {
			// A later window opened at the limit with nothing to record
			// (Q-22), or will at the next request: the same hash.
			est = &State{Balance: t.Limit, Limit: t.Limit, Window: cur}
		} else {
			est.Window = w
		}
	default:
		return est, opened, fmt.Sprintf("its last cycle %s is after the window the cycle engine computes for now", c)
	}
	return est, opened, ""
}
