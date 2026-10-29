package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	MinServerMajor  = 16
	migrationDir    = "migrations"
	advisoryLockKey = 6067046871116144151
)

func Open(ctx context.Context, url string) (*sql.DB, error) {
	return OpenWith(ctx, url, OpenOptions{})
}

// OpenOptions are the per-connection settings deployment.md section 7 asks
// for. They are runtime parameters sent at connect, so every connection the
// pool opens has them and none depends on a SET that a reset could lose.
type OpenOptions struct {
	// StatementTimeout bounds every statement on the pool, so a hung admin
	// query fails rather than holding a slot. Zero leaves the server's.
	StatementTimeout time.Duration
}

// OpenWith is Open with the options. The search_path is always explicit
// (public, the one schema the migrations write), unless the URL sets its own,
// so a server or role default cannot point the service at another schema.
func OpenWith(ctx context.Context, url string, opts OpenOptions) (*sql.DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	params := cfg.ConnConfig.RuntimeParams
	if _, set := params["search_path"]; !set {
		params["search_path"] = "public"
	}
	if opts.StatementTimeout > 0 {
		params["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	db := stdlib.OpenDB(*cfg.ConnConfig)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := CheckServerVersion(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func CheckServerVersion(ctx context.Context, db *sql.DB) error {
	var raw string
	if err := db.QueryRowContext(ctx, "SHOW server_version").Scan(&raw); err != nil {
		return fmt.Errorf("read server version: %w", err)
	}
	return checkServerVersion(raw)
}

func checkServerVersion(raw string) error {
	major, err := strconv.Atoi(leadingDigits(raw))
	if err != nil {
		return fmt.Errorf("parse server version %q: %w", raw, err)
	}
	if major < MinServerMajor {
		return fmt.Errorf("PostgreSQL %d or newer is required, this server is %d", MinServerMajor, major)
	}
	return nil
}

func leadingDigits(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return v[:i]
		}
	}
	return v
}

type MigrateOptions struct {
	Disabled bool
}

// Migrate applies the embedded migrations, up only, under a session-level
// advisory lock so N replicas starting together produce one run (ADR-0011).
// The lock is taken and released on one pinned connection: on a pool, an
// unlock sent down a different connection releases nothing, and the lock would
// stay held by an idle connection for the life of the process.
func Migrate(ctx context.Context, db *sql.DB, opts MigrateOptions) error {
	if opts.Disabled {
		return nil
	}
	return migrate(ctx, db, embeddedMigrations(), 0)
}

// migrate is Migrate over any file set and, when target is non-zero, only up
// to that version. The tests use both: an edited copy of the set proves the
// checksum refusal, and a target proves every earlier version upgrades.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS, target int64) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)

	sums, err := fileChecksums(fsys)
	if err != nil {
		return err
	}
	if err := verifyChecksums(ctx, conn, sums); err != nil {
		return err
	}
	provider, err := newProvider(db, fsys)
	if err != nil {
		return err
	}
	if target == 0 {
		_, err = provider.Up(ctx)
	} else {
		_, err = provider.UpTo(ctx, target)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	applied, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read applied migration version: %w", err)
	}
	return recordChecksums(ctx, conn, sums, applied)
}

func newProvider(db *sql.DB, fsys fs.FS) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return nil, fmt.Errorf("open migration provider: %w", err)
	}
	return p, nil
}

func embeddedMigrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, migrationDir)
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

func MigrationsCurrent(ctx context.Context, db *sql.DB) (bool, error) {
	want, err := EmbeddedVersion()
	if err != nil {
		return false, err
	}
	provider, err := newProvider(db, embeddedMigrations())
	if err != nil {
		return false, err
	}
	got, err := provider.GetDBVersion(ctx)
	if err != nil {
		return false, fmt.Errorf("read applied migration version: %w", err)
	}
	return got == want, nil
}

func EmbeddedVersion() (int64, error) {
	versions, err := versionedFiles(embeddedMigrations())
	if err != nil {
		return 0, err
	}
	var max int64
	for v := range versions {
		if v > max {
			max = v
		}
	}
	return max, nil
}

// versionedFiles maps each migration's version to its file name.
func versionedFiles(fsys fs.FS) (map[int64]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	out := map[int64]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s does not follow NNNNN_name.sql", name)
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s has an unreadable version prefix: %w", name, err)
		}
		out[v] = name
	}
	return out, nil
}
