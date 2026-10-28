package script

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// IP-15, found by T-13 against the reference stack: a script without a
// shebang is checked for memory only at its first write, and only when that
// write is one that can grow memory. PERSIST is not, and every transition
// runs one first, so a store at maxmemory under noeviction took every consume
// and grew without bound (DR-048). Every body now opens with the shebang, which
// makes the store refuse the whole script before its first command.

func TestEveryScriptIsFlagged(t *testing.T) {
	for _, b := range allBodies() {
		if !strings.HasPrefix(b.text, "#!lua\n") {
			t.Errorf("%s does not open with #!lua, so a full store checks it only at its first growing write (DR-048)", b.op)
		}
	}
}

// TestAFullStoreRefusesEveryScript runs each body, as the store receives it,
// against a store with no room: each is refused with OOM and writes nothing.
// The control is the same consume without its shebang, which the store takes,
// so the test fails if the flag ever stops being what refuses it. It needs a
// store it may reconfigure: QUOTACORE_SCRATCH_REDIS_URL.
func TestAFullStoreRefusesEveryScript(t *testing.T) {
	raw := os.Getenv("QUOTACORE_SCRATCH_REDIS_URL")
	if raw == "" {
		if os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != "" {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but QUOTACORE_SCRATCH_REDIS_URL is unset")
		}
		t.Skip("QUOTACORE_SCRATCH_REDIS_URL is unset")
	}
	opt, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()
	ctx := context.Background()
	const bal, idem = "qc:{t:oom}:bal:f", "qc:{t:oom}:idem:k"
	rdb.Del(ctx, bal, idem)
	rdb.HSet(ctx, bal, "balance", 100, "limit", 100, "bonus", 0, "cycle_index", 0, "window_start", 0, "window_end", "")
	rdb.ConfigSet(ctx, "maxmemory-policy", "noeviction")
	if err := rdb.ConfigSet(ctx, "maxmemory", "1").Err(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		rdb.ConfigSet(ctx, "maxmemory", "0")
		rdb.Del(ctx, bal, idem)
	}()
	args := []any{5, 0, 0, "", 0, 0, strings.Repeat("a", 32)}
	for _, b := range allBodies() {
		err := rdb.Eval(ctx, b.text, []string{bal, idem}, args...).Err()
		if err == nil || !strings.HasPrefix(err.Error(), "OOM") {
			t.Errorf("%s against a full store: %v, want OOM", b.op, err)
		}
	}
	if n := rdb.Exists(ctx, idem).Val(); n != 0 {
		t.Fatal("a refused script wrote an idempotency record")
	}
	compat := strings.TrimPrefix(consumeBody.text, "#!lua\n")
	if err := rdb.Eval(ctx, compat, []string{bal, idem}, args...).Err(); err != nil && strings.HasPrefix(err.Error(), "OOM") {
		t.Skip("this store refuses even an unflagged consume when full; the control proves nothing here")
	}
	if rdb.Exists(ctx, idem).Val() == 0 {
		t.Fatal("the unflagged control wrote no record, so it does not show what the flag prevents")
	}
}
