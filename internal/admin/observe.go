package admin

import (
	"context"
	"net/http"
	"runtime"
	"strconv"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/envelope"
)

var (
	eventTypes   = []string{"consumed", "refunded", "granted", "set", "plan_changed", "cycle_rolled_over", "force_rolled_over"}
	eventSources = []string{"api", "admin", "system"}
)

// listAudit reads the audit log newest first, filtered by tenant and time.
func (s *Service) listAudit(w http.ResponseWriter, r *http.Request) {
	var f AuditFilter
	l, ok := s.startList(w, r, "audit", func(q *query) []string {
		f.TenantExternalID = q.str("tenant_external_id", externalIDShape)
		f.From, f.To = q.instant("from"), q.instant("to")
		return []string{f.TenantExternalID, stamp(f.From), stamp(f.To)}
	})
	if !ok {
		return
	}
	rows, err := s.store.ListAudit(r.Context(), f, l.page)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, cur, err := s.next(r, l, len(rows), func(i int) Position { return pos(rows[i].OccurredAt, rows[i].ID) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &contract.AuditEntryList{Entries: rows[:n], NextCursor: cur})
}

func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// listEvents reads one tenant's ledger newest first by (created_at, seq)
// (S10). A tenant that is soft-deleted is still readable here: a billing
// dispute after churn is what the history is kept for (S2).
func (s *Service) listEvents(w http.ResponseWriter, r *http.Request) {
	id, valid := tenantID(w, r)
	if !valid {
		return
	}
	var f EventFilter
	l, ok := s.startList(w, r, "events", func(q *query) []string {
		f.FeatureKey = q.str("feature_key", featureKeyShape)
		f.EventType = q.str("event_type", nil, eventTypes...)
		f.Source = q.str("source", nil, eventSources...)
		f.ClientKey = q.str("idempotency_key", clientKeyShape)
		f.From, f.To = q.instant("from"), q.instant("to")
		return []string{id, f.FeatureKey, f.EventType, f.Source, f.ClientKey, stamp(f.From), stamp(f.To)}
	})
	if !ok {
		return
	}
	page, err := s.store.ListEvents(r.Context(), id, f, l.page)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows := page.Events
	n, cur, err := s.next(r, l, len(rows), func(i int) Position {
		return Position{At: at(rows[i].OccurredAt), Tie: strconv.FormatInt(rows[i].Seq, 10)}
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	loc, err := time.LoadLocation(page.Timezone)
	if err != nil {
		loc = time.UTC
	}
	out := make([]contract.UsageEvent, n)
	for i, e := range rows[:n] {
		ev := e.UsageEvent
		ev.CycleStart, ev.CycleEnd = window(page.Anchor, loc, e)
		out[i] = ev
	}
	respond(w, r, http.StatusOK, &contract.UsageEventList{Events: out, NextCursor: cur})
}

// window recomputes an event's cycle from the tenant's current anchor, zone
// and cadence. It is null for an event older than the anchor, whose window
// was defined by an anchor that has since been replaced, and for a feature
// the tenant no longer holds; the end is null for a never entitlement. It is
// null too where the row's stored cycle_start names a different window: a
// consume applied against a hash the re-anchor drain had not yet rewritten,
// which the instant comparison cannot see (IP-12 S5). The hash holds its
// window in milliseconds, so the two are compared at that precision.
func window(anchor time.Time, loc *time.Location, e Event) (contract.NilTimestamp, contract.NilTimestamp) {
	none := contract.NilTimestamp{Null: true}
	if e.Interval == "" || time.Time(e.OccurredAt).Before(anchor) {
		return none, none
	}
	iv, err := parseInterval(e.Interval)
	if err != nil {
		return none, none
	}
	if iv == cycle.Never {
		if !e.Start.IsZero() && e.Start.UnixMilli() != anchor.UnixMilli() {
			return none, none
		}
		return contract.NewNilTimestamp(envelope.Boundary(anchor, loc)), none
	}
	start := cycle.Add(anchor, iv, loc, e.Index)
	if !e.Start.IsZero() && e.Start.UnixMilli() != start.UnixMilli() {
		return none, none
	}
	end := cycle.Add(anchor, iv, loc, e.Index+1)
	return contract.NewNilTimestamp(envelope.Boundary(start, loc)), contract.NewNilTimestamp(envelope.Boundary(end, loc))
}

// serveStatus reports each dependency as data at 200 (S9): a probe that fails
// is a row with healthy false, because a dependency being down is the one
// thing this route exists to say. Its refusals are bare, as the contract
// lists no codes for it.
func (s *Service) serveStatus(w http.ResponseWriter, r *http.Request) {
	deps := []contract.DependencyHealth{
		s.probe(r.Context(), contract.DependencyHealthNameRedis, s.data.Ping),
		s.probe(r.Context(), contract.DependencyHealthNamePostgres, s.store.Ping),
	}
	status := contract.StatusStatusOk
	for _, d := range deps {
		if !d.Healthy {
			status = contract.StatusStatusDegraded
		}
	}
	var cache contract.CacheStats
	if s.status.Cache != nil {
		cache = s.status.Cache()
	}
	now := accepted(r)
	uptime := int64(0)
	if !s.status.Started.IsZero() {
		uptime = int64(now.Sub(s.status.Started) / time.Second)
	}
	// The ledger's queue, writer and drops, for this instance (IP-12 D8). With
	// no ledger wired, the honest values are an empty queue, a stopped writer
	// and no drops.
	depth, worker, dropped := int64(0), contract.StatusWorkerStopped, int64(0)
	if l := s.status.Ledger; l != nil {
		depth, worker, dropped = int64(l.Depth()), contract.StatusWorker(l.State()), l.Dropped()
	}
	mode := contract.StatusModeNormal
	if s.readOnly() {
		mode = contract.StatusModeReadonly
	}
	respond(w, r, http.StatusOK, &contract.Status{
		Status:  status,
		Mode:    mode,
		Version: s.status.Version,
		Commit:  s.status.Commit,
		// The zone database is the one embedded by the toolchain that built
		// the binary (time/tzdata), so the toolchain's release names it.
		ZoneDataVersion:    runtime.Version(),
		ServerTime:         contract.Timestamp(now.UTC()),
		UptimeS:            uptime,
		Dependencies:       deps,
		Cache:              cache,
		QueueDepth:         depth,
		Worker:             worker,
		LedgerDroppedTotal: dropped,
	})
}

func (s *Service) probe(ctx context.Context, name contract.DependencyHealthName, ping func(context.Context) error) contract.DependencyHealth {
	ctx, cancel := context.WithTimeout(ctx, s.status.ProbeTimeout)
	defer cancel()
	start := time.Now()
	err := ping(ctx)
	d := contract.DependencyHealth{Name: name, Healthy: err == nil}
	if err != nil {
		d.LatencyMs = contract.NilFloat64{Null: true}
		d.Error = contract.NewNilString("the probe did not succeed within its timeout")
		if ctx.Err() == nil {
			d.Error = contract.NewNilString("the probe failed: the dependency refused or dropped the connection")
		}
		return d
	}
	d.LatencyMs = contract.NewNilFloat64(float64(time.Since(start).Microseconds()) / 1000)
	d.Error = contract.NilString{Null: true}
	return d
}
