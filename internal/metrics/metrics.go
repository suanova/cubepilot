// Package metrics exposes the phase-one /metrics endpoint with the five
// instrumentation families required by the design doc §9 (observability):
//
//   - dialogue: session count / message count / first-token latency / total
//     reply latency / stream interruption rate
//   - agent:    tool-call count / success rate / L0-L1 distribution
//   - cost:     LLM token consumption (input/output)
//   - ops:      task execution duration / abnormal-item distribution
//   - pool:     active/warm/reclaimed instance count, cold-start latency,
//     rebuild rate
//
// Collection and dashboard presentation are deferred (NFR-011); phase one
// exposes a text exposition endpoint and in-process counters updated by the
// assistant service.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry is a tiny Prometheus-text-format counter registry. Gauges are
// updated by the Instance Manager (pool family); counters by the server.
type Registry struct {
	mu sync.Mutex

	// counters: name -> map[labelValue]count
	counters map[string]map[string]int64

	// gauges: name -> value
	gauges map[string]int64

	// firstToken / turn latency histograms (buckets in ms)
	firstTokenMs map[string][]int64
	turnMs       map[string][]int64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		counters:     map[string]map[string]int64{},
		gauges:       map[string]int64{},
		firstTokenMs: map[string][]int64{},
		turnMs:       map[string][]int64{},
	}
}

// Inc bumps a counter under a label (empty label = single series).
func (r *Registry) Inc(name, label string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters[name] == nil {
		r.counters[name] = map[string]int64{}
	}
	r.counters[name][label] += delta
}

// SetGauge sets a gauge value.
func (r *Registry) SetGauge(name string, v int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = v
}

// ObserveFirstToken records a first-token latency in milliseconds.
func (r *Registry) ObserveFirstToken(ms int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.firstTokenMs[""] = append(r.firstTokenMs[""], ms)
}

// ObserveTurn records a full-turn latency in milliseconds.
func (r *Registry) ObserveTurn(ms int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnMs[""] = append(r.turnMs[""], ms)
}

func pct95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func avg(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	var sum int64
	for _, v := range vals {
		sum += v
	}
	return sum / int64(len(vals))
}

// Write renders the registry in Prometheus text exposition format.
func (r *Registry) Write(w http.ResponseWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder

	// Counters.
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(fmt.Sprintf("# TYPE %s counter\n", n))
		labels := r.counters[n]
		lkeys := make([]string, 0, len(labels))
		for l := range labels {
			lkeys = append(lkeys, l)
		}
		sort.Strings(lkeys)
		for _, l := range lkeys {
			if l == "" {
				b.WriteString(fmt.Sprintf("%s %d\n", n, labels[l]))
			} else {
				b.WriteString(fmt.Sprintf("%s{label=%q} %d\n", n, l, labels[l]))
			}
		}
	}

	// Gauges.
	gkeys := make([]string, 0, len(r.gauges))
	for n := range r.gauges {
		gkeys = append(gkeys, n)
	}
	sort.Strings(gkeys)
	for _, n := range gkeys {
		b.WriteString(fmt.Sprintf("# TYPE %s gauge\n%s %d\n", n, n, r.gauges[n]))
	}

	// Histograms (P95 + avg).
	b.WriteString("# TYPE cubepilot_first_token_ms summary\n")
	b.WriteString(fmt.Sprintf("cubepilot_first_token_ms_p95 %d\n", pct95(r.firstTokenMs[""])))
	b.WriteString(fmt.Sprintf("cubepilot_first_token_ms_avg %d\n", avg(r.firstTokenMs[""])))
	b.WriteString("# TYPE cubepilot_turn_ms summary\n")
	b.WriteString(fmt.Sprintf("cubepilot_turn_ms_p95 %d\n", pct95(r.turnMs[""])))
	b.WriteString(fmt.Sprintf("cubepilot_turn_ms_avg %d\n", avg(r.turnMs[""])))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}

// Global registry used by the assistant service.
var (
	global = New()
	// UpTime is set once at process start.
	startedAt = time.Now()
)

// Global returns the process-wide registry.
func Global() *Registry { return global }

// UpSeconds returns process uptime in seconds (pool family helper).
func UpSeconds() int64 { return int64(time.Since(startedAt).Seconds()) }

// Convenience wrappers over Global().
func Inc(name, label string, delta int64) { global.Inc(name, label, delta) }
func SetGauge(name string, v int64)       { global.SetGauge(name, v) }
func ObserveFirstToken(ms int64)          { global.ObserveFirstToken(ms) }
func ObserveTurn(ms int64)                { global.ObserveTurn(ms) }

// Handler returns the /metrics HTTP handler.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { global.Write(w) }
}
