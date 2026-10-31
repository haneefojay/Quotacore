package rolling

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// frozenCycle is every file of internal/cycle as IP-03 proved it and as IP-19
// found it, by SHA-256 of its bytes with line endings normalised. Rolling
// windows arrive beside the calendar engine and must not modify it (ADR-0008,
// roadmap-index section 6), and cycle-engine.md section 9 is the review a
// change to it has to pass. This is that review's mechanical form for IP-19
// DoD 3: matrix_test.go is the IP-03 boundary table, and it and the engine are
// byte-identical, so the table still passes byte for byte because nothing it
// tests or asserts has moved.
//
// A deliberate change to the calendar engine updates this list in the same
// change, which puts the cycle-engine.md section 9 checklist in front of its
// reviewer.
var frozenCycle = map[string]string{
	"benchmark_test.go": "2863c70001b34e663cd899b91687901cf51fc49bf8a4f5d5145e782314b4ee55",
	"cycle.go":          "17dc23755424ea80632d2465115bc3687c0133d4f73bf8e864a7fae3335c2355",
	"daylight_test.go":  "875e9efd8e3c5f37f1786ccc1e0256553f91924f02c530bd06929cf0b337c260",
	"matrix_test.go":    "e4ef944d2d18f2fc1ccdde390824f0bfaf1cffbf17b84f5c03b90f077220c9da",
	"monotonic_test.go": "6384b24efc1bc09290330e383e9ffbad31cd7ba58912910d5fc5995b95834d77",
	"never_test.go":     "e7174dbaa36912e02f1748a0803e84f1f8cb53fa0bc4a3d8437c4229ca729e3f",
	"property_test.go":  "08c922ffd03afd6322de09c4e8194e3f30549b7e0ac14d7ad418f143ba3f00ee",
	"tzdata_test.go":    "9da744a5075e77b32b46866af4ea29b0f8c6a97cffc24654c66d0d4abed9fce8",
}

func TestT18TheCalendarEngineIsUnchanged(t *testing.T) {
	dir := filepath.Join("..", "cycle")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seen = append(seen, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")))
		want, ok := frozenCycle[e.Name()]
		switch {
		case !ok:
			t.Errorf("internal/cycle/%s is new; the calendar engine is frozen at IP-03 (cycle-engine.md section 9)", e.Name())
		case hex.EncodeToString(sum[:]) != want:
			t.Errorf("internal/cycle/%s changed; the calendar engine and the IP-03 boundary table must be byte-identical "+
				"(IP-19 DoD 3, cycle-engine.md section 9)", e.Name())
		}
	}
	sort.Strings(seen)
	if len(seen) != len(frozenCycle) {
		t.Errorf("internal/cycle holds %v; the frozen set has %d files", seen, len(frozenCycle))
	}
}
