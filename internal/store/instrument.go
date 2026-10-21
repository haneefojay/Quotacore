package store

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/observability"
)

// Instrument counts every command the client sends: its round trip, its
// failure class, and for a script its digest call or its body load
// (observability.md section 1.3, IP-13 D2). The hook is added after the
// read-only guard, so it runs inside it, and a write the guard refused never
// reached the store and is not counted as the store's error.
func (s *Store) Instrument(d *observability.Dependencies) {
	if d != nil {
		s.client.AddHook(instrument{d})
	}
}

type instrument struct{ d *observability.Dependencies }

func (h instrument) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h instrument) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.d.DataStoreLatency.Observe(time.Since(start).Seconds())
		h.count(cmd, err)
		return err
	}
}

func (h instrument) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		h.d.DataStoreLatency.Observe(time.Since(start).Seconds())
		for _, c := range cmds {
			h.count(c, c.Err())
		}
		return err
	}
}

func (h instrument) count(cmd redis.Cmder, err error) {
	switch strings.ToLower(cmd.Name()) {
	case "evalsha", "evalsha_ro":
		h.d.ScriptLoads.WithLabelValues(observability.CommandDigest).Inc()
		if err == nil || (isServerError(err) && Class(err) != observability.ClassNoScript) {
			h.d.ScriptCacheHits.Inc()
		}
	case "script":
		if args := cmd.Args(); len(args) > 1 && strings.EqualFold(argString(args[1]), "load") {
			h.d.ScriptLoads.WithLabelValues(observability.CommandBody).Inc()
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		h.d.DataStoreErrors.WithLabelValues(Class(err)).Inc()
	}
}

func isServerError(err error) bool {
	var re redis.Error
	return errors.As(err, &re)
}

// Class is the closed set of datastore_errors_total's class label for one
// failed command. An error none of the four names is other, so every failure
// is counted somewhere (IP-13 S3).
func Class(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return observability.ClassTimeout
	case redis.HasErrorPrefix(err, "NOSCRIPT"), errors.Is(err, redis.ErrNoScript):
		return observability.ClassNoScript
	case redis.HasErrorPrefix(err, "OOM"):
		return observability.ClassOOM
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET), errors.Is(err, redis.ErrClosed):
		return observability.ClassRefused
	}
	return observability.ClassOther
}

// Clock reads the store's clock with TIME and returns how far this process's
// clock is ahead of it, corrected for half the round trip, for the clock-skew
// gauge and readiness's store term (Q-26).
func (s *Store) Clock(ctx context.Context) (skew time.Duration, err error) {
	sent := time.Now()
	theirs, err := s.client.Time(ctx).Result()
	if err != nil {
		return 0, err
	}
	back := time.Now()
	mid := sent.Add(back.Sub(sent) / 2)
	return mid.Sub(theirs), nil
}

// Memory reads used_memory and maxmemory from INFO memory. A maxmemory of
// zero is an unbounded store.
func (s *Store) Memory(ctx context.Context) (used, limit int64, err error) {
	info, err := s.client.Info(ctx, "memory").Result()
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch k {
		case "used_memory":
			used, _ = strconv.ParseInt(v, 10, 64)
		case "maxmemory":
			limit, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return used, limit, nil
}
