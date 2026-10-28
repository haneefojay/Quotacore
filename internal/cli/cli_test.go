package cli

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/envelope"
)

func document(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// section is the text of one ## section.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	at := strings.Index(doc, "\n## "+heading+"\n")
	if at < 0 {
		t.Fatalf("no section %q", heading)
	}
	rest := doc[at+1:]
	if end := strings.Index(rest[3:], "\n## "); end >= 0 {
		rest = rest[:end+3]
	}
	return rest
}

var (
	commandRow = regexp.MustCompile("(?m)^\\| `([a-z -]+?)((?: <[a-z_]+>)*)` \\| (.+?) \\| (.+?) \\|$")
	opCell     = regexp.MustCompile("^`([A-Z]+) (\\S+)` \\(`([A-Za-z]+)`\\)$")
)

// TestTheMappingTableIsTheBinary (IP-14 DoD 1, D2): cli.md section 1 and the
// binary's table name the same commands, with the same arguments, and every
// endpoint row is the contract's operation with that method and path.
func TestTheMappingTableIsTheBinary(t *testing.T) {
	rows := commandRow.FindAllStringSubmatch(section(t, document(t, "docs/architecture/cli.md"), "1. Commands"), -1)
	doc := map[string]bool{}
	for _, r := range rows {
		name, args, maps := r[1], strings.TrimSpace(r[2]), r[3]
		doc[name] = true
		c, rest, ok := Lookup(strings.Fields(name))
		if !ok || len(rest) != 0 {
			t.Errorf("cli.md lists %q, which the binary does not have", name)
			continue
		}
		var want []string
		for _, a := range strings.Fields(args) {
			want = append(want, strings.Trim(a, "<>"))
		}
		if !slices.Equal(want, c.Args) {
			t.Errorf("%s: cli.md takes %v, the binary %v", name, want, c.Args)
		}
		if writes := r[4]; (writes == "yes") != c.Writes && writes != "with `--apply`" {
			t.Errorf("%s: cli.md says it changes state %q, the binary %v", name, writes, c.Writes)
		}
		m := opCell.FindStringSubmatch(maps)
		switch {
		case c.Kind == Remote && m == nil:
			t.Errorf("%s is remote and cli.md maps it to %q", name, maps)
		case c.Kind == Remote:
			op, ok := operation(m[3])
			if !ok || op.ID != c.Op || op.Method != m[1] || op.Path != m[2] {
				t.Errorf("%s: cli.md says %s %s (%s); the binary calls %s, which the contract has as %+v", name, m[1], m[2], m[3], c.Op, op)
			}
		case c.Kind == Composite:
			if !strings.HasPrefix(maps, "composite: ") {
				t.Errorf("%s is composite and cli.md maps it to %q", name, maps)
			}
			for _, call := range c.Calls {
				if !strings.Contains(maps, "`"+call+"`") {
					t.Errorf("%s calls %s and cli.md does not say so", name, call)
				}
				if _, ok := operation(call); !ok {
					t.Errorf("%s calls %s, which is not in the contract", name, call)
				}
			}
		case !strings.HasPrefix(maps, "local: "):
			t.Errorf("%s is local and cli.md maps it to %q", name, maps)
		}
	}
	for _, c := range Commands() {
		if !doc[c.Name] {
			t.Errorf("the binary has %q, which cli.md does not list", c.Name)
		}
	}
	if len(rows) != len(Commands()) {
		t.Errorf("cli.md has %d rows and the binary %d commands", len(rows), len(Commands()))
	}
}

// TestEveryAdminOperationHasACommand (IP-14 DoD 1): exactly one command per
// admin operation of the contract, and no command for any other.
func TestEveryAdminOperationHasACommand(t *testing.T) {
	by := map[string][]string{}
	for _, c := range Commands() {
		if c.Kind == Remote {
			by[c.Op] = append(by[c.Op], c.Name)
		}
	}
	admin := 0
	for _, op := range envelope.MustOperations() {
		if op.RequiredScope != "admin" {
			if by[op.ID] != nil {
				t.Errorf("%s is not an admin operation and %v call it", op.ID, by[op.ID])
			}
			continue
		}
		admin++
		if len(by[op.ID]) != 1 {
			t.Errorf("%s (%s) has commands %v, want exactly one", op.ID, op.Endpoint(), by[op.ID])
		}
	}
	if admin < 32 {
		t.Errorf("found %d admin operations; the contract has 32", admin)
	}
}

var (
	catalogueRow = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9_]*)` \\| \\d{3} \\|")
	exitRow      = regexp.MustCompile("(?m)^\\| (\\d+) \\| [^|]+ \\| ([^|]+) \\|$")
	backticked   = regexp.MustCompile("`([a-z_]+)`")
)

// TestEveryCatalogueCodeHasOneExitCode (IP-14 DoD 5, D5): each code of
// error-catalog.md section 2 is in exactly one row of cli.md section 3, that
// row's exit code is ExitFor's, and the catalogue is the generated enum.
func TestEveryCatalogueCodeHasOneExitCode(t *testing.T) {
	var catalogue []string
	for _, m := range catalogueRow.FindAllStringSubmatch(section(t, document(t, "docs/product/error-catalog.md"), "2. Catalogue"), -1) {
		catalogue = append(catalogue, m[1])
	}
	var enum []string
	for _, c := range contract.ErrorCode("").AllValues() {
		enum = append(enum, string(c))
	}
	slices.Sort(catalogue)
	slices.Sort(enum)
	if !slices.Equal(catalogue, enum) {
		t.Fatalf("the catalogue %v is not the contract's enum %v", catalogue, enum)
	}
	rows := 0
	exitOf := map[string][]int{}
	for _, m := range exitRow.FindAllStringSubmatch(section(t, document(t, "docs/architecture/cli.md"), "3. Exit codes"), -1) {
		rows++
		n, _ := strconv.Atoi(m[1])
		if n >= 100 {
			t.Errorf("exit code %d could be mistaken for an HTTP status", n)
		}
		for _, c := range backticked.FindAllStringSubmatch(m[2], -1) {
			exitOf[c[1]] = append(exitOf[c[1]], n)
		}
	}
	if rows != 11 {
		t.Errorf("cli.md section 3 has %d rows, want 0 to 10", rows)
	}
	for _, code := range catalogue {
		got := exitOf[code]
		if len(got) != 1 {
			t.Errorf("%s has exit codes %v in cli.md, want exactly one", code, got)
			continue
		}
		if want := ExitFor(contract.ErrorCode(code)); got[0] != want {
			t.Errorf("%s: cli.md says %d, the binary %d", code, got[0], want)
		}
		delete(exitOf, code)
	}
	for code := range exitOf {
		t.Errorf("cli.md maps %s, which the catalogue does not have", code)
	}
	if ExitFor("no_such_code") != ExitInternal {
		t.Error("an undefined code is not internal")
	}
}

// fake is a service that records each request and answers from a script.
type fake struct {
	mu       sync.Mutex
	requests []recorded
	answer   func(r *http.Request, body []byte) (int, string)
	srv      *httptest.Server
}

type recorded struct {
	Method, Path, Query, Auth, RequestID, Agent, ContentType string
	Body                                                     string
}

func newFake(t *testing.T, answer func(r *http.Request, body []byte) (int, string)) *fake {
	f := &fake{answer: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"),
			r.Header.Get("X-Request-Id"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), string(body)})
		f.mu.Unlock()
		status, out := f.answer(r, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) seen() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

const canaryKey = "qc_admin_0123456789abcdef.CANARYsecretCANARYsecretCANARY0"

// run runs the CLI against url with the canary key.
func run(t *testing.T, url string, terminal bool, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Run(Options{
		Args: args,
		Getenv: func(k string) string {
			switch k {
			case "QUOTACORE_URL":
				return url
			case "QUOTACORE_ADMIN_KEY":
				return canaryKey
			}
			return ""
		},
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errw, Terminal: terminal, Version: "test",
		Sleep: func(time.Duration) {},
	})
	if strings.Contains(out.String()+errw.String(), "CANARY") {
		t.Errorf("quotacore %v printed the admin key", args)
	}
	return code, out.String(), errw.String()
}

func envelopeBody(code contract.ErrorCode, details string) string {
	if details == "" {
		details = "{}"
	}
	return `{"error":{"code":"` + string(code) + `","message":"m","request_id":"rid-1","details":` + details + `}}`
}

// TestARemoteCommandIsOneAdminRequest (IP-14 S5, D6, D9): grant is one POST
// to its route with the admin key, a fresh request id, the contract's body
// and the binary's User-Agent; the answer is the stdout and nothing else.
func TestARemoteCommandIsOneAdminRequest(t *testing.T) {
	f := newFake(t, func(*http.Request, []byte) (int, string) { return 200, `{"balance":105}` })
	code, out, errw := run(t, f.srv.URL, false, "", "grant", "acme/one", "--feature", "llm.tokens", "--amount", "5", "--reason", "outage")
	if code != ExitOK || errw != "" {
		t.Fatalf("exit %d, stderr %q", code, errw)
	}
	if out != "{\n  \"balance\": 105\n}\n" {
		t.Errorf("stdout %q", out)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want one", len(reqs))
	}
	r := reqs[0]
	if r.Method != "POST" || r.Path != "/v1/admin/tenants/acme%2Fone/grant" || r.Auth != "Bearer "+canaryKey ||
		r.Agent != "quotacore/test" || r.ContentType != "application/json" {
		t.Errorf("request %+v", r)
	}
	if len(r.RequestID) != 36 {
		t.Errorf("request id %q is not a UUID", r.RequestID)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(r.Body), &b); err != nil || b["feature_key"] != "llm.tokens" || b["amount"] != float64(5) || b["reason"] != "outage" {
		t.Errorf("body %s", r.Body)
	}
	// A second run is a second id.
	run(t, f.srv.URL, false, "", "status")
	if reqs := f.seen(); reqs[1].RequestID == r.RequestID || reqs[1].Method != "GET" || reqs[1].Path != "/v1/admin/status" || reqs[1].Body != "" {
		t.Errorf("status request %+v", reqs[1])
	}
}

// TestExitCodesFollowTheCode (IP-14 DoD 5): each error answer's exit code
// is its code's, the envelope is on stderr with the request id, and a 2xx
// is 0.
func TestExitCodesFollowTheCode(t *testing.T) {
	for _, code := range contract.ErrorCode("").AllValues() {
		f := newFake(t, func(*http.Request, []byte) (int, string) { return envelope.Status(code), envelopeBody(code, "") })
		got, out, errw := run(t, f.srv.URL, false, "", "tenant", "get", "acme")
		if got != ExitFor(code) || out != "" || !strings.Contains(errw, "error: "+string(code)+": m (request_id rid-1)") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", code, got, out, errw)
		}
	}
	f := newFake(t, func(*http.Request, []byte) (int, string) { return 502, "<html>bad gateway</html>" })
	if got, _, errw := run(t, f.srv.URL, false, "", "status"); got != ExitInternal || !strings.Contains(errw, "request_id ") {
		t.Errorf("an answer the contract does not define: exit %d, %q", got, errw)
	}
	f = newFake(t, func(*http.Request, []byte) (int, string) { return 204, "" })
	if got, out, _ := run(t, f.srv.URL, false, "", "key", "revoke", "4f8a2c61-9d03-4e57-b2a8-6c1d7e0f3b24"); got != ExitOK || out != "" {
		t.Errorf("a 204: exit %d, stdout %q", got, out)
	}
}

// TestAConnectionFailureIsUnavailable (IP-14 DoD 5): no answer at all is 9,
// with the request id the CLI minted.
func TestAConnectionFailureIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	code, out, errw := run(t, "http://"+addr, false, "", "tenant", "list")
	if code != ExitUnavailable || out != "" || !regexp.MustCompile(`request_id [0-9a-f-]{36}`).MatchString(errw) {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errw)
	}
}

// TestUsageIsTwoAndSendsNothing: a mistake in what was typed is exit 2 and no
// request; a body the contract refuses is exit 3 and no request.
func TestUsageIsTwoAndSendsNothing(t *testing.T) {
	f := newFake(t, func(*http.Request, []byte) (int, string) { return 200, "{}" })
	for _, args := range [][]string{
		{"nonsense"},
		{"tenant"},
		{"tenant", "get"},
		{"tenant", "get", "a", "b"},
		{"grant", "acme", "--feature", "f", "--amount", "5"},
		{"grant", "acme", "--feature", "f", "--amount", "five", "--reason", "r"},
		{"tenant", "update", "acme", "--name", "n"},
		{"plan", "create", "--key", "p", "--name", "n", "--entitlement", "f=10"},
		{"tenant", "get", "acme", "--bogus"},
	} {
		if code, _, errw := run(t, f.srv.URL, false, "", args...); code != ExitUsage || errw == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errw)
		}
	}
	if code, _, _ := run(t, f.srv.URL, false, "", "grant", "acme", "--feature", "f", "--amount", "-5", "--reason", "r"); code != ExitClient {
		t.Errorf("a negative grant: exit %d, want 3", code)
	}
	if code, _, _ := run(t, f.srv.URL, false, "", "feature", "create", "--key", "NOT A KEY", "--name", "n", "--unit", "u"); code != ExitClient {
		t.Errorf("an invalid feature key: exit %d, want 3", code)
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
	var out, errw bytes.Buffer
	if code := Run(Options{Args: []string{"status"}, Stdout: &out, Stderr: &errw}); code != ExitUsage || !strings.Contains(errw.String(), "QUOTACORE_ADMIN_KEY") {
		t.Errorf("no credential: exit %d, %q", code, errw.String())
	}
	if code, out, _ := run(t, f.srv.URL, false, "", "help"); code != ExitOK || !strings.Contains(out, "tenant override set") {
		t.Errorf("help: exit %d", code)
	}
	if code, out, _ := run(t, f.srv.URL, false, "", "grant", "--help"); code != ExitOK || !strings.Contains(out, "POST /v1/admin/tenants/{id}/grant") {
		t.Errorf("grant --help: exit %d, %q", code, out)
	}
}

// TestKeyMintIsAnAdminToken (IP-14 DoD 4, D8): one token on stdout that
// parses as an admin key, the warning on stderr, and no request.
func TestKeyMintIsAnAdminToken(t *testing.T) {
	code, out, errw := run(t, "http://127.0.0.1:1", false, "", "key", "mint")
	tok, err := auth.Parse(strings.TrimSpace(out))
	if code != ExitOK || err != nil || tok.Label != auth.Admin || strings.Count(out, "\n") != 1 {
		t.Fatalf("exit %d, %q, %v", code, out, err)
	}
	if !strings.Contains(errw, "shown once") || strings.Contains(errw, strings.TrimSpace(out)) {
		t.Errorf("stderr %q", errw)
	}
	_, again, _ := run(t, "http://127.0.0.1:1", false, "", "key", "mint")
	if again == out {
		t.Error("two mints printed the same token")
	}
}

// TestKeyCreateWarnsOnce: the plaintext is on stdout once, never on stderr,
// and stderr says it cannot be shown again (INV-K3).
func TestKeyCreateWarnsOnce(t *testing.T) {
	const plain = "qc_runtime_9f2c1a4e7b8d.kZ3nQ7xR2vT9wY4bH6mP1sD8fG5jL0cA"
	f := newFake(t, func(*http.Request, []byte) (int, string) {
		return 201, `{"id":"4f8a2c61-9d03-4e57-b2a8-6c1d7e0f3b24","name":"n","prefix":"9f2c1a4e7b8d","scope":"runtime","state":"active","created_at":"2026-09-28T14:10:00Z","expires_at":null,"key":"` + plain + `"}`
	})
	code, out, errw := run(t, f.srv.URL, false, "", "key", "create", "--name", "n", "--scope", "runtime")
	if code != ExitOK || strings.Count(out, plain) != 1 || strings.Contains(errw, plain) || !strings.Contains(errw, "shown once") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errw)
	}
}

// TestPlanApplyNeverReissues (IP-14 D9): a confirmation_required is exit 6
// with the token printed for the operator, and exactly one request.
func TestPlanApplyNeverReissues(t *testing.T) {
	// A token of the contract's shape that no scanner reads as a secret.
	confirmation := "cnf_" + strings.Repeat("0", 26)
	f := newFake(t, func(*http.Request, []byte) (int, string) {
		return 409, envelopeBody(contract.ErrorCodeConfirmationRequired,
			`{"affected_tenants":51,"earliest_cycle_end":"2026-10-01T00:00:00Z","confirmation_token":"`+confirmation+`"}`)
	})
	code, _, errw := run(t, f.srv.URL, false, "", "plan", "apply", "starter", "--reason", "new limits")
	if code != ExitConflict || !strings.Contains(errw, "--confirmation-token "+confirmation) || len(f.seen()) != 1 {
		t.Errorf("exit %d, %d requests, stderr %q", code, len(f.seen()), errw)
	}
	run(t, f.srv.URL, false, "", "plan", "apply", "starter", "--reason", "new limits", "--confirmation-token", confirmation)
	if b := f.seen()[1].Body; !strings.Contains(b, `"confirmation_token":"`+confirmation+`"`) {
		t.Errorf("the re-issue sent %s", b)
	}
}

// TestAWriteIsNeverRetried (IP-14 S4): a rate-limited grant is exit 8 after
// one request; --all on a read waits out one and carries on.
func TestAWriteIsNeverRetried(t *testing.T) {
	limited := envelopeBody(contract.ErrorCodeRateLimited, "")
	f := newFake(t, func(*http.Request, []byte) (int, string) { return 429, limited })
	if code, _, _ := run(t, f.srv.URL, false, "", "grant", "acme", "--feature", "f", "--amount", "1", "--reason", "r"); code != ExitRateLimited || len(f.seen()) != 1 {
		t.Errorf("exit %d after %d requests", code, len(f.seen()))
	}
	calls := 0
	f = newFake(t, func(r *http.Request, _ []byte) (int, string) {
		calls++
		switch {
		case calls == 2:
			return 429, limited
		case r.URL.Query().Get("cursor") == "":
			return 200, `{"tenants":[{"external_id":"a"}],"next_cursor":"c2"}`
		}
		return 200, `{"tenants":[{"external_id":"b"}],"next_cursor":null}`
	})
	code, out, _ := run(t, f.srv.URL, false, "", "tenant", "list", "--all", "--limit", "1")
	var page struct {
		Tenants []map[string]string `json:"tenants"`
	}
	if err := json.Unmarshal([]byte(out), &page); code != ExitOK || err != nil || len(page.Tenants) != 2 || page.Tenants[1]["external_id"] != "b" {
		t.Errorf("exit %d, %q", code, out)
	}
	if reqs := f.seen(); len(reqs) != 3 || !strings.Contains(reqs[0].Query, "limit=1") || !strings.Contains(reqs[2].Query, "cursor=c2") {
		t.Errorf("requests %+v", reqs)
	}
}

// initService answers init's five calls.
func initService(t *testing.T) *fake {
	return newFake(t, func(r *http.Request, _ []byte) (int, string) {
		switch r.URL.Path {
		case "/v1/admin/keys":
			return 201, `{"id":"4f8a2c61-9d03-4e57-b2a8-6c1d7e0f3b24","key":"qc_runtime_9f2c1a4e7b8d.kZ3nQ7xR2vT9wY4bH6mP1sD8fG5jL0cA"}`
		case "/v1/balance":
			return 200, `{"tenant_external_id":"local-dev","entries":[{"feature_key":"llm.tokens.output","cycle_start":"2026-10-01T00:00:00Z","cycle_end":"2026-11-01T00:00:00Z"}]}`
		}
		return 201, `{}`
	})
}

// TestInitPromptsOnlyAtATerminal (IP-14 DoD 6, D3): with no terminal, or
// with --yes, init reads nothing; at a terminal it offers each default and
// takes an answer. Either way it makes the five calls in order and prints
// the runtime key once.
func TestInitPromptsOnlyAtATerminal(t *testing.T) {
	const plain = "qc_runtime_9f2c1a4e7b8d.kZ3nQ7xR2vT9wY4bH6mP1sD8fG5jL0cA"
	for _, tc := range []struct {
		name     string
		terminal bool
		stdin    string
		args     []string
		tenant   string
	}{
		{"no terminal", false, "ignored\n", nil, "local-dev"},
		{"--yes", true, "ignored\n", []string{"--yes"}, "local-dev"},
		{"a terminal", true, "\n\n\n\n\nacme\n", nil, "acme"},
	} {
		f := initService(t)
		code, out, errw := run(t, f.srv.URL, tc.terminal, tc.stdin, append([]string{"init"}, tc.args...)...)
		if code != ExitOK || strings.Count(out, plain) != 1 || strings.Contains(errw, plain) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", tc.name, code, out, errw)
		}
		if !strings.Contains(out, "2026-10-01T00:00:00Z → 2026-11-01T00:00:00Z") {
			t.Errorf("%s: no cycle in %q", tc.name, out)
		}
		prompted := strings.Contains(errw, "feature key [llm.tokens.output]: ")
		if prompted != (tc.terminal && tc.args == nil) {
			t.Errorf("%s: prompted %v; stderr %q", tc.name, prompted, errw)
		}
		var calls []string
		for _, r := range f.seen() {
			calls = append(calls, r.Method+" "+r.Path)
		}
		want := []string{"POST /v1/admin/features", "POST /v1/admin/plans", "POST /v1/admin/tenants", "POST /v1/admin/keys", "GET /v1/balance"}
		if !slices.Equal(calls, want) {
			t.Errorf("%s: calls %v", tc.name, calls)
		}
		if reqs := f.seen(); len(reqs) == 5 {
			if !strings.Contains(reqs[2].Body, `"external_id":"`+tc.tenant+`"`) || reqs[4].Auth != "Bearer "+plain {
				t.Errorf("%s: tenant %s, balance auth %q", tc.name, reqs[2].Body, reqs[4].Auth)
			}
		}
	}
}

// TestInitStopsAtTheFirstFailure: a conflict on the plan stops init with
// exit 6, names what it made, and sends nothing after.
func TestInitStopsAtTheFirstFailure(t *testing.T) {
	f := newFake(t, func(r *http.Request, _ []byte) (int, string) {
		if r.URL.Path == "/v1/admin/plans" {
			return 409, envelopeBody(contract.ErrorCodePlanKeyExists, "")
		}
		return 201, `{}`
	})
	code, _, errw := run(t, f.srv.URL, false, "", "init")
	if code != ExitConflict || len(f.seen()) != 2 || !strings.Contains(errw, "created the feature llm.tokens.output, which it leaves in place") {
		t.Errorf("exit %d, %d requests, %q", code, len(f.seen()), errw)
	}
}

// TestCheckParsesWithoutRunning is the runbook test's half (IP-14 D10).
func TestCheckParsesWithoutRunning(t *testing.T) {
	for _, ok := range [][]string{
		{"tenant", "get", "$TENANT_ID"}, {"rollover", "$TENANT_ID", "--feature", "F", "--reason", "stuck cycle"},
		{"audit", "--limit", "20"}, {"key", "list", "--all"}, {"init", "--yes"}, {"key", "mint"}, {"verify-all"},
	} {
		if err := Check(ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"tenant", "show", "x"}, {"rollover", "x", "--reason", "r"}, {"audit", "--tenantt", "x"}, {"key", "mint", "x"}, {"init", "x"},
	} {
		if err := Check(bad); err == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

// TestTheCLIReachesNoDatastore (IP-14 D12): the package imports no store,
// script, SQL or Redis package, so a remote command can reach the service
// only over HTTP.
func TestTheCLIReachesNoDatastore(t *testing.T) {
	forbidden := []string{"database/sql", "github.com/jackc/", "github.com/redis/", "internal/db", "internal/store",
		"internal/script", "internal/snapshot", "internal/dataplane", "internal/admin"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		read++
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.Contains(path, bad) {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
	}
	if read < 5 {
		t.Errorf("read %d files", read)
	}
}

// TestHealthcheckAsksItsOwnReadyz (IP-15 D4): the image's healthcheck is a
// GET /readyz on loopback at QUOTACORE_LISTEN_ADDR's port, never QUOTACORE_URL;
// ready is 0, anything else is 9, and nothing reaches stdout either way.
func TestHealthcheckAsksItsOwnReadyz(t *testing.T) {
	status := http.StatusOK
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Error("the healthcheck sent a credential")
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"status":"not_ready"}`)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	check := func() (int, string, string) {
		var out, errw bytes.Buffer
		code := Run(Options{
			Args: []string{"healthcheck"},
			Getenv: func(k string) string {
				switch k {
				case "QUOTACORE_LISTEN_ADDR":
					return ":" + port
				case "QUOTACORE_URL":
					return "http://192.0.2.1:1" // never asked
				}
				return ""
			},
			Stdout: &out, Stderr: &errw,
		})
		return code, out.String(), errw.String()
	}
	if code, out, _ := check(); code != ExitOK || out != "" {
		t.Fatalf("ready: exit %d, stdout %q", code, out)
	}
	status = http.StatusServiceUnavailable
	if code, out, errw := check(); code != ExitUnavailable || out != "" || !strings.Contains(errw, "503") {
		t.Fatalf("not ready: exit %d, stdout %q, stderr %q", code, out, errw)
	}
	srv.Close()
	if code, _, _ := check(); code != ExitUnavailable {
		t.Fatalf("no listener: exit %d, want %d", code, ExitUnavailable)
	}
	if !slices.Equal(paths, []string{"GET /readyz", "GET /readyz"}) {
		t.Fatalf("calls %v", paths)
	}
}
