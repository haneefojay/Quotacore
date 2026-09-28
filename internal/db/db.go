package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"strings"

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
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
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

func Migrate(ctx context.Context, db *sql.DB, opts MigrateOptions) error {
	if opts.Disabled {
		return nil
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if _, err := db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer db.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	if err := goose.UpContext(ctx, db, migrationDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func MigrationsCurrent(ctx context.Context, db *sql.DB) (bool, error) {
	want, err := EmbeddedVersion()
	if err != nil {
		return false, err
	}
	got, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return false, fmt.Errorf("read applied migration version: %w", err)
	}
	return got == want, nil
}

func EmbeddedVersion() (int64, error) {
	entries, err := migrationsFS.ReadDir(migrationDir)
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var max int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return 0, fmt.Errorf("migration %s does not follow NNNNN_name.sql", name)
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %s has an unreadable version prefix: %w", name, err)
		}
		if v > max {
			max = v
		}
	}
	return max, nil
}
