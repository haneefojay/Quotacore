package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ErrReadOnly is a write to a tenant's keys refused because the process is in
// read-only mode: started with QUOTACORE_STARTUP_MODE=readonly for a rebuild,
// or without its hold on the rebuild fence (consistency-and-recovery.md
// section 4.1, IP-11 D5 and D6). The caller answers it 503
// service_unavailable, like any other store that cannot take the write.
var ErrReadOnly = errors.New("store: the service is in read-only mode and writes no tenant key")

// tenantPrefix is the start of every tenant key, the balance hashes and the
// idempotency records among them (data-model.md section 3.1).
const tenantPrefix = namespace + ":{t:"

// readCommands are the commands that may still name a tenant key in read-only
// mode, because none of them can change one. Everything else that names a
// tenant key is refused, so a write added later is refused by default rather
// than allowed by omission.
var readCommands = map[string]bool{
	"hmget": true, "hget": true, "hgetall": true, "hexists": true, "hlen": true,
	"get": true, "mget": true, "exists": true, "type": true, "ttl": true, "pttl": true,
}

// SetReadOnly enters or leaves read-only mode. Entered, every command that
// could write a tenant key, a script among them, is refused before it reaches
// the store, so no route and no job can reach around it (IP-11 D5).
func (s *Store) SetReadOnly(on bool) { s.readOnly.Store(on) }

// ReadOnly reports whether the store is refusing tenant writes.
func (s *Store) ReadOnly() bool { return s.readOnly.Load() }

// guard is the go-redis hook that enforces read-only mode on every command
// and pipeline the client sends.
type guard struct{ s *Store }

func (g guard) DialHook(next redis.DialHook) redis.DialHook { return next }

func (g guard) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if g.s.ReadOnly() && writesTenant(cmd.Args()) {
			cmd.SetErr(ErrReadOnly)
			return ErrReadOnly
		}
		return next(ctx, cmd)
	}
}

func (g guard) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if g.s.ReadOnly() {
			for _, c := range cmds {
				if writesTenant(c.Args()) {
					for _, each := range cmds {
						each.SetErr(ErrReadOnly)
					}
					return ErrReadOnly
				}
			}
		}
		return next(ctx, cmds)
	}
}

// writesTenant reports whether a command could change a tenant key: a script
// whose keys include one, or any command other than a read whose key is one.
// A command that names no tenant key (the configuration version, the cursor
// key, a confirmation token, a script load) is never refused, because
// read-only protects the balances and nothing else (IP-11 D5).
func writesTenant(args []any) bool {
	if len(args) < 2 {
		return false
	}
	name := strings.ToLower(argString(args[0]))
	switch name {
	case "multi", "exec", "discard", "watch", "unwatch":
		return false
	case "eval", "evalsha", "eval_ro", "evalsha_ro", "fcall", "fcall_ro":
		if len(args) < 3 {
			return false
		}
		n, err := strconv.Atoi(argString(args[2]))
		if err != nil {
			// A script whose keys cannot be read is refused rather than
			// trusted.
			return true
		}
		for i := 3; i < 3+n && i < len(args); i++ {
			if strings.HasPrefix(argString(args[i]), tenantPrefix) {
				return true
			}
		}
		return false
	}
	if readCommands[name] {
		return false
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(argString(a), tenantPrefix) {
			return true
		}
	}
	return false
}

func argString(a any) string {
	switch v := a.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}
