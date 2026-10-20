package main

import (
	"context"
	"errors"
	"time"

	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/reconcile"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// The adapters between internal/reconcile, which decides and does no I/O,
// and the packages that do it (IP-11 D10). They live here, at the one place
// that already wires every package, so reconcile imports no store, script
// or SQL package.

// balances reads a hash for the reconciler.
type balances struct{ data *store.Store }

func (b balances) Hash(ctx context.Context, sub reconcile.Subject) (reconcile.Hash, error) {
	got, err := b.data.ReadBalances(ctx, sub.TenantID, []string{sub.Feature})
	if err != nil {
		return reconcile.Hash{}, err
	}
	return toHash(got[0]), nil
}

func toHash(st store.Stored) reconcile.Hash {
	if !st.Present {
		return reconcile.Hash{}
	}
	return reconcile.Hash{Present: true, Balance: st.Balance, Limit: st.Limit, Bonus: st.Bonus,
		Cycle: reconcile.Cycle{Start: st.Start.UTC(), Index: st.Index}}
}

// refunder issues a reversal's refund through the data plane.
type refunder struct {
	plane *dataplane.Service
	now   func() time.Time
}

func (r refunder) Refund(ctx context.Context, d reconcile.Duplicate, key string) (reconcile.Refunded, error) {
	at := r.now()
	out, err := r.plane.Reverse(ctx, d.TenantExternal, d.Feature, key, d.Amount, at)
	if errors.Is(err, dataplane.ErrNotHeld) {
		return reconcile.Refunded{}, errors.Join(reconcile.ErrNotReversible, err)
	}
	if err != nil {
		return reconcile.Refunded{}, err
	}
	res := reconcile.Refunded{Balance: out.Balance, CycleIndex: out.Index, CycleStart: out.Start, At: at}
	switch out.Decision {
	case script.Applied:
		res.Outcome = reconcile.RefundApplied
	case script.Replayed:
		res.Outcome = reconcile.RefundReplayed
	default:
		res.Outcome = reconcile.RefundRefused
	}
	if ev, ok := dataplane.Rollover("", d.TenantID, d.Feature, out, at); ok {
		res.Rolled = &reconcile.RolledOver{Delta: ev.Delta, Balance: ev.Balance, CycleIndex: ev.CycleIndex,
			CycleStart: ev.CycleStart}
	}
	return res, nil
}
