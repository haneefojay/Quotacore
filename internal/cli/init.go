package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	contract "github.com/quotacore/quotacore/api"
)

// The values deployment.md section 2 shows, which init offers as defaults.
var initDefaults = []struct{ flag, prompt, value string }{
	{"feature", "feature key", "llm.tokens.output"},
	{"unit", "unit", "tokens"},
	{"plan", "plan key", "starter"},
	{"limit", "limit per cycle", "100000"},
	{"interval", "reset interval", "monthly"},
	{"tenant", "tenant external id", "local-dev"},
	{"timezone", "time zone", "UTC"},
	{"key-name", "runtime key name", "local-dev-runtime"},
}

type initValues struct {
	values    map[string]*string
	url       *string
	yes, json *bool
}

func initFlags(c Command) (*flags, initValues) {
	f := newFlags(c.Name)
	v := initValues{values: map[string]*string{}}
	v.url = f.str("url", "the service's base URL; QUOTACORE_URL by default")
	for _, d := range initDefaults {
		v.values[d.flag] = f.fs.String(d.flag, d.value, "the "+d.prompt)
	}
	v.yes = f.fs.Bool("yes", false, "ask nothing; use the flags and the defaults")
	v.json = f.fs.Bool("json", false, "print the five answers as one JSON object instead of the summary")
	return f, v
}

// init makes the five calls of the quickstart in order (IP-14 D3). It asks
// for each value only when stdin is a terminal and --yes is absent, stops at
// the first failure with that failure's exit code, and never deletes what it
// already made.
func (o Options) init(c Command, args []string) int {
	f, v := initFlags(c)
	values, urlFlag, yes, asJSON := v.values, v.url, v.yes, v.json
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
	if o.Terminal && !*yes {
		in := bufio.NewReader(o.Stdin)
		for _, d := range initDefaults {
			if f.has(d.flag) {
				continue
			}
			fmt.Fprintf(o.Stderr, "%s [%s]: ", d.prompt, *values[d.flag])
			line, err := in.ReadString('\n')
			if v := strings.TrimSpace(line); v != "" {
				*values[d.flag] = v
			}
			if err != nil {
				fmt.Fprintln(o.Stderr)
				break
			}
		}
	}
	limit, err := strconv.ParseInt(*values["limit"], 10, 64)
	if err != nil {
		return o.usage(c, usagef("--limit %q is not a whole number", *values["limit"]))
	}
	feature, plan, tenant := *values["feature"], *values["plan"], *values["tenant"]
	steps := []struct {
		what, op string
		body     body
	}{
		{"feature " + feature, "createFeature", &contract.FeatureCreate{FeatureKey: contract.FeatureKey(feature), Name: feature,
			Unit: contract.Unit(*values["unit"])}},
		{"plan " + plan, "createPlan", &contract.PlanCreate{PlanKey: contract.PlanKey(plan), Name: plan,
			Entitlements: []contract.PlanEntitlement{{FeatureKey: contract.FeatureKey(feature), Limit: contract.NewNilLimit(contract.Limit(limit)),
				ResetInterval: contract.NewNilResetInterval(contract.ResetInterval(*values["interval"]))}}}},
		{"tenant " + tenant, "createTenant", &contract.TenantCreate{ExternalID: contract.ExternalId(tenant), DisplayName: tenant,
			PlanKey: contract.PlanKey(plan), Timezone: contract.NewOptString(*values["timezone"])}},
		{"runtime key " + *values["key-name"], "createKey", &contract.KeyCreate{Name: *values["key-name"], Scope: contract.KeyScopeRuntime}},
	}
	for _, s := range steps {
		if err := s.body.Validate(); err != nil {
			fmt.Fprintf(o.Stderr, "error: the %s is not one the contract accepts, so nothing was sent: %v\n", s.what, err)
			return ExitClient
		}
	}
	cl, err := o.client(*urlFlag)
	if err != nil {
		return o.usage(c, err)
	}
	answers := map[string]json.RawMessage{}
	var made []string
	for _, s := range steps {
		path, _ := pathOf(s.op, nil)
		r := cl.call(s.op, path, nil, s.body, cl.key)
		if r.err != nil || r.status < 200 || r.status > 299 {
			code := cl.finish(c, r)
			stopped(o.Stderr, made, s.what)
			return code
		}
		answers[strings.Fields(s.what)[0]] = r.body
		made = append(made, s.what)
	}
	var key struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(answers["runtime"], &key)
	answers["key"] = answers["runtime"]
	delete(answers, "runtime")

	// The first read, with the new runtime key: it proves the key works and
	// is the authoritative cycle window. A tenant just provisioned may take
	// a moment to be visible (DR-031), so a not-found is read again briefly.
	q := url.Values{"tenant_external_id": {tenant}}
	path, _ := pathOf("balance", nil)
	var r result
	for attempt := 0; attempt < 20; attempt++ {
		r = cl.call("balance", path, q, nil, key.Key)
		if e, ok := parseError(r.body); r.err != nil || !ok || e.Error.Code != contract.ErrorCodeTenantNotFound {
			break
		}
		cl.sleep(250 * time.Millisecond)
	}
	if r.err != nil || r.status != 200 {
		code := cl.finish(c, r)
		stopped(o.Stderr, made, "the first balance read")
		// The key exists now and is lost unless it is shown here, once.
		_ = writeJSON(o.Stdout, answers["key"])
		fmt.Fprintln(o.Stderr, shownOnce)
		return code
	}
	answers["balance"] = r.body
	if *asJSON {
		raw, _ := json.Marshal(answers)
		if err := writeJSON(o.Stdout, raw); err != nil {
			return ExitInternal
		}
		fmt.Fprintln(o.Stderr, shownOnce)
		return ExitOK
	}
	summary(o.Stdout, o.Version, cl.base, values, limit, key.Key, r.body)
	fmt.Fprintln(o.Stderr, shownOnce)
	return ExitOK
}

func stopped(w io.Writer, made []string, at string) {
	if len(made) == 0 {
		fmt.Fprintf(w, "init stopped at the %s and created nothing\n", at)
		return
	}
	fmt.Fprintf(w, "init stopped at the %s; it had created the %s, which it leaves in place\n", at, strings.Join(made, ", the "))
}

// summary is deployment.md section 2's output. The plaintext appears on the
// key line and nowhere else; the try line names a variable for it (INV-K3).
func summary(w io.Writer, version string, base *url.URL, v map[string]*string, limit int64, key string, balance []byte) {
	if version == "" {
		version = "dev"
	}
	var b struct {
		Entries []struct {
			CycleStart *string `json:"cycle_start"`
			CycleEnd   *string `json:"cycle_end"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(balance, &b)
	cycle := "none: the entitlement never resets"
	if len(b.Entries) > 0 && b.Entries[0].CycleStart != nil && b.Entries[0].CycleEnd != nil {
		cycle = *b.Entries[0].CycleStart + " → " + *b.Entries[0].CycleEnd
	}
	pad := func(s string) string { return fmt.Sprintf("%-20s", s) }
	fmt.Fprintf(w, "quotacore %s\n", version)
	fmt.Fprintf(w, "  feature  %s (metered, unit: %s)\n", pad(*v["feature"]), *v["unit"])
	fmt.Fprintf(w, "  plan     %s (1 entitlement: %d %s, %s)\n", pad(*v["plan"]), limit, *v["unit"], *v["interval"])
	fmt.Fprintf(w, "  tenant   %s (plan: %s, timezone: %s)\n", pad(*v["tenant"]), *v["plan"], *v["timezone"])
	fmt.Fprintf(w, "  key      %s   ← shown once\n\n", key)
	fmt.Fprintf(w, "  cycle    %s\n", cycle)
	fmt.Fprintf(w, "  try      curl -s '%s/v1/balance?tenant_external_id=%s' \\\n             -H \"Authorization: Bearer $QC_RUNTIME_KEY\"\n",
		strings.TrimSuffix(base.String(), "/"), url.QueryEscape(*v["tenant"]))
}
