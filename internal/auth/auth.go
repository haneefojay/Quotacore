package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Key is one api_keys row as authentication needs it.
type Key struct {
	ID        string
	Name      string // the audit actor of an admin request (DR-041)
	Prefix    string
	Scope     Scope
	State     string // active, revoked or expired
	ExpiresAt *time.Time
	Hash      []byte
}

// KeyLoader is the control-plane read the key cache may perform. internal/db
// implements it; a test double implements it without a database.
type KeyLoader interface {
	// LoadKey is the one indexed read a cache miss makes. A prefix with no row
	// is ErrKeyUnknown.
	LoadKey(ctx context.Context, prefix string) (Key, error)
	// ReconcileKeys re-reads every listed prefix in one statement and returns
	// the rows that exist, by prefix.
	ReconcileKeys(ctx context.Context, prefixes []string) (map[string]Key, error)
	// TouchLastUsed records last_used_at for each key id, in one statement.
	TouchLastUsed(ctx context.Context, used map[string]time.Time) error
}

// The ways authentication fails. The HTTP mapping is the router's:
// ErrNoCredential is 401 unauthorized, ErrInvalid is 401 api_key_invalid,
// ErrMissRefused is 503 service_unavailable and ErrUnavailable is 503
// control_plane_unavailable.
var (
	ErrKeyUnknown   = errors.New("auth: no key has that prefix")
	ErrNoCredential = errors.New("auth: no credential was presented")
	ErrInvalid      = errors.New("auth: the key is unknown, revoked, expired or malformed")
	ErrMissRefused  = errors.New("auth: a key-cache miss was refused by the bounded-miss limiter")
	ErrUnavailable  = errors.New("auth: the control plane did not answer a key-cache miss")
)

// dummyHash is what an unknown prefix is compared against, so that the
// comparison an unknown prefix costs is the comparison a wrong secret costs.
var dummyHash = func() []byte { d := Digest(make([]byte, SecretBytes)); return d[:] }()

// Options configure an Authenticator.
type Options struct {
	Loader       KeyLoader
	CacheEntries int           // QUOTACORE_KEY_CACHE_ENTRIES
	MissRate     float64       // misses per second, the same bound as the snapshot's
	Timeout      time.Duration // QUOTACORE_CONTROLPLANE_TIMEOUT, for one miss
	Now          func() time.Time
	// TouchEvery is the least interval between two last_used_at writes for one
	// key (ADR-0012: at most once a minute).
	TouchEvery time.Duration
}

// Authenticator verifies bearer tokens. Its methods are safe for concurrent
// use.
type Authenticator struct {
	loader  KeyLoader
	cache   *keyCache
	limiter *limiter
	misses  singleflight.Group
	timeout time.Duration
	now     func() time.Time

	touchEvery time.Duration
	touchMu    sync.Mutex
	pending    map[string]time.Time // key id → last use not yet written
	written    map[string]time.Time // key id → last instant written or queued

	kick chan struct{}
}

// New builds an Authenticator with an empty cache.
func New(opts Options) *Authenticator {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.CacheEntries <= 0 {
		opts.CacheEntries = 10_000
	}
	if opts.MissRate <= 0 {
		opts.MissRate = 20
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.TouchEvery <= 0 {
		opts.TouchEvery = time.Minute
	}
	return &Authenticator{
		loader:     opts.Loader,
		cache:      newKeyCache(opts.CacheEntries),
		limiter:    newLimiter(opts.MissRate, now),
		timeout:    opts.Timeout,
		now:        now,
		touchEvery: opts.TouchEvery,
		pending:    map[string]time.Time{},
		written:    map[string]time.Time{},
		kick:       make(chan struct{}, 1),
	}
}

// Authenticate verifies the Authorization header's value at the instant the
// request was accepted. It returns the stored key, whose Scope is the one that
// authorises; the token's label has already been checked against it.
func (a *Authenticator) Authenticate(header string, at time.Time) (Key, error) {
	if header == "" {
		return Key{}, ErrNoCredential
	}
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return Key{}, ErrInvalid
	}
	tok, err := Parse(raw)
	if err != nil {
		return Key{}, ErrInvalid
	}
	en, err := a.lookup(tok.Prefix)
	if err != nil {
		return Key{}, err
	}
	want := dummyHash
	if en.known && len(en.key.Hash) == len(dummyHash) {
		want = en.key.Hash
	}
	// Every check runs, and the secret comparison is constant time, so the
	// reason a key is refused is not visible in how long the refusal took.
	match := subtle.ConstantTimeCompare(tok.digest[:], want) == 1
	valid := match && en.known &&
		en.key.State == "active" &&
		(en.key.ExpiresAt == nil || at.Before(*en.key.ExpiresAt)) &&
		en.key.Scope == tok.Label
	if !valid {
		return Key{}, ErrInvalid
	}
	a.noteUse(en.key.ID, at)
	return en.key, nil
}

// lookup returns the cached entry for a prefix, or spends one limiter token on
// one read by prefix. Concurrent misses of one prefix share the read, so a
// burst from a key the cache has not seen costs one token, not one per
// request.
func (a *Authenticator) lookup(prefix string) (entry, error) {
	if en, ok := a.cache.get(prefix); ok {
		return en, nil
	}
	v, err, _ := a.misses.Do(prefix, func() (any, error) { return a.miss(prefix) })
	if err != nil {
		return entry{}, err
	}
	return v.(entry), nil
}

func (a *Authenticator) miss(prefix string) (entry, error) {
	if !a.limiter.allow() {
		return entry{}, ErrMissRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	// A revoke that lands while the read is in flight moves the epoch, and the
	// read's answer is then used once and not cached (K-2).
	since := a.cache.now()
	k, err := a.loader.LoadKey(ctx, prefix)
	switch {
	case errors.Is(err, ErrKeyUnknown):
		en := entry{}
		a.cache.put(prefix, en, since)
		return en, nil
	case err != nil:
		return entry{}, ErrUnavailable
	}
	en := entry{key: k, known: true}
	a.cache.put(prefix, en, since)
	return en, nil
}

// noteUse queues a last_used_at write for the key if none was written or
// queued in the last TouchEvery. It costs a mutex and a map write.
func (a *Authenticator) noteUse(id string, at time.Time) {
	a.touchMu.Lock()
	defer a.touchMu.Unlock()
	if last, ok := a.written[id]; ok && at.Sub(last) < a.touchEvery {
		return
	}
	a.written[id] = at
	a.pending[id] = at
}

// Invalidate asks for a reconcile of every cached prefix, off the request path.
// Calls coalesce: one reconcile in flight and one queued is the most there
// will ever be, so an invalidation storm costs at most two reads.
func (a *Authenticator) Invalidate() {
	a.cache.bump()
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Reconcile re-reads every cached prefix in one statement and applies the
// answer: a revoked, expired or re-scoped key is refused from the next
// request, and a prefix that has since been issued is accepted. A failure
// keeps the cache as it was, the same rule the snapshot follows.
func (a *Authenticator) Reconcile(ctx context.Context) error {
	since := a.cache.now()
	asked := a.cache.prefixes()
	if len(asked) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	rows, err := a.loader.ReconcileKeys(ctx, asked)
	if err != nil {
		return err
	}
	a.cache.apply(asked, rows, since)
	return nil
}

// Flush writes the queued last_used_at values in one statement. A failure
// puts them back, so a use is recorded late rather than lost.
func (a *Authenticator) Flush(ctx context.Context) error {
	a.touchMu.Lock()
	batch := a.pending
	a.pending = map[string]time.Time{}
	a.touchMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if err := a.loader.TouchLastUsed(ctx, batch); err != nil {
		a.touchMu.Lock()
		for id, at := range batch {
			if cur, ok := a.pending[id]; !ok || at.After(cur) {
				a.pending[id] = at
			}
		}
		a.touchMu.Unlock()
		return err
	}
	return nil
}

// Run reconciles on every Invalidate and every refresh tick, and flushes
// last_used_at on the same tick, until ctx ends. It is the only place the key
// cache talks to the control plane outside a miss.
func (a *Authenticator) Run(ctx context.Context, refresh time.Duration, logErr func(what string, err error)) {
	if refresh <= 0 {
		refresh = 30 * time.Second
	}
	t := time.NewTicker(refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), a.timeout)
			_ = a.Flush(flushCtx)
			cancel()
			return
		case <-a.kick:
			if err := a.Reconcile(ctx); err != nil && logErr != nil {
				logErr("reconcile the key cache", err)
			}
		case <-t.C:
			if err := a.Reconcile(ctx); err != nil && logErr != nil {
				logErr("reconcile the key cache", err)
			}
			if err := a.Flush(ctx); err != nil && logErr != nil {
				logErr("record last_used_at", err)
			}
		}
	}
}

// Forget drops a prefix from the cache, so the next request with it is a miss
// that reads the stored row. The admin routes call it after issuing or
// revoking a key, which is what makes a revocation hold on the serving
// instance's very next request, without waiting for the invalidation (K-2).
func (a *Authenticator) Forget(prefix string) { a.cache.forget(prefix) }

// CachedKeys is the number of prefixes cached, known or not.
func (a *Authenticator) CachedKeys() int { return a.cache.len() }
