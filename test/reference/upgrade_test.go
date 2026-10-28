package reference_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// previousImage is the build the upgrade starts from: QUOTACORE_PREVIOUS_IMAGE,
// which CI's reference job builds from the PR's merge base. Until v0.1 is
// tagged, "the previous release" is the previous build (IP-15 D11).
func previousImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("QUOTACORE_PREVIOUS_IMAGE")
	if img == "" {
		img = "quotacore:previous"
	}
	return img
}

func waitReady(t *testing.T) {
	t.Helper()
	eventually(t, 90*time.Second, "/readyz", func() bool {
		r := do("GET", "/readyz", "", "")
		return r.err == nil && r.status == 200
	})
}

// TestUpgradeAndRollback (IP-15 DoD 9, NFR-OPS7) follows deployment.md
// section 11 with the commands it prints: a backup, the upgrade as a drain
// and restart onto the new image, and the rollback as a restore into a new
// database with the previous image pinned. Across the upgrade nothing moves;
// after the rollback the tenant with no activity since the backup verifies
// clean, and the one with activity since shows exactly the truncation section
// 4.2 says to expect.
func TestUpgradeAndRollback(t *testing.T) {
	requireReference(t)
	prev, next := previousImage(t), image()
	if out, err := runDocker("image", "inspect", prev); err != nil {
		if os.Getenv("QUOTACORE_REQUIRE_REFERENCE") != "" {
			t.Fatalf("the previous build %s is not built: %s", prev, out)
		}
		t.Skipf("the previous build %s is not built", prev)
	}
	s := newStack(t, noUp, withOverride("compose.previous.yml"), withEnv("QUOTACORE_IMAGE", prev))
	s.compose("up", "-d", "--no-build", "--wait-timeout", "120")
	waitReady(t)

	feature, tenants := provision(t, s, "upgrade", 2, 1_000_000)
	quiet, busy := tenants[0], tenants[1]
	token := s.runtimeKey()
	for i := range 40 {
		if r := consume(token, tenants[i%2], feature, 7, newIdemKey()); r.status != 200 {
			t.Fatalf("consume on the previous build: %d %s", r.status, r.body)
		}
	}
	s.settleLedger()
	before := map[string]string{}
	for _, tn := range tenants {
		_, r := balanceOf(t, token, tn)
		before[tn] = string(r.body)
	}

	// Backup, as section 7 says: pg_dump -Fc, kept outside the database.
	s.compose("exec", "-T", "-e", "PGPASSWORD="+appPassword, "postgres",
		"pg_dump", "-Fc", "-U", "quotacore", "-d", "quotacore", "-f", "/tmp/pre-upgrade.dump")

	// Upgrade: the new image, drain and restart; migrations apply at start.
	s.files = s.files[:1]
	s.env["QUOTACORE_IMAGE"] = next
	s.up("quotacore")
	if code, out, errw := s.cli("status"); code != 0 || !strings.Contains(out, `"mode": "normal"`) {
		t.Fatalf("status after the upgrade: %d %s %s", code, out, errw)
	}
	for _, tn := range tenants {
		if _, r := balanceOf(t, token, tn); string(r.body) != before[tn] {
			t.Errorf("%s changed across the upgrade:\n%s\nwas\n%s", tn, r.body, before[tn])
		}
	}
	if code, out, errw := s.cli("verify-all"); code != 0 {
		t.Fatalf("verify-all after the upgrade: %d\n%s\n%s", code, out, errw)
	}
	for range 5 {
		consume(token, busy, feature, 3, newIdemKey())
	}
	s.settleLedger()
	_, r := balanceOf(t, token, busy)
	busyAfter := string(r.body)

	// Rollback is a restore: stop, restore into a new database, pin the
	// previous image, point the service at the restored database, start.
	s.compose("stop", "quotacore")
	s.psql("postgres", "CREATE DATABASE quotacore_restore OWNER quotacore")
	s.compose("exec", "-T", "-e", "PGPASSWORD="+appPassword, "postgres",
		"pg_restore", "-U", "quotacore", "-d", "quotacore_restore", "--no-owner", "/tmp/pre-upgrade.dump")
	s.files = append(s.files, repoRoot(t)+"/test/reference/compose.previous.yml")
	s.env["QUOTACORE_IMAGE"] = prev
	s.env["QUOTACORE_DATABASE_URL"] = "postgresql://quotacore:" + appPassword + "@postgres:5432/quotacore_restore?sslmode=disable"
	s.compose("up", "-d", "--no-build", "--no-deps", "quotacore")
	waitReady(t)

	if _, r := balanceOf(t, token, quiet); string(r.body) != before[quiet] {
		t.Errorf("%s after the rollback:\n%s\nwas\n%s", quiet, r.body, before[quiet])
	}
	if _, r := balanceOf(t, token, busy); string(r.body) != busyAfter {
		t.Errorf("the data store's balance for %s moved with the rollback, which restores only Postgres", busy)
	}
	if code, out, errw := s.cli("verify-tenant", quiet); code != 0 {
		t.Errorf("verify-tenant %s after the rollback: %d\n%s\n%s", quiet, code, out, errw)
	}
	// The busy tenant's ledger ends at the backup and its balance does not:
	// a difference after a known truncation point, which section 4.2 says is
	// expected and the runbook says to record.
	if code, out, _ := s.cli("verify-tenant", busy); code != 1 {
		t.Errorf("verify-tenant %s after the rollback exited %d, want 1 for the truncation since the backup:\n%s", busy, code, out)
	}
}

func runDocker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}
