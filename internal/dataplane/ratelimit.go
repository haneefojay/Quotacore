package dataplane

import (
	"container/list"
	"math"
	"sync"
	"time"
)

// RateLimiter is the service's own protective limit, per API key (DR-040,
// QUOTACORE_RATELIMIT_RUNTIME_RPS and _BURST). It bounds the blast radius of
// one misbehaving key; it is not a quota and never touches a balance. The set
// of buckets is bounded by QUOTACORE_RATE_LIMITER_MAX_KEYS, least recently used
// first out, so a key-churn flood costs a bounded amount of memory. An evicted
// key comes back with a full bucket, which errs towards admitting.
type RateLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	max     int
	ll      *list.List
	buckets map[string]*list.Element
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter of rate tokens per second and a bucket of
// burst, holding at most maxKeys buckets.
func NewRateLimiter(rate, burst float64, maxKeys int) *RateLimiter {
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &RateLimiter{rate: rate, burst: burst, max: maxKeys, ll: list.New(), buckets: map[string]*list.Element{}}
}

// Rate is the sustained limit per second, for the envelope's details.
func (l *RateLimiter) Rate() float64 { return l.rate }

// Allow spends one token from key's bucket at the instant now. When there is
// none, it reports how long until there will be.
func (l *RateLimiter) Allow(key string, now time.Time) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if e, found := l.buckets[key]; found {
		l.ll.MoveToFront(e)
		b = e.Value.(*bucket)
		if gain := now.Sub(b.last).Seconds() * l.rate; gain > 0 {
			b.tokens = math.Min(b.tokens+gain, l.burst)
		}
		if now.After(b.last) {
			b.last = now
		}
	} else {
		b = &bucket{key: key, tokens: l.burst, last: now}
		l.buckets[key] = l.ll.PushFront(b)
		for l.ll.Len() > l.max {
			back := l.ll.Back()
			l.ll.Remove(back)
			delete(l.buckets, back.Value.(*bucket).key)
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	if l.rate <= 0 {
		return false, time.Second
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}
