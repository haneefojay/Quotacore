// Package harness is what IP-16's suites against the Compose stack share: the
// stack itself, started from the root file as an operator starts it plus an
// override of this directory, the admin calls that seed it, an open-loop load
// generator, and a sampler of the service's own metrics. test/load and
// test/chaos use it. It holds no code the service runs, and it reads the
// service only as an operator can: HTTP, the metrics listener, docker, and the
// two stores' own clients where a measurement is about a store.
package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// The development passwords the Compose file defaults to, which the perf
// override publishes the stores with on loopback.
const (
	API         = "http://127.0.0.1:8080"
	MetricsURL  = "http://127.0.0.1:9090/metrics"
	RedisURL    = "redis://127.0.0.1:16379/0"
	DatabaseURL = "postgresql://postgres:quotacore_dev_only@127.0.0.1:15432/quotacore?sslmode=disable"
)

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// Image is the image under test, which the suites never build.
func Image() string {
	if i := os.Getenv("QUOTACORE_IMAGE"); i != "" {
		return i
	}
	return "quotacore:local"
}

// Require skips unless QUOTACORE_<gate> is set, and fails instead when
// QUOTACORE_REQUIRE_<gate> is, as the workflows set it.
func Require(t testing.TB, gate string) {
	t.Helper()
	fail := t.Skip
	if os.Getenv("QUOTACORE_REQUIRE_"+gate) != "" {
		fail = t.Fatal
	} else if os.Getenv("QUOTACORE_"+gate) == "" {
		t.Skip("QUOTACORE_" + gate + " is unset; this suite needs Docker and a built image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fail("docker is not on PATH")
	}
	if out, err := exec.Command("docker", "image", "inspect", Image()).CombinedOutput(); err != nil {
		fail(fmt.Sprintf("the image %s is not built (docker compose build): %s", Image(), out))
	}
}

// Stack is one Compose project: the root file and this directory's perf
// override, torn down with its volumes when the test ends.
type Stack struct {
	T       testing.TB
	Project string
	Files   []string
	Env     map[string]string
	// Admin is the bootstrap admin key the stack was started with.
	Admin  string
	client *http.Client
}

// Option configures a stack before it starts.
type Option func(*Stack)

// WithEnv sets variables the Compose files interpolate.
func WithEnv(kv ...string) Option {
	return func(s *Stack) {
		for i := 0; i+1 < len(kv); i += 2 {
			s.Env[kv[i]] = kv[i+1]
		}
	}
}

// WithOverride adds a Compose file, by path from the repository root.
func WithOverride(path string) Option {
	return func(s *Stack) { s.Files = append(s.Files, filepath.Join(Root(), path)) }
}

// NoUp leaves the stack unstarted.
func NoUp(s *Stack) { s.Env["_noUp"] = "1" }

// New starts a stack from docker-compose.yml and test/harness/compose.perf.yml.
func New(t testing.TB, opts ...Option) *Stack {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	s := &Stack{T: t, Project: "qc16" + hex.EncodeToString(b), Env: map[string]string{},
		Files:  []string{filepath.Join(Root(), "docker-compose.yml"), filepath.Join(Root(), "test", "harness", "compose.perf.yml")},
		client: &http.Client{Timeout: 30 * time.Second}}
	s.Admin = mint(t)
	s.Env["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = s.Admin
	s.Env["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
	s.Env["QUOTACORE_IMAGE"] = Image()
	// The suites seed through the admin API from one address.
	s.Env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "6000000"
	// deployment.md section 5 sizes the store for 24 h of idempotency records;
	// the suites hold at most a soak's worth.
	s.Env["QUOTACORE_VALKEY_MAXMEMORY"] = "7gb"
	for _, o := range opts {
		o(s)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := s.ComposeOut("logs", "--no-color", "--tail", "40")
			t.Logf("logs of %s:\n%s", s.Project, out)
		}
		_, _ = s.ComposeOut("down", "-v", "--remove-orphans", "--timeout", "5")
	})
	if s.Env["_noUp"] == "" {
		s.Up()
	}
	return s
}

func mint(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", Image(), "key", "mint").Output()
	if err != nil {
		t.Fatalf("key mint: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Cmd is a docker compose command for this project.
func (s *Stack) Cmd(args ...string) *exec.Cmd {
	full := []string{"compose", "-p", s.Project}
	for _, f := range s.Files {
		full = append(full, "-f", f)
	}
	c := exec.Command("docker", append(full, args...)...)
	c.Dir = Root()
	c.Env = os.Environ()
	for k, v := range s.Env {
		if !strings.HasPrefix(k, "_") {
			c.Env = append(c.Env, k+"="+v)
		}
	}
	return c
}

// ComposeOut runs docker compose and returns its combined output.
func (s *Stack) ComposeOut(args ...string) (string, error) {
	out, err := s.Cmd(args...).CombinedOutput()
	return string(out), err
}

// Compose runs docker compose and fails the test on an error.
func (s *Stack) Compose(args ...string) string {
	s.T.Helper()
	out, err := s.ComposeOut(args...)
	if err != nil {
		s.T.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// Up starts the stack, or the named services, and waits for every
// healthcheck, which for the service is /readyz.
func (s *Stack) Up(services ...string) {
	s.T.Helper()
	s.Compose(append([]string{"up", "-d", "--wait", "--no-build", "--wait-timeout", "180"}, services...)...)
}

// Container is the id of a service's container.
func (s *Stack) Container(service string) string {
	s.T.Helper()
	return strings.TrimSpace(s.Compose("ps", "-q", service))
}

// PID is the host pid of a service's main process.
func (s *Stack) PID(service string) int {
	s.T.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", s.Container(service)).Output()
	if err != nil {
		s.T.Fatalf("inspect %s: %v", service, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid == 0 {
		s.T.Fatalf("%s has no running process: %q", service, out)
	}
	return pid
}

// OpenFDs counts a service's open file descriptors from /proc. The service
// runs as another user, so off a root shell the count goes through sudo, which
// a CI runner allows without a password.
func (s *Stack) OpenFDs(service string) (int, error) {
	dir := "/proc/" + strconv.Itoa(s.PID(service)) + "/fd"
	if entries, err := os.ReadDir(dir); err == nil {
		return len(entries), nil
	}
	out, err := exec.Command("sudo", "-n", "ls", dir).Output()
	if err != nil {
		return 0, err
	}
	return len(strings.Fields(string(out))), nil
}

// Do sends one request to the service with a token and returns the status
// and the body decoded as an object, if it is one.
func (s *Stack) Do(method, path, token, body string) (int, map[string]any) {
	s.T.Helper()
	status, raw := s.DoRaw(method, path, token, body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return status, m
}

// DoWithKey is Do with an Idempotency-Key.
func (s *Stack) DoWithKey(method, path, token, body, key string) (int, map[string]any) {
	status, raw := s.send(method, path, token, body, key)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return status, m
}

// DoRaw is Do with the body undecoded. A transport error is status 0.
func (s *Stack) DoRaw(method, path, token, body string) (int, []byte) {
	return s.send(method, path, token, body, "")
}

func (s *Stack) send(method, path, token, body, key string) (int, []byte) {
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, API+path, rd)
	if err != nil {
		s.T.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// Must sends an admin request and fails the test unless the status is want.
func (s *Stack) Must(want int, method, path, body string) map[string]any {
	s.T.Helper()
	status, raw := s.DoRaw(method, path, s.Admin, body)
	if status != want {
		s.T.Fatalf("%s %s: %d, want %d: %s", method, path, status, want, raw)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// WaitReady polls /readyz until it answers 200 or the timeout passes.
func (s *Stack) WaitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if status, _ := s.DoRaw(http.MethodGet, "/readyz", "", ""); status == http.StatusOK {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// Store is a client of the stack's data store, published on loopback.
func (s *Stack) Store() *redis.Client {
	opt, err := redis.ParseURL(RedisURL)
	if err != nil {
		s.T.Fatal(err)
	}
	c := redis.NewClient(opt)
	s.T.Cleanup(func() { c.Close() })
	return c
}

// StoreInfo reads one numeric field of the data store's INFO.
func (s *Stack) StoreInfo(section, field string) float64 {
	raw, err := s.Store().Info(context.Background(), section).Result()
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(raw, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == field {
			n, _ := strconv.ParseFloat(v, 64)
			return n
		}
	}
	return -1
}

// SQL runs one query as the Postgres superuser and returns its first column
// of its first row, as text.
func (s *Stack) SQL(query string, args ...any) string {
	s.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, DatabaseURL)
	if err != nil {
		s.T.Fatalf("connect to the stack's Postgres: %v", err)
	}
	defer conn.Close(ctx)
	var out any
	if err := conn.QueryRow(ctx, query, args...).Scan(&out); err != nil {
		s.T.Fatalf("%s: %v", query, err)
	}
	return fmt.Sprint(out)
}

// Direct is the service's API at its container's address on the published
// network, which the host reaches without Docker's port proxy. The load suites
// measure there: the proxy is a userland copy of every byte that a deployment
// behind a load balancer does not have, and it would be measured as the
// service's latency.
func (s *Stack) Direct() string {
	s.T.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		`{{(index .NetworkSettings.Networks "`+s.Project+`_published").IPAddress}}`, s.Container("quotacore")).Output()
	ip := strings.TrimSpace(string(out))
	if err != nil || ip == "" {
		s.T.Fatalf("the service's address on the published network: %v %q", err, out)
	}
	return "http://" + ip + ":8080"
}

// Prime checks every tenant and feature once, retrying a refusal, so every
// tenant is in the snapshot before a measurement starts: a first touch is a
// snapshot miss, and misses are rate limited (DR-039).
func (s *Stack) Prime(seeded Seeded) {
	s.T.Helper()
	for _, tn := range seeded.Tenants {
		body := `{"feature_key":"` + seeded.Features[0] + `","amount":1,"tenant_external_id":"` + tn + `"}`
		deadline := time.Now().Add(time.Minute)
		for {
			status, raw := s.DoRaw(http.MethodPost, "/v1/check", seeded.Keys[0], body)
			if status == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				s.T.Fatalf("priming %s: %d %s", tn, status, raw)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// ServerLatency reads the service's own access log since a time and returns
// the percentiles of the duration it recorded for one endpoint, such as
// "POST /v1/consume", in microseconds. It is the service's time for a request
// with the network and the client taken out, which the load suites record
// beside the client's figure: a hosted runner's idle wake-up adds a few
// hundred microseconds to every hop, and that is the runner, not the service.
func (s *Stack) ServerLatency(endpoint string, since time.Time) Percentiles {
	s.T.Helper()
	var d []time.Duration
	for _, e := range s.serverEntries(endpoint, since) {
		d = append(d, e.took)
	}
	return percentiles(d)
}

// ServerLatencyPerMinute is ServerLatency for each minute since a time, by
// the time the service logged each request.
func (s *Stack) ServerLatencyPerMinute(endpoint string, since time.Time) []MinutePercentile {
	s.T.Helper()
	var byMinute [][]time.Duration
	for _, e := range s.serverEntries(endpoint, since) {
		m := int(e.at.Sub(since) / time.Minute)
		if m < 0 {
			continue
		}
		for len(byMinute) <= m {
			byMinute = append(byMinute, nil)
		}
		byMinute[m] = append(byMinute[m], e.took)
	}
	var out []MinutePercentile
	for m, v := range byMinute {
		if len(v) > 0 {
			out = append(out, MinutePercentile{Minute: m, Percentiles: percentiles(v)})
		}
	}
	return out
}

type serverEntry struct {
	at   time.Time
	took time.Duration
}

func (s *Stack) serverEntries(endpoint string, since time.Time) []serverEntry {
	s.T.Helper()
	var entries []serverEntry
	want := []byte(`"endpoint":"` + endpoint + `"`)
	s.scanLog("quotacore", since, func(line []byte) {
		if !bytes.Contains(line, want) {
			return
		}
		var entry struct {
			At       time.Time `json:"ts"`
			Duration float64   `json:"duration_ms"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Duration > 0 {
			entries = append(entries, serverEntry{at: entry.At, took: time.Duration(entry.Duration * float64(time.Millisecond))})
		}
	})
	return entries
}

// ServerErrors counts the service's access-log lines with a 5xx status since a
// time, by the minute since then, the endpoint, the status, the error code and
// the cause the service logged, so a burst of failures in a long run can be
// placed in time and attributed.
func (s *Stack) ServerErrors(since time.Time) map[string]int64 {
	s.T.Helper()
	out := map[string]int64{}
	s.scanLog("quotacore", since, func(line []byte) {
		if !bytes.Contains(line, []byte(`"status":5`)) {
			return
		}
		var entry struct {
			At       time.Time `json:"ts"`
			Endpoint string    `json:"endpoint"`
			Status   int       `json:"status"`
			Code     string    `json:"code"`
			Error    string    `json:"error"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Status < 500 {
			return
		}
		out[fmt.Sprintf("minute %d: %s %d %s %s", int(entry.At.Sub(since)/time.Minute), entry.Endpoint,
			entry.Status, entry.Code, entry.Error)]++
	})
	return out
}

// LogLines is a service's log lines since a time that contain any of match.
func (s *Stack) LogLines(service string, since time.Time, match ...string) []string {
	s.T.Helper()
	var out []string
	s.scanLog(service, since, func(line []byte) {
		for _, m := range match {
			if bytes.Contains(line, []byte(m)) {
				out = append(out, string(line))
				return
			}
		}
	})
	return out
}

func (s *Stack) scanLog(service string, since time.Time, fn func(line []byte)) {
	s.T.Helper()
	cmd := exec.Command("docker", "logs", "--since", since.UTC().Format(time.RFC3339Nano), s.Container(service))
	out, err := cmd.StdoutPipe()
	if err != nil {
		s.T.Fatal(err)
	}
	// Postgres logs to stderr; the service to stdout.
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		s.T.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	_ = cmd.Wait()
}

// StoreRejections is how many commands the data store has answered with an
// error of one kind, such as OOM, from INFO errorstats.
func (s *Stack) StoreRejections(kind string) float64 {
	raw, err := s.Store().Info(context.Background(), "errorstats").Result()
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "errorstat_"+kind+":count="); ok {
			n, _ := strconv.ParseFloat(strings.SplitN(v, ",", 2)[0], 64)
			return n
		}
	}
	return 0
}
