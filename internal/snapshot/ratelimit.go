package snapshot

import (
	"sync"
	"time"
)

// rateLimiter is the bounded-miss token bucket
// (QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT, deployment.md section 4). A flat
// twenty-per-second burst is generous enough never to throttle a legitimate
// miss and strict enough that the data plane cannot turn a Postgres outage
// into a stampede (request-lifecycle.md section 3). A denied miss is the
// fail-closed 503, never a wait: the request path must not block on the
// control plane at any cost it can choose to pay (NFR-A2).
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens earned per second
	burst  float64 // maximum tokens the bucket holds
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newRateLimiter(rate float64, now func() time.Time) *rateLimiter {
	return &rateLimiter{rate: rate, burst: rate, tokens: rate, last: now(), now: now}
}

// allow consumes one token and reports whether the caller may proceed. The
// bucket refills lazily on the caller's clock, so a quiet bucket costs the mutex
// and a couple of float comparisons between calls.
func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.last.IsZero() {
		if gain := now.Sub(l.last).Seconds() * l.rate; gain > 0 {
			l.tokens += gain
			if l.tokens > l.burst {
				l.tokens = l.burst
			}
		}
	}
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}
