// Package reference_test drives the reference deployment (IP-15): the image
// and the Compose file at the repository root, started as an operator starts
// them, and read only through what an operator has — the CLI, HTTP, docker
// inspect, and the two stores' own clients where a DoD item is about a store.
//
// The suite needs Docker and a built image, so it runs only when
// QUOTACORE_REFERENCE is set, and fails rather than skips when
// QUOTACORE_REQUIRE_REFERENCE is set too, as CI's reference job does. The
// image is QUOTACORE_IMAGE, quotacore:local by default; the tests never build
// it, so what they test is what the job built once.
package reference_test

import (
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cli"
)

// api is where the reference file publishes the service: host loopback only.
const api = "http://127.0.0.1:8080"

// The development passwords the Compose file defaults to; low-entropy on
// purpose, and replaced by any real deployment.
const (
	superuserPassword = "quotacore_dev_only"
	appPassword       = "quotacore_dev_only"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func image() string {
	if i := os.Getenv("QUOTACORE_IMAGE"); i != "" {
		return i
	}
	return "quotacore:local"
}

// requireReference skips unless the suite was asked for, and fails when it
// was required and cannot run.
func requireReference(t *testing.T) {
	t.Helper()
	fail := t.Skip
	if os.Getenv("QUOTACORE_REQUIRE_REFERENCE") != "" {
		fail = t.Fatal
	} else if os.Getenv("QUOTACORE_REFERENCE") == "" {
		t.Skip("QUOTACORE_REFERENCE is unset; the reference suite needs Docker and a built image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		fail("docker is not on PATH")
	}
	if out, err := exec.Command("docker", "image", "inspect", image()).CombinedOutput(); err != nil {
		fail(fmt.Sprintf("the image %s is not built (docker compose build): %s", image(), out))
	}
}

// stack is one Compose project from the root file, torn down with its volumes
// when the test ends.
type stack struct {
	t       *testing.T
	project string
	files   []string
	env     map[string]string
	admin   string
}

type stackOption func(*stack)

// withEnv sets variables the Compose file interpolates.
func withEnv(kv ...string) stackOption {
	return func(s *stack) {
		for i := 0; i+1 < len(kv); i += 2 {
			s.env[kv[i]] = kv[i+1]
		}
	}
}

// withOverride adds a Compose file from this directory.
func withOverride(name string) stackOption {
	return func(s *stack) { s.files = append(s.files, filepath.Join(repoRoot(s.t), "test", "reference", name)) }
}

// noUp leaves the stack unstarted, for a test that starts it its own way.
func noUp(s *stack) { s.env["_noUp"] = "1" }

func newStack(t *testing.T, opts ...stackOption) *stack {
	t.Helper()
	requireReference(t)
	b := make([]byte, 4)
	rand.Read(b)
	s := &stack{t: t, project: "qcref" + hex.EncodeToString(b),
		files: []string{filepath.Join(repoRoot(t), "docker-compose.yml")}, env: map[string]string{}}
	// The quickstart's two lines: the operator mints the key with the image's
	// own command and acknowledges it (Q1).
	s.admin = s.mint()
	s.env["QUOTACORE_BOOTSTRAP_ADMIN_KEY"] = s.admin
	s.env["QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK"] = "yes"
	s.env["QUOTACORE_IMAGE"] = image()
	// Every request of a test comes from one address, so the admin limit is
	// lifted unless a test is about it.
	s.env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "6000000"
	for _, o := range opts {
		o(s)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := s.composeOut("logs", "--no-color", "--tail", "200")
			t.Logf("logs of %s:\n%s", s.project, out)
		}
		s.composeOut("down", "-v", "--remove-orphans", "--timeout", "5")
	})
	if s.env["_noUp"] == "" {
		s.up()
	}
	return s
}

func (s *stack) mint() string {
	s.t.Helper()
	out, err := exec.Command("docker", "run", "--rm", image(), "key", "mint").Output()
	if err != nil {
		s.t.Fatalf("key mint: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func (s *stack) cmd(args ...string) *exec.Cmd {
	full := []string{"compose", "-p", s.project}
	for _, f := range s.files {
		full = append(full, "-f", f)
	}
	c := exec.Command("docker", append(full, args...)...)
	c.Dir = repoRoot(s.t)
	c.Env = os.Environ()
	for k, v := range s.env {
		if !strings.HasPrefix(k, "_") {
			c.Env = append(c.Env, k+"="+v)
		}
	}
	return c
}

// composeOut runs docker compose and returns its combined output.
func (s *stack) composeOut(args ...string) (string, error) {
	out, err := s.cmd(args...).CombinedOutput()
	return string(out), err
}

// compose runs docker compose and fails the test on an error.
func (s *stack) compose(args ...string) string {
	s.t.Helper()
	out, err := s.composeOut(args...)
	if err != nil {
		s.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// up starts the stack, or the named services, and waits until every
// healthcheck passes, which for the service means /readyz (D4).
func (s *stack) up(services ...string) {
	s.t.Helper()
	s.compose(append([]string{"up", "-d", "--wait", "--no-build", "--wait-timeout", "120"}, services...)...)
}

// cli runs a command in the service's container, as `docker compose exec`
// does in the quickstart and the runbooks.
func (s *stack) cli(args ...string) (int, string, string) {
	s.t.Helper()
	c := s.cmd(append([]string{"exec", "-T", "quotacore", "quotacore"}, args...)...)
	var out, errw bytes.Buffer
	c.Stdout, c.Stderr = &out, &errw
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatalf("exec %v: %v", args, err)
	}
	return code, out.String(), errw.String()
}

// admin runs a remote command in process, against the published port, with
// the bootstrap key: the same code the binary runs, without a container exec
// per call, for the hundreds of calls a seed makes.
func (s *stack) adminCLI(args ...string) (int, string, string) {
	s.t.Helper()
	var out, errw bytes.Buffer
	code := cli.Run(cli.Options{
		Args: args,
		Getenv: func(k string) string {
			switch k {
			case "QUOTACORE_URL":
				return api
			case "QUOTACORE_ADMIN_KEY":
				return s.admin
			}
			return ""
		},
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errw, Version: "reference-test",
	})
	return code, out.String(), errw.String()
}

// mustAdmin is adminCLI that fails the test on a non-zero exit and decodes
// the JSON it printed.
func (s *stack) mustAdmin(args ...string) map[string]any {
	s.t.Helper()
	code, out, errw := s.adminCLI(args...)
	if code != 0 {
		s.t.Fatalf("quotacore %s: exit %d: %s", strings.Join(args, " "), code, errw)
	}
	var v map[string]any
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			s.t.Fatalf("quotacore %s printed %q: %v", strings.Join(args, " "), out, err)
		}
	}
	return v
}

// runtimeKey issues a runtime key and returns its plaintext.
func (s *stack) runtimeKey() string {
	s.t.Helper()
	k := s.mustAdmin("key", "create", "--name", "reference-"+s.project, "--scope", "runtime")
	return k["key"].(string)
}

var client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

type response struct {
	status int
	body   []byte
	err    error
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("not JSON (%d): %q", r.status, r.body)
	}
	return v
}

func (r response) code() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(r.body, &e)
	return e.Error.Code
}

func do(method, path, token, body string, headers ...string) response {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, api+path, rd)
	if err != nil {
		return response{err: err}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: b, err: err}
}

func consume(token, tenant, feature string, amount int64, key string) response {
	return do("POST", "/v1/consume", token,
		fmt.Sprintf(`{"tenant_external_id":%q,"feature_key":%q,"amount":%d}`, tenant, feature, amount),
		"Idempotency-Key", key)
}

func newIdemKey() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "ref-" + hex.EncodeToString(b)
}

// balanceOf reads one tenant's balances, feature by feature.
func balanceOf(t *testing.T, token, tenant string) (map[string]int64, response) {
	t.Helper()
	r := do("GET", "/v1/balance?tenant_external_id="+tenant, token, "")
	if r.err != nil || r.status != 200 {
		return nil, r
	}
	var v struct {
		Entries []struct {
			Feature string `json:"feature_key"`
			Balance int64  `json:"balance"`
			Limit   int64  `json:"limit"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("balance %s: %q", tenant, r.body)
	}
	out := map[string]int64{}
	for _, e := range v.Entries {
		out[e.Feature] = e.Balance
	}
	return out, r
}

// psql runs SQL as the Postgres superuser in the postgres container, for the
// two things an operator does there: a fault a drill injects, and a backup.
func (s *stack) psql(db, sql string) string {
	s.t.Helper()
	return strings.TrimSpace(s.compose("exec", "-T", "-e", "PGPASSWORD="+superuserPassword, "postgres",
		"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", db, "-tAc", sql))
}

func (s *stack) valkey(args ...string) string {
	s.t.Helper()
	return strings.TrimSpace(s.compose(append([]string{"exec", "-T", "valkey", "valkey-cli"}, args...)...))
}

// info reads one field of a valkey INFO section.
func (s *stack) info(section, field string) int64 {
	s.t.Helper()
	for _, line := range strings.Split(s.valkey("info", section), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == field {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	s.t.Fatalf("INFO %s has no %s", section, field)
	return 0
}

// status reads GET /v1/admin/status through the CLI.
func (s *stack) status() map[string]any {
	s.t.Helper()
	return s.mustAdmin("status")
}

// settleLedger waits until the writer's queue is empty and the ledger's row
// count has stopped moving, so a read of it sees every event.
func (s *stack) settleLedger() int {
	s.t.Helper()
	last, stable := -1, 0
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		n, _ := strconv.Atoi(s.psql("quotacore", "SELECT count(*) FROM usage_events"))
		if q, _ := s.status()["queue_depth"].(float64); q == 0 && n == last {
			if stable++; stable >= 3 {
				return n
			}
		} else {
			stable = 0
		}
		last = n
	}
	s.t.Fatal("the ledger did not settle within a minute")
	return 0
}

// restartIn restarts the service in a startup mode. A read-only instance is
// not ready by design, so it is not waited on with --wait; the wait is for
// the mode the status reports.
func (s *stack) restartIn(mode string) {
	s.t.Helper()
	s.env["QUOTACORE_STARTUP_MODE"] = mode
	if mode == "normal" {
		s.up("quotacore")
		return
	}
	s.compose("up", "-d", "--no-build", "--no-deps", "quotacore")
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		code, out, _ := s.adminCLI("status")
		if code == 0 && strings.Contains(out, `"mode": "`+mode+`"`) {
			return
		}
	}
	s.t.Fatalf("the service did not come up in %s mode", mode)
}

// eventually polls f until it is true or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if f() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: not within %s", what, timeout)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
