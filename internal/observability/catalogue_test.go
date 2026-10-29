package observability

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	descName   = regexp.MustCompile(`fqName: "([^"]+)"`)
	descConst  = regexp.MustCompile(`constLabels: \{([^}]*)\}`)
	descVar    = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
	constLabel = regexp.MustCompile(`([a-z_]+)=`)
)

// described is every metric a collector set declares, with its label names
// sorted, read from the descriptors rather than a scrape, so a vector no
// request has touched yet is still seen.
func described(t *testing.T, sets ...Set) map[string][]string {
	t.Helper()
	if _, err := NewRegistry(sets...); err != nil {
		t.Fatalf("one registry for every set: %v", err)
	}
	var d describer
	for _, s := range sets {
		if err := s.Register(&d); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string][]string{}
	for _, desc := range d.descs {
		str := desc.String()
		name := descName.FindStringSubmatch(str)
		if name == nil {
			t.Fatalf("a descriptor with no name: %s", str)
		}
		var labels []string
		if m := descConst.FindStringSubmatch(str); m != nil {
			for _, l := range constLabel.FindAllStringSubmatch(m[1], -1) {
				labels = append(labels, l[1])
			}
		}
		if m := descVar.FindStringSubmatch(str); m != nil && strings.TrimSpace(m[1]) != "" {
			for _, l := range strings.Split(m[1], ",") {
				labels = append(labels, strings.TrimSpace(l))
			}
		}
		sort.Strings(labels)
		out[name[1]] = labels
	}
	return out
}

// describer is a Registerer that keeps every descriptor it is handed.
type describer struct{ descs []*prometheus.Desc }

func (d *describer) Register(c prometheus.Collector) error {
	ch := make(chan *prometheus.Desc)
	go func() { c.Describe(ch); close(ch) }()
	for desc := range ch {
		d.descs = append(d.descs, desc)
	}
	return nil
}

func (d *describer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		_ = d.Register(c)
	}
}

func (d *describer) Unregister(prometheus.Collector) bool { return false }

// Every set the process registers, built as assemble builds them.
func everySet() []Set {
	return []Set{
		New(Options{HitRatio: func() float64 { return 0 }}),
		NewDataPlane([][2]string{{"internal_error", "POST /v1/consume"}}),
		NewLedger(func() float64 { return 0 }),
		NewReconcile(),
		NewDependencies(),
		NewCycles(),
		NewHTTP(),
		NewProcess(Build{Version: "v", Commit: "c", ZoneDataVersion: "z"}, time.Now()),
	}
}

// TestEveryDocumentedMetricIsRegistered (IP-13 DoD 1, D1): the registry the
// process serves holds exactly the metrics observability.md section 1 names,
// with exactly its labels, both ways. Removing a metric from the code, adding
// one the document does not name, or changing a label fails here.
func TestEveryDocumentedMetricIsRegistered(t *testing.T) {
	doc, err := os.ReadFile("../../docs/architecture/observability.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := DocumentedMetrics(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := described(t, everySet()...)
	for name, labels := range want {
		have, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s is in observability.md section 1 and no collector registers it", name)
		case !slices.Equal(have, labels):
			t.Errorf("%s has labels %v, and observability.md says %v", name, have, labels)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is registered and observability.md section 1 does not name it", name)
		}
	}
	if len(want) < 40 {
		t.Errorf("the parser read %d metrics from section 1, fewer than the catalogue holds; it is misreading the document", len(want))
	}
}

// TestTheCatalogueParserReadsLabelsAndValues pins the two forms a Labels cell
// takes and the section 1.6 prose form.
func TestTheCatalogueParserReadsLabelsAndValues(t *testing.T) {
	doc := []byte("x\n## 1. Metrics\n\n| Metric | Type | Labels | Meaning |\n| --- | --- | --- | --- |\n" +
		"| `quotacore_a_total` | counter | `outcome` = `allowed` \\| `denied` | a |\n" +
		"| `quotacore_b_seconds` | histogram | histogram, `endpoint` | b |\n" +
		"| `quotacore_c` | gauge | — | c |\n\n" +
		"> A note naming `quotacore_d_total{x}` is not read.\n\n" +
		"Prose names `quotacore_e_total{status,method}` and `quotacore_a_total`.\n" +
		"\n## 2. Logs\n`quotacore_f_total` is outside section 1.\n")
	got, err := DocumentedMetrics(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"quotacore_a_total":   {"outcome"},
		"quotacore_b_seconds": {"endpoint"},
		"quotacore_c":         nil,
		"quotacore_e_total":   {"method", "status"},
	}
	if len(got) != len(want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			t.Errorf("%s: labels %v, want %v", k, got[k], v)
		}
	}
}

// TestTheBuildAndProcessSeries (IP-13 DoD 1): build_info is 1 with the
// build's labels, and the process series read the runtime.
func TestTheBuildAndProcessSeries(t *testing.T) {
	reg, err := NewRegistry(NewProcess(Build{Version: "v9", Commit: "abc", ZoneDataVersion: "go1"}, time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
		m := f.GetMetric()[0]
		switch f.GetName() {
		case "quotacore_build_info":
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if m.GetGauge().GetValue() != 1 || labels["version"] != "v9" || labels["commit"] != "abc" || labels["zone_data_version"] != "go1" {
				t.Errorf("build_info = %v %v", m.GetGauge().GetValue(), labels)
			}
		case "quotacore_goroutines", "quotacore_memory_bytes":
			if m.GetGauge().GetValue() <= 0 {
				t.Errorf("%s = %v", f.GetName(), m.GetGauge().GetValue())
			}
		case "quotacore_uptime_seconds":
			if v := m.GetGauge().GetValue(); v < 59 {
				t.Errorf("uptime_seconds = %v, want about 60", v)
			}
		case "quotacore_gc_pause_seconds":
			if len(m.GetHistogram().GetBucket()) != len(GCPauseBuckets) {
				t.Errorf("gc_pause_seconds has %d buckets", len(m.GetHistogram().GetBucket()))
			}
		}
	}
	if len(seen) != 5 {
		t.Errorf("the process set registered %v, want five families", seen)
	}
}
