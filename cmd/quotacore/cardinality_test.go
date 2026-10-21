package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
	"github.com/quotacore/quotacore/internal/store"
)

// IP-13 DoD 3 (NFR-O2): no label takes a value per tenant, so the series
// count is the same over 50 tenants and over 2,000; every label value is in
// its closed or bounded set; and nothing a customer sent is a label.

// canary is a value no code can produce by accident.
func canary(kind string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "cnry" + kind + hex.EncodeToString(b[:])
}

// cardinalityRun is one stack with one plan of features entitlements and
// tenants tenants on it, every tenant consuming once and the first tenant on
// every feature, then every provocation the data-plane routes have. It returns
// the scrape, the features and key prefixes the run registered, and every
// customer value it sent.
type cardinalityRun struct {
	all      []series
	raw      string
	features map[string]bool
	prefixes map[string]bool
	canaries []string
}

func runCardinality(t *testing.T, tenants, features int) cardinalityRun {
	t.Helper()
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_CONFIG_REFRESH_MISS_RATE_LIMIT"] = "1000000"
	})
	ctx := context.Background()
	run := cardinalityRun{features: map[string]bool{}, prefixes: map[string]bool{}}
	var planID string
	if err := s.seedDB.QueryRowContext(ctx, `INSERT INTO plans (key, name) VALUES ($1, 'P') RETURNING id`,
		uniq("card")).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	prefix := uniq("cf")
	var keys []string
	for i := 0; i < features; i++ {
		keys = append(keys, fmt.Sprintf("%s.%d", prefix, i))
		run.features[keys[i]] = true
	}
	if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO features (key, name, unit, state)
		SELECT k, 'F', 'token', 'active' FROM unnest($1::text[]) k`, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO plan_entitlements (plan_id, feature_id, limit_value, interval)
		SELECT $1, id, 1000, 'monthly' FROM features WHERE key = ANY($2)`, planID, keys); err != nil {
		t.Fatal(err)
	}
	var externals []string
	for i := 0; i < tenants; i++ {
		externals = append(externals, canary("ext"))
	}
	rows, err := s.seedDB.QueryContext(ctx, `INSERT INTO tenants (external_id, display_name, plan_id, timezone, anchor_at)
		SELECT e, $2, $3, 'UTC', $4 FROM unnest($1::text[]) e RETURNING id, external_id`,
		externals, canary("name"), planID, monthlyAnchor)
	if err != nil {
		t.Fatal(err)
	}
	var all []tenant
	for rows.Next() {
		tn := tenant{Zone: "UTC", Anchor: monthlyAnchor, ents: map[string]ent{}}
		if err := rows.Scan(&tn.ID, &tn.External); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			tn.ents[k] = ent{k, 1000, "monthly", false}
		}
		all = append(all, tn)
		run.canaries = append(run.canaries, tn.ID, tn.External)
	}
	rows.Close()
	if _, err := s.seedDB.ExecContext(ctx, `INSERT INTO tenant_entitlement_pins (tenant_id, feature_id, interval)
		SELECT t.id, pe.feature_id, pe.interval FROM tenants t JOIN plan_entitlements pe ON pe.plan_id = t.plan_id
		WHERE t.plan_id = $1`, planID); err != nil {
		t.Fatal(err)
	}
	// The balance hashes the workload reads: each tenant's own feature, and
	// every feature for the first tenant.
	w := s.currentWindow(all[0], keys[0], time.Now())
	pipe := s.rdb.Pipeline()
	var written []string
	put := func(tn tenant, feature string) {
		k, err := store.BalanceKey(tn.ID, feature)
		if err != nil {
			t.Fatal(err)
		}
		pipe.HSet(ctx, k, "balance", 1000, "limit", 1000, "bonus", 0, "cycle_index", w.Index,
			"window_start", w.Start.UnixMilli(), "window_end", strconv.FormatInt(w.End.UnixMilli(), 10))
		written = append(written, k)
	}
	for i, tn := range all {
		pipe.HSet(ctx, store.ConfigTenantsKey(), tn.ID, 1)
		put(tn, keys[i%features])
	}
	for _, k := range keys[1:] {
		put(all[0], k)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := make([]string, len(all))
		for i, tn := range all {
			ids[i] = tn.ID
		}
		s.rdb.HDel(context.Background(), store.ConfigTenantsKey(), ids...)
		s.rdb.Del(context.Background(), written...)
	})

	meta := canary("meta")
	idem := canary("idem")
	run.canaries = append(run.canaries, meta, idem)
	body := func(feature, external string) string {
		return `{"feature_key":"` + feature + `","amount":1,"tenant_external_id":"` + external +
			`","metadata":{"` + canary("mkey") + `":"` + meta + `"}}`
	}
	consume := func(tok, feature, external string) response {
		r := s.do(http.MethodPost, "/v1/consume", tok, body(feature, external), "Idempotency-Key", idem+strconv.Itoa(len(run.canaries)))
		run.canaries = append(run.canaries, r.Header.Get("X-Request-Id"))
		return r
	}
	for i, tn := range all {
		if r := consume(s.runtime, keys[i%features], tn.External); r.Status != http.StatusOK {
			t.Fatalf("tenant %d: %d %s", i, r.Status, r.Body)
		}
	}
	for _, k := range keys {
		mustStatus(t, consume(s.runtime, k, all[0].External), http.StatusOK)
		mustStatus(t, s.do(http.MethodPost, "/v1/check", s.runtime, body(k, all[0].External)), http.StatusOK)
	}
	// What a stranger can send: unknown tenants, features, paths and
	// methods, and malformed, unknown and revoked keys.
	revoked := s.key(auth.Runtime)
	if _, err := s.seedDB.Exec(`UPDATE api_keys SET state = 'revoked', revoked_at = now() WHERE prefix = $1`,
		prefixOf(t, revoked)); err != nil {
		t.Fatal(err)
	}
	s.app.Keys.Invalidate()
	for i := 0; i < 20; i++ {
		consume(s.runtime, keys[0], canary("nobody"))
		consume(s.runtime, canary("feat"), all[0].External)
		consume("qc_runtime_"+canary("tok"), keys[0], all[0].External)
		consume(revoked, keys[0], all[0].External)
		s.do(http.MethodGet, "/"+canary("path"), "", "")
		s.do(canary("M"), "/v1/consume", s.runtime, "")
	}
	run.prefixes[prefixOf(t, s.runtime)] = true
	for _, p := range provocations(t, s) {
		p.send()
		run.features[p.ftr] = true
	}
	for _, f := range []string{"mx.f", "mx.zero", "mx.full", "mx.old", "mx.gone"} {
		run.features[f] = true
	}
	run.all, run.raw = s.scrape()
	return run
}

func prefixOf(t *testing.T, token string) string {
	t.Helper()
	tok, err := auth.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	return tok.Prefix
}

// closedValues are the label values every label may take whatever the
// workload, with feature_key and key_prefix checked against the run's own
// bounded sets.
func closedValues(t *testing.T) map[string]map[string]bool {
	t.Helper()
	set := func(vs ...string) map[string]bool {
		m := map[string]bool{}
		for _, v := range vs {
			m[v] = true
		}
		return m
	}
	endpoints, methods := set("unmatched"), set(observability.MethodOther)
	for _, op := range envelope.MustOperations() {
		endpoints[op.Endpoint()] = true
		methods[op.Method] = true
	}
	return map[string]map[string]bool{
		"outcome": set(observability.OutcomeAllowed, observability.OutcomeDenied, observability.OutcomeReplayed,
			observability.OutcomeRejected, "refused"),
		"code":     catalogue(t),
		"endpoint": endpoints,
		"method":   methods,
		"interval": set(observability.Intervals...),
		"reason": set(observability.DropQueueFull, observability.DropError, observability.DropShutdown,
			observability.DeadDuplicate, observability.DeadRejected),
		"result":  set(observability.MissLoaded, observability.MissUnknown, observability.MissRefused, observability.MissError, observability.RefreshOK, observability.RefreshSkipped),
		"class":   set(observability.ClassTimeout, observability.ClassNoScript, observability.ClassOOM, observability.ClassRefused, observability.ClassOther),
		"command": set(observability.CommandDigest, observability.CommandBody),
	}
}

func checkValues(t *testing.T, run cardinalityRun) {
	t.Helper()
	closed := closedValues(t)
	for _, sr := range run.all {
		for k, v := range sr.labels {
			switch k {
			case "feature_key":
				if !run.features[v] {
					t.Errorf("%s{feature_key=%q} is not a registered feature", sr.name, v)
				}
			case "key_prefix":
				if !run.prefixes[v] {
					t.Errorf("%s{key_prefix=%q} is not an issued key", sr.name, v)
				}
			case "status":
				if n, err := strconv.Atoi(v); err != nil || n < 200 || n > 599 {
					t.Errorf("%s{status=%q} is not an HTTP status", sr.name, v)
				}
			case "version", "commit", "zone_data_version":
				if sr.name != "quotacore_build_info" {
					t.Errorf("%s carries the build label %s", sr.name, k)
				}
			default:
				if allowed, ok := closed[k]; !ok || !allowed[v] {
					t.Errorf("%s{%s=%q} is outside its closed set", sr.name, k, v)
				}
			}
		}
	}
}

// bound is the most series the contract allows, whatever the traffic: the
// error cells, the data-plane latency cells, every (endpoint, method,
// status) a request can be counted under and every endpoint's duration
// histogram, one series per feature and per key, and the fixed rest.
func bound(features, keys int) int {
	ops := envelope.MustOperations()
	cells := 0
	for _, op := range ops {
		cells += len(op.Codes)
	}
	buckets := len(observability.LatencyBuckets) + 2
	endpoints := len(ops) + 1
	statuses := 12 // 200, 201, 204, 400, 401, 403, 404, 405, 409, 413, 429, 500 and 503 share the space with method
	methods := 6
	return cells + 4*5*buckets + endpoints*methods*(statuses+1) + endpoints*buckets + features + keys + 300
}

// TestCardinalityIsIndependentOfTenantCount (IP-13 DoD 3, D5).
func TestCardinalityIsIndependentOfTenantCount(t *testing.T) {
	small := runCardinality(t, 50, 100)
	large := runCardinality(t, 2000, 100)
	if len(small.all) != len(large.all) {
		t.Errorf("series over 50 tenants = %d and over 2,000 = %d; a label is taking a value per tenant", len(small.all), len(large.all))
		names := map[string]int{}
		for _, sr := range large.all {
			names[sr.name]++
		}
		for _, sr := range small.all {
			names[sr.name]--
		}
		for n, d := range names {
			if d != 0 {
				t.Logf("%s differs by %d", n, d)
			}
		}
	}
	checkValues(t, small)
	checkValues(t, large)
	if max := bound(len(large.features), len(large.prefixes)); len(large.all) > max {
		t.Errorf("%d series, above the contract's bound of %d", len(large.all), max)
	}
}

// TestNoLabelValueIsCustomerData (IP-13 DoD 3, testing-strategy.md section 8):
// no external_id, tenant id, request id, metadata key or value,
// Idempotency-Key, token or invented path or method appears anywhere in the
// scrape.
func TestNoLabelValueIsCustomerData(t *testing.T) {
	run := runCardinality(t, 20, 5)
	if strings.Contains(run.raw, "cnry") {
		for _, line := range strings.Split(run.raw, "\n") {
			if strings.Contains(line, "cnry") {
				t.Errorf("customer data in the scrape: %s", line)
			}
		}
	}
	for _, c := range run.canaries {
		if c != "" && strings.Contains(run.raw, c) {
			t.Errorf("%q from the workload is in the scrape", c)
		}
	}
}
