package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The three operations below are the ones whose refusal is a schema property
// rather than a request shape: a plan or feature that something references
// cannot be deleted (DR-035, DR-016, P-6, F-6), and a feature's unit cannot
// change once anything references it (INV-F2). Every other control-plane
// write is IP-09's, and it calls Classify on what the database answers.

// DeletePlan deletes a plan and its own entitlements in one transaction, or
// refuses with ErrPlanInUse while any tenant, including a soft-deleted one,
// is assigned to it. The entitlements go first because they reference the
// plan too, and their foreign key would otherwise fire before the tenants'
// and answer for the wrong reason.
func DeletePlan(ctx context.Context, db *sql.DB, planID string) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM plan_entitlements WHERE plan_id = $1", planID); err != nil {
			return Classify(OpDeletePlan, err)
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM plans WHERE id = $1", planID)
		if err != nil {
			return Classify(OpDeletePlan, err)
		}
		return requireOne(res, ErrPlanNotFound)
	})
}

// DeleteFeature deletes a feature, or refuses with ErrFeatureInUse while a
// plan entitlement, an override or a usage event references it.
func DeleteFeature(ctx context.Context, db *sql.DB, featureID string) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM features WHERE id = $1", featureID)
		if err != nil {
			return Classify(OpDeleteFeature, err)
		}
		return requireOne(res, ErrFeatureNotFound)
	})
}

// SetFeatureUnit changes a feature's unit while nothing references it, and
// refuses with ErrFeatureUnitImmutable once a plan entitlement, an override
// or a usage event does. A usage event counts even after its entitlement is
// removed, because the unit is what every historical amount is counted in.
//
// The feature row is locked first. An assignment's foreign-key check takes a
// key-share lock on the same row, which the exclusive lock waits for, so a
// unit change and an assignment are ordered one after the other rather than
// interleaved, and two unit changes are serialised.
func SetFeatureUnit(ctx context.Context, db *sql.DB, featureID, unit string) error {
	return inTx(ctx, db, func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx, "SELECT unit FROM features WHERE id = $1 FOR UPDATE", featureID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFeatureNotFound
		}
		if err != nil {
			return Classify(OpWrite, err)
		}
		if current == unit {
			return nil
		}
		var referenced bool
		err = tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM plan_entitlements WHERE feature_id = $1)
			OR EXISTS (SELECT 1 FROM tenant_entitlement_overrides WHERE feature_id = $1)
			OR EXISTS (SELECT 1 FROM usage_events WHERE feature_id = $1)`, featureID).Scan(&referenced)
		if err != nil {
			return Classify(OpWrite, err)
		}
		if referenced {
			return ErrFeatureUnitImmutable
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE features SET unit = $2, version = version + 1, updated_at = now() WHERE id = $1",
			featureID, unit)
		return Classify(OpWrite, err)
	})
}

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin control-plane transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return Classify(OpWrite, err)
	}
	return nil
}

func requireOne(res sql.Result, missing error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing
	}
	return nil
}
