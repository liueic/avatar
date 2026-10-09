// Package metrics is a dependency-free Prometheus text-exposition registry
// (counters, gauges, histograms with label sets) for SPEC §15.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// DefaultBuckets cover avatar-serving and inference latencies (seconds).
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Registry holds all service metrics.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]*counterFamily
	gauges     map[string]*gaugeFamily
	histograms map[string]*histogramFamily
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		counters:   make(map[string]*counterFamily),
		gauges:     make(map[string]*gaugeFamily),
		histograms: make(map[string]*histogramFamily),
	}
}

type labelKey string

func labelKeyOf(name string, labels map[string]string) labelKey {
	if len(labels) == 0 {
		return labelKey(name)
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteByte(0x1f)
		b.WriteString(k)
		b.WriteByte(0x1e)
		b.WriteString(labels[k])
	}
	return labelKey(b.String())
}

// --- counters ---

type counterFamily struct {
	name, help string
	labels     []string
	series     map[string]map[string]string // labelKey -> labelSet
	values     map[string]float64
}

// Counter increments a labeled counter, registering it on first use.
func (r *Registry) Counter(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.counters[name]
	if f == nil {
		f = &counterFamily{name: name, help: help, series: make(map[string]map[string]string), values: make(map[string]float64)}
		for k := range labels {
			f.labels = append(f.labels, k)
		}
		sort.Strings(f.labels)
		r.counters[name] = f
	}
	key := labelKeyOf(name, labels)
	if _, ok := f.series[string(key)]; !ok {
		f.series[string(key)] = labels
	}
	f.bump(string(key), v)
}

// --- gauges ---

type gaugeFamily struct {
	name, help string
	labels     []string
	series     map[string]map[string]string
	values     map[string]float64
}

// Gauge sets a labeled gauge to v.
func (r *Registry) Gauge(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.gauges[name]
	if f == nil {
		f = &gaugeFamily{name: name, help: help, series: make(map[string]map[string]string), values: make(map[string]float64)}
		for k := range labels {
			f.labels = append(f.labels, k)
		}
		sort.Strings(f.labels)
		r.gauges[name] = f
	}
	key := labelKeyOf(name, labels)
	if _, ok := f.series[string(key)]; !ok {
		f.series[string(key)] = labels
	}
	f.values[string(key)] = v
}

// --- histograms ---

type histogramFamily struct {
	name, help string
	labels     []string
	buckets    []float64
	series     map[string]*histogramSeries
}

type histogramSeries struct {
	labels map[string]string
	counts []uint64 // per bucket, cumulative
	sum    float64
	count  uint64
}

// Histogram observes v in a labeled histogram.
func (r *Registry) Histogram(name, help string, labels map[string]string, buckets []float64, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.histograms[name]
	if f == nil {
		if len(buckets) == 0 {
			buckets = DefaultBuckets
		}
		bs := make([]float64, len(buckets))
		copy(bs, buckets)
		sort.Float64s(bs)
		f = &histogramFamily{name: name, help: help, buckets: bs, series: make(map[string]*histogramSeries)}
		for k := range labels {
			f.labels = append(f.labels, k)
		}
		sort.Strings(f.labels)
		r.histograms[name] = f
	}
	key := string(labelKeyOf(name, labels))
	s := f.series[key]
	if s == nil {
		s = &histogramSeries{labels: labels, counts: make([]uint64, len(f.buckets))}
		f.series[key] = s
	}
	for i, b := range f.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

// Render produces the Prometheus text exposition format (format version 0.0.4).
func (r *Registry) Render() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	writeFam := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
	}

	// Deterministic output order for stable scraping diffs.
	names := make([]string, 0, len(r.counters)+len(r.gauges)+len(r.histograms))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.counters[n]
		writeFam(f.name, f.help, "counter")
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(renderSample(f.name, f.labels, f.series[k], f.values[k]))
		}
	}

	gnames := make([]string, 0, len(r.gauges))
	for n := range r.gauges {
		gnames = append(gnames, n)
	}
	sort.Strings(gnames)
	for _, n := range gnames {
		f := r.gauges[n]
		writeFam(f.name, f.help, "gauge")
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(renderSample(f.name, f.labels, f.series[k], f.values[k]))
		}
	}

	hnames := make([]string, 0, len(r.histograms))
	for n := range r.histograms {
		hnames = append(hnames, n)
	}
	sort.Strings(hnames)
	for _, n := range hnames {
		f := r.histograms[n]
		writeFam(f.name, f.help, "histogram")
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			for i, bound := range f.buckets {
				lbls := copyLabels(s.labels)
				lbls["le"] = formatFloat(bound)
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, renderLabels(f.labels, lbls), s.counts[i])
			}
			lbls := copyLabels(s.labels)
			lbls["le"] = "+Inf"
			fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, renderLabels(f.labels, lbls), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", f.name, renderLabels(f.labels, s.labels), formatFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", f.name, renderLabels(f.labels, s.labels), s.count)
		}
	}
	return []byte(b.String())
}

func (f *counterFamily) bump(key string, v float64) {
	f.values[key] += v
}

func escapeHelp(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

func renderLabels(defOrder []string, labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, k, escapeLabel(labels[k]))
	}
	b.WriteByte('}')
	return b.String()
}

func renderSample(name string, defOrder []string, labels map[string]string, v float64) string {
	return fmt.Sprintf("%s%s %s\n", name, renderLabels(defOrder, labels), formatFloat(v))
}

func copyLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}
