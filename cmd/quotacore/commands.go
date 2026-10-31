package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/quotacore/quotacore/internal/cli"
	"github.com/quotacore/quotacore/internal/cycle"
	"github.com/quotacore/quotacore/internal/db"
	"github.com/quotacore/quotacore/internal/reconcile"
	"github.com/quotacore/quotacore/internal/script"
	"github.com/quotacore/quotacore/internal/snapshot"
	"github.com/quotacore/quotacore/internal/store"
)

// The binary's local operations (IP-11 D2b): `verify-tenant`, `verify-all`,
// `reconcile-report` and `rebuild`, and IP-16's two for a Postgres restore
// (Q-28): `config bump` and `audit note`. Each reads Postgres and the fast store
// directly, prints JSON, and exits 1 when it has something a person must look
// at. They are rows of internal/cli's command table, which dispatches to
// them (IP-14 D11).

// Exit codes of a local operation, from the binary's one table (IP-14 D5).
// exitError marks an error that exitFor classifies once it is known.
const (
	exitClean   = cli.ExitOK
	exitFinding = cli.ExitFinding
	exitUsage   = cli.ExitUsage
	exitError   = cli.ExitInternal
)

// errUsage marks a mistake in what the operator typed.
type errUsage struct{ error }

// exitFor is the exit code of a local operation's error: usage, a tenant
// that does not exist, a dependency that did not answer, or internal.
func exitFor(err error) int {
	var u errUsage
	var ne net.Error
	var ce *pgconn.ConnectError
	switch {
	case errors.As(err, &u):
		return exitUsage
	case errors.Is(err, snapshot.ErrTenantUnknown):
		return cli.ExitNotFound
	case errors.As(err, &ne), errors.As(err, &ce), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, context.DeadlineExceeded):
		return cli.ExitUnavailable
	}
	return exitError
}

// fenceGrace is how long a rebuild waits after taking the fence before it
// reads or writes anything: two of an instance's five-second fence checks, so
// an instance whose fence connection dropped just before has found it gone
// and stopped writing (IP-11 D6).
const fenceGrace = 10 * time.Second

// sessionStale is how long a ledger writer's session may go unseen before a
// rebuild reads it as a crash: twelve of its five-second heartbeats.
const sessionStale = time.Minute

// commands is what the local operations need, built once from the
// configuration or, in a test, from the stack.
type commands struct {
	db      *sql.DB
	data    *store.Store
	scripts *script.Runner
	rec     *db.Reconciler
	loader  *db.Loader
	settle  time.Duration
	// retention is the configured event retention; zero keeps everything.
	retention time.Duration
	host      string
	now       func() time.Time
	out       io.Writer
	// grace is fenceGrace, shorter in a test.
	grace time.Duration
}

// runCommand opens what the operation needs and runs it.
func runCommand(args []string, stdout, stderr io.Writer) int {
	cfg, err := Load(OSGetenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	ctx := context.Background()
	dbConn, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFor(err)
	}
	defer dbConn.Close()
	data, err := store.Open(store.Options{URL: cfg.RedisURL, DataScriptTimeout: cfg.DataScriptTimeout})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFor(err)
	}
	defer data.Close()
	runner, err := script.New(script.Options{Client: data.Client(), Timeout: cfg.DataScriptTimeout})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFor(err)
	}
	c := &commands{db: dbConn, data: data, scripts: runner, rec: db.NewReconciler(dbConn),
		loader: db.NewLoader(dbConn, data), settle: cfg.Settle(),
		retention: time.Duration(cfg.EventRetentionDays) * 24 * time.Hour, host: hostname(), now: time.Now,
		out: stdout, grace: fenceGrace}
	code, err := c.run(ctx, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if code == exitError {
			code = exitFor(err)
		}
	}
	return code
}

func (c *commands) run(ctx context.Context, args []string) (int, error) {
	switch args[0] {
	case "verify-tenant":
		if len(args) != 2 {
			return exitUsage, errUsage{errors.New("usage: quotacore verify-tenant <tenant id or external id>")}
		}
		return c.verifyTenant(ctx, args[1])
	case "verify-all":
		if len(args) != 1 {
			return exitUsage, errUsage{errors.New("usage: quotacore verify-all")}
		}
		return c.verifyAll(ctx)
	case "reconcile-report":
		return c.report(ctx, args[1:])
	case "config bump":
		if len(args) != 1 {
			return exitUsage, errUsage{errors.New("usage: quotacore config bump")}
		}
		return c.configBump(ctx)
	case "audit note":
		return c.auditNote(ctx, args[1:])
	case "rebuild":
		opts, err := parseRebuild(args[1:])
		if err != nil {
			return exitUsage, errUsage{err}
		}
		rep, code, err := c.rebuild(ctx, opts)
		if err != nil {
			return exitError, err
		}
		if err := c.print(rep, opts.reportFile); err != nil {
			return exitError, err
		}
		return code, nil
	}
	return exitError, fmt.Errorf("unknown command %q", args[0])
}

func (c *commands) print(v any, file string) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if file != "" {
		return os.WriteFile(file, body, 0o600)
	}
	_, err = c.out.Write(body)
	return err
}

// tenantID resolves an id or an external id to the tenant's id.
func (c *commands) tenantID(ctx context.Context, ref string) (string, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return ref, nil
	}
	return c.loader.TenantIDByExternalID(ctx, ref)
}

func (c *commands) pass() *reconcile.Pass {
	return reconcile.NewPass(reconcile.PassOptions{Ledger: c.rec, Balances: balances{c.data}, Settle: c.settle,
		Now: c.now})
}

// verified is the output of verify-tenant and verify-all.
type verified struct {
	Verified int                 `json:"verified"`
	Findings []reconcile.Finding `json:"findings"`
	Recorded int                 `json:"recorded"`
}

func (c *commands) verifyTenant(ctx context.Context, ref string) (int, error) {
	id, err := c.tenantID(ctx, ref)
	if err != nil {
		return exitError, err
	}
	subs, err := c.subjects(ctx, id)
	if err != nil {
		return exitError, err
	}
	return c.verify(ctx, subs)
}

func (c *commands) verifyAll(ctx context.Context) (int, error) {
	subs, err := c.subjects(ctx, "")
	if err != nil {
		return exitError, err
	}
	return c.verify(ctx, subs)
}

func (c *commands) verify(ctx context.Context, subs []reconcile.Subject) (int, error) {
	found, recorded, err := c.pass().Verify(ctx, subs)
	if err != nil {
		return exitError, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Fingerprint() < found[j].Fingerprint() })
	if found == nil {
		found = []reconcile.Finding{}
	}
	if err := c.print(verified{Verified: len(subs), Findings: found, Recorded: recorded}, ""); err != nil {
		return exitError, err
	}
	if len(found) > 0 {
		return exitFinding, nil
	}
	return exitClean, nil
}

// subjects is every tenant-feature the ledger names and every feature the
// tenant holds, of one tenant or of all.
func (c *commands) subjects(ctx context.Context, tenantID string) ([]reconcile.Subject, error) {
	subs, err := c.rec.Subjects(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ids := []string{tenantID}
	if tenantID == "" {
		if ids, err = c.rec.Tenants(ctx); err != nil {
			return nil, err
		}
	}
	seen := map[reconcile.Subject]bool{}
	for _, s := range subs {
		seen[s] = true
	}
	now := c.now()
	for _, id := range ids {
		lt, err := c.loader.Load(ctx, id)
		if errors.Is(err, snapshot.ErrTenantUnknown) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for f, e := range snapshot.NewTenant(lt).Features(now) {
			// A boolean or rolling entitlement has no balance and no ledger
			// rows to reconcile (DR-050, DR-052).
			if !e.Anchored() {
				continue
			}
			s := reconcile.Subject{TenantID: id, Feature: f}
			if !seen[s] {
				seen[s] = true
				subs = append(subs, s)
			}
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].TenantID != subs[j].TenantID {
			return subs[i].TenantID < subs[j].TenantID
		}
		return subs[i].Feature < subs[j].Feature
	})
	return subs, nil
}

// report prints the findings table, newest first (S5).
func (c *commands) report(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("reconcile-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.String("since", "", "an RFC 3339 instant, or a duration back from now such as 24h")
	limit := fs.Int("limit", 1000, "the most findings to print")
	if err := fs.Parse(args); err != nil {
		return exitUsage, errUsage{fmt.Errorf("usage: quotacore reconcile-report [--since <instant or duration>] [--limit n]: %w", err)}
	}
	from := time.Time{}
	if *since != "" {
		if d, err := time.ParseDuration(*since); err == nil {
			from = c.now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, *since); err == nil {
			from = t
		} else {
			return exitUsage, errUsage{fmt.Errorf("--since %q is neither an RFC 3339 instant nor a duration", *since)}
		}
	}
	found, err := c.rec.Findings(ctx, from, *limit)
	if err != nil {
		return exitError, err
	}
	if found == nil {
		found = []db.StoredFinding{}
	}
	if err := c.print(map[string]any{"findings": found}, ""); err != nil {
		return exitError, err
	}
	if len(found) > 0 {
		return exitFinding, nil
	}
	return exitClean, nil
}

// rebuildOptions are the rebuild's flags (IP-11 D7).
type rebuildOptions struct {
	apply      bool
	reportFile string
	accept     []reconcile.Subject
	set        map[reconcile.Subject]int64
	reason     string
	restoredAt time.Time
	// refs keep what the operator typed, for the tenant ids to resolve.
	acceptRefs []string
	setRefs    map[string]int64
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func parseRebuild(args []string) (rebuildOptions, error) {
	fs := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o rebuildOptions
	var accept, set listFlag
	var restored string
	fs.BoolVar(&o.apply, "apply", false, "write every exact hash; without it the rebuild is a dry run")
	fs.StringVar(&o.reportFile, "report", "", "write the report to this file rather than stdout")
	fs.Var(&accept, "accept", "<tenant>:<feature>: write the ledger's estimate for one flagged tenant-feature")
	fs.Var(&set, "set", "<tenant>:<feature>=<n>: write n for one flagged tenant-feature")
	fs.StringVar(&o.reason, "reason", "", "why; required whenever the rebuild writes")
	fs.StringVar(&restored, "ledger-restored-at", "", "the RFC 3339 instant Postgres was restored to, if it was")
	usage := "usage: quotacore rebuild [--apply] [--report <file>] [--accept <tenant>:<feature>]... " +
		"[--set <tenant>:<feature>=<n>]... [--reason <text>] [--ledger-restored-at <instant>]"
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return o, fmt.Errorf("%s: %v", usage, err)
	}
	o.acceptRefs = accept
	o.setRefs = map[string]int64{}
	for _, s := range set {
		ref, value, ok := strings.Cut(s, "=")
		n, err := strconv.ParseInt(value, 10, 64)
		if !ok || err != nil || n < 0 {
			return o, fmt.Errorf("--set %q is not <tenant>:<feature>=<n> with n a whole number", s)
		}
		o.setRefs[ref] = n
	}
	for _, ref := range append(append([]string(nil), accept...), keys(o.setRefs)...) {
		if t, f, ok := strings.Cut(ref, ":"); !ok || t == "" || f == "" {
			return o, fmt.Errorf("%q is not <tenant>:<feature>", ref)
		}
	}
	if restored != "" {
		t, err := time.Parse(time.RFC3339, restored)
		if err != nil {
			return o, fmt.Errorf("--ledger-restored-at %q is not an RFC 3339 instant", restored)
		}
		o.restoredAt = t
	}
	if (o.apply || len(accept) > 0 || len(set) > 0) && strings.TrimSpace(o.reason) == "" {
		return o, errors.New("--reason is required whenever the rebuild writes (--apply, --accept or --set)")
	}
	return o, nil
}

func keys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (o rebuildOptions) writes() bool { return o.apply || len(o.accept) > 0 || len(o.set) > 0 }

// rebuildEntry is one tenant-feature in the report.
type rebuildEntry struct {
	reconcile.Rebuilt
	// Action is what this run did: written (an exact hash), accepted, set,
	// or empty when it wrote nothing.
	Action string `json:"action,omitempty"`
	// Written is the hash it wrote.
	Written *reconcile.State `json:"written,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// rebuildReport is the rebuild's output.
type rebuildReport struct {
	Applied   bool           `json:"applied"`
	At        time.Time      `json:"at"`
	RequestID string         `json:"request_id"`
	Counts    map[string]int `json:"counts"`
	Entries   []rebuildEntry `json:"entries"`
}

// rebuild enumerates every tenant-feature, classifies each absent hash, and,
// when told to, writes the exact ones and the ones an operator accepts or
// sets, through the create-only script, under the exclusive fence (IP-11 D6,
// D7). It never rewrites a hash the store holds. It exits 1 while anything it
// found is left unwritten or disagrees.
func (c *commands) rebuild(ctx context.Context, o rebuildOptions) (rebuildReport, int, error) {
	rep := rebuildReport{Applied: o.apply, RequestID: "rebuild-" + uuid.NewString(), Counts: map[string]int{}}
	if err := c.resolveRefs(ctx, &o); err != nil {
		return rep, exitError, err
	}
	if o.writes() {
		release, ok, err := db.ExclusiveFence(ctx, c.db)
		if err != nil {
			return rep, exitError, err
		}
		if !ok {
			return rep, cli.ExitConflict, errors.New("refusing to write: an instance in normal mode, or another rebuild, holds " +
				"the fence; restart every instance with QUOTACORE_STARTUP_MODE=readonly first " +
				"(consistency-and-recovery.md section 4.1)")
		}
		defer release()
		select {
		case <-ctx.Done():
			return rep, exitError, ctx.Err()
		case <-time.After(c.grace):
		}
	}
	now := c.now()
	rep.At = now.UTC()
	losses, err := c.rec.Losses(ctx, now, sessionStale)
	if err != nil {
		return rep, exitError, err
	}
	if !o.restoredAt.IsZero() {
		// Everything after the instant Postgres was restored to is gone from
		// the ledger, for every tenant.
		losses = append(losses, reconcile.Loss{Reason: "ledger_restored", From: o.restoredAt, To: now})
	}
	rejected, err := c.rec.Rejected(ctx)
	if err != nil {
		return rep, exitError, err
	}
	var horizon time.Time
	if c.retention > 0 {
		horizon = now.Add(-c.retention)
	}
	ids, err := c.rec.Tenants(ctx)
	if err != nil {
		return rep, exitError, err
	}
	asked := map[reconcile.Subject]bool{}
	for _, s := range o.accept {
		asked[s] = true
	}
	for s := range o.set {
		asked[s] = true
	}
	for _, id := range ids {
		entries, err := c.rebuildTenant(ctx, id, now, horizon, losses, rejected, o, rep.RequestID)
		if err != nil {
			return rep, exitError, err
		}
		for _, e := range entries {
			delete(asked, e.Subject)
		}
		rep.Entries = append(rep.Entries, entries...)
	}
	if len(asked) > 0 {
		var names []string
		for s := range asked {
			names = append(names, s.TenantID+":"+s.Feature)
		}
		sort.Strings(names)
		return rep, cli.ExitNotFound, fmt.Errorf("no tenant holds %s", strings.Join(names, ", "))
	}
	code := exitClean
	for _, e := range rep.Entries {
		rep.Counts[e.Class]++
		if e.Action != "" {
			rep.Counts[e.Action]++
		}
		switch {
		case e.Error != "":
			code = exitFinding
		case e.Class == reconcile.ClassPresent && len(e.Findings) > 0:
			code = exitFinding
		case e.Class != reconcile.ClassPresent && e.Written == nil:
			code = exitFinding
		}
	}
	if rep.Entries == nil {
		rep.Entries = []rebuildEntry{}
	}
	return rep, code, nil
}

// resolveRefs turns the operator's <tenant>:<feature> references into
// subjects, accepting an id or an external id for the tenant.
func (c *commands) resolveRefs(ctx context.Context, o *rebuildOptions) error {
	sub := func(ref string) (reconcile.Subject, error) {
		t, f, _ := strings.Cut(ref, ":")
		id, err := c.tenantID(ctx, t)
		if err != nil {
			return reconcile.Subject{}, fmt.Errorf("%s: %w", ref, err)
		}
		return reconcile.Subject{TenantID: id, Feature: f}, nil
	}
	o.accept = nil
	for _, ref := range o.acceptRefs {
		s, err := sub(ref)
		if err != nil {
			return err
		}
		o.accept = append(o.accept, s)
	}
	o.set = map[reconcile.Subject]int64{}
	for ref, n := range o.setRefs {
		s, err := sub(ref)
		if err != nil {
			return err
		}
		o.set[s] = n
	}
	return nil
}

func (c *commands) rebuildTenant(ctx context.Context, id string, now, horizon time.Time, losses []reconcile.Loss,
	rejected map[reconcile.Subject]int, o rebuildOptions, requestID string) ([]rebuildEntry, error) {
	lt, err := c.loader.Load(ctx, id)
	if errors.Is(err, snapshot.ErrTenantUnknown) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := snapshot.NewTenant(lt)
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return nil, fmt.Errorf("tenant %s: load zone %q: %w", id, t.Timezone, err)
	}
	planID := ""
	if lt.Plan != nil {
		planID = lt.Plan.ID
	}
	changed, err := c.rec.ConfigChanged(ctx, id, planID)
	if err != nil {
		return nil, err
	}
	held := t.Features(now)
	features := make([]string, 0, len(held))
	for f, e := range held {
		// A boolean or rolling entitlement has no balance hash to rebuild
		// (DR-050, DR-052).
		if e.Anchored() {
			features = append(features, f)
		}
	}
	sort.Strings(features)
	var out []rebuildEntry
	var written []db.Rebuilt
	for _, f := range features {
		e := held[f]
		sub := reconcile.Subject{TenantID: id, Feature: f}
		rows, _, err := c.rec.Rows(ctx, sub, time.Time{})
		if err != nil {
			return nil, err
		}
		hash, err := balances{c.data}.Hash(ctx, sub)
		if err != nil {
			return nil, err
		}
		r := reconcile.Classify(reconcile.Target{Subject: sub, ExternalID: t.ExternalID, Anchor: t.Anchor,
			Interval: e.Interval, Location: loc, Limit: e.Limit, Now: now, Horizon: horizon, ConfigChanged: changed},
			reconcile.Evidence{Rows: rows, Hash: hash, Losses: losses, Rejected: rejected[sub]})
		entry := rebuildEntry{Rebuilt: r}
		state, action, err := c.decide(r, o)
		if err != nil {
			entry.Error = err.Error()
		}
		if state != nil {
			err := c.scripts.Rebuild(ctx, script.Restore{TenantID: id, FeatureKey: f, Window: toWindow(state.Window),
				Balance: state.Balance, Limit: state.Limit, Bonus: state.Bonus})
			switch {
			case errors.Is(err, script.ErrExists):
				// Something wrote the hash since it was read: it is left as
				// it stands, as a present hash always is.
				entry.Error = "the hash appeared while the rebuild ran and was left as it stands"
			case err != nil:
				return nil, fmt.Errorf("%s:%s: %w", id, f, err)
			default:
				entry.Action, entry.Written = action, state
				written = append(written, db.Rebuilt{Feature: f, Class: r.Class, Action: action, State: state,
					Estimate: r.Estimate})
			}
		}
		if o.apply && r.Class == reconcile.ClassUnreconcilable {
			if _, err := c.rec.Record(ctx, reconcile.Finding{Subject: sub, Kind: reconcile.KindUnreconcilable,
				Detail: r.Reason}, "rebuild"); err != nil {
				return nil, err
			}
		}
		out = append(out, entry)
	}
	if len(written) > 0 {
		if err := c.rec.RecordRebuild(ctx, c.host, requestID, id, written, o.reason, now); err != nil {
			return nil, fmt.Errorf("tenant %s: the hashes are written and their audit did not commit: %w", id, err)
		}
	}
	return out, nil
}

// decide is what the run writes for one tenant-feature: an exact hash under
// --apply, the estimate under --accept, the operator's value under --set, and
// never a hash the store holds.
func (c *commands) decide(r reconcile.Rebuilt, o rebuildOptions) (*reconcile.State, string, error) {
	n, set := o.set[r.Subject]
	accept := false
	for _, s := range o.accept {
		accept = accept || s == r.Subject
	}
	switch {
	case (set || accept) && r.Class == reconcile.ClassPresent:
		return nil, "", errors.New("the store holds this hash, and a rebuild never rewrites one")
	case set:
		if r.Estimate == nil {
			return nil, "", errors.New("the ledger names no window to set a balance in")
		}
		st := reconcile.State{Balance: n, Limit: r.Estimate.Limit, Bonus: r.Estimate.Bonus, Window: r.Estimate.Window}
		if n > st.Limit+st.Bonus {
			return nil, "", fmt.Errorf("%d is above the cycle's limit and bonus, %d (DR-019)", n, st.Limit+st.Bonus)
		}
		return &st, "set", nil
	case accept:
		if r.Estimate == nil {
			return nil, "", errors.New("the ledger offers no estimate to accept")
		}
		st := *r.Estimate
		return &st, "accepted", nil
	case o.apply && r.Class == reconcile.ClassExact:
		st := *r.Estimate
		return &st, "written", nil
	}
	return nil, "", nil
}

func toWindow(w reconcile.Window) cycle.Window {
	return cycle.Window{Index: w.Index, Start: w.Start, End: w.End}
}

// configBump is consistency-and-recovery.md section 4.2, step 4: after a
// Postgres restore, every snapshot is made to re-read the restored
// configuration now rather than within a refresh interval. It records a new
// configuration version against every tenant and publishes the invalidation,
// the propagation step a committed admin write takes, for all of them at once.
func (c *commands) configBump(ctx context.Context) (int, error) {
	ids, err := db.TenantIDs(ctx, c.db)
	if err != nil {
		return exitError, err
	}
	version, err := c.data.Publish(ctx, ids)
	if err != nil {
		return exitError, err
	}
	return exitClean, c.print(map[string]any{"config_version": version, "tenants": len(ids)}, "")
}

// auditNote is section 4.2, step 6: the truncation a restore leaves in the
// history, written to the audit log as a system note with the operator's
// words, so the gap is documented rather than discovered later.
func (c *commands) auditNote(ctx context.Context, args []string) (int, error) {
	f := flag.NewFlagSet("audit note", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	reason := f.String("reason", "", "what the note records, such as the backup's time and the restore's")
	if err := f.Parse(args); err != nil || f.NArg() != 0 || strings.TrimSpace(*reason) == "" {
		return exitUsage, errUsage{errors.New(`usage: quotacore audit note --reason "<what the note records>"`)}
	}
	n, err := db.WriteNote(ctx, c.db, "cli:"+c.host, "cli-"+uuid.NewString(), *reason)
	if err != nil {
		return exitError, err
	}
	return exitClean, c.print(n, "")
}
