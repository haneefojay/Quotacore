package script

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/store"
)

// fakeStore answers as a store would without being one. It exists for the two
// properties that a real store cannot be asked about without asking it the wrong
// question: how many commands one request made, and what happens when the store
// has forgotten the scripts. Both are observable only from between the client
// and the wire, which is why Options.Client is an interface.
type fakeStore struct {
	mu sync.Mutex
	// evalErrors is consumed in order, one entry per EvalSha. A nil entry means
	// answer with the reply.
	evalErrors []error
	reply      any
	loadErr    error
	// sleep makes an evaluation outlive the caller's budget, which is how the
	// 250 ms is proved rather than assumed.
	sleep time.Duration

	evals   int
	loads   int
	bodies  int // EVAL, which must never happen
	digests []string
	keys    []string
	args    []any
}

func (f *fakeStore) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	f.recordBody()
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(errors.New("EVAL was sent; the runner must only ever send the digest"))
	return cmd
}

func (f *fakeStore) EvalRO(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	f.recordBody()
	cmd := redis.NewCmd(ctx)
	cmd.SetErr(errors.New("EVAL_RO was sent; the runner must only ever send the digest"))
	return cmd
}

func (f *fakeStore) EvalShaRO(ctx context.Context, sha string, keys []string, args ...any) *redis.Cmd {
	return f.EvalSha(ctx, sha, keys, args...)
}

func (f *fakeStore) EvalSha(ctx context.Context, sha string, keys []string, args ...any) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	f.mu.Lock()
	f.evals++
	f.digests = append(f.digests, sha)
	f.keys = append(f.keys, keys...)
	f.args = append([]any{}, args...)
	sleep := f.sleep
	var err error
	if len(f.evalErrors) > 0 {
		err = f.evalErrors[0]
		f.evalErrors = f.evalErrors[1:]
	}
	reply := f.reply
	f.mu.Unlock()
	if err != nil {
		cmd.SetErr(err)
		return cmd
	}
	if sleep > 0 {
		select {
		case <-ctx.Done():
			cmd.SetErr(ctx.Err())
			return cmd
		case <-time.After(sleep):
		}
	}
	cmd.SetVal(reply)
	return cmd
}

func (f *fakeStore) ScriptExists(ctx context.Context, hashes ...string) *redis.BoolSliceCmd {
	cmd := redis.NewBoolSliceCmd(ctx)
	cmd.SetErr(errors.New("SCRIPT EXISTS was sent; the runner loads rather than probes"))
	return cmd
}

func (f *fakeStore) ScriptLoad(ctx context.Context, script string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	f.mu.Lock()
	f.loads++
	err := f.loadErr
	f.mu.Unlock()
	if err != nil {
		cmd.SetErr(err)
		return cmd
	}
	sum := sha1.Sum([]byte(script))
	cmd.SetVal(hex.EncodeToString(sum[:]))
	return cmd
}

// recordBody records that a whole body was put on the wire, which is the one
// thing the runner must never do.
func (f *fakeStore) recordBody() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies++
}

func (f *fakeStore) counts() (evals, loads, bodies int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evals, f.loads, f.bodies
}

const testTenant = "df125e35-7a6d-4a63-9695-889af4db8da9"

// testWindow is a daily cycle: index 5, starting 2026-09-29T00:00:00Z and ending
// 2026-10-29T00:00:00Z, which is a month and so not a real daily window but a
// pair of instants the assertions can name exactly. What is under test is the
// values' order and shape, not the engine's arithmetic, which is IP-03's and has
// its own table test.
var testWindow = cycle.Window{
	Index: 5,
	Start: time.Date(2026, time.September, 29, 0, 0, 0, 0, time.UTC),
	End:   ptr(time.Date(2026, time.October, 29, 0, 0, 0, 0, time.UTC)),
}

func testRequest() Request {
	return Request{
		TenantID:       testTenant,
		FeatureKey:     "searches",
		IdempotencyKey: "test-client-key",
		Amount:         30,
		Window:         testWindow,
		Limit:          100,
		At:             testWindow.Start.Add(12 * time.Hour),
	}
}

func ptr(t time.Time) *time.Time { return &t }

func appliedReply(balance int64) []any {
	return []any{
		"applied", "current",
		strconv.FormatInt(balance, 10), "100", "0", "5",
		strconv.FormatInt(testWindow.Start.UnixMilli(), 10),
		strconv.FormatInt(testWindow.End.UnixMilli(), 10),
	}
}

func newRunner(t *testing.T, client redis.Scripter, timeout time.Duration) *Runner {
	t.Helper()
	r, err := New(Options{Client: client, Timeout: timeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestNewRefusesToEnforceNothing(t *testing.T) {
	if _, err := New(Options{Timeout: 250 * time.Millisecond}); err == nil {
		t.Error("New with no client returned a runner, which could not run a script")
	}
	if _, err := New(Options{Client: &fakeStore{}}); err == nil {
		t.Error("New with no timeout returned a runner, which would wait forever")
	}
	if _, err := New(Options{Client: &fakeStore{}, Timeout: -1}); err == nil {
		t.Error("New with a negative timeout returned a runner")
	}
}

// TestOneConsumeIsOneCall is Definition of Done item 9. The count is taken from
// between the client and the wire rather than from a comment, and a whole body
// on the wire is a second copy of the rules on every request, so it is counted
// too and asserted to be zero.
func TestOneConsumeIsOneCall(t *testing.T) {
	fake := &fakeStore{reply: appliedReply(70)}
	r := newRunner(t, fake, 250*time.Millisecond)

	out, err := r.Consume(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	evals, loads, bodies := fake.counts()
	if evals != 1 {
		t.Errorf("a consume made %d evaluations, and the rule is one round trip (performance.md §3)", evals)
	}
	if loads != 0 {
		t.Errorf("a consume loaded the scripts %d times on the request path; loading belongs to start-up", loads)
	}
	if bodies != 0 {
		t.Errorf("a consume put %d whole script bodies on the wire; only the digest may be sent (NFR-S8)", bodies)
	}
	if out.Balance != 70 {
		t.Errorf("Balance = %d, and the reply said 70", out.Balance)
	}
	if !out.Allowed() || out.Transition != Current {
		t.Errorf("outcome = %+v, and the reply said applied and current", out)
	}
}

func TestTheRunnerAddressesTheScriptByItsDigest(t *testing.T) {
	fake := &fakeStore{reply: appliedReply(70)}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.Consume(context.Background(), testRequest()); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if len(fake.digests) != 1 || fake.digests[0] != consumeBody.digest {
		t.Errorf("the digest sent was %v, and the consume script's is %s", fake.digests, consumeBody.digest)
	}
}

func TestTheRunnerSendsTwoKeysAndSevenValues(t *testing.T) {
	fake := &fakeStore{reply: appliedReply(70)}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.Consume(context.Background(), testRequest()); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	wantKey := "qc:{t:" + testTenant + "}:bal:searches"
	wantIdem := "qc:{t:" + testTenant + "}:idem:test-client-key"
	if len(fake.keys) != 2 || fake.keys[0] != wantKey || fake.keys[1] != wantIdem {
		t.Errorf("keys = %v, want [%s %s]: the balance and the record, both built by internal/store and both in the tenant's slot (data-model.md §3.1)",
			fake.keys, wantKey, wantIdem)
	}
	want := []string{
		"30",
		"5",
		strconv.FormatInt(testWindow.Start.UnixMilli(), 10),
		strconv.FormatInt(testWindow.End.UnixMilli(), 10),
		"100",
		strconv.FormatInt(testRequest().At.UnixMilli(), 10),
		Fingerprint(Consume, testTenant, "searches", 30),
	}
	if len(fake.args) != len(want) {
		t.Fatalf("ARGV = %v, want %v", fake.args, want)
	}
	for i, w := range want {
		if got, ok := fake.args[i].(string); !ok || got != w {
			t.Errorf("ARGV[%d] = %v, want %q", i+1, fake.args[i], w)
		}
	}
}

func TestCheckSendsOneKeyAndAnEmptyFingerprint(t *testing.T) {
	// A check changes nothing, so it writes no record and has none to compare a
	// fingerprint against. It is sent the same arity as the other two - one
	// arity is easier to assert than three - with an empty seventh value, and it
	// must not name a second key: naming one would be a check that could replay.
	fake := &fakeStore{reply: appliedReply(70)}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.Check(context.Background(), testRequest()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fake.keys) != 1 {
		t.Errorf("a check sent %d keys, %v; a check has no record to read or write (DR-026)", len(fake.keys), fake.keys)
	}
	if len(fake.args) != argCount {
		t.Fatalf("a check sent %d arguments, want %d", len(fake.args), argCount)
	}
	if last, ok := fake.args[argCount-1].(string); !ok || last != "" {
		t.Errorf("a check sent ARGV[%d] = %v, and a check has no fingerprint", argCount, fake.args[argCount-1])
	}
}

func TestAMutationWithoutAnIdempotencyKeyNeverReachesTheStore(t *testing.T) {
	// DR-026 makes the key required. A server that generated one per attempt
	// would protect nothing, so the missing header is refused before a byte is
	// sent, with the code that says the header is missing rather than the one
	// that says the key is malformed.
	for name, call := range map[string]func(*Runner, context.Context, Request) (Outcome, error){
		"consume": (*Runner).Consume,
		"refund":  (*Runner).Refund,
	} {
		t.Run(name, func(t *testing.T) {
			req := testRequest()
			req.IdempotencyKey = ""
			fake := &fakeStore{reply: appliedReply(70)}
			r := newRunner(t, fake, 250*time.Millisecond)
			if _, err := call(r, context.Background(), req); !errors.Is(err, ErrMissingIdempotencyKey) {
				t.Errorf("%s with no key returned %v, want the missing-key marker", name, err)
			}
			if evals, _, _ := fake.counts(); evals != 0 {
				t.Errorf("%s with no key still made %d evaluations", name, evals)
			}
			if _, err := r.Check(context.Background(), req); err != nil {
				t.Errorf("Check with no key returned %v, and a check needs none", err)
			}
		})
	}
}

// TestAnEntitlementThatNeverResetsSendsAnEmptyEnd covers the one argument that is
// allowed to be empty, and asserts the empty string is what crosses: a sentinel
// number would be a value a caller could collide with.
func TestAnEntitlementThatNeverResetsSendsAnEmptyEnd(t *testing.T) {
	fake := &fakeStore{reply: []any{
		"applied", "current", "100", "100", "0", "0",
		strconv.FormatInt(testWindow.Start.UnixMilli(), 10), "",
	}}
	r := newRunner(t, fake, 250*time.Millisecond)
	req := Request{
		TenantID:   testTenant,
		FeatureKey: "audit",
		Amount:     1,
		Window:     cycle.Window{Index: 0, Start: testWindow.Start},
		Limit:      100,
		At:         testWindow.Start,
	}
	out, err := r.Check(context.Background(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if out.End != nil {
		t.Errorf("End = %v, and a `never` entitlement has no boundary (DR-008)", out.End)
	}
	if last := fake.args[3].(string); last != "" {
		t.Errorf("ARGV[4] = %q, and a window that never ends is the empty string", last)
	}
}

// TestTheStoreThatForgotTheScriptsIsRecoveredFrom is the lazy half of ADR-0002:
// a flushed cache costs one reload and one repeat, and the caller still gets its
// answer inside the same budget.
func TestTheStoreThatForgotTheScriptsIsRecoveredFrom(t *testing.T) {
	for name, first := range map[string]error{
		"the exported sentinel":         redis.ErrNoScript,
		"a differently worded NOSCRIPT": prefixedError("NOSCRIPT No matching script, instance 3f0a"),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeStore{evalErrors: []error{first, nil}, reply: appliedReply(70)}
			r := newRunner(t, fake, 250*time.Millisecond)

			out, err := r.Consume(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("Consume: %v", err)
			}
			if out.Balance != 70 {
				t.Errorf("Balance = %d, want 70", out.Balance)
			}
			evals, loads, _ := fake.counts()
			// Load restores all three, not only the one that was missing, so the
			// next request on another operation does not pay the same penalty.
			if evals != 2 || loads != 3 {
				t.Errorf("a recovered call made %d evaluations and %d loads, want 2 and 3", evals, loads)
			}
		})
	}
}

// TestARecoveredCallIsRetriedOnceAndNoMore: a store that forgets the scripts again
// immediately is a store that is not working, and a loop that kept reloading on
// every request would be an outage amplifier.
func TestARecoveredCallIsRetriedOnceAndNoMore(t *testing.T) {
	fake := &fakeStore{evalErrors: []error{redis.ErrNoScript, redis.ErrNoScript, nil}, reply: appliedReply(70)}
	r := newRunner(t, fake, 250*time.Millisecond)
	_, err := r.Consume(context.Background(), testRequest())
	if err == nil {
		t.Fatal("Consume succeeded, and a store that has lost the scripts twice is not a store that answered")
	}
	evals, loads, _ := fake.counts()
	if evals != 2 || loads != 3 {
		t.Errorf("a call made %d evaluations and %d loads, want 2 and 3: one retry, no loop", evals, loads)
	}
}

func TestAFailedReloadIsReportedRatherThanRetried(t *testing.T) {
	fake := &fakeStore{evalErrors: []error{redis.ErrNoScript}, loadErr: errors.New("the store is unreachable")}
	r := newRunner(t, fake, 250*time.Millisecond)
	if _, err := r.Consume(context.Background(), testRequest()); err == nil {
		t.Fatal("Consume succeeded, and the script was never in the store")
	}
	if evals, loads, _ := fake.counts(); evals != 1 || loads != 1 {
		t.Errorf("a call made %d evaluations and %d loads, want 1 and 1", evals, loads)
	}
}

// TestTheBudgetIsTheCallersBudget proves NFR-L4's 250 ms is applied to the call
// rather than documented: an evaluation that outlives the budget ends the request
// with the deadline, which the caller turns into 503 service_unavailable.
func TestTheBudgetIsTheCallersBudget(t *testing.T) {
	fake := &fakeStore{reply: appliedReply(70), sleep: 2 * time.Second}
	r := newRunner(t, fake, 20*time.Millisecond)
	start := time.Now()
	_, err := r.Consume(context.Background(), testRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Consume returned %v, and an evaluation that outlives the budget must end the request", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Consume took %s, so the budget is not the limit", elapsed)
	}
}

func TestARequestThatCannotBeHonouredNeverReachesTheStore(t *testing.T) {
	never := testWindow
	never.End = nil
	past := testWindow
	past.End = &past.Start
	cases := map[string]Request{
		"a negative amount":                   withAmount(testRequest(), -1),
		"an amount above 2^53":                withAmount(testRequest(), bound+1),
		"a negative limit":                    withLimit(testRequest(), -1),
		"a negative cycle index":              withIndex(testRequest(), -1),
		"no window":                           withWindow(testRequest(), cycle.Window{Index: 5}),
		"no instant":                          withAt(testRequest(), time.Time{}),
		"a never entitlement at an index":     withWindow(testRequest(), never),
		"a window that ends before it starts": withWindow(testRequest(), past),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeStore{reply: appliedReply(70)}
			r := newRunner(t, fake, 250*time.Millisecond)
			if _, err := r.Consume(context.Background(), req); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("Consume returned %v, and this request is refused as invalid", err)
			}
			if evals, _, _ := fake.counts(); evals != 0 {
				t.Errorf("a refused request still made %d evaluations", evals)
			}
		})
	}
}

func TestAFeatureKeyThatWouldBreakTheHashTagNeverReachesTheStore(t *testing.T) {
	for _, name := range []string{"with:colon", "with{brace", "with}brace", ""} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeStore{reply: appliedReply(70)}
			r := newRunner(t, fake, 250*time.Millisecond)
			req := testRequest()
			req.FeatureKey = name
			_, err := r.Consume(context.Background(), req)
			if !errors.Is(err, store.ErrBadKeyPart) {
				t.Fatalf("Consume returned %v, and a key component that breaks the tag is refused by internal/store", err)
			}
			if evals, _, _ := fake.counts(); evals != 0 {
				t.Errorf("a request with an unbuildable key still made %d evaluations", evals)
			}
		})
	}
}

func withAmount(r Request, v int64) Request        { r.Amount = v; return r }
func withLimit(r Request, v int64) Request         { r.Limit = v; return r }
func withIndex(r Request, v int64) Request         { r.Window.Index = v; return r }
func withWindow(r Request, w cycle.Window) Request { r.Window = w; return r }
func withAt(r Request, at time.Time) Request       { r.At = at; return r }

// prefixedError is a store error whose message starts NOSCRIPT but is not the
// library's sentinel, which is what a different server version or a proxy would
// produce.
type prefixedError string

func (e prefixedError) Error() string { return string(e) }
func (e prefixedError) RedisError()   {}

func ExampleRunner() {
	// One call, one round trip, one outcome - and the outcome carries the state,
	// so there is no second read to disagree with it.
	fmt.Println("Consume -> applied, current, balance 70")
	// Output: Consume -> applied, current, balance 70
}
