package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/quotacore/quotacore/internal/admin"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/dataplane"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/ledger"
	"github.com/quotacore/quotacore/internal/store"
)

// The integration harness assembles the request path exactly as run does,
// against a fresh Postgres database and the configured datastore, and serves
// it from httptest. Tenants, plans, keys and balance hashes are seeded
// directly, because the routes that create them are IP-09's. Every tenant id
// is a fresh UUID, so tests share the datastore without sharing a key.

func integrationURLs(t *testing.T) (dbURL, redisURL string) {
	t.Helper()
	dbURL, redisURL = os.Getenv("QUOTACORE_DATABASE_URL"), os.Getenv("QUOTACORE_REDIS_URL")
	if dbURL == "" || redisURL == "" {
		if os.Getenv("QUOTACORE_REQUIRE_INTEGRATION") != "" {
			t.Fatal("QUOTACORE_REQUIRE_INTEGRATION is set but a URL is unset, so this test would skip silently")
		}
		t.Skip("QUOTACORE_DATABASE_URL or QUOTACORE_REDIS_URL is unset")
	}
	return dbURL, redisURL
}

// freshDatabaseURL creates an empty, migrated database and drops it when the
// test ends.
func freshDatabaseURL(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := db.Open(ctx, base)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	name := "qc_t_" + hex.EncodeToString(nonce[:])
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	conn, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := db.Migrate(ctx, conn, db.MigrateOptions{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return u.String()
}

type capturedEvents struct {
	mu     sync.Mutex
	events []dataplane.Event
}

func (c *capturedEvents) Enqueue(e dataplane.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *capturedEvents) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stack struct {
	t       *testing.T
	app     *enforcement
	server  *httptest.Server
	sql     *sql.DB // the service's own handle
	seedDB  *sql.DB // a separate handle the test seeds with
	rdb     *redis.Client
	events  *capturedEvents
	logs    *syncBuffer
	reg     *prometheus.Registry
	dbURL   string
	client  *http.Client
	runtime string
	admin   string
}

type stackOptions struct {
	redisURL string // overrides the configured datastore, e.g. a proxy
	dbURL    string // overrides the fresh database, e.g. a proxy in front of it
	env      map[string]string
	// adminStore wraps the Postgres admin store, e.g. with a commit hook.
	adminStore func(*db.Admin) admin.Store
	// logs, when set, is where the stack logs, so two stacks can share one.
	logs *syncBuffer
	// appender wraps the ledger's Postgres appender, e.g. to stall it.
	appender func(*db.Ledger) ledger.Appender
	// noWriter leaves the ledger writer unstarted.
	noWriter bool
}

func newStack(t *testing.T, opts ...func(*stackOptions)) *stack {
	t.Helper()
	baseDB, redisURL := integrationURLs(t)
	dbURL := freshDatabaseURL(t, baseDB)
	o := stackOptions{redisURL: redisURL, dbURL: dbURL, env: map[string]string{}}
	for _, f := range opts {
		f(&o)
	}
	env := map[string]string{
		"QUOTACORE_DATABASE_URL":            o.dbURL,
		"QUOTACORE_REDIS_URL":               o.redisURL,
		"QUOTACORE_CONFIG_REFRESH_INTERVAL": "1s",
		// Every request of a test comes from one address, so the admin
		// limit is lifted unless a test is about it.
		"QUOTACORE_RATELIMIT_ADMIN_RPM": "6000000",
	}
	for k, v := range o.env {
		env[k] = v
	}
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	seed, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.Close() })
	data, err := store.Open(store.Options{URL: cfg.RedisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { data.Close() })
	direct, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(direct)
	t.Cleanup(func() { rdb.Close() })

	logs := o.logs
	if logs == nil {
		logs = &syncBuffer{}
	}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	events := &capturedEvents{}
	var aopt assembleOptions
	if o.adminStore != nil {
		aopt.adminStore = o.adminStore(db.NewAdmin(conn))
	}
	aopt.appender = o.appender
	aopt.noWriter = o.noWriter
	// Retention's first tick waits an hour in a test, so it never prunes a
	// test's rows unless the test calls it.
	aopt.retentionFirstRun = time.Hour
	app, err := assemble(ctx, cfg, log, conn, data, func() bool { return true }, events, aopt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	reg := prometheus.NewRegistry()
	if err := app.Plane.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := app.Ledger.Metrics().Register(reg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	s := &stack{t: t, app: app, server: srv, sql: conn, seedDB: seed, rdb: rdb, events: events, logs: logs,
		reg: reg, dbURL: dbURL, client: srv.Client()}
	s.runtime = s.key(auth.Runtime)
	s.admin = s.key(auth.Admin)
	return s
}

// key inserts an api_keys row and returns its token.
func (s *stack) key(scope auth.Scope) string {
	s.t.Helper()
	is, err := auth.IssueToken(scope)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.seedDB.Exec(`INSERT INTO api_keys (name, prefix, key_hash, scope) VALUES ($1, $2, $3, $4)`,
		"k-"+is.Prefix, is.Prefix, is.Hash, string(scope)); err != nil {
		s.t.Fatal(err)
	}
	return is.Token
}

// ent is one entitlement a seeded plan grants.
type ent struct {
	feature  string
	limit    int64
	interval string
	archived bool
}

type tenant struct {
	ID       string
	External string
	Zone     string
	Anchor   time.Time
	ents     map[string]ent
}

// tenant seeds a plan with the entitlements, a tenant on it, and the
// tenant's entry in qc:cfg:tenants; it does not seed any balance hash.
func (s *stack) tenant(external, zone string, anchor time.Time, ents ...ent) tenant {
	s.t.Helper()
	ctx := context.Background()
	var planID string
	if err := s.seedDB.QueryRowContext(ctx, `INSERT INTO plans (key, name) VALUES ($1, 'P') RETURNING id`,
		"p-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:16]).Scan(&planID); err != nil {
		s.t.Fatal(err)
	}
	tn := tenant{External: external, Zone: zone, Anchor: anchor, ents: map[string]ent{}}
	for _, e := range ents {
		var fid string
		err := s.seedDB.QueryRowContext(ctx, `SELECT id FROM features WHERE key = $1`, e.feature).Scan(&fid)
		if err == sql.ErrNoRows {
			state := "active"
			if e.archived {
				state = "archived"
			}
			err = s.seedDB.QueryRowContext(ctx, `INSERT INTO features (key, name, unit, state) VALUES ($1, 'F', 'token', $2) RETURNING id`,
				e.feature, state).Scan(&fid)
		}
		if err != nil {
			s.t.Fatal(err)
		}
		if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval) VALUES ($1, $2, $3, $4)`,
			planID, fid, e.limit, e.interval); err != nil {
			s.t.Fatal(err)
		}
		tn.ents[e.feature] = e
	}
	if err := s.seedDB.QueryRowContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, timezone, anchor_at)
		VALUES ($1, 'T', $2, $3, $4) RETURNING id`, external, planID, zone, anchor).Scan(&tn.ID); err != nil {
		s.t.Fatal(err)
	}
	// The tenant is pinned to its plan's cadences, as provisioning pins it
	// (IP-10, D1).
	if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
		SELECT $1, feature_id, interval FROM plan_entitlements WHERE plan_id = $2`, tn.ID, planID); err != nil {
		s.t.Fatal(err)
	}
	if err := s.rdb.HSet(ctx, store.ConfigTenantsKey(), tn.ID, 1).Err(); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { s.rdb.HDel(context.Background(), store.ConfigTenantsKey(), tn.ID) })
	return tn
}

// stored is the six-field hash as the control plane writes it.
type stored struct {
	Balance, Limit, Bonus, Index int64
	Start                        time.Time
	End                          *time.Time
}

// currentWindow is the window the engine computes now for one of the
// tenant's entitlements.
func (s *stack) currentWindow(tn tenant, feature string, at time.Time) cycle.Window {
	s.t.Helper()
	loc, err := time.LoadLocation(tn.Zone)
	if err != nil {
		s.t.Fatal(err)
	}
	return cycle.CurrentWindow(tn.Anchor, intervalOf(s.t, tn.ents[feature].interval), loc, at)
}

func intervalOf(t *testing.T, label string) cycle.Interval {
	for iv := cycle.Hourly; iv <= cycle.Never; iv++ {
		if iv.String() == label {
			return iv
		}
	}
	t.Fatalf("no interval %q", label)
	return 0
}

// seedCurrent writes a full balance for the current window.
func (s *stack) seedCurrent(tn tenant, feature string, balance int64) stored {
	s.t.Helper()
	w := s.currentWindow(tn, feature, time.Now())
	st := stored{Balance: balance, Limit: tn.ents[feature].limit, Index: w.Index, Start: w.Start, End: w.End}
	s.seed(tn, feature, st)
	return st
}

func (s *stack) seed(tn tenant, feature string, st stored) {
	s.t.Helper()
	key := s.balanceKey(tn, feature)
	end := ""
	if st.End != nil {
		end = strconv.FormatInt(st.End.UnixMilli(), 10)
	}
	ctx := context.Background()
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, "balance", st.Balance, "limit", st.Limit, "bonus", st.Bonus, "cycle_index", st.Index,
		"window_start", st.Start.UnixMilli(), "window_end", end)
	// The scripts' rule is window_end + 24h. A test that seeds a window
	// which ended longer ago than that, to read it back verbatim, leaves the
	// key without an expiry rather than have the store delete it at once.
	if st.End != nil && st.End.Add(24*time.Hour).After(time.Now()) {
		pipe.ExpireAt(ctx, key, st.End.Add(24*time.Hour))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { s.rdb.Del(context.Background(), key) })
}

func (s *stack) balanceKey(tn tenant, feature string) string {
	s.t.Helper()
	k, err := store.BalanceKey(tn.ID, feature)
	if err != nil {
		s.t.Fatal(err)
	}
	return k
}

// state is everything a refusal must not change for one feature: the six
// fields, the absolute expiry, every idempotency record of the tenant, and
// the events handed to the sink.
type state struct {
	Hash    map[string]string
	Expiry  time.Duration
	Records map[string]string
	Events  int
}

func (s *stack) state(tn tenant, feature string) state {
	s.t.Helper()
	ctx := context.Background()
	key := s.balanceKey(tn, feature)
	hash, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		s.t.Fatal(err)
	}
	exp, err := s.rdb.PExpireTime(ctx, key).Result()
	if err != nil {
		s.t.Fatal(err)
	}
	records := map[string]string{}
	iter := s.rdb.Scan(ctx, 0, "qc:{t:"+tn.ID+"}:idem:*", 1000).Iterator()
	for iter.Next(ctx) {
		v, err := s.rdb.Get(ctx, iter.Val()).Result()
		if err != nil {
			s.t.Fatal(err)
		}
		records[iter.Val()] = v
	}
	if err := iter.Err(); err != nil {
		s.t.Fatal(err)
	}
	return state{Hash: hash, Expiry: exp, Records: records, Events: s.events.count()}
}

func (s *stack) balance(tn tenant, feature string) int64 {
	s.t.Helper()
	n, err := s.rdb.HGet(context.Background(), s.balanceKey(tn, feature), "balance").Int64()
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%d: not JSON: %s", r.Status, r.Body)
	}
	return m
}

func (r response) code(t *testing.T) string {
	t.Helper()
	if r.Status < 400 {
		return ""
	}
	e, _ := r.json(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r response) details(t *testing.T) map[string]any {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

func (s *stack) do(method, path, token, body string, headers ...string) response {
	s.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.server.URL+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

var keyCounter atomic.Int64

// newKey is a fresh Idempotency-Key.
func newKey() string {
	return "k-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(keyCounter.Add(1), 10)
}

func metered(feature string, amount int64, external string) string {
	return `{"feature_key":"` + feature + `","amount":` + strconv.FormatInt(amount, 10) + `,"tenant_external_id":"` + external + `"}`
}

func (s *stack) consume(token, feature string, amount int64, external, key string) response {
	return s.do(http.MethodPost, "/v1/consume", token, metered(feature, amount, external), "Idempotency-Key", key)
}

// externalID is a fresh tenant identifier.
func externalID(prefix string) string {
	return prefix + "-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

// proxy is a TCP proxy whose behaviour a test switches while connections are
// open: pass forwards both ways, blackhole swallows what the client sends and
// answers nothing, deaf forwards the client's bytes and drops the server's
// replies, and refuse closes every connection and turns new ones away.
type proxy struct {
	ln     net.Listener
	target string
	mode   atomic.Value // string
	mu     sync.Mutex
	conns  []net.Conn
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target}
	p.mode.Store("pass")
	t.Cleanup(func() { ln.Close(); p.closeAll() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if p.mode.Load() == "refuse" {
				c.Close()
				continue
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) set(mode string) {
	p.mode.Store(mode)
	if mode == "refuse" {
		p.closeAll()
	}
}

func (p *proxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *proxy) serve(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, client, server)
	p.mu.Unlock()
	pump := func(dst, src net.Conn, upstream bool) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				mode := p.mode.Load().(string)
				forward := mode == "pass" || (mode == "deaf" && upstream)
				if forward {
					if _, werr := dst.Write(buf[:n]); werr != nil {
						return
					}
				}
			}
			if err != nil {
				dst.Close()
				src.Close()
				return
			}
		}
	}
	go pump(server, client, true)
	go pump(client, server, false)
}

// redisURLVia rewrites a redis URL to dial addr.
func urlVia(t *testing.T, raw, addr string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = addr
	return u.String()
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// flush waits until every event enqueued so far is written, dropped or
// dead-lettered, so a test reads the ledger table without polling it.
func (s *stack) flush() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.app.Ledger.Flush(ctx); err != nil {
		s.t.Fatalf("flush the ledger: %v", err)
	}
}

func (s *stack) counter(name string, labels ...string) float64 {
	s.t.Helper()
	families, err := s.reg.Gather()
	if err != nil {
		s.t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			for i, l := range m.GetLabel() {
				if i >= len(labels) || l.GetValue() != labels[i] {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}
