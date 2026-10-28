// Package chaos_test is the chaos rows of testing-strategy.md section 6 that
// need a container's disk or the host's clock, against the built image on the
// Compose stack (IP-16 D7, D8). The rows a proxy or a store command can
// inject run in process, under -race, in cmd/quotacore's chaos_test.go; the
// rest are cited by name in docs/release/v0-1-evidence.md.
//
// The suite runs only when QUOTACORE_CHAOS is set, and fails rather than
// skips when QUOTACORE_REQUIRE_CHAOS is. The clock pair moves the host's clock
// and runs only when QUOTACORE_CHAOS_CLOCK=1 as well, which the clock job of
// the full-suite workflow sets and a workstation never should.
package chaos_test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/test/harness"
)

func consume(s *harness.Stack, token, tenant, feature string, amount int64, key string) (int, map[string]any) {
	return s.DoWithKey(http.MethodPost, "/v1/consume", token,
		fmt.Sprintf(`{"feature_key":%q,"amount":%d,"tenant_external_id":%q}`, feature, amount, tenant), key)
}

func code(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

// TestChaosPostgresDiskFull: with the disk under Postgres full, an
// administrative write fails loudly, and no write the service acknowledged is
// missing once the disk is freed. Cached tenants keep enforcing throughout,
// because the request path does not touch Postgres.
func TestChaosPostgresDiskFull(t *testing.T) {
	harness.Require(t, "CHAOS")
	s := harness.New(t, harness.WithOverride("test/chaos/compose.chaos.yml"))
	seeded := s.Seed("disk", 1, 1, 1, 1000000, 1)
	tenant, feature, token := seeded.Tenants[0], seeded.Features[0], seeded.Keys[0]
	if status, body := consume(s, token, tenant, feature, 1, "disk-warm"); status != 200 {
		t.Fatalf("warm-up: %d %v", status, body)
	}

	// dd stops at ENOSPC and exits non-zero, which is the point.
	_, _ = s.ComposeOut("exec", "-T", "postgres", "sh", "-c", "dd if=/dev/zero of=/pgtmp/fill bs=1M 2>/dev/null; sync")
	freed := false
	t.Cleanup(func() {
		if !freed {
			_, _ = s.ComposeOut("exec", "-T", "postgres", "rm", "-f", "/pgtmp/fill")
		}
	})

	acknowledged := map[string]bool{}
	failed := 0
	for i := range 20 {
		key := fmt.Sprintf("disk.full%02d", i)
		status, raw := s.DoRaw(http.MethodPost, "/v1/admin/features", s.Admin, `{"feature_key":"`+key+`","name":"F","unit":"unit"}`)
		switch {
		case status == http.StatusCreated:
			acknowledged[key] = true
		case status >= 500 || status == 0:
			failed++
		default:
			t.Errorf("an administrative write on a full disk answered %d %s, neither a success nor a failure", status, raw)
		}
		if status, body := consume(s, token, tenant, feature, 1, fmt.Sprintf("disk-%d", i)); status != 200 {
			t.Errorf("a cached tenant was refused with the control plane's disk full: %d %v", status, body)
		}
	}
	if failed == 0 {
		t.Fatal("no administrative write failed with the disk full, so the fault did not take")
	}

	_, _ = s.ComposeOut("exec", "-T", "postgres", "rm", "-f", "/pgtmp/fill")
	freed = true
	// Postgres may have stopped on a failed WAL write; it is started again
	// as an operator would, and the service reconnects on its own.
	s.Up("postgres")
	deadline := time.Now().Add(60 * time.Second)
	for {
		status, _ := s.DoRaw(http.MethodGet, "/v1/admin/features?limit=200", s.Admin, "")
		if status == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	for key := range acknowledged {
		if status, raw := s.DoRaw(http.MethodGet, "/v1/admin/features/"+key, s.Admin, ""); status != http.StatusOK {
			t.Errorf("%s was acknowledged with 201 on a full disk and is gone: %d %s", key, status, raw)
		}
	}
	s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/features", `{"feature_key":"disk.after","name":"F","unit":"unit"}`)
	t.Logf("on a full disk: %d writes failed, %d were acknowledged and kept", failed, len(acknowledged))
}

// shiftClock moves the host's clock, which every container shares, by d.
func shiftClock(t *testing.T, d time.Duration) {
	t.Helper()
	target := time.Now().Add(d).UTC().Format("2006-01-02 15:04:05")
	if out, err := exec.Command("sudo", "-n", "date", "-u", "-s", target).CombinedOutput(); err != nil {
		t.Fatalf("set the clock to %s: %v: %s", target, err, out)
	}
}

// TestChaosClockJump is the clock pair. The tenant is daily, so a jump of
// +24 h crosses a boundary: the next request advances it once, and the
// allowance is re-granted at the new window (boundaries shift). The clock is
// then put back, which is a jump of -24 h against the store: every transition
// is STALE, nothing moves, the stored window stays where the jump left it, no
// allowance is re-granted, and quotacore_rollover_stale_total rises, which is
// the P1 alert's expression (observability.md section 5). The tenant is stuck
// until the clock catches up, which is the correct behaviour of a monotonic
// guard and the point of the test.
//
// The clock-skew alert compares this process with the data store, and a host
// jump moves both, so it is not this test's to fire; the in-process skew test
// of IP-13 is its proof, and v0-1-evidence.md says so.
func TestChaosClockJump(t *testing.T) {
	harness.Require(t, "CHAOS")
	if os.Getenv("QUOTACORE_CHAOS_CLOCK") != "1" {
		t.Skip("QUOTACORE_CHAOS_CLOCK=1 is unset; this test moves the host's clock")
	}
	s := harness.New(t)
	s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/features", `{"feature_key":"clock.f","name":"F","unit":"unit"}`)
	s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/plans",
		`{"plan_key":"clock-plan","name":"P","entitlements":[{"feature_key":"clock.f","limit":100,"reset_interval":"daily"}]}`)
	s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"clock-t","display_name":"T","plan_key":"clock-plan","timezone":"UTC"}`)
	token := s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/keys", `{"name":"clock","scope":"runtime"}`)["key"].(string)
	if status, body := consume(s, token, "clock-t", "clock.f", 40, "clock-1"); status != 200 {
		t.Fatalf("before the jump: %d %v", status, body)
	}
	shifted := time.Duration(0)
	t.Cleanup(func() {
		if shifted != 0 {
			shiftClock(t, -shifted)
		}
		_ = exec.Command("sudo", "-n", "timedatectl", "set-ntp", "true").Run()
	})
	_ = exec.Command("sudo", "-n", "timedatectl", "set-ntp", "false").Run()

	before, _ := harness.Scrape()
	shiftClock(t, 24*time.Hour)
	shifted = 24 * time.Hour
	status, body := consume(s, token, "clock-t", "clock.f", 1, "clock-2")
	if status != 200 || body["balance"] != float64(99) {
		t.Fatalf("+24 h: %d %v, want the boundary crossed and a fresh allowance of 100 less 1", status, body)
	}
	advanced, _ := harness.Scrape()
	if advanced.Sum("quotacore_rolled_over_total")-before.Sum("quotacore_rolled_over_total") < 1 {
		t.Error("+24 h: the advance was not counted")
	}
	window := body["cycle_end"]

	shiftClock(t, -24*time.Hour)
	shifted = 0
	for i := range 5 {
		status, body := consume(s, token, "clock-t", "clock.f", 1, "clock-3-"+strconv.Itoa(i))
		if status != 200 {
			t.Fatalf("-24 h: %d %v", status, body)
		}
		if body["balance"] != float64(98-i) || body["cycle_end"] != window {
			t.Errorf("-24 h: %v, want the balance to keep falling in the window the jump left (%v)", body, window)
		}
	}
	after, _ := harness.Scrape()
	if stale := after.Sum("quotacore_rollover_stale_total") - advanced.Sum("quotacore_rollover_stale_total"); stale < 5 {
		t.Errorf("-24 h: rollover_stale_total rose by %v over five requests; the P1 alert would not fire", stale)
	}
	if !strings.Contains(fmt.Sprint(window), "T") {
		t.Errorf("the response carried no cycle_end: %v", window)
	}
}
