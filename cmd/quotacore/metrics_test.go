package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/envelope"
	"github.com/quotacore/quotacore/internal/observability"
)

// IP-13's metrics, proved on the stack's own scrape of its own metrics
// listener: never a process-global registry, so a parallel package cannot
// move a number a test reads.

// series is one line of a scrape.
type series struct {
	family string
	name   string
	labels map[string]string
	value  float64
}

var (
	seriesLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})? (\S+)$`)
	labelPair  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// scrape GETs /metrics from the stack's metrics listener and returns the
// series, each with the family its # TYPE line declared, and the raw text.
func (s *stack) scrape() ([]series, string) {
	s.t.Helper()
	resp, err := http.Get(s.metrics.URL + "/metrics")
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET /metrics = %d %s", resp.StatusCode, raw)
	}
	histograms := map[string]bool{}
	var out []series
	for _, line := range strings.Split(string(raw), "\n") {
		if f, ok := strings.CutPrefix(line, "# TYPE "); ok {
			if name, typ, _ := strings.Cut(f, " "); typ == "histogram" {
				histograms[name] = true
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := seriesLine.FindStringSubmatch(line)
		if m == nil {
			s.t.Fatalf("a scrape line that is not a series: %q", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			s.t.Fatalf("a series whose value is not a number: %q", line)
		}
		sr := series{family: m[1], name: m[1], labels: map[string]string{}, value: v}
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if base, ok := strings.CutSuffix(m[1], suffix); ok && histograms[base] {
				sr.family = base
			}
		}
		for _, p := range labelPair.FindAllStringSubmatch(m[2], -1) {
			if p[1] == "le" && sr.family != sr.name {
				continue
			}
			sr.labels[p[1]] = p[2]
		}
		out = append(out, sr)
	}
	return out, string(raw)
}

// value is the sum of a family's series whose labels include want.
func value(all []series, name string, want ...string) float64 {
	total := 0.0
next:
	for _, sr := range all {
		if sr.name != name {
			continue
		}
		for i := 0; i+1 < len(want); i += 2 {
			if sr.labels[want[i]] != want[i+1] {
				continue next
			}
		}
		total += sr.value
	}
	return total
}

func documentedMetrics(t *testing.T) map[string][]string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := observability.DocumentedMetrics(doc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// waitFor polls a condition, never with a consume, and fails at the deadline.
func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEveryDocumentedMetricIsServed (IP-13 DoD 1, D1): after a workload that
// reaches every collector, the metrics listener serves exactly the families
// observability.md section 1 names, each with exactly its labels.
func TestEveryDocumentedMetricIsServed(t *testing.T) {
	s := newStack(t, func(o *stackOptions) {
		o.env["QUOTACORE_RATELIMIT_RUNTIME_RPS"] = "0.001"
		o.env["QUOTACORE_RATELIMIT_RUNTIME_BURST"] = "1000"
	})
	f, _, tn := s.provisioned(1000)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.refund(f, 1, tn.External, newKey()), http.StatusOK)
	mustStatus(t, s.do(http.MethodPost, "/v1/check", s.runtime, metered(f, 1, tn.External)), http.StatusOK)
	mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, ""), http.StatusOK)
	s.do(http.MethodGet, "/nowhere", "", "")
	limited := s.key("runtime")
	for i := 0; i < 1001; i++ {
		if s.do(http.MethodPost, "/v1/check", limited, metered(f, 1, tn.External)).Status == http.StatusTooManyRequests {
			break
		}
	}

	want := documentedMetrics(t)
	all, _ := s.scrape()
	served := map[string]bool{}
	for _, sr := range all {
		served[sr.family] = true
		labels, ok := want[sr.family]
		if !ok {
			t.Errorf("%s is served and observability.md section 1 does not name it", sr.name)
			continue
		}
		var names []string
		for k := range sr.labels {
			names = append(names, k)
		}
		sort.Strings(names)
		if !slices.Equal(names, labels) {
			t.Errorf("%s is served with labels %v, and observability.md says %v", sr.name, names, labels)
		}
	}
	for name := range want {
		if !served[name] {
			t.Errorf("%s is in observability.md section 1 and is not served", name)
		}
	}
}

// TestTheServedCodeLabelsAreTheCatalogue (IP-13 DoD 2, D4): the scrape's
// code values of quotacore_errors_total are error-catalog.md section 2, and
// each (code, endpoint) cell is one the contract lists, both ways.
func TestTheServedCodeLabelsAreTheCatalogue(t *testing.T) {
	s := newStack(t)
	all, _ := s.scrape()
	codes, cells := map[string]bool{}, map[[2]string]bool{}
	for _, sr := range all {
		if sr.name == "quotacore_errors_total" {
			codes[sr.labels["code"]] = true
			cells[[2]string{sr.labels["code"], sr.labels["endpoint"]}] = true
		}
	}
	for code := range catalogue(t) {
		if !codes[code] {
			t.Errorf("%s is in error-catalog.md and no errors_total series carries it", code)
		}
	}
	cat := catalogue(t)
	for code := range codes {
		if !cat[code] {
			t.Errorf("errors_total carries %s, which error-catalog.md does not list", code)
		}
	}
	want := map[[2]string]bool{}
	for _, op := range envelope.MustOperations() {
		for _, c := range op.Codes {
			want[[2]string{string(c), op.Endpoint()}] = true
		}
	}
	for c := range want {
		if !cells[c] {
			t.Errorf("the contract lists %s on %s and it is not served", c[0], c[1])
		}
	}
	for c := range cells {
		if !want[c] {
			t.Errorf("errors_total{code=%q,endpoint=%q} is served and the contract does not list it", c[0], c[1])
		}
	}
}

// TestRolledOverCountsEveryTransitionOnce (IP-13 DoD 1): a request that
// crosses a boundary counts one roll under its entitlement's interval, a
// replay of it counts none, and a check, a grant and a request that crosses
// nothing count as they transition.
func TestRolledOverCountsEveryTransitionOnce(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("ro"), "UTC", monthlyAnchor,
		ent{"ro.h", 100, "hourly", false}, ent{"ro.m", 100, "monthly", false}, ent{"ro.n", 100, "never", false})
	s.seedPrevious(tn, "ro.h", 3)
	s.seedPrevious(tn, "ro.m", 3)
	s.seedCurrent(tn, "ro.n", 100)
	read := func(iv string) float64 { return s.counter("quotacore_rolled_over_total", iv) }

	key := newKey()
	mustStatus(t, s.consume(s.runtime, "ro.h", 1, tn.External, key), http.StatusOK)
	if got := read("hourly"); got != 1 {
		t.Errorf("rolled_over_total{hourly} after a crossing consume = %v, want 1", got)
	}
	mustStatus(t, s.consume(s.runtime, "ro.h", 1, tn.External, key), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, "ro.h", 1, tn.External, newKey()), http.StatusOK)
	if got := read("hourly"); got != 1 {
		t.Errorf("rolled_over_total{hourly} after a replay and a request in the window = %v, want 1", got)
	}
	mustStatus(t, s.do(http.MethodPost, "/v1/check", s.runtime, metered("ro.m", 1, tn.External)), http.StatusOK)
	if got := read("monthly"); got != 1 {
		t.Errorf("rolled_over_total{monthly} after a crossing check = %v, want 1", got)
	}
	mustStatus(t, s.consume(s.runtime, "ro.n", 1, tn.External, newKey()), http.StatusOK)
	if got := read("never"); got != 0 {
		t.Errorf("rolled_over_total{never} = %v, want 0", got)
	}

	// A grant runs the shared transition too.
	s.seedPrevious(tn, "ro.h", 3)
	mustStatus(t, s.adminDo(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/grant",
		`{"feature_key":"ro.h","amount":5,"reason":"r"}`), http.StatusOK)
	if got := read("hourly"); got != 2 {
		t.Errorf("rolled_over_total{hourly} after a crossing grant = %v, want 2", got)
	}
}

// TestAStaleTransitionIsCountedAndABoundaryRaceIsNot (IP-13 DoD 1, D3): a
// stored index ahead of the caller's is a clock or data defect and counted,
// unless it is one ahead and opened within the skew threshold of the
// caller's clock; a stored index behind an open window (a snapshot behind a
// re-anchor) is not counted.
func TestAStaleTransitionIsCountedAndABoundaryRaceIsNot(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("st"), "UTC", monthlyAnchor, ent{"st.f", 100, "hourly", false})
	now := time.Now()
	cur := s.currentWindow(tn, "st.f", now)
	read := func() float64 { return s.counter("quotacore_rollover_stale_total") }
	ahead := func(by int64, start time.Time) {
		end := start.Add(time.Hour)
		s.seed(tn, "st.f", stored{Balance: 50, Limit: 100, Index: cur.Index + by, Start: start, End: &end})
	}

	// One ahead, opened 300 ms after the caller's reading: a boundary race.
	ahead(1, time.Now().Add(300*time.Millisecond))
	mustStatus(t, s.consume(s.runtime, "st.f", 1, tn.External, newKey()), http.StatusOK)
	if got := read(); got != 0 {
		t.Errorf("stale counted %v times for a boundary race within the threshold, want 0", got)
	}
	// One ahead, an hour ahead: the store's clock or its data is wrong.
	ahead(1, cur.Start.Add(time.Hour))
	mustStatus(t, s.consume(s.runtime, "st.f", 1, tn.External, newKey()), http.StatusOK)
	if got := read(); got != 1 {
		t.Errorf("stale counted %v times for an index an hour ahead, want 1", got)
	}
	// Two ahead.
	ahead(2, cur.Start.Add(2*time.Hour))
	mustStatus(t, s.consume(s.runtime, "st.f", 1, tn.External, newKey()), http.StatusOK)
	if got := read(); got != 2 {
		t.Errorf("stale counted %v times after an index two ahead, want 2", got)
	}
	// Behind an open window: a caller ahead of a re-anchored store.
	end := cur.End.Add(time.Hour)
	s.seed(tn, "st.f", stored{Balance: 50, Limit: 100, Index: cur.Index - 1, Start: cur.Start.Add(-time.Hour), End: &end})
	mustStatus(t, s.consume(s.runtime, "st.f", 1, tn.External, newKey()), http.StatusOK)
	if got := read(); got != 2 {
		t.Errorf("stale counted for an index behind an open window: %v, want 2", got)
	}
}

// TestACeilingViolationIsCounted (IP-13 DoD 1): a hash above limit plus
// bonus, read by a consume or by the balance route, is counted (DR-019).
func TestACeilingViolationIsCounted(t *testing.T) {
	s := newStack(t)
	tn := s.tenant(externalID("cv"), "UTC", monthlyAnchor, ent{"cv.f", 100, "monthly", false})
	w := s.currentWindow(tn, "cv.f", time.Now())
	s.seed(tn, "cv.f", stored{Balance: 150, Limit: 100, Bonus: 10, Index: w.Index, Start: w.Start, End: w.End})
	mustStatus(t, s.consume(s.runtime, "cv.f", 1, tn.External, newKey()), http.StatusOK)
	if got := s.counter("quotacore_ceiling_violation_total"); got != 1 {
		t.Errorf("ceiling_violation_total after a consume of a broken hash = %v, want 1", got)
	}
	mustStatus(t, s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, ""), http.StatusOK)
	if got := s.counter("quotacore_ceiling_violation_total"); got != 2 {
		t.Errorf("ceiling_violation_total after the balance route read it = %v, want 2", got)
	}
	s.seedCurrent(tn, "cv.f", 100)
	mustStatus(t, s.consume(s.runtime, "cv.f", 1, tn.External, newKey()), http.StatusOK)
	if got := s.counter("quotacore_ceiling_violation_total"); got != 2 {
		t.Errorf("ceiling_violation_total after a sound hash = %v, want 2", got)
	}
}

// TestTheStoreMetricsFollowAnOutage (IP-13 DoD 1): through the stack's own
// proxy, datastore_up falls to 0 within two probe intervals of a cut and
// returns to 1 after it; a silent store is a timeout and a refused one is
// refused.
func TestTheStoreMetricsFollowAnOutage(t *testing.T) {
	_, redisURL := integrationURLs(t)
	p := newProxy(t, hostOf(t, redisURL))
	s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, p.addr()) })
	f, _, tn := s.provisioned(100)
	up := func() float64 { return s.counter("quotacore_datastore_up") }
	waitFor(t, time.Second, "datastore_up = 1", func() bool { return up() == 1 })
	if lat := histogramSum(t, s.app.Dependencies.DataStoreLatency); lat <= 0 {
		t.Errorf("datastore_latency_seconds has observed nothing: %v", lat)
	}
	if skew := s.counter("quotacore_clock_skew_seconds"); skew > 1 || skew < -1 {
		t.Errorf("clock_skew_seconds against a local store = %v", skew)
	}

	timeouts := s.counter("quotacore_datastore_errors_total", observability.ClassTimeout)
	p.set("blackhole")
	waitFor(t, time.Second, "datastore_up = 0", func() bool { return up() == 0 })
	if got := s.counter("quotacore_datastore_errors_total", observability.ClassTimeout); got <= timeouts {
		t.Errorf("a silent store counted no timeout: %v → %v", timeouts, got)
	}
	p.set("pass")
	waitFor(t, 2*time.Second, "datastore_up = 1 again", func() bool { return up() == 1 })

	refused := s.counter("quotacore_datastore_errors_total", observability.ClassRefused)
	p.set("refuse")
	s.consume(s.runtime, f, 1, tn.External, newKey())
	waitFor(t, 2*time.Second, "a refused connection counted as refused", func() bool {
		return s.counter("quotacore_datastore_errors_total", observability.ClassRefused) > refused
	})
	p.set("pass")
}

// TestControlPlaneUpFollowsPostgres (IP-13 DoD 1).
func TestControlPlaneUpFollowsPostgres(t *testing.T) {
	baseDB, _ := integrationURLs(t)
	var p *proxy
	s := newStack(t, func(o *stackOptions) {
		p = newProxy(t, hostOf(t, baseDB))
		o.dbURL = urlVia(t, o.dbURL, p.addr())
	})
	up := func() float64 { return s.counter("quotacore_controlplane_up") }
	waitFor(t, 2*time.Second, "controlplane_up = 1", func() bool { return up() == 1 })
	p.set("refuse")
	s.sql.SetMaxIdleConns(0)
	waitFor(t, 5*time.Second, "controlplane_up = 0", func() bool { return up() == 0 })
	p.set("pass")
	waitFor(t, 5*time.Second, "controlplane_up = 1 again", func() bool { return up() == 1 })
}

// TestTheScriptCacheCountsAReload (IP-13 DoD 1, S2): after SCRIPT FLUSH, one
// consume is two digest calls (the miss and the retry), one hit, and one body
// load per embedded script.
func TestTheScriptCacheCountsAReload(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	digest := s.counter("quotacore_script_loads_total", observability.CommandDigest)
	body := s.counter("quotacore_script_loads_total", observability.CommandBody)
	hits := s.counter("quotacore_script_cache_hits_total")
	if body < 1 {
		t.Fatalf("no script body load was counted at start: %v", body)
	}
	scripts := body // the startup load put every script once

	if err := s.rdb.ScriptFlush(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	// Another package's test can flush too, so the bounds allow one more
	// reload than this test caused, and never fewer.
	if got := s.counter("quotacore_script_loads_total", observability.CommandDigest) - digest; got < 2 {
		t.Errorf("digest calls for one consume after a flush = %v, want at least 2", got)
	}
	if got := s.counter("quotacore_script_loads_total", observability.CommandBody) - body; got < scripts {
		t.Errorf("body loads after a flush = %v, want at least %v", got, scripts)
	}
	if got := s.counter("quotacore_script_cache_hits_total") - hits; got < 1 {
		t.Errorf("cache hits after a flush = %v, want the retry's 1", got)
	}
}

// TestEveryRequestIsCountedOnce (IP-13 DoD 1): every route, the probes and
// the unmatched path included, counts one request with its endpoint, method
// and status.
func TestEveryRequestIsCountedOnce(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	read := func(endpoint, method, status string) float64 {
		return s.counter("quotacore_http_requests_total", endpoint, method, status)
	}
	cases := []struct {
		endpoint, method, status string
		send                     func() response
	}{
		{routeConsume, "POST", "200", func() response { return s.consume(s.runtime, f, 1, tn.External, newKey()) }},
		{routeBalance, "GET", "200", func() response {
			return s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn.External, s.runtime, "")
		}},
		{"GET /healthz", "GET", "200", func() response { return s.do(http.MethodGet, "/healthz", "", "") }},
		{"unmatched", "GET", "404", func() response { return s.do(http.MethodGet, "/nowhere", "", "") }},
		{"GET /v1/admin/status", "GET", "200", func() response { return s.adminDo(http.MethodGet, "/v1/admin/status", "") }},
	}
	for _, c := range cases {
		before := read(c.endpoint, c.method, c.status)
		c.send()
		if got := read(c.endpoint, c.method, c.status) - before; got != 1 {
			t.Errorf("%s %s counted %v times, want 1", c.method, c.endpoint, got)
		}
	}
}

// TestReanchorsPendingFollowsTheTable (IP-13 DoD 1, D10): while the store
// cannot take a re-anchor's rewrite, the gauge equals the pending rows; once
// the drain finishes them it is 0.
func TestReanchorsPendingFollowsTheTable(t *testing.T) {
	_, redisURL := integrationURLs(t)
	p := newProxy(t, hostOf(t, redisURL))
	s := newStack(t, func(o *stackOptions) { o.redisURL = urlVia(t, redisURL, p.addr()) })
	_, _, tn := s.provisioned(100)
	p.set("blackhole")
	if _, err := s.seedDB.Exec(`INSERT INTO pending_reanchors (tenant_id, anchor_at, kind, request_id)
		VALUES ($1, $2, 'apply_now', 'req')`, tn.ID, tn.Anchor); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "reanchors_pending = 1", func() bool { return s.counter("quotacore_reanchors_pending") == 1 })
	p.set("pass")
	waitFor(t, 5*time.Second, "reanchors_pending = 0", func() bool { return s.counter("quotacore_reanchors_pending") == 0 })
	if n := s.rows(`SELECT count(*) FROM pending_reanchors`); n != 0 {
		t.Errorf("pending rows after the drain = %d", n)
	}
}

// TestStatusAgreesWithTheMetrics (IP-13, the IP-09 hand-on): status's queue
// depth, drops, mode and dependencies say what the metrics say, read at the
// same instant with no traffic between.
func TestStatusAgreesWithTheMetrics(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	mustStatus(t, s.consume(s.runtime, f, 1, tn.External, newKey()), http.StatusOK)
	s.flush()
	waitFor(t, 2*time.Second, "both dependencies up", func() bool {
		return s.counter("quotacore_datastore_up") == 1 && s.counter("quotacore_controlplane_up") == 1
	})
	st := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/status", ""), http.StatusOK)
	all, _ := s.scrape()
	if got, want := num(t, st, "queue_depth"), value(all, "quotacore_event_queue_depth"); float64(got) != want {
		t.Errorf("status queue_depth = %d, event_queue_depth = %v", got, want)
	}
	if got, want := num(t, st, "ledger_dropped_total"), value(all, "quotacore_event_sink_dropped_total"); float64(got) != want {
		t.Errorf("status ledger_dropped_total = %d, event_sink_dropped_total = %v", got, want)
	}
	if mode := str(st, "mode"); mode != "normal" || s.probe("/readyz").Status != http.StatusOK {
		t.Errorf("status mode = %q with /readyz %d", mode, s.probe("/readyz").Status)
	}
	for _, d := range asSlice(st["dependencies"]) {
		dep := asMap(d)
		metric := map[string]string{"redis": "quotacore_datastore_up", "postgres": "quotacore_controlplane_up"}[str(dep, "name")]
		if healthy, _ := dep["healthy"].(bool); healthy != (value(all, metric) == 1) {
			t.Errorf("status says %s healthy=%v and %s = %v", str(dep, "name"), healthy, metric, value(all, metric))
		}
	}
}
