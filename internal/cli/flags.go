package cli

import (
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

// usageError is a mistake in what the operator typed, exit 2. It is never
// built from a credential, so its text is safe to print.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

// flags is one command's flag set: the standard library's, plus the names a
// command requires and the help text each flag prints.
type flags struct {
	fs       *flag.FlagSet
	required []string
	set      map[string]bool
}

func newFlags(name string) *flags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flags{fs: fs}
}

func (f *flags) str(name, usage string) *string { return f.fs.String(name, "", usage) }

func (f *flags) need(names ...string) { f.required = append(f.required, names...) }

func (f *flags) has(name string) bool { return f.set[name] }

// list is a repeatable flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func (f *flags) list(name, usage string) *list {
	l := &list{}
	f.fs.Var(l, name, usage)
	return l
}

// parse reads flags and positional arguments in any order, since an operator
// types `quotacore grant acme --amount 5` as readily as the other way round.
func (f *flags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		rest := f.fs.Args()
		if len(rest) == 0 {
			break
		}
		if rest[0] == "--" {
			pos = append(pos, rest[1:]...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	f.set = map[string]bool{}
	f.fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
	return pos, nil
}

func (f *flags) missing() []string {
	var out []string
	for _, n := range f.required {
		if !f.set[n] {
			out = append(out, "--"+n)
		}
	}
	return out
}

func parseInt(name, v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, usagef("--%s %q is not a whole number", name, v)
	}
	return n, nil
}

func parseTime(name, v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, usagef("--%s %q is not an RFC 3339 instant with an offset, such as 2026-10-01T00:00:00Z", name, v)
	}
	return t, nil
}

// query copies the set flags onto the query parameters they name.
func (f *flags) query(q url.Values, names map[string]*string) {
	for flagName, p := range names {
		if f.set[flagName] {
			q.Set(queryName(flagName), *p)
		}
	}
}

// queryName is the contract's parameter for a flag.
func queryName(flagName string) string {
	switch flagName {
	case "feature":
		return "feature_key"
	case "type":
		return "event_type"
	case "plan":
		return "plan_key"
	case "tenant":
		return "tenant_external_id"
	}
	return strings.ReplaceAll(flagName, "-", "_")
}

// entitlement parses `<feature_key>=<limit>/<interval>` for a metered
// feature, `<feature_key>=<limit>/<window>` for a rolling one, such as
// api.calls=1000/rolling_1h (DR-052), or a bare `<feature_key>` for a boolean
// one, whose limit and interval are null (DR-050).
func entitlement(v string) (contract.PlanEntitlement, error) {
	if !strings.ContainsAny(v, "=/") && v != "" {
		return contract.PlanEntitlement{FeatureKey: contract.FeatureKey(v), Limit: contract.NilLimit{Null: true},
			ResetInterval: contract.NilResetInterval{Null: true}}, nil
	}
	key, rest, ok := strings.Cut(v, "=")
	limit, interval, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || key == "" {
		return contract.PlanEntitlement{}, usagef("--entitlement %q is not <feature_key>=<limit>/<interval>, such as llm.tokens.output=100000/monthly, "+
			"nor a bare <feature_key> for a boolean feature", v)
	}
	n, err := strconv.ParseInt(limit, 10, 64)
	if err != nil {
		return contract.PlanEntitlement{}, usagef("--entitlement %q: the limit is not a whole number", v)
	}
	if strings.HasPrefix(interval, "rolling_") {
		// A rolling feature's window in the interval's place, such as
		// api.calls=1000/rolling_1h: a limit over a sliding window and no
		// reset interval (DR-052).
		return contract.PlanEntitlement{FeatureKey: contract.FeatureKey(key), Limit: contract.NewNilLimit(contract.Limit(n)),
			ResetInterval: contract.NilResetInterval{Null: true},
			Window:        contract.NewOptNilRollingWindow(contract.RollingWindow(interval))}, nil
	}
	return contract.PlanEntitlement{FeatureKey: contract.FeatureKey(key), Limit: contract.NewNilLimit(contract.Limit(n)),
		ResetInterval: contract.NewNilResetInterval(contract.ResetInterval(interval))}, nil
}

func entitlements(vs []string) ([]contract.PlanEntitlement, error) {
	out := make([]contract.PlanEntitlement, 0, len(vs))
	for _, v := range vs {
		e, err := entitlement(v)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
