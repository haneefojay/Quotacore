package cycle

import (
	"testing"
	"time"
)

// TestZoneDatabaseIsPresent is DoD item 7 for IP-03 at the package level: the
// zone database is embedded via time/tzdata (NFR-OPS8), so a boundary is
// correct with no system zone database and no egress. On Windows, where no
// system zone data exists, this test fails if the blank import in cycle.go is
// ever removed; the binary half of the assertion is the same import in
// cmd/quotacore.
func TestZoneDatabaseIsPresent(t *testing.T) {
	probes := []struct {
		zone   string
		when   string
		offset int // seconds east of UTC
	}{
		{"Europe/Berlin", "2026-07-01T00:00:00Z", 7200},
		{"Europe/Berlin", "2026-01-01T00:00:00Z", 3600},
		{"America/New_York", "2026-07-01T00:00:00Z", -14400},
		{"America/New_York", "2026-01-01T00:00:00Z", -18000},
		{"Asia/Kolkata", "2026-07-01T00:00:00Z", 19800},
		{"Australia/Lord_Howe", "2026-07-01T00:00:00Z", 37800},
		{"Australia/Lord_Howe", "2026-01-01T00:00:00Z", 39600},
	}
	for _, p := range probes {
		loc, err := time.LoadLocation(p.zone)
		if err != nil {
			t.Errorf("zone %s: %v (the embedded database is not available)", p.zone, err)
			continue
		}
		at := mustParseRFC3339(t, p.when)
		_, off := at.In(loc).Zone()
		if off != p.offset {
			t.Errorf("zone %s at %s: offset %ds, want %ds", p.zone, p.when, off, p.offset)
		}
	}
}
