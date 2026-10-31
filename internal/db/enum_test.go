package db

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/quotacore/quotacore/api"
)

// contractEnums pairs every enum the migrations create with the schema the
// wire contract states it as. The values come from the generated types, which
// are generated from api/openapi.yaml, so this is the contract and not a copy.
var contractEnums = map[string][]string{
	"tenant_state":      enumStrings(api.TenantState("").AllValues()),
	"archive_state":     enumStrings(api.ArchiveState("").AllValues()),
	"feature_kind":      enumStrings(api.FeatureKind("").AllValues()),
	"reset_interval":    enumStrings(api.ResetInterval("").AllValues()),
	"key_scope":         enumStrings(api.KeyScope("").AllValues()),
	"key_state":         enumStrings(api.KeyState("").AllValues()),
	"event_source":      enumStrings(api.EventSource("").AllValues()),
	"event_type":        enumStrings(api.EventType("").AllValues()),
	"override_apply_at": enumStrings(api.OverrideApplyAt("").AllValues()),
}

func enumStrings[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	a, b = append([]string{}, a...), append([]string{}, b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, ",") == strings.Join(b, ",")
}

var createType = regexp.MustCompile(`(?s)CREATE TYPE\s+(\w+)\s+AS ENUM\s*\(([^)]*)\)`)
var quoted = regexp.MustCompile(`'([a-z0-9_]+)'`)

// addValue is a later migration extending an enum, as 00014 extends
// feature_kind with rolling (IP-19).
var addValue = regexp.MustCompile(`ALTER TYPE\s+(\w+)\s+ADD VALUE\s+(?:IF NOT EXISTS\s+)?'([a-z0-9_]+)'`)

// migrationEnums reads every enum out of the embedded migration text.
func migrationEnums(t *testing.T) map[string][]string {
	t.Helper()
	files, err := versionedFiles(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, name := range files {
		body, err := migrationsFS.ReadFile(migrationDir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createType.FindAllStringSubmatch(string(body), -1) {
			for _, v := range quoted.FindAllStringSubmatch(m[2], -1) {
				out[m[1]] = append(out[m[1]], v[1])
			}
		}
		for _, m := range addValue.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = append(out[m[1]], m[2])
		}
	}
	return out
}

// TestEveryMigrationEnumHasAContractPair fails when a migration creates an
// enum the contract does not state, or the pairing names one that no
// migration creates, so a new enum cannot arrive unpaired.
func TestEveryMigrationEnumHasAContractPair(t *testing.T) {
	got := migrationEnums(t)
	for name, values := range got {
		want, ok := contractEnums[name]
		if !ok {
			t.Errorf("migrations create enum %s, which has no contract pairing", name)
			continue
		}
		if !sameSet(values, want) {
			t.Errorf("enum %s: migrations say %v, the contract says %v", name, values, want)
		}
	}
	for name := range contractEnums {
		if _, ok := got[name]; !ok {
			t.Errorf("the pairing names %s, which no migration creates", name)
		}
	}
}

// TestEveryEnumMatchesTheContract is DoD 8 against the database itself: every
// label the migrated schema accepts is a value the API accepts, and the
// reverse, for every enum and for the one CHECK-backed set, audit_log's
// subject_type. api-conventions.md section 9's two filter lists are held to
// the same values.
func TestEveryEnumMatchesTheContract(t *testing.T) {
	db := migratedDatabase(t)
	rows, err := db.QueryContext(context.Background(), `SELECT t.typname, e.enumlabel
		FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public' ORDER BY t.typname, e.enumsortorder`)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string][]string{}
	for rows.Next() {
		var typ, label string
		if err := rows.Scan(&typ, &label); err != nil {
			t.Fatal(err)
		}
		labels[typ] = append(labels[typ], label)
	}
	rows.Close()
	if len(labels) != len(contractEnums) {
		t.Errorf("the database has %d enums, the contract pairing has %d", len(labels), len(contractEnums))
	}
	for name, want := range contractEnums {
		if !sameSet(labels[name], want) {
			t.Errorf("enum %s: the database accepts %v, the API accepts %v", name, labels[name], want)
		}
	}

	var def string
	if err := db.QueryRowContext(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'audit_log_subject_type'`).Scan(&def); err != nil {
		t.Fatalf("audit_log_subject_type: %v", err)
	}
	var subjects []string
	for _, m := range quoted.FindAllStringSubmatch(def, -1) {
		subjects = append(subjects, m[1])
	}
	if want := enumStrings(api.AuditEntrySubjectType("").AllValues()); !sameSet(subjects, want) {
		t.Errorf("audit_log.subject_type accepts %v, the API documents %v", subjects, want)
	}

	conv := repoFile(t, "docs/architecture/api-conventions.md")
	for filter, enum := range map[string]string{"event_type": "event_type", "source": "event_source"} {
		re := regexp.MustCompile("`" + filter + "` is one of ([^.]*)\\.")
		m := re.FindStringSubmatch(conv)
		if m == nil {
			t.Errorf("api-conventions.md no longer states the %s filter values", filter)
			continue
		}
		var vals []string
		for _, v := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(m[1], -1) {
			vals = append(vals, v[1])
		}
		if !sameSet(vals, labels[enum]) {
			t.Errorf("the %s filter accepts %v, the database %s holds %v", filter, vals, enum, labels[enum])
		}
	}
}
