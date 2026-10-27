package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/cli"
	"github.com/quotacore/quotacore/internal/envelope"
)

// IP-14: the CLI against a real stack. Every test runs it with stdin that is
// not a terminal, so CI exercises each command non-interactively (DoD 6),
// and reads only what it prints: no test below opens the database to decide
// what the operator could see.

// ran is one invocation's outcome.
type ran struct {
	code        int
	out, errOut string
}

func (r ran) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.out), &m); err != nil {
		t.Fatalf("exit %d: stdout is not a JSON object: %q (stderr %q)", r.code, r.out, r.errOut)
	}
	return m
}

// cliAt runs the CLI against url with key, and the local operations
// against the stack's stores.
func (s *stack) cliAt(url, key string, args ...string) ran {
	s.t.Helper()
	var out, errOut bytes.Buffer
	locals := map[string]func([]string) int{"serve": func([]string) int { return cli.ExitUsage }}
	for _, name := range []string{"verify-tenant", "verify-all", "reconcile-report", "rebuild"} {
		locals[name] = func(args []string) int {
			c, buf := s.commands()
			code, err := c.run(context.Background(), append([]string{name}, args...))
			out.Write(buf.Bytes())
			if err != nil {
				fmt.Fprintln(&errOut, err)
				if code == exitError {
					code = exitFor(err)
				}
			}
			return code
		}
	}
	code := cli.Run(cli.Options{
		Args: args,
		Getenv: func(k string) string {
			switch k {
			case "QUOTACORE_URL":
				return url
			case "QUOTACORE_ADMIN_KEY":
				return key
			}
			return ""
		},
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut, Version: "test", Locals: locals,
	})
	return ran{code, out.String(), errOut.String()}
}

func (s *stack) cli(args ...string) ran {
	s.t.Helper()
	return s.cliAt(s.server.URL, s.admin, args...)
}

// mustCLI runs a command that must succeed.
func (s *stack) mustCLI(args ...string) ran {
	s.t.Helper()
	r := s.cli(args...)
	if r.code != cli.ExitOK {
		s.t.Fatalf("quotacore %s: exit %d, stderr %q", strings.Join(args, " "), r.code, r.errOut)
	}
	return r
}

// recorder fronts the stack and records the operation of every call.
type recorder struct {
	mu  sync.Mutex
	ops []string
	srv *httptest.Server
}

func (s *stack) recorder() *recorder {
	s.t.Helper()
	target, _ := url.Parse(s.server.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	type route struct {
		re *regexp.Regexp
		op envelope.Operation
	}
	var routes []route
	param := regexp.MustCompile(`\\\{[a-z_]+\\\}`)
	for _, op := range envelope.MustOperations() {
		routes = append(routes, route{regexp.MustCompile("^" + param.ReplaceAllString(regexp.QuoteMeta(op.Path), `[^/]+`) + "$"), op})
	}
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, params := req.Method+" "+req.URL.EscapedPath(), -1
		for _, rt := range routes {
			// A literal segment outranks a parameter, as the router's does.
			n := strings.Count(rt.op.Path, "{")
			if rt.op.Method == req.Method && rt.re.MatchString(req.URL.EscapedPath()) && (params < 0 || n < params) {
				id, params = rt.op.ID, n
			}
		}
		r.mu.Lock()
		r.ops = append(r.ops, id)
		r.mu.Unlock()
		proxy.ServeHTTP(w, req)
	}))
	s.t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.ops
	r.ops = nil
	return out
}

// TestEveryCommandCallsItsRow (IP-14 DoD 1): every remote command, run once
// against a provisioned stack, makes exactly the call its row names and
// nothing else, and every remote command is run.
func TestEveryCommandCallsItsRow(t *testing.T) {
	s := newStack(t)
	rec := s.recorder()
	done := map[string]bool{}
	run := func(args ...string) map[string]any {
		t.Helper()
		c, _, ok := cli.Lookup(args)
		if !ok {
			t.Fatalf("no command %v", args)
		}
		r := s.cliAt(rec.srv.URL, s.admin, args...)
		if r.code != cli.ExitOK {
			t.Fatalf("quotacore %s: exit %d, stderr %q", strings.Join(args, " "), r.code, r.errOut)
		}
		if got := rec.take(); !slices.Equal(got, []string{c.Op}) {
			t.Errorf("quotacore %s made %v, want exactly %s", c.Name, got, c.Op)
		}
		done[c.Name] = true
		if strings.TrimSpace(r.out) == "" {
			return nil
		}
		return r.json(t)
	}
	version := func(m map[string]any) string { return fmt.Sprint(int64(m["version"].(float64))) }

	f, f2, p, p2, tn := uniq("cf"), uniq("cf2"), uniq("cp"), uniq("cp2"), externalID("ct")
	run("feature", "create", "--key", f, "--name", "F", "--unit", "tokens")
	run("feature", "create", "--key", f2, "--name", "F2", "--unit", "tokens", "--description", "spare")
	run("feature", "list", "--limit", "5")
	fv := version(run("feature", "get", f))
	run("feature", "update", f, "--version", fv, "--name", "Tokens")
	run("plan", "create", "--key", p, "--name", "P", "--entitlement", f+"=100/monthly")
	run("plan", "create", "--key", p2, "--name", "P2", "--entitlement", f+"=10/daily")
	run("plan", "list")
	pv := version(run("plan", "get", p))
	run("plan", "update", p, "--version", pv, "--name", "Starter")
	run("plan", "impact", p)
	run("tenant", "create", "--external-id", tn, "--name", "T", "--plan", p, "--metadata", `{"crm":"x"}`)
	run("tenant", "list", "--plan", p)
	tv := version(run("tenant", "get", tn))
	tv = version(run("tenant", "update", tn, "--version", tv, "--name", "Acme"))
	run("grant", tn, "--feature", f, "--amount", "5", "--reason", "goodwill")
	run("set", tn, "--feature", f, "--value", "50", "--reason", "migration")
	run("rollover", tn, "--feature", f, "--reason", "stuck")
	run("tenant", "override", "set", tn, "--feature", f, "--limit", "200", "--interval", "inherit")
	run("tenant", "override", "clear", tn, f, "--apply-at", "immediate")
	run("plan", "apply", p, "--reason", "new limits")
	tv = version(run("tenant", "get", tn))
	tv = version(run("tenant", "suspend", tn, "--version", tv, "--reason", "unpaid"))
	run("tenant", "resume", tn, "--version", tv, "--reason", "paid")
	s.flush()
	run("events", tn, "--feature", f)
	k := run("key", "create", "--name", uniq("ck"), "--scope", "runtime")
	run("key", "list")
	run("key", "revoke", k["id"].(string))
	run("audit", "--tenant", tn)
	run("status")
	run("plan", "archive", p2, "--version", version(run("plan", "get", p2)), "--reason", "retired")
	run("plan", "delete", p2, "--reason", "retired")
	run("feature", "archive", f2, "--version", version(run("feature", "get", f2)), "--reason", "unused")
	run("feature", "delete", f2, "--reason", "unused")
	run("tenant", "delete", tn, "--reason", "churned")

	// init's calls are its row's, in order.
	r := s.cliAt(rec.srv.URL, s.admin, "init", "--yes", "--feature", uniq("if"), "--plan", uniq("ip"), "--tenant", externalID("it"),
		"--key-name", uniq("ik"))
	got := rec.take()
	init, _, _ := cli.Lookup([]string{"init"})
	// A balance read may repeat while the new tenant becomes visible (DR-031).
	for len(got) > len(init.Calls) && got[len(got)-1] == "balance" && got[len(got)-2] == "balance" {
		got = got[:len(got)-1]
	}
	if r.code != cli.ExitOK || !slices.Equal(got, init.Calls) {
		t.Errorf("init: exit %d, calls %v, want %v; stderr %q", r.code, got, init.Calls, r.errOut)
	}
	for _, c := range cli.Commands() {
		if c.Kind == cli.Remote && !done[c.Name] {
			t.Errorf("%s was not run", c.Name)
		}
	}
}

// TestGrantSetAndRolloverFromTheCLI (IP-14 DoD 2): the three dispute-time
// operations do what their routes do, each with its audit row and its
// ledger event, and a rollover on a never entitlement is exit 6.
func TestGrantSetAndRolloverFromTheCLI(t *testing.T) {
	s := newStack(t)
	f, never, plan := uniq("gf"), uniq("gn"), uniq("gp")
	tn := externalID("gt")
	s.mustCLI("feature", "create", "--key", f, "--name", "F", "--unit", "tokens")
	s.mustCLI("feature", "create", "--key", never, "--name", "N", "--unit", "tokens")
	s.mustCLI("plan", "create", "--key", plan, "--name", "P", "--entitlement", f+"=100/monthly", "--entitlement", never+"=10/never")
	ten := asTenant(t, s.mustCLI("tenant", "create", "--external-id", tn, "--name", "T", "--plan", plan).json(t), ent{f, 100, "monthly", false})
	mustStatus(t, s.consume(s.runtime, f, 30, tn, newKey()), http.StatusOK)

	s.mustCLI("grant", tn, "--feature", f, "--amount", "5", "--reason", "outage")
	h := s.rdb.HGetAll(context.Background(), s.balanceKey(ten, f)).Val()
	if h["balance"] != "75" || h["bonus"] != "5" {
		t.Errorf("after the grant: %v, want balance 75 and bonus 5", h)
	}
	s.mustCLI("set", tn, "--feature", f, "--value", "42", "--reason", "migration")
	if b := s.balance(ten, f); b != 42 {
		t.Errorf("after the set: %d, want 42", b)
	}
	// A force-rollover refills in the cycle the anchor puts now in; it does
	// not move the anchor (the route's description).
	s.mustCLI("rollover", tn, "--feature", f, "--reason", "stuck")
	h = s.rdb.HGetAll(context.Background(), s.balanceKey(ten, f)).Val()
	if h["balance"] != "100" || h["bonus"] != "0" {
		t.Errorf("after the rollover: %v, want a refill to 100 with no bonus", h)
	}
	r := s.cli("rollover", tn, "--feature", never, "--reason", "nothing to roll")
	if r.code != cli.ExitConflict || !strings.Contains(r.errOut, "reset_not_supported") {
		t.Errorf("a never rollover: exit %d, %q", r.code, r.errOut)
	}
	s.flush()
	events := s.mustCLI("events", tn, "--feature", f).json(t)["events"].([]any)
	var types []string
	for _, e := range events {
		types = append(types, e.(map[string]any)["event_type"].(string))
	}
	for _, want := range []string{"granted", "set", "force_rolled_over"} {
		if !slices.Contains(types, want) {
			t.Errorf("no %s event in %v", want, types)
		}
	}
	entries := s.mustCLI("audit", "--tenant", tn).json(t)["entries"].([]any)
	reasons := map[string]bool{}
	for _, e := range entries {
		reasons[fmt.Sprint(e.(map[string]any)["reason"])] = true
	}
	for _, want := range []string{"outage", "migration", "stuck"} {
		if !reasons[want] {
			t.Errorf("no audit row with reason %q in %v", want, reasons)
		}
	}
}

// TestADisputeIsResolvedFromTheCLI (IP-14 DoD 3, J-5): a customer says they
// were charged twice. The operator finds the tenant, reads its events,
// checks that the deltas sum to the balance, looks up the disputed key and
// sees one deduction, and corrects with a grant, all from stdout.
func TestADisputeIsResolvedFromTheCLI(t *testing.T) {
	s := newStack(t)
	f, plan, tn := uniq("df"), uniq("dp"), externalID("dt")
	s.mustCLI("feature", "create", "--key", f, "--name", "F", "--unit", "tokens")
	s.mustCLI("plan", "create", "--key", plan, "--name", "P", "--entitlement", f+"=100/monthly")
	s.mustCLI("tenant", "create", "--external-id", tn, "--name", "T", "--plan", plan)
	disputed := newKey()
	// The customer's retry: the same key twice, then one other call.
	mustStatus(t, s.consume(s.runtime, f, 30, tn, disputed), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 30, tn, disputed), http.StatusOK)
	mustStatus(t, s.consume(s.runtime, f, 20, tn, newKey()), http.StatusOK)
	s.flush()

	// The operator's investigation, from the CLI alone.
	tenant := s.mustCLI("tenant", "get", tn).json(t)
	if tenant["external_id"] != tn || tenant["state"] != "active" {
		t.Fatalf("tenant get: %v", tenant)
	}
	events := s.mustCLI("events", tn, "--feature", f, "--all").json(t)["events"].([]any)
	var sum, latest float64
	var latestAt string
	for _, raw := range events {
		e := raw.(map[string]any)
		sum += e["delta"].(float64)
		if at := e["occurred_at"].(string); at > latestAt {
			latestAt, latest = at, e["balance_after"].(float64)
		}
	}
	if len(events) != 2 || 100+sum != latest || latest != 50 {
		t.Fatalf("%d events summing to %v, latest balance %v: the arithmetic does not close", len(events), sum, latest)
	}
	one := s.mustCLI("events", tn, "--idempotency-key", disputed).json(t)["events"].([]any)
	if len(one) != 1 || one[0].(map[string]any)["delta"].(float64) != -30 {
		t.Fatalf("the disputed key shows %v, want one deduction of 30", one)
	}
	// The correction: the customer is credited as a gesture.
	s.mustCLI("grant", tn, "--feature", f, "--amount", "30", "--reason", "dispute: goodwill")
	s.flush()
	granted := s.mustCLI("events", tn, "--type", "granted").json(t)["events"].([]any)
	if len(granted) != 1 || granted[0].(map[string]any)["balance_after"].(float64) != 80 {
		t.Fatalf("the correction shows %v, want one grant to 80", granted)
	}
}

// TestNoCommandPrintsASecret (IP-14 DoD 4): every command, on its success
// path and on a provoked failure, never prints the admin key, and key create
// prints its plaintext exactly once, on stdout.
func TestNoCommandPrintsASecret(t *testing.T) {
	s := newStack(t)
	f, plan, tn := uniq("sf"), uniq("sp"), externalID("st")
	secretOf := func(token string) string { return token[strings.Index(token, ".")+1:] }
	check := func(r ran, args ...string) {
		t.Helper()
		for _, secret := range []string{s.admin, secretOf(s.admin), s.runtime, secretOf(s.runtime)} {
			if strings.Contains(r.out+r.errOut, secret) {
				t.Errorf("quotacore %s printed a key's secret", strings.Join(args, " "))
			}
		}
	}
	for _, args := range [][]string{
		{"feature", "create", "--key", f, "--name", "F", "--unit", "tokens"},
		{"plan", "create", "--key", plan, "--name", "P", "--entitlement", f + "=100/monthly"},
		{"tenant", "create", "--external-id", tn, "--name", "T", "--plan", plan},
		{"feature", "list"}, {"plan", "list"}, {"tenant", "list"}, {"key", "list", "--all"}, {"audit"}, {"status"},
		{"events", tn}, {"grant", tn, "--feature", f, "--amount", "1", "--reason", "r"},
		{"tenant", "get", "no-such-tenant"}, {"feature", "create", "--key", f, "--name", "F", "--unit", "tokens"},
		{"grant", tn, "--feature", f, "--amount", "1"}, {"key", "mint"},
	} {
		check(s.cli(args...), args...)
	}
	// A refused credential, a revoked one and a dead service.
	revoked := s.key(auth.Admin)
	id := ""
	for _, k := range s.mustCLI("key", "list", "--all").json(t)["keys"].([]any) {
		if strings.HasPrefix(revoked, "qc_admin_"+k.(map[string]any)["prefix"].(string)+".") {
			id = k.(map[string]any)["id"].(string)
		}
	}
	s.mustCLI("key", "revoke", id)
	for _, r := range []ran{
		s.cliAt(s.server.URL, revoked, "tenant", "list"),
		s.cliAt(s.server.URL, "qc_admin_0123456789abcdef.not-the-secret-not-the-secret-not-the-s", "status"),
		s.cliAt("http://127.0.0.1:1", s.admin, "status"),
	} {
		if strings.Contains(r.out+r.errOut, secretOf(revoked)) || strings.Contains(r.out+r.errOut, secretOf(s.admin)) ||
			strings.Contains(r.out+r.errOut, "not-the-secret") {
			t.Errorf("a failure printed a secret: %q", r.errOut)
		}
		if r.code != cli.ExitCredential && r.code != cli.ExitUnavailable {
			t.Errorf("a refused or unreachable call: exit %d", r.code)
		}
	}
	// key create: the plaintext once, on stdout, with the warning beside it.
	r := s.mustCLI("key", "create", "--name", uniq("sk"), "--scope", "runtime")
	plain := r.json(t)["key"].(string)
	if strings.Count(r.out, plain) != 1 || strings.Contains(r.errOut, plain) || !strings.Contains(r.errOut, "shown once") {
		t.Errorf("key create: stdout %q, stderr %q", r.out, r.errOut)
	}
	if _, err := auth.Parse(plain); err != nil {
		t.Errorf("the printed key does not parse: %v", err)
	}
	// It is never shown again.
	if listed := s.mustCLI("key", "list", "--all"); strings.Contains(listed.out, secretOf(plain)) {
		t.Error("key list printed a secret")
	}
}

// TestExitCodesFollowTheEnvelope (IP-14 DoD 5): provoked answers from a
// real stack exit with the table's code.
func TestExitCodesFollowTheEnvelope(t *testing.T) {
	s := newStack(t)
	f, plan, tn := uniq("ef"), uniq("ep"), externalID("et")
	s.mustCLI("feature", "create", "--key", f, "--name", "F", "--unit", "tokens")
	s.mustCLI("plan", "create", "--key", plan, "--name", "P", "--entitlement", f+"=100/monthly")
	s.mustCLI("tenant", "create", "--external-id", tn, "--name", "T", "--plan", plan)
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"tenant", "get", "no-such-tenant"}, cli.ExitNotFound},
		{[]string{"plan", "get", "no-such-plan"}, cli.ExitNotFound},
		{[]string{"feature", "create", "--key", f, "--name", "F", "--unit", "tokens"}, cli.ExitConflict},
		{[]string{"tenant", "update", tn, "--version", "99", "--name", "x"}, cli.ExitConflict},
		{[]string{"set", tn, "--feature", f, "--value", "500", "--reason", "too much"}, cli.ExitConflict},
		{[]string{"events", tn, "--from", "yesterday"}, cli.ExitClient},
		{[]string{"tenant", "create", "--external-id", externalID("z"), "--name", "Z", "--plan", plan, "--timezone", "CEST"}, cli.ExitClient},
		{[]string{"grant", tn, "--feature", uniq("nf"), "--amount", "1", "--reason", "r"}, cli.ExitNotFound},
	} {
		if r := s.cli(tc.args...); r.code != tc.want {
			t.Errorf("%v: exit %d, want %d; stderr %q", tc.args, r.code, tc.want, r.errOut)
		}
	}
	if r := s.cliAt(s.server.URL, s.runtime, "tenant", "list"); r.code != cli.ExitCredential || !strings.Contains(r.errOut, "insufficient_scope") {
		t.Errorf("a runtime key: exit %d, %q", r.code, r.errOut)
	}
	// Status refuses a caller with a bare status and no envelope, by contract.
	if r := s.cliAt(s.server.URL, s.runtime, "status"); r.code != cli.ExitCredential {
		t.Errorf("a runtime key on status: exit %d, %q", r.code, r.errOut)
	}
	limited := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_RATELIMIT_ADMIN_RPM"] = "1" })
	limited.cli("status")
	if r := limited.cli("status"); r.code != cli.ExitRateLimited {
		t.Errorf("past the admin limit: exit %d, %q", r.code, r.errOut)
	}
	// A local operation's error is classified too.
	if r := s.cli("verify-tenant", "no-such-tenant"); r.code != cli.ExitNotFound {
		t.Errorf("verify-tenant of no tenant: exit %d, %q", r.code, r.errOut)
	}
	if r := s.cli("reconcile-report", "--since", "whenever"); r.code != cli.ExitUsage {
		t.Errorf("a bad --since: exit %d", r.code)
	}
}

// TestInitRunsWithoutAPrompt (IP-14 DoD 6): init with no terminal makes the
// quickstart's objects, and the runtime key it prints works.
func TestInitRunsWithoutAPrompt(t *testing.T) {
	s := newStack(t)
	tn := externalID("init")
	r := s.cli("init", "--feature", uniq("if"), "--plan", uniq("ip"), "--tenant", tn, "--key-name", uniq("ik"), "--json")
	if r.code != cli.ExitOK {
		t.Fatalf("init: exit %d, %q", r.code, r.errOut)
	}
	m := r.json(t)
	key := m["key"].(map[string]any)["key"].(string)
	if strings.Count(r.out, key) != 1 {
		t.Error("init printed the runtime key more than once")
	}
	entries := m["balance"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["balance"].(float64) != 100000 {
		t.Errorf("the first read: %v", entries)
	}
	if got := s.do(http.MethodGet, "/v1/balance?tenant_external_id="+tn, key, ""); got.Status != http.StatusOK {
		t.Errorf("the printed runtime key does not work: %d", got.Status)
	}
	// Run again, it stops at the first conflict and creates nothing more.
	again := s.cli("init", "--feature", m["feature"].(map[string]any)["feature_key"].(string), "--yes")
	if again.code != cli.ExitConflict || !strings.Contains(again.errOut, "created nothing") {
		t.Errorf("init again: exit %d, %q", again.code, again.errOut)
	}
}

// TestTheLocalOperationsAreRows (IP-14 DoD 7, IP-11 note): the four local
// operations are rows of the binary's table, dispatched by it, with D5's
// exit codes.
func TestTheLocalOperationsAreRows(t *testing.T) {
	for _, name := range []string{"verify-tenant", "verify-all", "reconcile-report", "rebuild", "serve"} {
		c, _, ok := cli.Lookup([]string{name})
		if !ok || c.Kind != cli.Local {
			t.Errorf("%s is not a local row", name)
		}
	}
	s := newStack(t)
	if r := s.cli("verify-all"); r.code != cli.ExitOK || !strings.Contains(r.out, `"verified"`) {
		t.Errorf("verify-all: exit %d, %q %q", r.code, r.out, r.errOut)
	}
	if r := s.cli("verify-all", "extra"); r.code != cli.ExitUsage {
		t.Errorf("verify-all extra: exit %d", r.code)
	}
	if r := s.cli("rebuild", "--apply"); r.code != cli.ExitUsage {
		t.Errorf("rebuild --apply with no reason: exit %d", r.code)
	}
}

// TestTheBinaryIsTheServerAndTheCLI (IP-14 DoD 7): the built file serves
// with no argument and with serve, and the same file administers it:
// init --yes, tenant list and events, each a separate process.
func TestTheBinaryIsTheServerAndTheCLI(t *testing.T) {
	bin := binary(t)
	boot, err := auth.IssueToken(auth.Admin)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh database, so the bootstrap key is installed on first start.
	baseDB, _ := integrationURLs(t)
	fresh := &stack{t: t, dbURL: freshDatabaseURL(t, baseDB)}
	for _, args := range [][]string{nil, {"serve"}} {
		var logs bytes.Buffer
		p := fresh.startArgs(args, "", &logs, "QUOTACORE_METRICS_ADDR=127.0.0.1:0", "QUOTACORE_LOG_LEVEL=warn",
			"QUOTACORE_BOOTSTRAP_ADMIN_KEY="+boot.Token, "QUOTACORE_BOOTSTRAP_ADMIN_KEY_ACK=yes")
		env := []string{"QUOTACORE_URL=" + p.base, "QUOTACORE_ADMIN_KEY=" + boot.Token, "PATH=/usr/bin:/bin"}
		cmd := func(args ...string) (int, string, string) {
			c := exec.Command(bin, args...)
			c.Env = env
			c.Stdin = strings.NewReader("")
			var out, errOut bytes.Buffer
			c.Stdout, c.Stderr = &out, &errOut
			err := c.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			return code, out.String(), errOut.String()
		}
		tn := externalID("bin")
		if code, out, errOut := cmd("init", "--yes", "--feature", uniq("bf"), "--plan", uniq("bp"), "--tenant", tn, "--key-name", uniq("bk")); code != 0 ||
			!strings.Contains(out, "shown once") || !strings.Contains(out, tn) {
			t.Fatalf("%v: init: exit %d, %q, %q", args, code, out, errOut)
		}
		if code, out, _ := cmd("tenant", "list"); code != 0 || !strings.Contains(out, tn) {
			t.Errorf("%v: tenant list: exit %d, %q", args, code, out)
		}
		if code, out, _ := cmd("events", tn); code != 0 || !strings.Contains(out, `"events"`) {
			t.Errorf("%v: events: exit %d, %q", args, code, out)
		}
		if code, _, _ := cmd("tenant", "get", "no-such-tenant"); code != cli.ExitNotFound {
			t.Errorf("%v: a missing tenant: exit %d", args, code)
		}
		if code, _, errOut := cmd("nonsense"); code != cli.ExitUsage || !strings.Contains(errOut, "unknown command") {
			t.Errorf("%v: an unknown command: exit %d", args, code)
		}
		p.kill()
	}
}
