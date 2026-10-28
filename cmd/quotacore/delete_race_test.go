package main

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestT15DeleteIsNotRetroactive is T-15 (testing-strategy §2): a consume
// raced against DELETE /v1/admin/tenants/{id} at randomised interleavings.
// Three outcomes are permitted and nothing else: the consume completes and its
// deduction stands, or it is denied 404 tenant_not_found, or 409
// tenant_deleted. A completed consume is never unwound: its deduction is still
// on the hash and is one consumed row in the ledger, with nothing reversing
// it. "Deleted before the request" is DR-047's: a request that reads the
// tenant after the delete commits is refused, at once on the instance that
// deleted and within NFR-D3's second on any other. The audit log holds the
// delete once.
func TestT15DeleteIsNotRetroactive(t *testing.T) {
	s := newStack(t)
	const runs = 40
	const feature = "t15-calls"
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewPCG(uint64(seed), 15))
	outcomes := map[string]int{}
	for run := range runs {
		tn := s.tenant(externalID("t15"), "UTC", time.Now().Add(-time.Hour), ent{feature: feature, limit: 100, interval: "monthly"})
		s.seedCurrent(tn, feature, 100)
		consumeDelay := time.Duration(rng.IntN(4000)) * time.Microsecond
		deleteDelay := time.Duration(rng.IntN(4000)) * time.Microsecond

		var wg sync.WaitGroup
		var consumed, deleted raw
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(consumeDelay)
			consumed = s.raw(http.MethodPost, "/v1/consume", s.runtime, metered(feature, 30, tn.External), "Idempotency-Key", newKey())
		}()
		go func() {
			defer wg.Done()
			<-start
			time.Sleep(deleteDelay)
			deleted = s.raw(http.MethodDelete, "/v1/admin/tenants/"+tn.ID, s.admin, `{"reason":"t15"}`)
		}()
		close(start)
		wg.Wait()
		if deleted.err != nil || deleted.status != http.StatusOK {
			t.Fatalf("run %d: the delete: %d %v %s", run, deleted.status, deleted.err, deleted.body)
		}
		if consumed.err != nil {
			t.Fatalf("run %d: the consume: %v", run, consumed.err)
		}
		var outcome string
		switch {
		case consumed.status == http.StatusOK:
			outcome = "completed"
		case consumed.status == http.StatusNotFound && strings.Contains(consumed.body, `"tenant_not_found"`):
			outcome = "tenant_not_found"
		case consumed.status == http.StatusConflict && strings.Contains(consumed.body, `"tenant_deleted"`):
			outcome = "tenant_deleted"
		default:
			t.Fatalf("run %d: an outcome T-15 does not permit: %d %s", run, consumed.status, consumed.body)
		}
		outcomes[outcome]++

		// Whatever the race decided, it is final: a completed deduction is on
		// the hash, and a refused one never reached it.
		want := int64(100)
		if outcome == "completed" {
			want = 70
		}
		if got := s.balance(tn, feature); got != want {
			t.Fatalf("run %d (%s): balance %d, want %d", run, outcome, got, want)
		}
		var consumedRows, reversed int
		for _, r := range s.ledger(tn) {
			switch {
			case r.Type == "consumed" && r.Delta == -30:
				consumedRows++
			case r.Type == "refunded" || r.Type == "reversed" || strings.HasPrefix(r.ClientKey, "#reversal/"):
				reversed++
			}
		}
		if (outcome == "completed") != (consumedRows == 1) || consumedRows > 1 || reversed != 0 {
			t.Fatalf("run %d (%s): %d consumed rows and %d reversals in the ledger", run, outcome, consumedRows, reversed)
		}
		var audits int
		if err := s.seedDB.QueryRowContext(context.Background(),
			`SELECT count(*) FROM audit_log WHERE action = 'tenant.delete' AND subject_id = $1`, tn.ID).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if audits != 1 {
			t.Fatalf("run %d: %d tenant.delete audit rows, want 1", run, audits)
		}
	}
	t.Logf("outcomes over %d runs: %v", runs, outcomes)

	// DR-047's other half: a request that reads the tenant after the delete
	// commits is refused. On the instance that served the delete that is the
	// very next request, every time; on any other it is within NFR-D3's
	// second, which the 20 consumes after the bound assert.
	for i := range 10 {
		tn := s.tenant(externalID("t15"), "UTC", time.Now().Add(-time.Hour), ent{feature: feature, limit: 100, interval: "monthly"})
		s.seedCurrent(tn, feature, 100)
		mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, ""), http.StatusOK)
		mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/tenants/"+tn.ID, `{"reason":"t15"}`), http.StatusOK)
		if r := s.consume(s.runtime, feature, 1, tn.External, newKey()); r.Status == http.StatusOK {
			t.Fatalf("delete %d: a consume sent after the delete returned, to the instance that deleted, completed: %s", i, r.Body)
		}
		if i < 9 {
			continue
		}
		time.Sleep(time.Second)
		for j := range 20 {
			r := s.consume(s.runtime, feature, 1, tn.External, newKey())
			if c := r.code(t); c != "tenant_not_found" && c != "tenant_deleted" {
				t.Fatalf("consume %d after the bound: %d %s", j, r.Status, r.Body)
			}
		}
		if got := s.balance(tn, feature); got != 100 {
			t.Fatalf("a consume after the delete moved the balance to %d", got)
		}
	}
}

// raw is one response read without failing the test, so a goroutine can
// make the call.
type raw struct {
	status int
	body   string
	err    error
}

func (s *stack) raw(method, path, token, body string, headers ...string) raw {
	req, err := http.NewRequest(method, s.server.URL+path, strings.NewReader(body))
	if err != nil {
		return raw{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return raw{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return raw{status: resp.StatusCode, body: string(b), err: err}
}
