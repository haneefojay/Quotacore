// Package cli is the quotacore administration tool (IP-14): one command per
// admin operation of the contract, init, and the dispatch that hands the
// local operations back to the binary. The mapping table is Commands, and
// docs/architecture/cli.md is the same table for a person.
//
// A remote command is an ordinary admin request (request-lifecycle.md steps 1
// to 3): it authenticates with QUOTACORE_ADMIN_KEY, is rate limited and
// audited as that key, and carries a fresh X-Request-Id. The key is read only
// from the environment, never from a flag, so it is never in a process list
// (NFR-S1), and nothing this package prints is built from it.
//
// The package reaches neither Postgres nor the data store: it talks to a
// running service over HTTP or touches nothing at all.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/envelope"
)

// Options is what Run needs from the process. main fills it from os; a test
// fills it from its stack.
type Options struct {
	Args   []string
	Getenv func(string) string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Terminal is true when stdin is a terminal; only init prompts, and only
	// then (IP-14 D3).
	Terminal bool
	// Locals runs the commands the binary carries out itself: serve and the
	// four IP-11 operations. Each returns its exit code.
	Locals map[string]func(args []string) int
	// HTTP is the client remote commands use; nil is a client with a timeout.
	HTTP *http.Client
	// Version is the build's version, for User-Agent.
	Version string
	// Sleep waits out a Retry-After; nil is time.Sleep.
	Sleep func(time.Duration)
}

// Run carries out one invocation and returns its exit code.
func Run(o Options) int {
	if o.Getenv == nil {
		o.Getenv = func(string) string { return "" }
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	args := o.Args
	if len(args) == 0 {
		return o.local("serve", nil)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		overview(o.Stdout)
		return ExitOK
	}
	c, rest, ok := Lookup(args)
	if !ok {
		if isGroup(args[0]) {
			w, code := o.Stderr, ExitUsage
			if len(args) > 1 && isHelp(args[1]) {
				w, code = o.Stdout, ExitOK
			}
			groupHelp(w, args[0])
			return code
		}
		fmt.Fprintf(o.Stderr, "error: unknown command %q; run quotacore help for the list\n", strings.Join(args, " "))
		return ExitUsage
	}
	switch c.Kind {
	case Local:
		if c.Name == "key mint" {
			return o.mint(c, rest)
		}
		if c.Name == "healthcheck" {
			return o.healthcheck(c, rest)
		}
		if len(rest) > 0 && isHelp(rest[0]) {
			commandHelp(o.Stdout, c, nil)
			return ExitOK
		}
		return o.local(c.Name, rest)
	case Composite:
		return o.init(c, rest)
	}
	return o.remote(c, rest)
}

func isHelp(a string) bool { return a == "-h" || a == "-help" || a == "--help" || a == "help" }

func (o Options) local(name string, args []string) int {
	run := o.Locals[name]
	if run == nil {
		fmt.Fprintf(o.Stderr, "error: %s is not available in this build\n", name)
		return ExitInternal
	}
	return run(args)
}

// prepared is a remote command parsed and built, ready to send.
type prepared struct {
	pos  []string
	req  request
	url  string
	all  bool
	help *flags // set when --help was asked for
}

// prepare parses a remote command's arguments and builds its request, with
// no I/O, so a runbook's command line can be checked without running it.
func prepare(c Command, args []string) (prepared, error) {
	f := newFlags(c.Name)
	urlFlag := f.str("url", "the service's base URL; QUOTACORE_URL by default")
	var limit, cursor *string
	all := new(bool)
	if c.List {
		limit, cursor = f.str("limit", "page size, 1 to 200"), f.str("cursor", "the next_cursor of the previous page")
		all = f.fs.Bool("all", false, "follow next_cursor and print every page as one")
	}
	var b build
	if c.setup != nil {
		b = c.setup(f)
	}
	pos, err := f.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return prepared{help: f}, nil
	}
	if err == nil {
		err = checkArgs(c, f, pos)
	}
	if err != nil {
		return prepared{}, err
	}
	p := prepared{pos: pos, req: request{query: url.Values{}}, url: *urlFlag, all: *all}
	if b != nil {
		if p.req, err = b(pos); err != nil {
			return prepared{}, err
		}
		if p.req.query == nil {
			p.req.query = url.Values{}
		}
	}
	if c.List {
		f.query(p.req.query, map[string]*string{"limit": limit, "cursor": cursor})
	}
	return p, nil
}

// remote parses one remote command and makes its call.
func (o Options) remote(c Command, args []string) int {
	p, err := prepare(c, args)
	if err != nil {
		return o.usage(c, err)
	}
	if p.help != nil {
		commandHelp(o.Stdout, c, p.help)
		return ExitOK
	}
	if p.req.body != nil {
		if err := p.req.body.Validate(); err != nil {
			fmt.Fprintf(o.Stderr, "error: the request is not one the contract accepts, so it was not sent: %v\n", err)
			return ExitClient
		}
	}
	cl, err := o.client(p.url)
	if err != nil {
		return o.usage(c, err)
	}
	path, err := pathOf(c.Op, p.pos)
	if err != nil {
		return o.usage(c, err)
	}
	if p.all {
		return cl.all(path, p.req.query, c.Op)
	}
	return cl.finish(c, cl.call(c.Op, path, p.req.query, p.req.body, cl.key))
}

// Check parses a command line as Run would, without running it: the command
// exists and, for every command but the four IP-11 operations, whose flags
// the binary parses, its arguments and flags are ones it accepts. The
// runbook test holds each runbook's command lines to it (IP-14 D10).
func Check(args []string) error {
	c, rest, ok := Lookup(args)
	if !ok {
		return fmt.Errorf("no command %q", strings.Join(args, " "))
	}
	switch {
	case c.Kind == Remote:
		_, err := prepare(c, rest)
		return err
	case c.Kind == Composite:
		f, _ := initFlags(c)
		pos, err := f.parse(rest)
		if err == nil {
			err = checkArgs(c, f, pos)
		}
		return err
	case c.Name == "key mint", c.Name == "healthcheck":
		f := newFlags(c.Name)
		pos, err := f.parse(rest)
		if err == nil {
			err = checkArgs(c, f, pos)
		}
		return err
	}
	return nil
}

func checkArgs(c Command, f *flags, pos []string) error {
	if len(pos) != len(c.Args) {
		want := "no argument"
		if len(c.Args) > 0 {
			want = "<" + strings.Join(c.Args, "> <") + ">"
		}
		return usagef("%s takes %s", c.Name, want)
	}
	if m := f.missing(); len(m) > 0 {
		return usagef("%s needs %s", c.Name, strings.Join(m, ", "))
	}
	return nil
}

func (o Options) usage(c Command, err error) int {
	fmt.Fprintf(o.Stderr, "error: %v\nrun quotacore %s --help for its flags\n", err, c.Name)
	return ExitUsage
}

// pathOf fills the operation's path template with the positional arguments,
// in the order the template names them.
func pathOf(op string, pos []string) (string, error) {
	for _, o := range envelope.MustOperations() {
		if o.ID != op {
			continue
		}
		path := o.Path
		for _, p := range pos {
			open := strings.Index(path, "{")
			end := strings.Index(path, "}")
			if open < 0 || end < open {
				return "", usagef("too many arguments")
			}
			path = path[:open] + url.PathEscape(p) + path[end+1:]
		}
		return path, nil
	}
	return "", fmt.Errorf("operation %s is not in the contract", op)
}

// operation is the contract's operation by id.
func operation(id string) (envelope.Operation, bool) {
	for _, o := range envelope.MustOperations() {
		if o.ID == id {
			return o, true
		}
	}
	return envelope.Operation{}, false
}

// baseURL is --url, else QUOTACORE_URL, else loopback on
// QUOTACORE_LISTEN_ADDR's port, so inside the reference container it needs
// nothing (IP-14 D4).
func baseURL(flagValue string, getenv func(string) string) (*url.URL, error) {
	raw := flagValue
	if raw == "" {
		raw = getenv("QUOTACORE_URL")
	}
	if raw == "" {
		port := "8080"
		if addr := getenv("QUOTACORE_LISTEN_ADDR"); addr != "" {
			if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
				port = p
			}
		}
		raw = "http://127.0.0.1:" + port
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usagef("the service URL is not an http or https URL; set QUOTACORE_URL or --url")
	}
	if u.User != nil {
		return nil, usagef("the service URL carries credentials; the admin key belongs in QUOTACORE_ADMIN_KEY")
	}
	return u, nil
}

// adminKey is QUOTACORE_ADMIN_KEY, else the bootstrap key the quickstart
// already set, so `docker compose exec quotacore quotacore init` needs
// nothing more.
func adminKey(getenv func(string) string) string {
	if k := getenv("QUOTACORE_ADMIN_KEY"); k != "" {
		return k
	}
	return getenv("QUOTACORE_BOOTSTRAP_ADMIN_KEY")
}

// mint prints a new admin token for QUOTACORE_BOOTSTRAP_ADMIN_KEY (IP-14 D8).
// It reaches no server and no database; the service installs the token on
// its first start, at warn (INV-K2).
func (o Options) mint(c Command, args []string) int {
	f := newFlags(c.Name)
	pos, err := f.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		commandHelp(o.Stdout, c, f)
		return ExitOK
	}
	if err == nil {
		err = checkArgs(c, f, pos)
	}
	if err != nil {
		return o.usage(c, err)
	}
	is, err := auth.IssueToken(auth.Admin)
	if err != nil {
		fmt.Fprintf(o.Stderr, "error: %v\n", err)
		return ExitInternal
	}
	fmt.Fprintln(o.Stdout, is.Token)
	fmt.Fprintln(o.Stderr, shownOnce)
	return ExitOK
}

// healthcheck asks the service on this host whether it is ready, for the
// image's HEALTHCHECK, which has no shell or curl to ask with (IP-15 D4). It
// calls /readyz on loopback at QUOTACORE_LISTEN_ADDR's port, never
// QUOTACORE_URL, because a healthcheck reports on its own container. Ready is
// 0 and anything else is ExitUnavailable; it prints nothing on success, and the
// reason on stderr otherwise, which Docker keeps in the health log.
func (o Options) healthcheck(c Command, args []string) int {
	f := newFlags(c.Name)
	pos, err := f.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		commandHelp(o.Stdout, c, f)
		return ExitOK
	}
	if err == nil {
		err = checkArgs(c, f, pos)
	}
	if err != nil {
		return o.usage(c, err)
	}
	base, err := baseURL("", func(k string) string {
		if k == "QUOTACORE_URL" {
			return ""
		}
		return o.Getenv(k)
	})
	if err != nil {
		fmt.Fprintf(o.Stderr, "error: %v\n", err)
		return ExitUsage
	}
	h := o.HTTP
	if h == nil {
		h = &http.Client{Timeout: healthcheckTimeout}
	}
	resp, err := h.Get(base.JoinPath("readyz").String())
	if err != nil {
		fmt.Fprintf(o.Stderr, "not ready: %v\n", err)
		return ExitUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(o.Stderr, "not ready: %d %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return ExitUnavailable
	}
	return ExitOK
}

// healthcheckTimeout is shorter than Docker's default healthcheck timeout of
// 30 s and the Compose file's 3 s, so the command answers before it is killed.
const healthcheckTimeout = 2 * time.Second

// shownOnce is the warning beside every plaintext a command prints (INV-K3).
const shownOnce = "warning: this key is shown once and cannot be recovered; store it now (INV-K3)"

// webhookShownOnce is the warning beside a webhook secret (Q-40).
const webhookShownOnce = "warning: this webhook secret is shown once and cannot be recovered; give it to the receiver now, or rotate it (Q-40)"

func overview(w io.Writer) {
	fmt.Fprint(w, "quotacore administers a Quotacore service, and is the service.\n\n"+
		"usage: quotacore <command> [arguments] [flags]\n\n"+
		"The service is QUOTACORE_URL (or --url) and the credential is QUOTACORE_ADMIN_KEY.\n"+
		"No argument, or serve, runs the service. Every command prints JSON on stdout.\n\n")
	names := make([]string, 0, len(commands))
	width := 0
	for _, c := range commands {
		names = append(names, c.Name)
		width = max(width, len(c.Name))
	}
	sort.Strings(names)
	for _, n := range names {
		c, _, _ := Lookup(strings.Fields(n))
		fmt.Fprintf(w, "  %-*s  %s\n", width, n, c.Summary)
	}
	fmt.Fprint(w, "\nRun quotacore <command> --help for its flags. Exit codes are in docs/architecture/cli.md.\n")
}

func groupHelp(w io.Writer, group string) {
	fmt.Fprintf(w, "usage: quotacore %s <command>\n\n", group)
	for _, c := range commands {
		if strings.HasPrefix(c.Name, group+" ") {
			fmt.Fprintf(w, "  %-22s  %s\n", c.Name, c.Summary)
		}
	}
}

func commandHelp(w io.Writer, c Command, f *flags) {
	use := "quotacore " + c.Name
	for _, a := range c.Args {
		use += " <" + a + ">"
	}
	fmt.Fprintf(w, "usage: %s [flags]\n\n%s.\n", use, c.Summary)
	switch c.Kind {
	case Remote:
		if op, ok := operation(c.Op); ok {
			fmt.Fprintf(w, "\nCalls %s %s (%s).\n", op.Method, op.Path, op.ID)
		}
	case Composite:
		fmt.Fprintf(w, "\nCalls, in order: %s.\n", strings.Join(c.Calls, ", "))
	case Local:
		fmt.Fprint(w, "\nA local operation: it reads the configuration, not QUOTACORE_URL.\n")
	}
	if f == nil {
		return
	}
	required := map[string]bool{}
	for _, n := range f.required {
		required[n] = true
	}
	fmt.Fprintln(w)
	f.fs.VisitAll(func(fl *flag.Flag) {
		mark := ""
		if required[fl.Name] {
			mark = " (required)"
		}
		fmt.Fprintf(w, "  --%-20s %s%s\n", fl.Name, fl.Usage, mark)
	})
}

// ctx bounds one call; an admin call that takes a minute is not answering.
func callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}
