package dataplane

import (
	"testing"
	"time"
)

// TestRateLimiterIsPerKeyBoundedAndRefills: each key has its own bucket of
// burst tokens refilled at rate, the set of buckets never exceeds its bound,
// and the wait reported is the time to the next token.
func TestRateLimiterIsPerKeyBoundedAndRefills(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	l := NewRateLimiter(10, 3, 2)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a", at); !ok {
			t.Fatalf("token %d of the burst refused", i)
		}
	}
	ok, wait := l.Allow("a", at)
	if ok || wait != 100*time.Millisecond {
		t.Errorf("past the burst: ok=%v wait=%v, want refused for 100ms", ok, wait)
	}
	if ok, _ := l.Allow("b", at); !ok {
		t.Error("another key was refused")
	}
	if ok, _ := l.Allow("a", at.Add(100*time.Millisecond)); !ok {
		t.Error("a refilled token was refused")
	}
	l.Allow("c", at)
	if n := len(l.buckets); n != 2 || l.ll.Len() != 2 {
		t.Errorf("%d buckets, want the bound, 2", n)
	}
	// A clock that steps backwards earns nothing and costs nothing.
	if ok, _ := l.Allow("c", at.Add(-time.Hour)); !ok {
		t.Error("a backwards clock refused a full bucket")
	}
}
