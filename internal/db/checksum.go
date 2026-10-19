package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
)

// The checksum record is ADR-0011's rule that a migration edited after it was
// applied is a startup error, not a silent divergence. goose records versions
// but no content, so the runner keeps its own table. It cannot be a migration:
// the comparison has to run before any migration does.
const checksumTable = `CREATE TABLE IF NOT EXISTS quotacore_migration_checksums (
  version     bigint PRIMARY KEY,
  sha256      text NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now()
)`

// ErrMigrationEdited reports an applied migration whose embedded bytes no
// longer match what was applied.
type ErrMigrationEdited struct {
	Version int64
	File    string
}

func (e *ErrMigrationEdited) Error() string {
	return fmt.Sprintf("migration %s (version %d) differs from the file that was applied; an applied migration is never edited (ADR-0011)", e.File, e.Version)
}

type fileSum struct {
	file string
	sum  string
}

func fileChecksums(fsys fs.FS) (map[int64]fileSum, error) {
	files, err := versionedFiles(fsys)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]fileSum, len(files))
	for v, name := range files {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		h := sha256.Sum256(body)
		out[v] = fileSum{file: name, sum: hex.EncodeToString(h[:])}
	}
	return out, nil
}

// verifyChecksums refuses a recorded version whose file now hashes
// differently. A recorded version with no file is a binary older than its
// schema, which deployment.md section 11 expects to work, so it is skipped.
func verifyChecksums(ctx context.Context, conn *sql.Conn, sums map[int64]fileSum) error {
	if _, err := conn.ExecContext(ctx, checksumTable); err != nil {
		return fmt.Errorf("create migration checksum table: %w", err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT version, sha256 FROM quotacore_migration_checksums")
	if err != nil {
		return fmt.Errorf("read migration checksums: %w", err)
	}
	defer rows.Close()
	var edited []*ErrMigrationEdited
	for rows.Next() {
		var v int64
		var recorded string
		if err := rows.Scan(&v, &recorded); err != nil {
			return fmt.Errorf("read migration checksums: %w", err)
		}
		if f, ok := sums[v]; ok && f.sum != recorded {
			edited = append(edited, &ErrMigrationEdited{Version: v, File: f.file})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read migration checksums: %w", err)
	}
	if len(edited) > 0 {
		sort.Slice(edited, func(i, j int) bool { return edited[i].Version < edited[j].Version })
		return edited[0]
	}
	return nil
}

// recordChecksums records every file up to the applied version that has no
// record yet. A database migrated before the record existed gets its record
// on the first start of a binary that keeps one.
func recordChecksums(ctx context.Context, conn *sql.Conn, sums map[int64]fileSum, applied int64) error {
	for v, f := range sums {
		if v > applied {
			continue
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO quotacore_migration_checksums (version, sha256) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING",
			v, f.sum); err != nil {
			return fmt.Errorf("record migration checksum %d: %w", v, err)
		}
	}
	return nil
}
