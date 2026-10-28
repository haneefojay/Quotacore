package reference_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// These tests read files and need no Docker, so they run in every `make test`.

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// section returns the text from a heading that starts with prefix to the
// next heading of the same or a higher level.
func section(t *testing.T, doc, prefix string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		level := len(l) - len(strings.TrimLeft(l, "#"))
		if level == 0 || !strings.HasPrefix(strings.TrimSpace(l[level:]), prefix) {
			continue
		}
		var out []string
		for _, m := range lines[i+1:] {
			if n := len(m) - len(strings.TrimLeft(m, "#")); n > 0 && n <= level && strings.HasPrefix(m[n:], " ") {
				break
			}
			out = append(out, m)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("no heading %q", prefix)
	return ""
}

var (
	composeVar   = regexp.MustCompile(`\$\{(QUOTACORE_[A-Z0-9_]+)`)
	composeEnv   = regexp.MustCompile(`(?m)^\s+(QUOTACORE_[A-Z0-9_]+):`)
	documentedAs = regexp.MustCompile("`(QUOTACORE_[A-Z0-9_]+)`")
	tableRowVar  = regexp.MustCompile("(?m)^\\| `(QUOTACORE_[A-Z0-9_]+)`")
)

func set(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// TestEveryComposeVariableIsDocumented (IP-15 D19, NFR-OPS2): every variable
// the reference Compose file reads is in deployment.md section 4, the ones the
// service reads in sections 4.1 to 4.6 and the ones only Compose reads in
// section 4.7; and every row of section 4.7 is one the file reads.
func TestEveryComposeVariableIsDocumented(t *testing.T) {
	compose := read(t, "docker-compose.yml")
	deployment := read(t, "docs/architecture/deployment.md")
	config := section(t, deployment, "4. Configuration")
	reference := section(t, config, "4.7")
	service := strings.Replace(config, reference, "", 1)

	served := set(documentedAs, service)
	composeOnly := set(tableRowVar, reference)
	// The service's own environment is the quotacore service's block; the
	// postgres service passes a QUOTACORE_ variable of its own to its init
	// script, which is Compose's and belongs in section 4.7.
	svc := compose[strings.Index(compose, "\n  quotacore:\n"):]
	if i := strings.Index(svc[1:], "\n  postgres:\n"); i > 0 {
		svc = svc[:i+1]
	}
	for _, v := range set(composeEnv, svc) {
		if !slices.Contains(served, v) {
			t.Errorf("docker-compose.yml passes %s to the service, and deployment.md sections 4.1 to 4.6 do not document it", v)
		}
	}
	read := set(composeVar, compose)
	for _, v := range read {
		if !slices.Contains(served, v) && !slices.Contains(composeOnly, v) {
			t.Errorf("docker-compose.yml reads %s, and deployment.md section 4 does not document it", v)
		}
	}
	for _, v := range composeOnly {
		if !slices.Contains(read, v) {
			t.Errorf("deployment.md section 4.7 documents %s, which docker-compose.yml does not read", v)
		}
	}
}

var fencedBash = regexp.MustCompile("(?s)```bash\n(.*?)```")

// TestTheQuickstartIsTheDeploymentGuide (IP-15 DoD 1, D16): the README's
// quickstart is deployment.md section 2's, character for character, so the
// block CI runs is the block both documents show.
func TestTheQuickstartIsTheDeploymentGuide(t *testing.T) {
	readme := fencedBash.FindStringSubmatch(section(t, read(t, "README.md"), "Quickstart"))
	guide := fencedBash.FindStringSubmatch(section(t, read(t, "docs/architecture/deployment.md"), "2. The quickstart"))
	if readme == nil || guide == nil {
		t.Fatal("the README's Quickstart section or deployment.md section 2 has no bash block")
	}
	if readme[1] != guide[1] {
		t.Fatalf("the README's quickstart differs from deployment.md section 2's:\nREADME:\n%s\ndeployment.md:\n%s", readme[1], guide[1])
	}
	for _, line := range []string{"docker compose up -d --wait", "docker compose exec quotacore quotacore init"} {
		if !strings.Contains(readme[1], line) {
			t.Errorf("the quickstart has no %q", line)
		}
	}
}

// TestDeploymentSizingCitesTheMeasurement (IP-15 DoD 6, NFR-T9): deployment.md
// section 5's data-store floor is the figure the committed measurement
// reports, and links it.
func TestDeploymentSizingCitesTheMeasurement(t *testing.T) {
	report := read(t, "docs/research/ip-15-idempotency-footprint.md")
	floor := regexp.MustCompile(`\| \*\*Floor\*\* \| [0-9]+ GiB \| \*\*([0-9]+) GiB\*\* \|`).FindStringSubmatch(report)
	if floor == nil {
		t.Fatal("the report states no floor")
	}
	sizing := section(t, read(t, "docs/architecture/deployment.md"), "5. Sizing")
	if !strings.Contains(sizing, "floor is **"+floor[1]+" GiB**") {
		t.Errorf("deployment.md section 5 does not state the measured floor of %s GiB", floor[1])
	}
	if !strings.Contains(sizing, "(../research/ip-15-idempotency-footprint.md)") {
		t.Error("deployment.md section 5 does not link the measurement")
	}
	if !strings.Contains(report, "valkey 8.1") {
		t.Error("the report does not name the engine it measured")
	}
}
