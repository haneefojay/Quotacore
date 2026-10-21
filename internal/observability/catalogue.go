package observability

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

// The catalogue is observability.md section 1, and the tests read it from the
// document rather than from a list here, so a metric removed from the code or
// added to it without the document fails the build (IP-13 D1). These are the
// parser, exported so the integration suite in cmd/quotacore reads the
// document the same way.

var (
	backticked   = regexp.MustCompile("`([^`]+)`")
	proseMetric  = regexp.MustCompile("`(quotacore_[a-z0-9_]+)(?:\\{([a-z_,]+)\\})?`")
	tableMetric  = regexp.MustCompile("^\\| `(quotacore_[a-z0-9_]+)` \\|")
	sectionStart = "\n## 1. Metrics"
	sectionEnd   = "\n## 2. "
)

// DocumentedMetrics returns every metric observability.md section 1 names,
// with its label names sorted. A table row is read from its Labels column; the
// section 1.6 prose from the `{labels}` beside each name. Dated notes (lines
// that begin with `>`) are never read, because they discuss metrics rather
// than define them.
func DocumentedMetrics(doc []byte) (map[string][]string, error) {
	text := string(doc)
	from := strings.Index(text, sectionStart)
	if from < 0 {
		return nil, errors.New("observability.md has no section 1. Metrics")
	}
	to := strings.Index(text[from:], sectionEnd)
	if to < 0 {
		return nil, errors.New("observability.md has no section 2 after section 1")
	}
	out := map[string][]string{}
	labelsAt := -1
	for _, line := range strings.Split(text[from:from+to], "\n") {
		if strings.HasPrefix(line, ">") {
			continue
		}
		if strings.HasPrefix(line, "| Metric |") {
			labelsAt = -1
			for i, h := range cells(line) {
				if h == "Labels" {
					labelsAt = i
				}
			}
			continue
		}
		if m := tableMetric.FindStringSubmatch(line); m != nil {
			var labels []string
			if c := cells(line); labelsAt >= 0 && labelsAt < len(c) {
				labels = labelNames(c[labelsAt])
			}
			out[m[1]] = labels
			continue
		}
		if strings.HasPrefix(line, "|") {
			continue
		}
		for _, m := range proseMetric.FindAllStringSubmatch(line, -1) {
			var labels []string
			if m[2] != "" {
				labels = strings.Split(m[2], ",")
				sort.Strings(labels)
			}
			out[m[1]] = labels
		}
	}
	if len(out) == 0 {
		return nil, errors.New("observability.md section 1 names no metric")
	}
	return out, nil
}

func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	// A `\|` inside a cell is a value separator, not a column.
	parts := strings.Split(strings.ReplaceAll(row, `\|`, "\x00"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(parts[i], "\x00", `\|`))
	}
	return parts
}

// labelNames reads a Labels cell such as "`outcome` = `allowed` \| `denied`",
// "histogram, `endpoint`, `outcome`" or "—": a backticked word is a label name
// unless it follows "=" or "|", which makes it a value.
func labelNames(cell string) []string {
	var names []string
	seen := map[string]bool{}
	for _, idx := range backticked.FindAllStringSubmatchIndex(cell, -1) {
		before := strings.TrimRight(cell[:idx[0]], " ")
		if strings.HasSuffix(before, "=") || strings.HasSuffix(before, `\|`) {
			continue
		}
		name := cell[idx[2]:idx[3]]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
