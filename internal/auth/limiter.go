package auth

import (
	"sync"
	"time"
)

// limiter is the key cache's bounded-miss token bucket. It is the snapshot's
// rule applied to keys (DR-039, request-lifecycle.md section 3): a miss that
// cannot have a token is a fail-closed 503, never a wait, so a flood of
// unknown prefixes cannot become a flood of database reads.
type limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newLimiter(rate float64, now func() time.Time) *limiter {
	return &limiter{rate: rate, tokens: rate, last: now(), now: now}
}

func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if gain := now.Sub(l.last).Seconds() * l.rate; gain > 0 {
		l.tokens = min(l.tokens+gain, l.rate)
	}
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}
