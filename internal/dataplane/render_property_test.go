package dataplane

import (
	"encoding/json"
	"testing"
	"time"

	"pgregory.net/rapid"

	contract "github.com/quotacore/quotacore/api"
)

// TestPropertyARenderedWindowParsesBackToTheSameInstantAndOffset is the last
// property of testing-strategy.md §3 (IP-16 D10): a window rendered for a
// tenant in its zone, as the wire carries it, parses back to the same instant
// at the zone's offset for that instant. The wire carries seconds and an
// anchor keeps milliseconds, so "the same instant" is the boundary rounded up
// to its second: never earlier than the boundary, and less than a second
// later (envelope.Boundary). The zones are §11's four, Lord Howe's half-hour
// daylight shift, and Kathmandu's quarter-hour offset.
func TestPropertyARenderedWindowParsesBackToTheSameInstantAndOffset(t *testing.T) {
	zones := []string{"UTC", "Europe/Berlin", "America/New_York", "Asia/Kolkata", "Australia/Lord_Howe", "Asia/Kathmandu"}
	locs := map[string]*time.Location{}
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Fatal(err)
		}
		locs[z] = loc
	}
	lo := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	hi := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	rapid.Check(t, func(rt *rapid.T) {
		loc := locs[rapid.SampledFrom(zones).Draw(rt, "zone")]
		start := time.UnixMilli(rapid.Int64Range(lo, hi).Draw(rt, "start ms"))
		end := start.Add(time.Duration(rapid.Int64Range(1, 400*24*3600*1000).Draw(rt, "length ms")) * time.Millisecond)
		s, e := render(start, &end, loc)
		for _, c := range []struct {
			want time.Time
			got  contract.NilTimestamp
		}{{start, s}, {end, e}} {
			raw, err := json.Marshal(c.got.Value)
			if err != nil {
				rt.Fatal(err)
			}
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				rt.Fatalf("%s is not a JSON string", raw)
			}
			back, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				rt.Fatalf("%s does not parse as RFC 3339: %v", text, err)
			}
			want := c.want.Truncate(time.Second)
			if !want.Equal(c.want) {
				want = want.Add(time.Second)
			}
			if !back.Equal(want) {
				rt.Fatalf("%s is %v, want %v, the boundary %v rounded up to its second", text, back, want, c.want)
			}
			_, wantOffset := want.In(loc).Zone()
			if _, gotOffset := back.Zone(); gotOffset != wantOffset {
				rt.Fatalf("%s carries offset %d s, the zone's is %d s", text, gotOffset, wantOffset)
			}
		}
	})
}
