package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/db"
)

// IP-13 DoD 4 (NFR-O3, NFR-O6, DR-043, D6): every line the stack writes, over
// a workload that reaches every route, every background job and both
// outages, is one JSON object of the documented shape, carries its request's
// id or its job's name, and carries nothing a customer sent.

// The jobs a line no request caused names (observability.md section 2).
var logJobs = map[string]bool{"startup": true, "drain": true, "reconcile": true, "retention": true,
	"ledger": true, "fence": true, "probe": true, "refresh": true, "shutdown": true}

var logTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z$`)

type logRun struct {
	lines    []map[string]any
	raw      string
	ids      []string // the request ids the workload's responses carried
	canaries []string
}

// runLogWorkload drives the stack through every route and job, with a
// canary in every customer value and in both connection strings.
func runLogWorkload(t *testing.T) logRun {
	t.Helper()
	baseDB, redisURL := integrationURLs(t)
	rp := newProxy(t, hostOf(t, redisURL))
	var pp *proxy
	appName, clientName := canary("pgapp"), canary("rdname")
	s := newStack(t, func(o *stackOptions) {
		o.redisURL = urlVia(t, redisURL, rp.addr()) + "?client_name=" + clientName
		pp = newProxy(t, hostOf(t, baseDB))
		u := urlVia(t, o.dbURL, pp.addr())
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		o.dbURL = u + sep + "application_name=" + appName
		o.retentionFirstRun = 200 * time.Millisecond
	})
	run := logRun{canaries: []string{appName, clientName}}
	keep := func(r response) response {
		if id := r.Header.Get("X-Request-Id"); id != "" {
			run.ids = append(run.ids, id)
		}
		return r
	}

	// The control plane, with a canary in every free-text field.
	f, plan := uniq("lf"), uniq("lp")
	s.createFeature(f)
	s.createPlan(plan, 100, "monthly", f)
	ext, name, reason := canary("ext"), canary("name"), canary("reason")
	run.canaries = append(run.canaries, ext, name, reason)
	tm := mustStatus(t, keep(s.adminDo(http.MethodPost, "/v1/admin/tenants",
		`{"external_id":"`+ext+`","display_name":"`+name+`","plan_key":"`+plan+`","timezone":"UTC"}`)), http.StatusCreated)
	tn := asTenant(t, tm, ent{f, 100, "monthly", false})
	mustStatus(t, keep(s.adminDo(http.MethodPost, "/v1/admin/tenants/"+tn.ID+"/grant",
		`{"feature_key":"`+f+`","amount":5,"reason":"`+reason+`"}`)), http.StatusOK)
	keyName := canary("key")
	issued := mustStatus(t, keep(s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"`+keyName+`","scope":"runtime"}`)), http.StatusCreated)
	token := str(issued, "key")
	if _, secret, ok := strings.Cut(token, "."); ok {
		run.canaries = append(run.canaries, keyName, secret)
	} else {
		t.Fatalf("the key create answered no token: %v", issued)
	}
	keep(s.adminDo(http.MethodGet, "/v1/admin/tenants/"+tn.ID, ""))
	keep(s.adminDo(http.MethodGet, "/v1/admin/status", ""))

	// The data plane, with canaries in the metadata, the key and the token.
	meta, mkey, idem := canary("meta"), canary("mkey"), canary("idem")
	run.canaries = append(run.canaries, meta, mkey, idem)
	withMeta := `{"feature_key":"` + f + `","amount":1,"tenant_external_id":"` + ext + `","metadata":{"` + mkey + `":"` + meta + `"}}`
	mustStatus(t, keep(s.do(http.MethodPost, "/v1/consume", s.runtime, withMeta, "Idempotency-Key", idem)), http.StatusOK)
	keep(s.do(http.MethodPost, "/v1/consume", s.runtime, withMeta, "Idempotency-Key", idem))
	keep(s.do(http.MethodPost, "/v1/refund", token, withMeta, "Idempotency-Key", idem+"r"))
	keep(s.do(http.MethodPost, "/v1/check", s.runtime, withMeta))
	keep(s.do(http.MethodGet, "/v1/balance?tenant_external_id="+ext, s.runtime, ""))
	bogus := canary("bearer")
	run.canaries = append(run.canaries, bogus)
	keep(s.do(http.MethodPost, "/v1/consume", bogus, withMeta, "Idempotency-Key", idem+"b"))
	keep(s.do(http.MethodPost, "/v1/consume", s.runtime, `{"feature_key":"`+f+`","amount":1,"tenant_external_id":"`+canary("nobody")+`"}`,
		"Idempotency-Key", idem+"n"))
	keep(s.do(http.MethodGet, "/"+canary("path"), "", ""))
	keep(s.do(http.MethodGet, "/healthz", "", ""))
	keep(s.do(http.MethodGet, "/readyz", "", ""))
	// The last four close the control plane, which the rest still needs.
	ps := provocations(t, s)
	for _, p := range ps[:len(ps)-4] {
		keep(p.send())
	}

	// The background jobs: a pass with a finding, retention, a drain, the
	// ledger writer.
	s.flush()
	if _, err := s.app.Pass.Once(context.Background()); err != nil {
		t.Logf("the pass: %v", err)
	}
	if _, err := s.seedDB.Exec(`INSERT INTO pending_reanchors (tenant_id, anchor_at, kind, request_id)
		VALUES ($1, $2, 'apply_now', 'req')`, tn.ID, tn.Anchor); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the drain", func() bool { return s.rows(`SELECT count(*) FROM pending_reanchors`) == 0 })

	// Both outages, and a fence loss.
	rp.set("blackhole")
	waitFor(t, 2*time.Second, "the store probe to fail", func() bool { return s.counter("quotacore_datastore_up") == 0 })
	keep(s.do(http.MethodPost, "/v1/consume", s.runtime, withMeta, "Idempotency-Key", idem+"x"))
	rp.set("pass")
	waitFor(t, 2*time.Second, "the store probe to recover", func() bool { return s.counter("quotacore_datastore_up") == 1 })
	pp.set("refuse")
	keep(s.do(http.MethodPost, "/v1/consume", s.runtime, `{"feature_key":"`+f+`","amount":1,"tenant_external_id":"`+canary("cold")+`"}`,
		"Idempotency-Key", idem+"c"))
	keep(s.adminDo(http.MethodGet, "/v1/admin/tenants/"+tn.ID, ""))
	waitFor(t, 5*time.Second, "controlplane_up = 0", func() bool { return s.counter("quotacore_controlplane_up") == 0 })
	pp.set("pass")
	conn, err := db.Open(context.Background(), s.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	if release, ok, err := db.ExclusiveFence(context.Background(), conn); err == nil && ok {
		waitFor(t, 2*time.Second, "read-only", s.app.Data.ReadOnly)
		release()
		waitFor(t, 2*time.Second, "the fence back", func() bool { return !s.app.Data.ReadOnly() })
	}
	time.Sleep(300 * time.Millisecond) // retention's first tick

	run.raw = s.logs.String()
	for _, line := range strings.Split(strings.TrimSpace(run.raw), "\n") {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || dec.More() {
			t.Errorf("a log line that is not one JSON object: %q", line)
			continue
		}
		run.lines = append(run.lines, m)
	}
	return run
}

// TestEveryLogLineIsJSONWithItsRequestId (IP-13 DoD 4).
func TestEveryLogLineIsJSONWithItsRequestId(t *testing.T) {
	run := runLogWorkload(t)
	if len(run.lines) < 50 {
		t.Fatalf("the workload wrote %d lines", len(run.lines))
	}
	served := map[string]int{} // request id → its access lines
	jobs := map[string]bool{}
	for _, m := range run.lines {
		ts, _ := m["ts"].(string)
		level, _ := m["level"].(string)
		msg, _ := m["msg"].(string)
		if !logTimestamp.MatchString(ts) || msg == "" || (level != "debug" && level != "info" && level != "warn" && level != "error") {
			t.Errorf("a line without ts in UTC to the microsecond, a lowercase level or a msg: %v", m)
		}
		if _, ok := m["time"]; ok {
			t.Errorf("a line still carries time: %v", m)
		}
		id, hasID := m["request_id"].(string)
		job, hasJob := m["job"].(string)
		switch {
		case hasJob && !logJobs[job]:
			t.Errorf("a line names job %q, which is not one of %v: %v", job, logJobs, m)
		case !hasID && !hasJob:
			t.Errorf("a line carries neither a request id nor a job: %v", m)
		}
		if hasJob {
			jobs[job] = true
		}
		endpoint, isRequest := m["endpoint"].(string)
		if hasID && isRequest && m["status"] != nil {
			served[id]++
			if strings.HasPrefix(endpoint, "POST /v1/") && !strings.HasPrefix(endpoint, "POST /v1/admin") || endpoint == "GET /v1/balance" {
				if _, ok := m["tenant_id"]; ok {
					t.Errorf("a data-plane line carries tenant_id: %v", m)
				}
			}
			status, _ := m["status"].(json.Number).Int64()
			if strings.Contains(endpoint, "/v1/admin/") && !strings.HasPrefix(endpoint, "GET ") && status < 300 {
				for _, k := range []string{"actor", "action", "subject_type"} {
					if m[k] == nil {
						t.Errorf("an admin write's line has no %s: %v", k, m)
					}
				}
			}
		}
	}
	for _, id := range run.ids {
		if served[id] != 1 {
			t.Errorf("request %s has %d access lines, want 1", id, served[id])
		}
	}
	for _, m := range run.lines {
		if id, ok := m["request_id"].(string); ok && served[id] == 0 && m["job"] == nil {
			t.Errorf("a line names request %s, which no request was: %v", id, m)
		}
	}
	// startup and shutdown lines are main's, which the binary tests read.
	for _, j := range []string{"probe", "drain", "fence", "retention"} {
		if !jobs[j] {
			t.Errorf("no line from job %s; the workload did not reach it", j)
		}
	}
}

// TestNoLogLineCarriesCustomerDataOrASecret (IP-13 DoD 4, DR-043): no
// external_id, display name, metadata key or value, Idempotency-Key,
// audit reason, key name, token or secret, and neither connection string,
// appears in any byte the stack wrote.
func TestNoLogLineCarriesCustomerDataOrASecret(t *testing.T) {
	run := runLogWorkload(t)
	for _, c := range run.canaries {
		if strings.Contains(run.raw, c) {
			for _, line := range strings.Split(run.raw, "\n") {
				if strings.Contains(line, c) {
					t.Errorf("%q is in a log line: %s", c, line)
				}
			}
		}
	}
	if strings.Contains(run.raw, "cnry") {
		t.Error("a canary prefix is in the log")
	}
	if strings.Contains(run.raw, "quotacore_dev_only") {
		t.Error("the database password is in the log")
	}
}

// TestTheBinaryLogsJSONBeforeItsConfiguration (IP-13 DoD 4): a binary that
// refuses to start, before it has read a configuration, says so in one JSON
// line of the same shape.
func TestTheBinaryLogsJSONBeforeItsConfiguration(t *testing.T) {
	integrationURLs(t)
	cmd := exec.Command(binary(t))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		t.Fatal("the binary started with no configuration")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("the refusal wrote %d lines: %s", len(lines), out.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("the refusal is not JSON: %s", lines[0])
	}
	if ts, _ := m["ts"].(string); !logTimestamp.MatchString(ts) || m["level"] != "error" || m["msg"] != "refusing to start" ||
		m["job"] != "startup" || !strings.Contains(m["error"].(string), "QUOTACORE_DATABASE_URL") {
		t.Errorf("the refusal = %v", m)
	}
}
