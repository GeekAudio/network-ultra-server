// Package metrics is a tiny zero-dependency Prometheus-text exposition.
// We avoid pulling in the official prometheus client to keep the binary
// small and dependencies minimal for the MIT release.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

type counter struct {
	v atomic.Uint64
}

func (c *counter) Inc()          { c.v.Add(1) }
func (c *counter) Add(n uint64)  { c.v.Add(n) }
func (c *counter) Value() uint64 { return c.v.Load() }

type gauge struct {
	v atomic.Int64
}

func (g *gauge) Set(n int64)  { g.v.Store(n) }
func (g *gauge) Inc()         { g.v.Add(1) }
func (g *gauge) Dec()         { g.v.Add(-1) }
func (g *gauge) Value() int64 { return g.v.Load() }

// Registry holds named counters and gauges + label families.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*counter
	gauges   map[string]*gauge
	families map[string]*labeledFamily // counter with labels
}

type labeledFamily struct {
	mu       sync.RWMutex
	counters map[string]*counter
	help     string
	labelKey string // e.g. "code"
}

func NewRegistry() *Registry {
	return &Registry{
		counters: make(map[string]*counter),
		gauges:   make(map[string]*gauge),
		families: make(map[string]*labeledFamily),
	}
}

func (r *Registry) Counter(name string) *counter {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c = &counter{}
	r.counters[name] = c
	return c
}

func (r *Registry) Gauge(name string) *gauge {
	r.mu.RLock()
	g, ok := r.gauges[name]
	r.mu.RUnlock()
	if ok {
		return g
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g = &gauge{}
	r.gauges[name] = g
	return g
}

// LabeledCounter returns a counter family keyed by a single label value.
func (r *Registry) LabeledCounter(name, labelKey string) *labeledFamily {
	r.mu.RLock()
	f, ok := r.families[name]
	r.mu.RUnlock()
	if ok {
		return f
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.families[name]; ok {
		return f
	}
	f = &labeledFamily{
		counters: make(map[string]*counter),
		labelKey: labelKey,
	}
	r.families[name] = f
	return f
}

func (f *labeledFamily) Inc(labelValue string) {
	f.mu.RLock()
	c, ok := f.counters[labelValue]
	f.mu.RUnlock()
	if ok {
		c.Inc()
		return
	}
	f.mu.Lock()
	if c, ok := f.counters[labelValue]; ok {
		f.mu.Unlock()
		c.Inc()
		return
	}
	c = &counter{}
	f.counters[labelValue] = c
	f.mu.Unlock()
	c.Inc()
}

// WriteText emits Prometheus text exposition.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cnames := make([]string, 0, len(r.counters))
	for k := range r.counters {
		cnames = append(cnames, k)
	}
	sort.Strings(cnames)
	for _, k := range cnames {
		fmt.Fprintf(w, "# TYPE %s counter\n%s %d\n", k, k, r.counters[k].Value())
	}

	gnames := make([]string, 0, len(r.gauges))
	for k := range r.gauges {
		gnames = append(gnames, k)
	}
	sort.Strings(gnames)
	for _, k := range gnames {
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n", k, k, r.gauges[k].Value())
	}

	fnames := make([]string, 0, len(r.families))
	for k := range r.families {
		fnames = append(fnames, k)
	}
	sort.Strings(fnames)
	for _, name := range fnames {
		fam := r.families[name]
		fam.mu.RLock()
		labels := make([]string, 0, len(fam.counters))
		for lv := range fam.counters {
			labels = append(labels, lv)
		}
		sort.Strings(labels)
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		for _, lv := range labels {
			fmt.Fprintf(w, "%s{%s=%q} %d\n", name, fam.labelKey, lv, fam.counters[lv].Value())
		}
		fam.mu.RUnlock()
	}
}
