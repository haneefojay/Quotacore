// Command idemsize measures what one idempotency record costs the fast store, and what one ledger
// row costs Postgres, so that deployment.md section 5's sizing is a measurement rather than a
// projection (IP-15 D7, DoD 6, NFR-T9).
//
// It drives the real consume script, through internal/script, against a scratch store that must
// be empty when it starts and is flushed when it ends: one tenant-feature whose balance cannot run
// out, and one fresh Idempotency-Key per call, so every call writes exactly one record. It reads
// used_memory from INFO memory at doubling record counts, each the same fraction past a power of
// two, so the engine's hash tables are at the same point of their growth at every reading and the
// marginal cost between two readings carries the tables' share of each record. It does that once
// per key length: a UUID's 36 characters, which is typical, and the contract's maximum of 255.
//
// The projection is the marginal cost at the longest key times the 24-hour window at NFR-T1's
// sustained 2,000 consume/s, 172.8 million records, plus a quarter for headroom.
//
// With -database it also measures a usage_events row: it migrates the given database, which must
// be a scratch one holding no usage_events, writes rows shaped like the writer's, and reads the
// table's total size, indexes and TOAST included. It never deletes a ledger row, so the database
// is dropped afterwards by whoever created it.
//
// It is a measurement, not a test, and CI does not run it. Its output is committed as
// docs/research/ip-15-idempotency-footprint.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/store"
)

// window is the 24-hour idempotency window (DR-029) at NFR-T1's sustained 2,000 consume/s.
const window = 2000 * 24 * 60 * 60

// bound is the largest amount the contract carries, 2^53 - 1, so the balance never runs out.
const bound = int64(1<<53 - 1)

type reading struct {
	records int64
	used    int64
	keys    int64
}

func main() {
	var (
		redisURL  = flag.String("redis", "", "the scratch store: empty at start, flushed at the end")
		dbURL     = flag.String("database", "", "a scratch Postgres database for the ledger row; empty skips it")
		keyLens   = flag.String("keylen", "36,255", "Idempotency-Key lengths to measure, comma-separated")
		base      = flag.Int64("base", 3<<19, "the first reading's record count; each later one doubles it")
		readings  = flag.Int("readings", 4, "how many readings per key length")
		workers   = flag.Int("workers", 4*runtime.NumCPU(), "concurrent calls")
		maxUsed   = flag.Int64("max-used", 8<<30, "stop a key length before used_memory would pass this")
		ledgerRow = flag.Int("rows", 1_000_000, "usage_events rows to write with -database")
	)
	flag.Parse()
	if *redisURL == "" {
		fmt.Fprintln(os.Stderr, "idemsize: -redis is required")
		os.Exit(2)
	}
	ctx := context.Background()
	if err := run(ctx, *redisURL, *keyLens, *base, *readings, *workers, *maxUsed); err != nil {
		fmt.Fprintln(os.Stderr, "idemsize:", err)
		os.Exit(1)
	}
	if *dbURL != "" {
		if err := ledger(ctx, *dbURL, *ledgerRow); err != nil {
			fmt.Fprintln(os.Stderr, "idemsize:", err)
			os.Exit(1)
		}
	}
}

func run(ctx context.Context, url, lens string, base int64, n, workers int, maxUsed int64) error {
	st, err := store.Open(store.Options{URL: url, DataScriptTimeout: 2 * time.Second})
	if err != nil {
		return err
	}
	c := st.Client()
	defer c.Close()
	if size, err := c.DBSize(ctx).Result(); err != nil || size != 0 {
		return fmt.Errorf("the store holds %d keys (%v); idemsize runs only against an empty scratch store", size, err)
	}
	defer c.FlushAll(ctx)
	server, _ := c.Info(ctx, "server").Result()
	mem, _ := c.Info(ctx, "memory").Result()
	version := field(server, "valkey_version")
	if version == "" {
		version = field(server, "redis_version")
	}
	fmt.Printf("engine    %s %s\n", field(server, "server_name"), version)
	fmt.Printf("allocator %s, policy %s, host %d CPUs %s/%s\n", field(mem, "mem_allocator"),
		field(mem, "maxmemory_policy"), runtime.NumCPU(), runtime.GOOS, runtime.GOARCH)
	if err := script.Load(ctx, c); err != nil {
		return err
	}
	runner, err := script.New(script.Options{Client: c, Timeout: 2 * time.Second})
	if err != nil {
		return err
	}

	var worst float64
	for _, s := range strings.Split(lens, ",") {
		keyLen, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || keyLen < 12 || keyLen > 255 {
			return fmt.Errorf("key length %q: want 12 to 255", s)
		}
		if err := c.FlushAll(ctx).Err(); err != nil {
			return err
		}
		marginal, err := measure(ctx, c, st, runner, keyLen, base, n, workers, maxUsed)
		if err != nil {
			return err
		}
		worst = math.Max(worst, marginal)
	}
	need := worst * window * 1.25
	fmt.Printf("\nprojection  %.1f bytes/record x %d records x 1.25 = %.1f GiB, so the floor is %d GiB\n",
		worst, window, need/(1<<30), int64(math.Ceil(need/(1<<30))))
	return nil
}

func measure(ctx context.Context, c *redis.Client, st *store.Store, runner *script.Runner,
	keyLen int, base int64, n, workers int, maxUsed int64) (float64, error) {
	tenant := uuid.NewString()
	start := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	const feature = "measure.units"
	if err := st.Provision(ctx, tenant, []store.Opening{{Feature: feature, Limit: bound, Start: start}}); err != nil {
		return 0, err
	}
	empty, err := used(ctx, c)
	if err != nil {
		return 0, err
	}
	fmt.Printf("\nkey length %d (empty store, one balance hash: %d bytes)\n", keyLen, empty)
	fmt.Printf("  %12s %14s %10s %18s\n", "records", "used_memory", "keys", "marginal bytes/rec")

	var written atomic.Int64
	var prev *reading
	// worst is the largest marginal cost; bound is what the next reading is sized by before
	// there is a marginal cost to size it by.
	worst, bound := 0.0, 0.0
	target := base
	for i := 0; i < n; i, target = i+1, target*2 {
		if prev != nil && prev.used+int64(float64(target-prev.records)*math.Max(worst, bound)*1.1) > maxUsed {
			fmt.Printf("  stopped: the next reading would pass -max-used, %d bytes\n", maxUsed)
			break
		}
		if err := fill(ctx, runner, tenant, feature, keyLen, start, &written, target, workers); err != nil {
			return 0, err
		}
		u, err := used(ctx, c)
		if err != nil {
			return 0, err
		}
		keys, _ := c.DBSize(ctx).Result()
		r := reading{records: written.Load(), used: u, keys: keys}
		if keys != r.records+1 {
			return 0, fmt.Errorf("the store holds %d keys after %d records; a call wrote other than one record", keys, r.records)
		}
		marginal := ""
		if prev != nil {
			m := float64(r.used-prev.used) / float64(r.records-prev.records)
			worst = math.Max(worst, m)
			marginal = fmt.Sprintf("%.1f", m)
		} else {
			// The first reading has no earlier one at the same phase, so it bounds the cost
			// from above rather than measuring it, and is not part of the projection.
			bound = float64(r.used-empty) / float64(r.records)
		}
		fmt.Printf("  %12d %14d %10d %18s\n", r.records, r.used, r.keys, marginal)
		prev = &r
	}
	sample, _ := store.IdempotencyKey(tenant, idemKey(keyLen, 0))
	usage, _ := c.MemoryUsage(ctx, sample).Result()
	fmt.Printf("  MEMORY USAGE of one record: %d bytes (the value and key alone, not the tables)\n", usage)
	if worst == 0 {
		return 0, errors.New("fewer than two readings, so no marginal cost was measured")
	}
	return worst, nil
}

// fill consumes until written reaches target, each call with its own key.
func fill(ctx context.Context, runner *script.Runner, tenant, feature string, keyLen int,
	start time.Time, written *atomic.Int64, target int64, workers int) error {
	next := atomic.Int64{}
	next.Store(written.Load())
	var wg sync.WaitGroup
	var first error
	var once sync.Once
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= target {
					return
				}
				out, err := runner.Consume(ctx, script.Request{TenantID: tenant, FeatureKey: feature,
					IdempotencyKey: idemKey(keyLen, i), Amount: 1, Window: cycle.Window{Start: start},
					Interval: cycle.Never, Limit: bound, At: time.Now()})
				if err == nil && (!out.Allowed() || out.Replayed()) {
					err = fmt.Errorf("record %d: %s, not a fresh application", i, out.Decision)
				}
				if err != nil {
					once.Do(func() { first = err })
					return
				}
				written.Add(1)
			}
		}()
	}
	wg.Wait()
	return first
}

// idemKey is the i-th key, exactly n characters: a counter padded with a fixed filler. The
// characters do not change what a record costs, only their number does.
func idemKey(n int, i int64) string {
	s := "m-" + strconv.FormatInt(i, 10) + "-"
	return s + strings.Repeat("x", n-len(s))
}

func used(ctx context.Context, c *redis.Client) (int64, error) {
	info, err := c.Info(ctx, "memory").Result()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(field(info, "used_memory"), 10, 64)
}

func field(info, name string) string {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+":"); ok {
			return v
		}
	}
	return ""
}

// ledger writes rows shaped like the ledger writer's consumed rows and reports their size.
func ledger(ctx context.Context, url string, rows int) error {
	conn, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := db.Migrate(ctx, conn, db.MigrateOptions{}); err != nil {
		return err
	}
	var existing int64
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM usage_events`).Scan(&existing); err != nil || existing != 0 {
		return fmt.Errorf("usage_events holds %d rows (%v); -database must be a scratch database", existing, err)
	}
	var version string
	conn.QueryRowContext(ctx, `SHOW server_version`).Scan(&version)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var feature, plan, tenant string
	steps := []struct {
		q   string
		out *string
	}{
		{`INSERT INTO features (key, name, unit) VALUES ('measure.units', 'measure', 'units') RETURNING id`, &feature},
		{`INSERT INTO plans (key, name) VALUES ('measure', 'measure') RETURNING id`, &plan},
	}
	for _, s := range steps {
		if err := tx.QueryRowContext(ctx, s.q).Scan(s.out); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, anchor_at)
		VALUES ('measure', 'measure', $1, now()) RETURNING id`, plan).Scan(&tenant); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	before, err := relationSize(ctx, conn)
	if err != nil {
		return err
	}
	// A consumed row as the writer writes it: a request id and an Idempotency-Key of a UUID's
	// length, empty metadata, and the cycle's start.
	const batch = 100_000
	for done := 0; done < rows; done += batch {
		n := min(batch, rows-done)
		if _, err := conn.ExecContext(ctx, `INSERT INTO usage_events (tenant_id, feature_id, cycle_index, delta,
				balance_after, source, event_type, request_id, idempotency_key, cycle_start)
			SELECT $1, $2, 0, -1, 9007199254740991 - g, 'api', 'consumed',
			       md5(g::text)::uuid::text, md5('k' || g::text)::uuid::text, now()
			  FROM generate_series($3::bigint, $4::bigint) AS g`, tenant, feature, done+1, done+n); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `VACUUM ANALYZE usage_events`); err != nil {
		return err
	}
	after, err := relationSize(ctx, conn)
	if err != nil {
		return err
	}
	per := float64(after-before) / float64(rows)
	fmt.Printf("\nPostgres %s: %d usage_events rows, %d bytes with indexes and TOAST, %.1f bytes/row, %.2f GiB per million rows\n",
		version, rows, after-before, per, per*1e6/(1<<30))
	// The ledger is append-only (DR-044) even here: the rows stay, and the scratch database is
	// dropped by whoever created it.
	fmt.Println("the rows remain; drop the scratch database")
	return nil
}

func relationSize(ctx context.Context, conn *sql.DB) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, `SELECT pg_total_relation_size('usage_events')`).Scan(&n)
	return n, err
}
