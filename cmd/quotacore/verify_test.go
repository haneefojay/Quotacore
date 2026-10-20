package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/reconcile"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// commands builds the local operations as `quotacore <command>` would, as
// a separate process: its own Postgres pool and its own store client, never
// the stack's, so a read-only stack does not make the rebuild read-only.
func (s *stack) commands() (*commands, *bytes.Buffer) {
	s.t.Helper()
	ctx := context.Background()
	_, redisURL := integrationURLs(s.t)
	conn, err := db.Open(ctx, s.dbURL)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { conn.Close() })
	data, err := store.Open(store.Options{URL: redisURL, DataScriptTimeout: 250 * time.Millisecond})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { data.Close() })
	runner, err := script.New(script.Options{Client: data.Client(), Timeout: time.Second})
	if err != nil {
		s.t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return &commands{db: conn, data: data, scripts: runner, rec: db.NewReconciler(conn), loader: db.NewLoader(conn, data),
		settle: s.settle, host: "test-host", now: time.Now, out: out, grace: 300 * time.Millisecond}, out
}

// verifyAll runs `quotacore verify-all` and reads what it printed.
func (s *stack) verifyAll() (int, verified) {
	s.t.Helper()
	c, out := s.commands()
	code, err := c.run(context.Background(), []string{"verify-all"})
	if err != nil {
		s.t.Fatalf("verify-all: %v", err)
	}
	var v verified
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		s.t.Fatalf("verify-all printed %q: %v", out.String(), err)
	}
	return code, v
}

type found struct{ kind, tenant, feature string }

func foundOf(fs []reconcile.Finding) []found {
	var out []found
	for _, f := range fs {
		out = append(out, found{f.Kind, f.TenantID, f.Feature})
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
	return out
}

// TestT11EveryCycleReconcilesAndAMismatchIsReported is IP-11 DoD 1 and T-11:
// over a seeded dataset in three zones, verify-all and the pass find
// nothing; then five faults, one per tenant-feature, are each found under
// their own kind and tenant-feature, counted once across two passes, and
// corrected by nothing (DR-042, INV-C4, D1).
func TestT11EveryCycleReconcilesAndAMismatchIsReported(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.appender = func(l *db.Ledger) ledger.Appender { return dropping{l} }
	})
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	tenants, features, _ := s.workload(t, rand.New(rand.NewSource(seed)), []string{"UTC", "America/New_York", "Asia/Kolkata"},
		6, 600)
	hourly, monthly, never := features[0], features[1], features[2]
	s.flush()
	if code, v := s.verifyAll(); code != exitClean || len(v.Findings) != 0 || v.Verified < len(tenants)*len(features) {
		t.Fatalf("verify-all over the dataset (seed %d): exit %d, verified %d, findings %s", seed, code, v.Verified,
			mustJSON(v.Findings))
	}
	if r := s.pass(); len(r.Confirmed) != 0 {
		t.Fatalf("the pass over the dataset (seed %d) confirmed %s", seed, mustJSON(r.Confirmed))
	}

	// The faults go to two fresh tenants, so each is the only thing wrong
	// with its tenant-feature.
	fresh := func() tenant {
		tn := s.tenant(externalID("t11"), "UTC", monthlyAnchor, ent{hourly, 100, "hourly", false},
			ent{monthly, 200, "monthly", false}, ent{never, 300, "never", false})
		for _, f := range features {
			s.seedCurrent(tn, f, tn.ents[f].limit)
		}
		return tn
	}
	a, b := fresh(), fresh()
	consume := func(tn tenant, f string, amount int64, key string) {
		t.Helper()
		mustStatus(t, s.consume(s.runtime, f, amount, tn.External, key), http.StatusOK)
	}
	// An event dropped mid-cycle: the row after it does not follow.
	consume(a, monthly, 10, newKey())
	consume(a, monthly, 5, "drop-"+newKey())
	consume(a, monthly, 3, newKey())
	// An event dropped at the end of a closed cycle: the hash is put in the
	// previous window with an end still ahead, so two requests land there as
	// stale, then the window is given its real end and the next request rolls
	// it, from the balance the ledger never saw.
	cur := s.currentWindow(a, hourly, time.Now())
	prev := s.currentWindow(a, hourly, cur.Start.Add(-time.Millisecond))
	limit := a.ents[hourly].limit
	ahead := time.Now().Add(time.Hour)
	s.seed(a, hourly, stored{Balance: limit, Limit: limit, Index: prev.Index, Start: prev.Start, End: &ahead})
	consume(a, hourly, 10, newKey())
	consume(a, hourly, 5, "drop-"+newKey())
	s.seed(a, hourly, stored{Balance: limit - 15, Limit: limit, Index: prev.Index, Start: prev.Start, End: prev.End})
	consume(a, hourly, 1, newKey())
	// An event dropped at the end of the open cycle.
	consume(b, monthly, 10, newKey())
	consume(b, monthly, 5, "drop-"+newKey())
	// A balance_after altered in place.
	consume(b, hourly, 4, newKey())
	consume(b, hourly, 6, newKey())
	consume(b, hourly, 1, newKey())
	s.flush()
	if _, err := s.seedDB.Exec(`UPDATE usage_events SET balance_after = balance_after + 2 WHERE id = (
		SELECT u.id FROM usage_events u JOIN features f ON f.id = u.feature_id
		 WHERE u.tenant_id = $1 AND f.key = $2 ORDER BY u.seq DESC OFFSET 1 LIMIT 1)`, b.ID, hourly); err != nil {
		t.Fatal(err)
	}
	// A hash balance altered in the store.
	consume(b, never, 7, newKey())
	s.flush()
	if err := s.rdb.HIncrBy(context.Background(), s.balanceKey(b, never), "balance", 3).Err(); err != nil {
		t.Fatal(err)
	}
	want := []found{
		{reconcile.KindIdentity, a.ID, monthly},
		{reconcile.KindChain, a.ID, hourly},
		{reconcile.KindHashBalance, b.ID, monthly},
		{reconcile.KindIdentity, b.ID, hourly},
		{reconcile.KindHashBalance, b.ID, never},
	}
	sort.Slice(want, func(i, j int) bool { return fmt.Sprint(want[i]) < fmt.Sprint(want[j]) })
	balances := map[string]int64{}
	for _, tn := range []tenant{a, b} {
		for _, f := range features {
			balances[tn.ID+f] = s.balance(tn, f)
		}
	}

	r := s.pass()
	if got := foundOf(r.Confirmed); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the pass confirmed %v, want %v: %s", got, want, mustJSON(r.Confirmed))
	}
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 5 {
		t.Errorf("reconcile_mismatch_total = %v after the first pass, want 5", got)
	}
	if r := s.pass(); len(r.Confirmed) != 0 {
		t.Errorf("a second pass with nothing new confirmed %s", mustJSON(r.Confirmed))
	}
	if got := s.counter("quotacore_event_reconcile_mismatch_total"); got != 5 {
		t.Errorf("reconcile_mismatch_total = %v after the second pass, want 5", got)
	}
	code, v := s.verifyAll()
	if got := foundOf(v.Findings); code != exitFinding || fmt.Sprint(got) != fmt.Sprint(want) || v.Recorded != 0 {
		t.Fatalf("verify-all: exit %d, recorded %d, found %v, want %v", code, v.Recorded, got, want)
	}
	if n := len(s.findings()); n != 5 {
		t.Errorf("%d findings recorded, want 5: %v", n, findingKinds(s.findings()))
	}
	for _, tn := range []tenant{a, b} {
		for _, f := range features {
			if got := s.balance(tn, f); got != balances[tn.ID+f] {
				t.Errorf("%s %s moved from %d to %d: reconciliation corrected a balance", tn.External, f,
					balances[tn.ID+f], got)
			}
		}
	}
	// verify-tenant reads one tenant's share of the same findings.
	c, out := s.commands()
	code, err := c.run(context.Background(), []string{"verify-tenant", b.External})
	if err != nil {
		t.Fatal(err)
	}
	var one verified
	if err := json.Unmarshal(out.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if got := foundOf(one.Findings); code != exitFinding || len(got) != 3 || !strings.Contains(fmt.Sprint(got), b.ID) {
		t.Errorf("verify-tenant %s: exit %d, found %v", b.External, code, got)
	}
}
