package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// balanceFields are the six fields of a balance hash, in the order
// data-model.md section 3.2 lists them and the scripts write them.
var balanceFields = []string{"balance", "limit", "bonus", "cycle_index", "window_start", "window_end"}

// Stored is one balance hash as it is stored. Present is false when the hash
// does not exist; the caller decides what an absence means (DR-045).
type Stored struct {
	Present bool
	Balance int64
	Limit   int64
	Bonus   int64
	Index   int64
	Start   time.Time
	End     *time.Time // nil for an entitlement that never resets
}

// ErrCorruptBalance is a hash with some of its six fields and not others, or
// a field that is not a number. The scripts never write one, so it is a defect
// or a hand edit, and the read fails closed.
var ErrCorruptBalance = errors.New("store: a balance hash is incomplete or unreadable")

// ReadBalances reads the listed features' balance hashes for one tenant in one
// pipelined round trip of HMGETs. Every key carries the tenant's hash tag, so
// the round trip addresses one slot. The read is bounded by the script
// timeout, because it is on the request path like a script call.
func (s *Store) ReadBalances(ctx context.Context, tenantID string, features []string) ([]Stored, error) {
	if len(features) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	pipe := s.client.Pipeline()
	cmds := make([]*redis.SliceCmd, len(features))
	for i, f := range features {
		key, err := BalanceKey(tenantID, f)
		if err != nil {
			return nil, err
		}
		cmds[i] = pipe.HMGet(ctx, key, balanceFields...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("read balances: %w", err)
	}
	out := make([]Stored, len(features))
	for i, cmd := range cmds {
		st, err := parseStored(cmd.Val())
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, features[i])
		}
		out[i] = st
	}
	return out, nil
}

func parseStored(vals []any) (Stored, error) {
	missing := 0
	for _, v := range vals {
		if v == nil {
			missing++
		}
	}
	if missing == len(vals) {
		return Stored{}, nil
	}
	if missing != 0 || len(vals) != len(balanceFields) {
		return Stored{}, ErrCorruptBalance
	}
	str := make([]string, len(vals))
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			return Stored{}, ErrCorruptBalance
		}
		str[i] = s
	}
	nums := make([]int64, 5)
	for i := range nums {
		n, err := strconv.ParseInt(str[i], 10, 64)
		if err != nil {
			return Stored{}, ErrCorruptBalance
		}
		nums[i] = n
	}
	st := Stored{Present: true, Balance: nums[0], Limit: nums[1], Bonus: nums[2], Index: nums[3],
		Start: time.UnixMilli(nums[4]).UTC()}
	if str[5] != "" {
		end, err := strconv.ParseInt(str[5], 10, 64)
		if err != nil {
			return Stored{}, ErrCorruptBalance
		}
		t := time.UnixMilli(end).UTC()
		st.End = &t
	}
	return st, nil
}
