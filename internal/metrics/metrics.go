// Package metrics is banyan's process-level telemetry registry: counters,
// gauges and histograms exposed in the Prometheus text format, with zero
// external dependencies. One global registry per process — each role
// (all-in-one, gateway, refs, storage) serves it at /metrics, and the
// metrics themselves live next to the code paths they measure.
//
// Naming follows Prometheus conventions: banyan_<noun>_<unit>_<type>, with
// labels for the dimensions an operator slices by (transport, operation,
// result). Every collector is safe for concurrent use.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Handler serves the registry in the Prometheus text exposition format —
// mounted at /metrics by every role. Scrapes are cheap (a render walks the
// in-memory series) and intentionally unauthenticated: it belongs behind
// the same network boundary as the process's internal endpoints.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(Render()))
	})
}

type metricType int

const (
	typeCounter metricType = iota
	typeGauge
	typeHistogram
)

// Default histogram buckets: the git-traffic shape — sub-second API calls
// on one end, multi-second pack streams on the other.
var defaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type series struct {
	labels []string // flattened key,value pairs, sorted by key at insert
}

// counter is a monotonically increasing value.
type counter struct {
	val atomic.Int64
}

func (c *counter) Add(n int64) { c.val.Add(n) }
func (c *counter) Inc()        { c.val.Add(1) }

// gauge is a value that can go up and down.
type gauge struct {
	mu  sync.Mutex
	val int64
}

func (g *gauge) Add(n int64) { g.mu.Lock(); g.val += n; g.mu.Unlock() }
func (g *gauge) Set(v int64) { g.mu.Lock(); g.val = v; g.mu.Unlock() }

// histogram counts observations into cumulative buckets plus sum and count.
type histogram struct {
	mu      sync.Mutex
	bounds  []float64
	buckets []int64
	count   int64
	sum     float64
}

func (h *histogram) Observe(v float64) {
	h.mu.Lock()
	h.sum += v
	h.count++
	for i, b := range h.bounds {
		if v <= b {
			h.buckets[i]++
		}
	}
	h.mu.Unlock()
}

// collector is one named metric (its label dimensionality is fixed at
// declaration; each distinct label combination is a series).
type collector struct {
	name    string
	help    string
	typ     metricType
	bounds  []float64 // histograms only
	labels  []string  // declared label keys, sorted

	mu      sync.Mutex
	series  map[string]*seriesData
}

type seriesData struct {
	labels   []string // key,value,... aligned with collector.labels
	c        *counter
	g        *gauge
	h        *histogram
}

// --- registry ---

var (
	regMu      sync.Mutex
	registry   = map[string]*collector{}
	order      []string // stable render order
)

func declare(name, help string, typ metricType, labelKeys []string, bounds []float64) *collector {
	regMu.Lock()
	defer regMu.Unlock()
	if c, ok := registry[name]; ok {
		return c // idempotent: packages redeclare on init
	}
	keys := append([]string(nil), labelKeys...)
	sort.Strings(keys)
	c := &collector{name: name, help: help, typ: typ, bounds: bounds, labels: keys, series: map[string]*seriesData{}}
	registry[name] = c
	order = append(order, name)
	sort.Strings(order)
	return c
}

// GetCounter declares (once) a counter metric and returns a resolver whose
// With(label values, in any order paired as key,value) yields the series.
func GetCounter(name, help string, labelKeys ...string) *Resolver {
	return &Resolver{c: declare(name, help, typeCounter, labelKeys, nil)}
}

// GetGauge declares (once) a gauge metric.
func GetGauge(name, help string, labelKeys ...string) *Resolver {
	return &Resolver{c: declare(name, help, typeGauge, labelKeys, nil)}
}

// GetHistogram declares (once) a histogram metric with the default buckets.
func GetHistogram(name, help string, labelKeys ...string) *Resolver {
	return &Resolver{c: declare(name, help, typeHistogram, labelKeys, defaultBuckets)}
}

// Resolver binds label values to a series. With panics on a malformed pair
// count — a programming error best found at first call, never at runtime.
type Resolver struct {
	c *collector
}

// With takes label pairs as key,value,key,value… and returns the series.
func (r *Resolver) With(kv ...string) *seriesData {
	if len(kv) != len(r.c.labels)*2 {
		panic(fmt.Sprintf("metrics %s: %d label values for %d keys", r.c.name, len(kv), len(r.c.labels)))
	}
	kvMap := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		kvMap[kv[i]] = kv[i+1]
	}
	var flat []string
	for _, k := range r.c.labels {
		flat = append(flat, k, kvMap[k])
	}
	key := strings.Join(flat, "\x00")
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	sd, ok := r.c.series[key]
	if !ok {
		sd = &seriesData{labels: flat}
		switch r.c.typ {
		case typeCounter:
			sd.c = &counter{}
		case typeGauge:
			sd.g = &gauge{}
		case typeHistogram:
			h := &histogram{bounds: r.c.bounds, buckets: make([]int64, len(r.c.bounds))}
			sd.h = h
		}
		r.c.series[key] = sd
	}
	return sd
}

// Counter convenience: single call for unlabeled counters.
func (r *Resolver) Inc() {
	if len(r.c.labels) != 0 {
		panic("metrics: labeled series needs With(...)")
	}
	r.With().Counter().Inc()
}

// The series accessors keep call sites terse.
func (sd *seriesData) Counter() *counter { return sd.c }
func (sd *seriesData) Gauge() *gauge     { return sd.g }
func (sd *seriesData) Hist() *histogram  { return sd.h }
func (sd *seriesData) Inc()              { sd.c.Inc() }
func (sd *seriesData) Add(n int64)       { sd.c.Add(n) }
func (sd *seriesData) Set(v int64)       { sd.g.Set(v) }
func (sd *seriesData) Observe(v float64) { sd.h.Observe(v) }

// Render emits the registry in the Prometheus text exposition format.
func Render() string {
	regMu.Lock()
	names := append([]string(nil), order...)
	cols := make([]*collector, len(names))
	for i, n := range names {
		cols[i] = registry[n]
	}
	regMu.Unlock()

	var b strings.Builder
	for _, c := range cols {
		c.mu.Lock()
		typeName := map[metricType]string{typeCounter: "counter", typeGauge: "gauge", typeHistogram: "histogram"}[c.typ]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", c.name, c.help, c.name, typeName)
		seriesKeys := make([]string, 0, len(c.series))
		for k := range c.series {
			seriesKeys = append(seriesKeys, k)
		}
		sort.Strings(seriesKeys)
		for _, k := range seriesKeys {
			sd := c.series[k]
			labelStr := renderLabels(sd.labels)
			switch c.typ {
			case typeCounter:
				fmt.Fprintf(&b, "%s%s %d\n", c.name, labelStr, sd.c.val.Load())
			case typeGauge:
				sd.g.mu.Lock()
				v := sd.g.val
				sd.g.mu.Unlock()
				fmt.Fprintf(&b, "%s%s %d\n", c.name, labelStr, v)
			case typeHistogram:
				sd.h.mu.Lock()
				for i, bound := range sd.h.bounds {
					fmt.Fprintf(&b, "%s_bucket%s %d\n", c.name, withLE(labelStr, formatFloat(bound)), sd.h.buckets[i])
				}
				fmt.Fprintf(&b, "%s_bucket%s %d\n", c.name, withLE(labelStr, "+Inf"), sd.h.count)
				fmt.Fprintf(&b, "%s_sum%s %s\n", c.name, labelStr, formatFloat(sd.h.sum))
				fmt.Fprintf(&b, "%s_count%s %d\n", c.name, labelStr, sd.h.count)
				sd.h.mu.Unlock()
			}
		}
		c.mu.Unlock()
	}
	return b.String()
}

func renderLabels(flat []string) string {
	if len(flat) == 0 {
		return ""
	}
	parts := make([]string, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		// %q escapes exactly the Prometheus label specials (backslash,
		// double quote, newline) — Go quoting and the exposition format
		// agree on those.
		parts = append(parts, fmt.Sprintf("%s=%q", flat[i], flat[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func withLE(labelStr, le string) string {
	if labelStr == "" {
		return "{le=\"" + le + "\"}"
	}
	return "{le=\"" + le + "\"," + labelStr[1:]
}

func formatFloat(v float64) string {
	// Buckets print as their canonical literal (0.1, 2.5, +Inf handled by
	// the caller); sums trim trailing zeros.
	s := fmt.Sprintf("%g", v)
	return s
}
