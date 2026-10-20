package observability

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestTheLedgerCollectorsAreTheDocumentedOnes pins IP-12's share of
// observability.md section 1.5 to the registry, as TestSevenCollectorsAndNoMore
// does for section 1.4: these eight names and no others, with every closed
// label value present from the first scrape.
func TestTheLedgerCollectorsAreTheDocumentedOnes(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := NewLedger(func() float64 { return 3 }).Register(reg); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string][]string{}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				labels[f.GetName()] = append(labels[f.GetName()], l.GetValue())
			}
		}
	}
	sort.Strings(names)
	want := []string{
		"quotacore_event_dead_lettered_total",
		"quotacore_event_dead_letters",
		"quotacore_event_duplicate_detected_total",
		"quotacore_event_enqueued_total",
		"quotacore_event_queue_depth",
		"quotacore_event_sink_dropped_total",
		"quotacore_event_write_latency_seconds",
		"quotacore_event_written_total",
	}
	if len(names) != len(want) {
		t.Fatalf("metric families = %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("metric family %d = %q, want %q", i, names[i], want[i])
		}
	}
	for name, want := range map[string][]string{
		"quotacore_event_sink_dropped_total":  {DropError, DropQueueFull, DropShutdown},
		"quotacore_event_dead_lettered_total": {DeadDuplicate, DeadRejected},
		"quotacore_event_dead_letters":        {DeadDuplicate, DeadRejected},
	} {
		got := labels[name]
		sort.Strings(got)
		if len(got) != len(want) {
			t.Errorf("%s reasons = %v, want %v", name, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s reasons = %v, want %v", name, got, want)
			}
		}
	}
}
