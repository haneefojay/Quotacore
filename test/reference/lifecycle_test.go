package reference_test

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// provision makes one plan with one large monthly entitlement and n tenants on
// it, and returns the feature key and the tenants' external ids.
func provision(t *testing.T, s *stack, prefix string, n int, limit int64) (string, []string) {
	t.Helper()
	feature := prefix + ".units"
	s.mustAdmin("feature", "create", "--key", feature, "--name", feature, "--unit", "units")
	s.mustAdmin("plan", "create", "--key", prefix+"-plan", "--name", prefix,
		"--entitlement", fmt.Sprintf("%s=%d/monthly", feature, limit))
	var tenants []string
	for i := range n {
		ext := fmt.Sprintf("%s-%03d", prefix, i)
		s.mustAdmin("tenant", "create", "--external-id", ext, "--name", ext, "--plan", prefix+"-plan")
		tenants = append(tenants, ext)
	}
	return feature, tenants
}

// load runs clients that consume 1 unit each with a fresh key until stop is
// closed, and tallies the outcome of every request.
type load struct {
	ok, refused, other atomic.Int64
	mu                 sync.Mutex
	unexpected         []string
	done               sync.WaitGroup
}

func startLoad(token, tenant, feature string, clients int, stop <-chan struct{}) *load {
	l := &load{}
	for range clients {
		l.done.Add(1)
		go func() {
			defer l.done.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r := consume(token, tenant, feature, 1, newIdemKey())
				switch {
				case r.err == nil && r.status == 200:
					l.ok.Add(1)
					// Under the per-key limit of 2,000/s (deployment.md 4.5).
					time.Sleep(10 * time.Millisecond)
				case r.err != nil && (errors.Is(r.err, syscall.ECONNREFUSED) || errors.Is(r.err, syscall.ECONNRESET) || errors.Is(r.err, io.EOF) ||
					// net/http's unexported errServerClosedIdle: the drain closed
					// a kept-alive connection as the client reused it.
					strings.Contains(r.err.Error(), "server closed idle connection")):
					// No answer. While the container is down Docker's port
					// proxy on the host still accepts and then resets, so a
					// reset is not by itself a cut-off response; whether the
					// service applied any of these is what the ledger count
					// below decides.
					l.refused.Add(1)
					time.Sleep(20 * time.Millisecond)
				case r.err == nil && r.status == 503:
					// Draining or not yet ready on the way back up: answered whole.
					l.refused.Add(1)
				default:
					l.other.Add(1)
					l.mu.Lock()
					if len(l.unexpected) < 10 {
						l.unexpected = append(l.unexpected, fmt.Sprintf("%d %v %s", r.status, r.err, r.body))
					}
					l.mu.Unlock()
				}
			}
		}()
	}
	return l
}

// consumedInLedger counts the consumed rows the ledger holds for a tenant.
func (s *stack) consumedInLedger(tenant string) int64 {
	s.t.Helper()
	n, _ := strconv.ParseInt(s.psql("quotacore", `SELECT count(*) FROM usage_events u JOIN tenants t ON t.id = u.tenant_id
		WHERE t.external_id = '`+tenant+`' AND u.event_type = 'consumed'`), 10, 64)
	return n
}

// TestAStopDrainsWithinTheGrace (IP-15 DoD 3 and 4): `docker compose stop`
// in the middle of a steady load. The stop finishes inside NFR-A5's 30 s, and
// no request was applied without being answered: after the restart the ledger
// holds exactly the deductions the clients were told succeeded, the balance is
// the limit less those, and verify-all finds nothing (no partial state). A
// request that got no answer at all is allowed, since the listener closes; one
// that the service applied and did not answer would make the ledger count
// exceed the successes, and fails.
func TestAStopDrainsWithinTheGrace(t *testing.T) {
	s := newStack(t)
	const limit = 10_000_000
	feature, tenants := provision(t, s, "drain", 1, limit)
	token := s.runtimeKey()

	stop := make(chan struct{})
	l := startLoad(token, tenants[0], feature, 16, stop)
	time.Sleep(2 * time.Second)
	began := time.Now()
	s.compose("stop", "quotacore")
	took := time.Since(began)
	time.Sleep(500 * time.Millisecond)
	s.up("quotacore")
	time.Sleep(time.Second)
	close(stop)
	l.done.Wait()

	if took > 30*time.Second {
		t.Errorf("the stop took %s, over NFR-A5's 30 s", took)
	}
	if n := l.other.Load(); n > 0 {
		t.Errorf("%d requests were neither answered nor refused: %v", n, l.unexpected)
	}
	if l.ok.Load() == 0 || l.refused.Load() == 0 {
		t.Fatalf("the load did not straddle the stop: %d ok, %d refused", l.ok.Load(), l.refused.Load())
	}
	s.settleLedger()
	if got := s.consumedInLedger(tenants[0]); got != l.ok.Load() {
		t.Errorf("the ledger holds %d consumed rows; the clients saw %d successes", got, l.ok.Load())
	}
	bal, r := balanceOf(t, token, tenants[0])
	if r.status != 200 || bal[feature] != limit-l.ok.Load() {
		t.Errorf("balance %v (%d), want %d", bal, r.status, limit-l.ok.Load())
	}
	if code, out, errw := s.cli("verify-all"); code != 0 {
		t.Errorf("verify-all after the stop: exit %d\n%s\n%s", code, out, errw)
	}
	t.Logf("stop took %s; %d answered, %d refused", took, l.ok.Load(), l.refused.Load())
}

// TestAKillLosesNothing (IP-15 DoD 4, NFR-D6, NFR-A6): SIGKILL the service
// and start it again. Every balance and every cycle is what it was, and every
// idempotency record answers its recorded response, because the process holds
// no authoritative state.
func TestAKillLosesNothing(t *testing.T) {
	s := newStack(t)
	feature, tenants := provision(t, s, "kill", 5, 1_000_000)
	token := s.runtimeKey()
	type record struct{ tenant, key, body string }
	var records []record
	for i := range 100 {
		tn, key := tenants[i%len(tenants)], newIdemKey()
		r := consume(token, tn, feature, int64(1+i%7), key)
		if r.status != 200 {
			t.Fatalf("consume: %d %s", r.status, r.body)
		}
		records = append(records, record{tn, key, string(r.body)})
	}
	before := map[string]string{}
	for _, tn := range tenants {
		_, r := balanceOf(t, token, tn)
		before[tn] = string(r.body)
	}

	s.compose("kill", "-s", "KILL", "quotacore")
	s.up("quotacore")

	for _, tn := range tenants {
		if _, r := balanceOf(t, token, tn); string(r.body) != before[tn] {
			t.Errorf("%s after the kill:\n%s\nbefore:\n%s", tn, r.body, before[tn])
		}
	}
	for i, rec := range records {
		r := consume(token, rec.tenant, feature, int64(1+i%7), rec.key)
		want := strings.Replace(rec.body, `"replayed":false`, `"replayed":true`, 1)
		if r.status != 200 || string(r.body) != want {
			t.Fatalf("replay of %s after the kill: %d %s, want %s", rec.key, r.status, r.body, want)
		}
	}
	// The clean-stop half of DoD 4: the same holds across docker compose restart.
	s.compose("restart", "quotacore")
	s.up("quotacore")
	for _, tn := range tenants {
		if _, r := balanceOf(t, token, tn); string(r.body) != before[tn] {
			t.Errorf("%s after a clean restart changed", tn)
		}
	}
}
