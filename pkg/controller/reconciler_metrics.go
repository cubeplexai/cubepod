package controller

import (
	"sort"
	"sync"
	"time"
)

var defaultReconcilePhaseBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// ReconcilePhaseTimings holds per-phase durations recorded during a single Reconcile call.
type ReconcilePhaseTimings struct {
	Operation    string
	PrepareDur   time.Duration
	EnsurePodDur time.Duration
	UpsertBoxDur time.Duration
	DeletePodDur time.Duration
	TotalDur     time.Duration
}

// PhaseHistogramSeries represents one labeled Prometheus histogram time series.
type PhaseHistogramSeries struct {
	Operation string
	Phase     string
	Buckets   []float64
	Counts    []uint64
	Sum       float64
	Count     uint64
}

type phaseKey struct {
	operation string
	phase     string
}

type phaseHistogramEntry struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

type reconcilePhaseMetricsCollector struct {
	mu     sync.Mutex
	series map[phaseKey]*phaseHistogramEntry
}

func (c *reconcilePhaseMetricsCollector) observe(operation, phase string, dur time.Duration) {
	if operation == "" || phase == "" {
		return
	}
	secs := dur.Seconds()
	if secs < 0 {
		secs = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.series == nil {
		c.series = make(map[phaseKey]*phaseHistogramEntry)
	}
	k := phaseKey{operation: operation, phase: phase}
	entry := c.series[k]
	if entry == nil {
		entry = &phaseHistogramEntry{
			buckets: defaultReconcilePhaseBuckets,
			counts:  make([]uint64, len(defaultReconcilePhaseBuckets)),
		}
		c.series[k] = entry
	}
	for i, upper := range entry.buckets {
		if secs <= upper {
			entry.counts[i]++
		}
	}
	entry.sum += secs
	entry.count++
}

func (c *reconcilePhaseMetricsCollector) snapshot() []PhaseHistogramSeries {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PhaseHistogramSeries, 0, len(c.series))
	for k, entry := range c.series {
		counts := make([]uint64, len(entry.counts))
		copy(counts, entry.counts)
		out = append(out, PhaseHistogramSeries{
			Operation: k.operation,
			Phase:     k.phase,
			Buckets:   entry.buckets,
			Counts:    counts,
			Sum:       entry.sum,
			Count:     entry.count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Operation != out[j].Operation {
			return out[i].Operation < out[j].Operation
		}
		return out[i].Phase < out[j].Phase
	})
	return out
}

// ReconcilePhaseMetrics returns a sorted snapshot of controller reconcile phase latency histograms.
func (r *Reconciler) ReconcilePhaseMetrics() []PhaseHistogramSeries {
	if r == nil {
		return nil
	}
	return r.phaseMetrics.snapshot()
}
