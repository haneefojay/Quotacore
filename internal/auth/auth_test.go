package auth

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLoader is api_keys in memory, counting the statements a real loader
// would issue.
type fakeLoader struct {
	mu         sync.Mutex
	rows       map[string]Key
	loads      int
	reconciles int
	touched    map[string]time.Time
	fail       error
}

func newFakeLoader() *fakeLoader {
	return &fakeLoader{rows: map[string]Key{}, touched: map[string]time.Time{}}
}

func (f *fakeLoader) LoadKey(_ context.Context, prefix string) (Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.fail != nil {
		return Key{}, f.fail
	}
	k, ok := f.rows[prefix]
	if !ok {
		return Key{}, ErrKeyUnknown
	}
	return k, nil
}

func (f *fakeLoader) ReconcileKeys(_ context.Context, prefixes []string) (map[string]Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconciles++
	if f.fail != nil {
		return nil, f.fail
	}
	out := map[string]Key{}
	for _, p := range prefixes {
		if k, ok := f.rows[p]; ok {
			out[p] = k
		}
	}
	return out, nil
}

func (f *fakeLoader) TouchLastUsed(_ context.Context, used map[string]time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	for id, at := range used {
		f.touched[id] = at
	}
	return nil
}

func (f *fakeLoader) issue(t *testing.T, scope Scope) (string, Key) {
	t.Helper()
	is, err := IssueToken(scope)
	if err != nil {
		t.Fatal(err)
	}
	k := Key{ID: "id-" + is.Prefix, Prefix: is.Prefix, Scope: scope, State: "active", Hash: is.Hash}
	f.mu.Lock()
	f.rows[is.Prefix] = k
	f.mu.Unlock()
	return is.Token, k
}

func (f *fakeLoader) set(k Key) {
	f.mu.Lock()
	f.rows[k.Prefix] = k
	f.mu.Unlock()
}

var accepted = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// TestTokenParsingAndVerification: only the contract's format is parsed, and a
// key is accepted only when its secret matches, its row is active and not
// expired at the instant of acceptance, and its label agrees with its stored
// scope (ADR-0018).
func TestTokenParsingAndVerification(t *testing.T) {
	loader := newFakeLoader()
	a := New(Options{Loader: loader})
	runtimeTok, runtimeKey := loader.issue(t, Runtime)
	adminTok, _ := loader.issue(t, Admin)

	if k, err := a.Authenticate("Bearer "+runtimeTok, accepted); err != nil || k.Scope != Runtime {
		t.Fatalf("a valid runtime key: %+v, %v", k, err)
	}
	if k, err := a.Authenticate("Bearer "+adminTok, accepted); err != nil || k.Scope != Admin {
		t.Fatalf("a valid admin key: %+v, %v", k, err)
	}
	if _, err := a.Authenticate("", accepted); !errors.Is(err, ErrNoCredential) {
		t.Errorf("no header: %v, want ErrNoCredential", err)
	}

	prefix, secret, _ := strings.Cut(strings.TrimPrefix(runtimeTok, "qc_runtime_"), ".")
	wrong := []byte(secret)
	wrong[0] ^= 1
	if wrong[0] == '-' || wrong[0] == '_' {
		wrong[0] = 'A'
	}
	malformed := map[string]string{
		"basic scheme":            "Basic " + runtimeTok,
		"lower-case scheme":       "bearer " + runtimeTok,
		"no prefix separator":     "Bearer qc_runtime_" + prefix + secret,
		"live label (old format)": "Bearer qc_live_" + prefix + "." + secret,
		"rt label (ADR-0012)":     "Bearer qc_rt_" + prefix + "." + secret,
		"upper-case prefix":       "Bearer qc_runtime_" + strings.ToUpper(prefix) + "." + secret,
		"short prefix":            "Bearer qc_runtime_abc." + secret,
		"short secret":            "Bearer qc_runtime_" + prefix + "." + secret[:42],
		"padded secret":           "Bearer qc_runtime_" + prefix + "." + secret + "=",
		"trailing space":          "Bearer " + runtimeTok + " ",
		"wrong secret":            "Bearer qc_runtime_" + prefix + "." + string(wrong),
		"label disagrees":         "Bearer qc_admin_" + prefix + "." + secret,
	}
	for name, header := range malformed {
		if _, err := a.Authenticate(header, accepted); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}

	// Expiry is read against the request's instant: valid one nanosecond
	// before, refused at the instant itself.
	exp := accepted.Add(time.Minute)
	expiring := runtimeKey
	expiring.ExpiresAt = &exp
	loader.set(expiring)
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate("Bearer "+runtimeTok, exp.Add(-time.Nanosecond)); err != nil {
		t.Errorf("before expiry: %v", err)
	}
	if _, err := a.Authenticate("Bearer "+runtimeTok, exp); !errors.Is(err, ErrInvalid) {
		t.Errorf("at expiry: %v, want ErrInvalid", err)
	}

	for _, state := range []string{"revoked", "expired"} {
		k := runtimeKey
		k.State = state
		loader.set(k)
		if err := a.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Authenticate("Bearer "+runtimeTok, accepted); !errors.Is(err, ErrInvalid) {
			t.Errorf("a %s key: %v, want ErrInvalid", state, err)
		}
	}

	// A row whose stored scope disagrees with the label is refused, whichever
	// way round: the label is cosmetic, but it may not lie.
	k := runtimeKey
	k.Scope = Admin
	loader.set(k)
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate("Bearer "+runtimeTok, accepted); !errors.Is(err, ErrInvalid) {
		t.Errorf("label runtime, row admin: %v, want ErrInvalid", err)
	}
}

func TestScopes(t *testing.T) {
	cases := []struct {
		key, required Scope
		want          bool
	}{
		{Runtime, Runtime, true}, {Admin, Runtime, true},
		{Runtime, Admin, false}, {Admin, Admin, true},
		{Runtime, "", true}, {Admin, "", true},
		{"", Runtime, false}, {Runtime, "other", false},
	}
	for _, c := range cases {
		if got := c.key.Allows(c.required); got != c.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", c.key, c.required, got, c.want)
		}
	}
}

// TestUnknownPrefixAndWrongSecretAreIndistinguishable: once a prefix has been
// seen, an unknown one and a wrong secret on a known one fail the same way and
// in the same time. The time is compared as medians of many runs, because a
// single run is noise; a difference of a database read would be orders of
// magnitude, and a short-circuiting comparison would show as a consistent
// gap far above this tolerance.
func TestUnknownPrefixAndWrongSecretAreIndistinguishable(t *testing.T) {
	loader := newFakeLoader()
	a := New(Options{Loader: loader})
	tok, _ := loader.issue(t, Runtime)
	prefix, secret, _ := strings.Cut(strings.TrimPrefix(tok, "qc_runtime_"), ".")
	other, err := IssueToken(Runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, otherSecret, _ := strings.Cut(strings.TrimPrefix(other.Token, "qc_runtime_"), ".")
	wrongSecret := "Bearer qc_runtime_" + prefix + "." + otherSecret
	unknownPrefix := "Bearer qc_runtime_" + other.Prefix + "." + secret

	// Warm both: the unknown prefix is cached as unknown after its first read.
	for _, h := range []string{wrongSecret, unknownPrefix} {
		if _, err := a.Authenticate(h, accepted); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v, want ErrInvalid", h, err)
		}
	}
	loads := loader.loads

	const rounds = 4001
	measure := func(h string) time.Duration {
		d := make([]time.Duration, rounds)
		for i := range d {
			start := time.Now()
			_, _ = a.Authenticate(h, accepted)
			d[i] = time.Since(start)
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[rounds/2]
	}
	wrongMedian, unknownMedian := measure(wrongSecret), measure(unknownPrefix)
	if loader.loads != loads {
		t.Errorf("a cached unknown prefix read the database again: %d loads, want %d", loader.loads, loads)
	}
	lo, hi := min(wrongMedian, unknownMedian), max(wrongMedian, unknownMedian)
	if hi > 2*lo+2*time.Microsecond {
		t.Errorf("median wrong secret %v, median unknown prefix %v: the two are distinguishable", wrongMedian, unknownMedian)
	}
}

// TestKeyCacheMissIsBounded: a miss spends a limiter token, and a miss with
// no token left is ErrMissRefused without a read; a loader failure is
// ErrUnavailable and is not cached.
func TestKeyCacheMissIsBounded(t *testing.T) {
	loader := newFakeLoader()
	now := accepted
	a := New(Options{Loader: loader, MissRate: 2, Now: func() time.Time { return now }})
	for i := 0; i < 2; i++ {
		is, _ := IssueToken(Runtime)
		if _, err := a.Authenticate("Bearer "+is.Token, accepted); !errors.Is(err, ErrInvalid) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	is, _ := IssueToken(Runtime)
	if _, err := a.Authenticate("Bearer "+is.Token, accepted); !errors.Is(err, ErrMissRefused) {
		t.Fatalf("third miss in the same instant: %v, want ErrMissRefused", err)
	}
	if loader.loads != 2 {
		t.Errorf("loads = %d, want 2", loader.loads)
	}
	now = now.Add(time.Second)
	loader.fail = errors.New("connection refused")
	if _, err := a.Authenticate("Bearer "+is.Token, accepted); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("loader down: %v, want ErrUnavailable", err)
	}
	loader.fail = nil
	tok, _ := loader.issue(t, Runtime)
	if _, err := a.Authenticate("Bearer "+tok, accepted); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
}

// TestKeyCacheIsBoundedAndReconcilesInOneRead: the cache holds at most its
// bound, a reconcile issues one read however many prefixes are cached, a
// failed reconcile keeps what was cached, and a prefix issued after it was
// cached as unknown is accepted after the next reconcile.
func TestKeyCacheIsBoundedAndReconcilesInOneRead(t *testing.T) {
	loader := newFakeLoader()
	a := New(Options{Loader: loader, CacheEntries: 3, MissRate: 1000})
	var toks []string
	for i := 0; i < 5; i++ {
		tok, _ := loader.issue(t, Runtime)
		toks = append(toks, tok)
		if _, err := a.Authenticate("Bearer "+tok, accepted); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.CachedKeys(); n != 3 {
		t.Errorf("cached = %d, want the bound, 3", n)
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loader.reconciles != 1 {
		t.Errorf("a reconcile of three prefixes issued %d reads, want 1", loader.reconciles)
	}

	loader.fail = errors.New("down")
	if err := a.Reconcile(context.Background()); err == nil {
		t.Error("a failed reconcile reported success")
	}
	loader.fail = nil
	loads := loader.loads
	if _, err := a.Authenticate("Bearer "+toks[4], accepted); err != nil || loader.loads != loads {
		t.Errorf("after a failed reconcile the cached key must still serve with no read: %v, loads %d→%d", err, loads, loader.loads)
	}

	is, _ := IssueToken(Runtime)
	if _, err := a.Authenticate("Bearer "+is.Token, accepted); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	loader.set(Key{ID: "late", Prefix: is.Prefix, Scope: Runtime, State: "active", Hash: is.Hash})
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate("Bearer "+is.Token, accepted); err != nil {
		t.Errorf("a key issued after its prefix was cached as unknown: %v", err)
	}
}

// TestLastUsedIsBatchedAndAtMostOncePerMinute (ADR-0012): the request path
// only queues; a flush writes one batch; a second use inside the minute
// queues nothing; a failed flush keeps the batch.
func TestLastUsedIsBatchedAndAtMostOncePerMinute(t *testing.T) {
	loader := newFakeLoader()
	a := New(Options{Loader: loader})
	tok, k := loader.issue(t, Runtime)
	for i := 0; i < 10; i++ {
		if _, err := a.Authenticate("Bearer "+tok, accepted.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(loader.touched) != 0 {
		t.Fatal("the request path wrote last_used_at")
	}
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := loader.touched[k.ID]; !got.Equal(accepted) {
		t.Errorf("last_used_at = %v, want the first use, %v", got, accepted)
	}
	if _, err := a.Authenticate("Bearer "+tok, accepted.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(a.pending) != 0 {
		t.Error("a use inside the minute queued a write")
	}
	later := accepted.Add(time.Minute)
	if _, err := a.Authenticate("Bearer "+tok, later); err != nil {
		t.Fatal(err)
	}
	loader.fail = errors.New("down")
	if err := a.Flush(context.Background()); err == nil {
		t.Error("a failed flush reported success")
	}
	loader.fail = nil
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := loader.touched[k.ID]; !got.Equal(later) {
		t.Errorf("after a failed flush and a retry, last_used_at = %v, want %v", got, later)
	}
}
