package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/reconcile"
)

var errRefusedByTest = errors.New("refused by the test")

// faulty is the ledger's appender with three faults keyed by idempotency
// key: drop- is lost silently, as by a writer that crashed holding it; gap-
// is refused and cannot be dead-lettered either, so the writer drops it and
// tallies a gap (IP-11 D8); rej- is refused and dead-lettered as rejected.
type faulty struct{ ledger.Appender }

func (f faulty) Append(ctx context.Context, events []dataplane.Event) (ledger.Appended, error) {
	kept := events[:0:0]
	for _, e := range events {
		switch {
		case strings.HasPrefix(e.ClientKey, "drop-"):
			continue
		case strings.HasPrefix(e.ClientKey, "gap-"), strings.HasPrefix(e.ClientKey, "rej-"):
			return ledger.Appended{}, errRefusedByTest
		}
		kept = append(kept, e)
	}
	if len(kept) == 0 {
		return ledger.Appended{}, nil
	}
	return f.Appender.Append(ctx, kept)
}

func (f faulty) Permanent(err error) bool {
	return errors.Is(err, errRefusedByTest) || f.Appender.Permanent(err)
}

func (f faulty) DeadLetter(ctx context.Context, ev dataplane.Event, cause error) (bool, error) {
	if strings.HasPrefix(ev.ClientKey, "gap-") {
		return false, errRefusedByTest
	}
	return f.Appender.DeadLetter(ctx, ev, cause)
}

// scene is the restore drill's shape (testing-strategy section 9 steps 1 to
// 4): a seeded workload over many tenants, intervals and zones, four
// injected faults on two fresh tenants, a snapshot of every hash, and then
// the fast store lost, every tenant key gone but one hash kept to show that
// a present hash is never rewritten.
type scene struct {
	s        *stack
	tenants  []tenant
	features []string
	pre      map[string]map[string]string
	injected map[reconcile.Subject]string
	keep     reconcile.Subject
	x, y     tenant
}

func (sc *scene) hashKey(sub reconcile.Subject) string {
	for _, tn := range sc.tenants {
		if tn.ID == sub.TenantID {
			return sc.s.balanceKey(tn, sub.Feature)
		}
	}
	sc.s.t.Fatalf("no tenant %s", sub.TenantID)
	return ""
}

func (sc *scene) hash(sub reconcile.Subject) map[string]string {
	return sc.s.rdb.HGetAll(context.Background(), sc.hashKey(sub)).Val()
}

func newScene(t *testing.T) *scene {
	t.Helper()
	s := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { return faulty{l} }
	})
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	tenants, features, _ := s.workload(t, rand.New(rand.NewSource(seed)), []string{"UTC", "America/New_York", "Asia/Kolkata"},
		6, 400)
	hourly, monthly, never := features[0], features[1], features[2]
	// Every tenant-feature gets a row in its current cycle, so none rests on
	// a limit no row records.
	for _, tn := range tenants {
		for _, f := range features {
			until(t, "", func() response { return s.consume(s.runtime, f, 1, tn.External, newKey()) })
		}
	}
	fresh := func() tenant {
		tn := s.tenant(externalID("rb"), "Europe/Berlin", monthlyAnchor, ent{hourly, 100, "hourly", false},
			ent{monthly, 200, "monthly", false}, ent{never, 300, "never", false})
		for _, f := range features {
			s.seedCurrent(tn, f, tn.ents[f].limit)
		}
		return tn
	}
	x, y := fresh(), fresh()
	// A 503 from the bounded-miss limiter applied nothing, so the same key
	// is sent again.
	consume := func(tn tenant, f string, amount int64, key string) {
		t.Helper()
		until(t, "", func() response { return s.consume(s.runtime, f, amount, tn.External, key) })
	}
	consume(x, monthly, 5, newKey())
	consume(x, monthly, 3, "drop-"+newKey())
	consume(x, monthly, 2, newKey())
	consume(x, hourly, 4, newKey())
	consume(x, hourly, 6, "gap-"+newKey())
	consume(y, monthly, 3, newKey())
	consume(y, monthly, 5, "rej-"+newKey())
	consume(y, hourly, 3, newKey())
	consume(y, hourly, 4, newKey())
	consume(x, never, 1, newKey())
	consume(y, never, 1, newKey())
	s.flush()
	if _, err := s.seedDB.Exec(`UPDATE usage_events SET balance_after = balance_after + 1 WHERE id = (
		SELECT u.id FROM usage_events u JOIN features f ON f.id = u.feature_id
		 WHERE u.tenant_id = $1 AND f.key = $2 ORDER BY u.seq LIMIT 1)`, y.ID, hourly); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.rows(`SELECT count(*) FROM usage_event_gaps WHERE tenant_id = $1::uuid`, x.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the writer recorded no gap for the dropped event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sc := &scene{s: s, tenants: append(append([]tenant(nil), tenants...), x, y), features: features, x: x, y: y,
		pre: map[string]map[string]string{},
		injected: map[reconcile.Subject]string{
			{TenantID: x.ID, Feature: monthly}: reconcile.ClassUnreconcilable,
			{TenantID: x.ID, Feature: hourly}:  reconcile.ClassUnverified,
			{TenantID: y.ID, Feature: monthly}: reconcile.ClassUnreconcilable,
			{TenantID: y.ID, Feature: hourly}:  reconcile.ClassUnreconcilable,
		},
		keep: reconcile.Subject{TenantID: tenants[0].ID, Feature: hourly},
	}
	ctx := context.Background()
	for _, tn := range sc.tenants {
		for _, f := range features {
			k := s.balanceKey(tn, f)
			sc.pre[k] = s.rdb.HGetAll(ctx, k).Val()
			if exp := s.rdb.PExpireTime(ctx, k).Val(); exp != -1 {
				t.Fatalf("%s carries an expiry, %d", k, exp)
			}
		}
	}
	// The store is lost: every tenant key but one.
	keep := sc.hashKey(sc.keep)
	for _, tn := range sc.tenants {
		for _, k := range s.rdb.Keys(ctx, "qc:{t:"+tn.ID+"}:*").Val() {
			if k != keep {
				s.rdb.Del(ctx, k)
			}
		}
	}
	return sc
}

func entriesBy(rep rebuildReport) map[reconcile.Subject]rebuildEntry {
	out := map[reconcile.Subject]rebuildEntry{}
	for _, e := range rep.Entries {
		out[e.Subject] = e
	}
	return out
}

func fields(st *reconcile.State) map[string]string {
	end := ""
	if st.Window.End != nil {
		end = strconv.FormatInt(st.Window.End.UnixMilli(), 10)
	}
	return map[string]string{"balance": strconv.FormatInt(st.Balance, 10), "limit": strconv.FormatInt(st.Limit, 10),
		"bonus": strconv.FormatInt(st.Bonus, 10), "cycle_index": strconv.FormatInt(st.Window.Index, 10),
		"window_start": strconv.FormatInt(st.Window.Start.UnixMilli(), 10), "window_end": end}
}

// TestTheRebuildIsAccurateInBothDirections is IP-11 DoD 5: after the store
// is lost, the rebuild flags exactly the injected faults, writes every other
// hash exactly as it was in all six fields and with no expiry, writes none
// it flags, and records an unreconcilable finding for each it calls so. A
// crashed writer's session makes every cycle it covers unverified, and a
// retention shorter than a `never` feature's age makes it unreconcilable
// (D7, D8, S9).
func TestTheRebuildIsAccurateInBothDirections(t *testing.T) {
	sc := newScene(t)
	s := sc.s
	ctx := context.Background()
	s.app.Close()
	c, _ := s.commands()

	// A crashed writer: a session left open and unseen for longer than a
	// rebuild waits, which covers every tenant up to its last sight.
	crash := uuid.New()
	if _, err := s.seedDB.Exec(`INSERT INTO ledger_sessions (id, host, started_at, settled_through, last_seen_at)
		VALUES ($1, 'crashed', now() - interval '1 hour', now() - interval '30 minutes', now() - interval '61 seconds')`,
		crash); err != nil {
		t.Fatal(err)
	}
	var crashTo time.Time
	if err := s.seedDB.QueryRow(`SELECT last_seen_at + interval '1 minute' FROM ledger_sessions WHERE id = $1`,
		crash).Scan(&crashTo); err != nil {
		t.Fatal(err)
	}
	dry, _, err := c.rebuild(ctx, rebuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for _, e := range dry.Entries {
		if _, bad := sc.injected[e.Subject]; bad || e.Class == reconcile.ClassPresent {
			continue
		}
		// Every cycle the crash may have written into is unverified; one a
		// re-anchor opened after the crashed writer was last seen is not.
		want := reconcile.ClassExact
		if !e.Estimate.Window.Start.After(crashTo) {
			want = reconcile.ClassUnverified
			covered++
		}
		if e.Class != want || (want == reconcile.ClassUnverified && !strings.Contains(e.Reason, "crash")) {
			t.Errorf("with a crashed writer, %s:%s opened at %s is %s (%s), want %s", e.TenantID, e.Feature,
				e.Estimate.Window.Start, e.Class, e.Reason, want)
		}
	}
	if covered == 0 {
		t.Errorf("the crash covered %d of %d tenant-features", covered, len(dry.Entries))
	}
	if _, err := s.seedDB.Exec(`DELETE FROM ledger_sessions WHERE id = $1`, crash); err != nil {
		t.Fatal(err)
	}

	// A retention shorter than the `never` features' age.
	c.retention = 2 * time.Hour
	short, _, err := c.rebuild(ctx, rebuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byShort := entriesBy(short)
	for _, tn := range []tenant{sc.x, sc.y} {
		e := byShort[reconcile.Subject{TenantID: tn.ID, Feature: sc.features[2]}]
		if e.Class != reconcile.ClassUnreconcilable || !strings.Contains(e.Reason, "retention") {
			t.Errorf("a `never` feature older than retention is %s (%s)", e.Class, e.Reason)
		}
	}
	c.retention = 0

	rep, code, err := c.rebuild(ctx, rebuildOptions{apply: true, reason: "the restore drill"})
	if err != nil {
		t.Fatal(err)
	}
	if code != exitFinding {
		t.Errorf("a rebuild that left hashes flagged exited %d", code)
	}
	flagged := map[reconcile.Subject]string{}
	var unreconcilable []string
	for _, e := range rep.Entries {
		key := sc.hashKey(e.Subject)
		got := sc.hash(e.Subject)
		switch e.Class {
		case reconcile.ClassExact:
			if e.Written == nil || e.Action != "written" {
				t.Errorf("exact %s was not written: %+v", key, e)
				continue
			}
			if mustJSON(got) != mustJSON(sc.pre[key]) {
				t.Errorf("%s rebuilt as %v, was %v before the loss", key, got, sc.pre[key])
			}
			if mustJSON(fields(e.Written)) != mustJSON(got) {
				t.Errorf("%s: the report says %v and the store holds %v", key, fields(e.Written), got)
			}
			if exp := s.rdb.PExpireTime(ctx, key).Val(); exp != -1 {
				t.Errorf("%s was rebuilt with an expiry, %d", key, exp)
			}
		case reconcile.ClassPresent:
			if e.Subject != sc.keep || mustJSON(got) != mustJSON(sc.pre[key]) || e.Written != nil {
				t.Errorf("present %s: %+v, holds %v", key, e, got)
			}
		default:
			flagged[e.Subject] = e.Class
			if len(got) != 0 || e.Written != nil {
				t.Errorf("flagged %s (%s) was written: %v", key, e.Class, got)
			}
			if e.Class == reconcile.ClassUnreconcilable {
				unreconcilable = append(unreconcilable, e.TenantID+":"+e.Feature)
			}
		}
	}
	if mustJSON(flagged) != mustJSON(sc.injected) {
		t.Errorf("the rebuild flagged %v, the faults were %v", flagged, sc.injected)
	}
	if n := len(rep.Entries); n != len(sc.tenants)*len(sc.features) {
		t.Errorf("the report has %d entries, want %d", n, len(sc.tenants)*len(sc.features))
	}
	var recorded []string
	for _, f := range s.findings() {
		if f.Kind == reconcile.KindUnreconcilable && f.FoundBy == "rebuild" {
			recorded = append(recorded, *f.TenantID+":"+*f.FeatureKey)
		}
	}
	sort.Strings(unreconcilable)
	sort.Strings(recorded)
	if mustJSON(recorded) != mustJSON(unreconcilable) {
		t.Errorf("the findings record %v unreconcilable, the report %v", recorded, unreconcilable)
	}
	written := map[string]bool{}
	for _, e := range rep.Entries {
		if e.Written != nil {
			written[e.TenantID] = true
		}
	}
	if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'tenant.rebuild' AND actor = 'cli:test-host'
		AND request_id = $1`, rep.RequestID); n != len(written) {
		t.Errorf("%d tenant.rebuild audit rows for %d tenants written", n, len(written))
	}
}

// TestNoPathGrantsAFullAllowanceDuringARebuild is IP-11 DoD 6: while a
// read-only fleet serves and a rebuild runs, no sample of any tenant key is
// above its balance before the loss, no flagged hash appears, no route or job
// materialises one, a second rebuild at the same time writes nothing, and the
// hash that survived is never rewritten. Then the operator's two actions each
// write their one tenant-feature, audited, and --set keeps the cycle
// reconciling (DR-045, testing-strategy section 9 step 6).
func TestNoPathGrantsAFullAllowanceDuringARebuild(t *testing.T) {
	sc := newScene(t)
	s := sc.s
	ctx := context.Background()
	s.app.Close()
	r := s.readOnlyPeer()
	hourly, monthly := sc.features[0], sc.features[1]
	preBalance := func(key string) int64 {
		n, _ := strconv.ParseInt(sc.pre[key]["balance"], 10, 64)
		return n
	}

	var mu sync.Mutex
	var violations []string
	violate := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(violations) < 20 {
			violations = append(violations, fmt.Sprintf(format, args...))
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, tn := range sc.tenants {
				for _, f := range sc.features {
					sub := reconcile.Subject{TenantID: tn.ID, Feature: f}
					key := sc.hashKey(sub)
					h := s.rdb.HGetAll(ctx, key).Val()
					if len(h) == 0 {
						continue
					}
					if _, flagged := sc.injected[sub]; flagged {
						violate("flagged %s present: %v", key, h)
					}
					if b, _ := strconv.ParseInt(h["balance"], 10, 64); b > preBalance(key) {
						violate("%s holds %d, above its %d before the loss", key, b, preBalance(key))
					}
					if sub == sc.keep && mustJSON(h) != mustJSON(sc.pre[key]) {
						violate("the surviving hash %s was rewritten: %v", key, h)
					}
				}
			}
		}
	}()
	other := uniq("po")
	s.createPlan(other, 50, "monthly", hourly)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(1))
		for {
			select {
			case <-stop:
				return
			default:
			}
			tn := sc.tenants[rng.Intn(len(sc.tenants))]
			f := sc.features[rng.Intn(len(sc.features))]
			for name, call := range writes(f, uniq("np"), other, tn, 1) {
				if got := call(r); got.Status != http.StatusServiceUnavailable ||
					!strings.Contains(string(got.Body), "service_unavailable") {
					violate("%s on the read-only fleet answered %d %s", name, got.Status, got.Body)
				}
			}
		}
	}()

	// Two rebuilds at once: one takes the fence, the other writes nothing.
	c1, _ := s.commands()
	c2, _ := s.commands()
	type result struct {
		rep rebuildReport
		err error
	}
	results := make(chan result, 2)
	for _, c := range []*commands{c1, c2} {
		go func(c *commands) {
			rep, _, err := c.rebuild(ctx, rebuildOptions{apply: true, reason: "the restore drill"})
			results <- result{rep, err}
		}(c)
	}
	var reps []rebuildReport
	for range 2 {
		res := <-results
		switch {
		case res.err != nil && strings.Contains(res.err.Error(), "holds the fence"):
		case res.err != nil:
			t.Fatal(res.err)
		default:
			reps = append(reps, res.rep)
		}
	}
	writers := 0
	for _, rep := range reps {
		for _, e := range rep.Entries {
			if e.Written != nil {
				writers++
				break
			}
		}
	}
	if len(reps) == 0 || writers != 1 {
		t.Errorf("of two concurrent rebuilds, %d ran and %d wrote", len(reps), writers)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	for _, v := range violations {
		t.Error(v)
	}

	// The operator's actions, each on one tenant-feature.
	before := s.tenantKeys(idsOf(sc.tenants)...)
	accept := reconcile.Subject{TenantID: sc.x.ID, Feature: hourly}
	set := reconcile.Subject{TenantID: sc.y.ID, Feature: monthly}
	c, _ := s.commands()
	rep, _, err := c.rebuild(ctx, rebuildOptions{acceptRefs: []string{sc.x.External + ":" + hourly},
		reason: "the customer agreed to the estimate"})
	if err != nil {
		t.Fatal(err)
	}
	e := entriesBy(rep)[accept]
	if e.Action != "accepted" || e.Estimate == nil || mustJSON(sc.hash(accept)) != mustJSON(fields(e.Estimate)) {
		t.Fatalf("--accept: %+v, holds %v", e, sc.hash(accept))
	}
	onlyChanged(t, before, s.tenantKeys(idsOf(sc.tenants)...), sc.hashKey(accept))
	before = s.tenantKeys(idsOf(sc.tenants)...)
	value := preBalance(sc.hashKey(set)) - 1
	rep, _, err = c.rebuild(ctx, rebuildOptions{setRefs: map[string]int64{sc.y.External + ":" + monthly: value},
		reason: "counted from the customer's own records"})
	if err != nil {
		t.Fatal(err)
	}
	e = entriesBy(rep)[set]
	if e.Action != "set" || sc.hash(set)["balance"] != strconv.FormatInt(value, 10) {
		t.Fatalf("--set: %+v, holds %v", e, sc.hash(set))
	}
	onlyChanged(t, before, s.tenantKeys(idsOf(sc.tenants)...), sc.hashKey(set))
	// The set row keeps the cycle reconciling against the hash it wrote.
	rows, _, err := db.NewReconciler(s.seedDB).Rows(ctx, set, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := balances{r.app.Data}.Hash(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if last := rows[len(rows)-1]; last.Type != "set" || last.After != value {
		t.Errorf("the last row after --set is %s to %d", last.Type, last.After)
	}
	if v := reconcile.Verify(set, rows, nil, &hash); len(v.Findings) != 0 {
		t.Errorf("after --set the cycle does not reconcile: %s", mustJSON(v.Findings))
	}
	for _, sub := range []reconcile.Subject{accept, set} {
		if n := s.rows(`SELECT count(*) FROM audit_log WHERE action = 'tenant.rebuild' AND subject_id = $1
			AND reason IS NOT NULL AND after::text LIKE '%' || $2 || '%'`, sub.TenantID, sub.Feature); n != 1 {
			t.Errorf("%d audit rows for the operator's action on %s:%s", n, sub.TenantID, sub.Feature)
		}
	}
	// A present hash refuses both actions.
	if rep, code, err := c.rebuild(ctx, rebuildOptions{setRefs: map[string]int64{sc.y.External + ":" + monthly: 0},
		reason: "again"}); err != nil || code != exitFinding || entriesBy(rep)[set].Error == "" {
		t.Errorf("--set on a present hash: exit %d, %v, %+v", code, err, entriesBy(rep)[set])
	}
}

func idsOf(tenants []tenant) []string {
	var out []string
	for _, tn := range tenants {
		out = append(out, tn.ID)
	}
	return out
}

func onlyChanged(t *testing.T, before, after map[string]string, key string) {
	t.Helper()
	for k, v := range after {
		if k != key && before[k] != v {
			t.Errorf("%s changed: %s, was %s", k, v, before[k])
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Errorf("%s was deleted", k)
		}
	}
	if _, ok := after[key]; !ok {
		t.Errorf("%s was not written", key)
	}
}
