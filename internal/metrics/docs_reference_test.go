package metrics

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/admin"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/storage/s3"
	"github.com/gtriggiano/statica/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricsReference is the public metrics reference this test keeps complete.
var metricsReference = filepath.Join("..", "..", "docs", "reference", "metrics.md")

// family is the documented or exported shape of one statica_ metric family.
type family struct {
	typ     string
	labels  []string            // sorted
	buckets []float64           // histograms only, without +Inf
	values  map[string][]string // exported only: every value of each label
	text    string              // documented only: the family's section
}

// startupFamilies are the families whose every series exists from startup, so
// the test can require their section to name each exported label value.
var startupFamilies = []string{
	"statica_cache_admissions_total", "statica_cache_evictions_total", "statica_cache_expirations_total",
	"statica_cache_revalidations_total", "statica_cache_fill_duration_seconds", "statica_origin_health_checks_total",
}

// exportedFamilies builds the registry as serve does, with every
// slice-specific collector, and drives the observations needed for the
// families that only appear once a series exists. It fails if a registered
// family was not gathered, so a new family cannot escape the comparison.
func exportedFamilies(t *testing.T) map[string]family {
	t.Helper()
	cfg := parseConfig(t, "")
	m := newMetrics(t, cfg)
	c, err := cache.FromConfig(context.Background(), cfg, &fakeOrigin{}, func(o *cache.Options) { o.OnFill = m.OnFill })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterCache(c, cfg.Cache); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterReadiness(admin.NewReadiness(nil)); err != nil {
		t.Fatal(err)
	}
	out := telemetry.NewOutput(io.Discard, telemetry.OutputOptions{})
	t.Cleanup(func() { _ = out.Close(context.Background()) })
	if err := m.RegisterLogOutput(out); err != nil {
		t.Fatal(err)
	}
	m.RecordSink(with(admitted("a.example", "GET", 200, "incomplete"), func(r *telemetry.Record) {
		r.CacheOutcome, r.TerminationReason = "hit", "disconnect"
	}))
	m.RecordSink(&telemetry.Record{Rejected: true, Reason: "unknown_host"})
	o := m.OriginObserver()
	o.ObserveOperation(s3.OperationGet, storage.PurposeRequest, "", time.Millisecond)
	o.ObserveAttempt(s3.OperationGet, storage.PurposeRequest, "")
	o.ObserveBodyRead(storage.PurposeRequest, 1)

	got := map[string]family{}
	for _, f := range gather(t, m) {
		if !strings.HasPrefix(f.GetName(), namespace+"_") {
			continue
		}
		exported := family{values: map[string][]string{}}
		switch f.GetType() {
		case dto.MetricType_COUNTER:
			exported.typ = "counter"
		case dto.MetricType_GAUGE:
			exported.typ = "gauge"
		case dto.MetricType_HISTOGRAM:
			exported.typ = "histogram"
		default:
			exported.typ = strings.ToLower(f.GetType().String())
		}
		metric := f.GetMetric()[0]
		for _, l := range metric.GetLabel() {
			exported.labels = append(exported.labels, l.GetName())
		}
		slices.Sort(exported.labels)
		for _, b := range metric.GetHistogram().GetBucket() {
			exported.buckets = append(exported.buckets, b.GetUpperBound())
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if !slices.Contains(exported.values[l.GetName()], l.GetValue()) {
					exported.values[l.GetName()] = append(exported.values[l.GetName()], l.GetValue())
				}
			}
		}
		got[f.GetName()] = exported
	}

	descs := make(chan *prometheus.Desc)
	go func() {
		m.registry.Describe(descs)
		close(descs)
	}()
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	for d := range descs {
		match := fqName.FindStringSubmatch(d.String())
		if match == nil {
			t.Fatalf("cannot read the name of %s", d)
		}
		if name := match[1]; strings.HasPrefix(name, namespace+"_") {
			if _, ok := got[name]; !ok {
				t.Errorf("%s is registered but has no series here: add an observation to this test", name)
			}
		}
	}
	return got
}

var (
	familyHeading = regexp.MustCompile("^### `(" + namespace + "_[a-z0-9_]+)`$")
	summaryRow    = regexp.MustCompile("^\\| \\[`(" + namespace + "_[a-z0-9_]+)`\\]\\(#([a-z0-9-]+)\\) \\| ([a-z]+) \\| (.+) \\|$")
	codeSpan      = regexp.MustCompile("`([^`]+)`")
)

// labelList parses "none" or a comma-separated list of backticked names.
func labelList(t *testing.T, line int, name, text string) []string {
	t.Helper()
	var labels []string
	if text != "none" {
		for _, m := range codeSpan.FindAllStringSubmatch(text, -1) {
			labels = append(labels, m[1])
		}
		if len(labels) == 0 {
			t.Errorf("%s:%d: %s: labels must be \"none\" or backticked names", metricsReference, line, name)
		}
	}
	slices.Sort(labels)
	return labels
}

// documentedFamilies reads the metrics reference. Each family has one
// "### `statica_…`" heading followed by the fixed-format lines
// "**Type:** <type>", "**Labels:** none" or "**Labels:** `a`, `b`" and, for
// histograms, "**Buckets:** <upper bounds in seconds>"; its section runs to
// the next heading outside a code fence. The summary table at the top has one
// "| [`statica_…`](#anchor) | <type> | <labels> |" row per family; it is
// returned separately, with type and labels only.
func documentedFamilies(t *testing.T) (sections, summary map[string]family) {
	t.Helper()
	f, err := os.Open(metricsReference)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sections, summary = map[string]family{}, map[string]family{}
	seenType, seenLabels := map[string]bool{}, map[string]bool{}
	current, fenced := "", false
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(text), "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(text, "#") {
			current = ""
			if match := familyHeading.FindStringSubmatch(text); match != nil {
				current = match[1]
				if _, dup := sections[current]; dup {
					t.Errorf("%s:%d: %s is documented twice", metricsReference, line, current)
				}
				sections[current] = family{}
			} else if strings.HasPrefix(text, "### ") && strings.Contains(text, "`"+namespace+"_") {
				t.Errorf("%s:%d: heading %q is not exactly one backticked family name", metricsReference, line, text)
			}
			continue
		}
		if match := summaryRow.FindStringSubmatch(text); match != nil && !fenced {
			name := match[1]
			if _, dup := summary[name]; dup {
				t.Errorf("%s:%d: %s has two summary rows", metricsReference, line, name)
			}
			if want := strings.ReplaceAll(name, "_", "-"); match[2] != want {
				t.Errorf("%s:%d: %s links to #%s, want #%s", metricsReference, line, name, match[2], want)
			}
			summary[name] = family{typ: match[3], labels: labelList(t, line, name, match[4])}
			continue
		}
		if current == "" {
			continue
		}
		doc := sections[current]
		doc.text += text + "\n"
		value := strings.TrimSuffix(text, `\`)
		switch {
		case fenced:
		case strings.HasPrefix(value, "**Type:** "):
			doc.typ = strings.TrimPrefix(value, "**Type:** ")
			seenType[current] = true
		case strings.HasPrefix(value, "**Labels:** "):
			seenLabels[current] = true
			doc.labels = labelList(t, line, current, strings.TrimPrefix(value, "**Labels:** "))
		case strings.HasPrefix(value, "**Buckets:** "):
			for _, field := range strings.Split(strings.TrimPrefix(value, "**Buckets:** "), ", ") {
				b, err := strconv.ParseFloat(field, 64)
				if err != nil {
					t.Errorf("%s:%d: %s: bucket %q is not a number", metricsReference, line, current, field)
				}
				doc.buckets = append(doc.buckets, b)
			}
		}
		sections[current] = doc
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if fenced {
		t.Errorf("%s: unclosed code fence", metricsReference)
	}
	for name := range sections {
		if !seenType[name] || !seenLabels[name] {
			t.Errorf("%s: %s lacks its **Type:** or **Labels:** line", metricsReference, name)
		}
	}
	return sections, summary
}

// TestMetricsReference keeps docs/reference/metrics.md complete: every
// exported statica_ family has a heading with its type, label names and, for
// histograms, bucket bounds, and a matching row in the summary table; every
// documented family exists; and the families whose series all exist from
// startup name every label value they export.
func TestMetricsReference(t *testing.T) {
	exported := exportedFamilies(t)
	documented, summary := documentedFamilies(t)
	if len(exported) == 0 || len(documented) == 0 {
		t.Fatalf("found %d exported and %d documented families", len(exported), len(documented))
	}
	for name, e := range exported {
		d, ok := documented[name]
		if !ok {
			t.Errorf("%s is exported but missing from %s", name, metricsReference)
			continue
		}
		if d.typ != e.typ {
			t.Errorf("%s: documented type %q, exported %q", name, d.typ, e.typ)
		}
		if !slices.Equal(d.labels, e.labels) {
			t.Errorf("%s: documented labels %v, exported %v", name, d.labels, e.labels)
		}
		if !slices.Equal(d.buckets, e.buckets) {
			t.Errorf("%s: documented buckets %v, exported %v", name, d.buckets, e.buckets)
		}
	}
	for name, d := range documented {
		if _, ok := exported[name]; !ok {
			t.Errorf("%s is documented in %s but not exported", name, metricsReference)
		}
		row, ok := summary[name]
		switch {
		case !ok:
			t.Errorf("%s has no row in the summary table of %s", name, metricsReference)
		case row.typ != d.typ || !slices.Equal(row.labels, d.labels):
			t.Errorf("%s: summary row says %s %v, its section %s %v", name, row.typ, row.labels, d.typ, d.labels)
		}
	}
	for name := range summary {
		if _, ok := documented[name]; !ok {
			t.Errorf("%s has a summary row but no section in %s", name, metricsReference)
		}
	}
	for _, name := range startupFamilies {
		e, ok := exported[name]
		if !ok {
			t.Errorf("%s is not exported", name)
			continue
		}
		for label, values := range e.values {
			for _, v := range values {
				if !strings.Contains(documented[name].text, "`"+v+"`") {
					t.Errorf("%s: the value %q of %s is not named in its section", name, v, label)
				}
			}
		}
	}
}
