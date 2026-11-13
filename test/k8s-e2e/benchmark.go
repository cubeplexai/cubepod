package k8s_e2e_test

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// SLO evaluation statuses for benchmark operations.
const (
	SLOStatusPass = "PASS"
	SLOStatusWarn = "WARN"
	SLOStatusFail = "FAIL"
	SLOStatusInfo = "INFO"
)

// Canonical CubePod benchmark operation identifiers.
const (
	OpColdCreate              = "cold_create"
	OpWarmPoolPrewarm         = "warmpool_prewarm"
	OpWarmPoolClaimController = "warmpool_claim_controller"
	OpWarmPoolClaimE2E        = "warmpool_claim_e2e"
	OpExecForegroundRTT       = "exec_foreground_rtt"
	OpArchiveSync             = "archive_sync"
	OpPause                   = "pause"
	OpPauseAutoResume         = "pause_auto_resume"
	OpResetRootFS             = "reset_rootfs"
	OpCoWFork                 = "cow_fork"
)

// SLOTarget defines a latency Service Level Objective in milliseconds.
// TargetMs <= 0 marks an informational metric without pass/fail thresholds.
type SLOTarget struct {
	TargetMs    float64 `json:"targetMs,omitempty"`
	WarnMs      float64 `json:"warnMs,omitempty"`
	Description string  `json:"description,omitempty"`
}

// DefaultCubePodSLOs returns the CubePod architecture latency targets from docs/architecture.md.
func DefaultCubePodSLOs() map[string]SLOTarget {
	return map[string]SLOTarget{
		OpWarmPoolClaimController: {
			TargetMs:    50,
			WarnMs:      100,
			Description: "WarmPool Claim controller overhead (< 50ms)",
		},
		OpWarmPoolClaimE2E: {
			TargetMs:    200,
			WarnMs:      500,
			Description: "WarmPool Claim end-to-end HTTP (< 200ms)",
		},
		OpPauseAutoResume: {
			TargetMs:    1500,
			WarnMs:      2500,
			Description: "Pause -> Auto-Resume (< 1500ms)",
		},
		OpCoWFork: {
			TargetMs:    1500,
			WarnMs:      2500,
			Description: "CoW Volume Fork (< 1500ms)",
		},
		OpArchiveSync: {
			TargetMs:    100,
			WarnMs:      250,
			Description: "archive-sync tar.gz extraction (< 100ms)",
		},
	}
}

// BenchmarkSample records a single timed benchmark measurement.
type BenchmarkSample struct {
	Operation      string            `json:"operation"`
	Iteration      int               `json:"iteration"`
	DurationMs     float64           `json:"durationMs"`
	Bytes          int64             `json:"bytes,omitempty"`
	ThroughputMBps float64           `json:"throughputMBps,omitempty"`
	Success        bool              `json:"success"`
	Error          string            `json:"error,omitempty"`
	Timestamp      time.Time         `json:"timestamp"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// OperationStats summarizes latency distribution, throughput, and SLO evaluation for one operation.
type OperationStats struct {
	Operation        string     `json:"operation"`
	Count            int        `json:"count"`
	SuccessCount     int        `json:"successCount"`
	FailureCount     int        `json:"failureCount"`
	MinMs            float64    `json:"minMs"`
	P50Ms            float64    `json:"p50Ms"`
	P95Ms            float64    `json:"p95Ms"`
	MaxMs            float64    `json:"maxMs"`
	MeanMs           float64    `json:"meanMs"`
	TotalBytes       int64      `json:"totalBytes,omitempty"`
	ThroughputMBps   float64    `json:"throughputMBps,omitempty"`
	SLO              *SLOTarget `json:"slo,omitempty"`
	Status           string     `json:"status"`
	EvaluationReason string     `json:"evaluationReason,omitempty"`
}

// BenchmarkSuiteReport aggregates per-operation statistics and raw samples for export.
type BenchmarkSuiteReport struct {
	SuiteName   string            `json:"suiteName"`
	GeneratedAt time.Time         `json:"generatedAt"`
	Environment map[string]string `json:"environment,omitempty"`
	Summaries   []OperationStats  `json:"summaries"`
	Samples     []BenchmarkSample `json:"samples"`
	PassedCount int               `json:"passedCount"`
	WarnCount   int               `json:"warnCount"`
	FailedCount int               `json:"failedCount"`
	InfoCount   int               `json:"infoCount"`
}

// BenchmarkCollector collects timed samples in a thread-safe manner and builds reports.
type BenchmarkCollector struct {
	mu        sync.Mutex
	suiteName string
	env       map[string]string
	slos      map[string]SLOTarget
	order     []string
	byOp      map[string][]BenchmarkSample
	samples   []BenchmarkSample
}

// NewBenchmarkCollector initializes a collector with optional environment metadata and SLO targets.
// When slos is nil, DefaultCubePodSLOs() is used.
func NewBenchmarkCollector(suiteName string, env map[string]string, slos map[string]SLOTarget) *BenchmarkCollector {
	envCopy := make(map[string]string, len(env))
	for k, v := range env {
		if v != "" {
			envCopy[k] = v
		}
	}
	if slos == nil {
		slos = DefaultCubePodSLOs()
	}
	sloCopy := make(map[string]SLOTarget, len(slos))
	for k, v := range slos {
		sloCopy[k] = v
	}
	return &BenchmarkCollector{
		suiteName: suiteName,
		env:       envCopy,
		slos:      sloCopy,
		byOp:      make(map[string][]BenchmarkSample),
	}
}

// SetEnvironment records or updates an environment metadata key.
func (c *BenchmarkCollector) SetEnvironment(key, value string) {
	if key == "" || value == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.env[key] = value
}

// Record appends a BenchmarkSample to the collector.
func (c *BenchmarkCollector) Record(sample BenchmarkSample) {
	if sample.Timestamp.IsZero() {
		sample.Timestamp = time.Now().UTC()
	}
	if sample.Bytes > 0 && sample.DurationMs > 0 && sample.ThroughputMBps == 0 {
		sample.ThroughputMBps = (float64(sample.Bytes) / (1024.0 * 1024.0)) / (sample.DurationMs / 1000.0)
	}
	if len(sample.Metadata) > 0 {
		metaCopy := make(map[string]string, len(sample.Metadata))
		for k, v := range sample.Metadata {
			metaCopy[k] = v
		}
		sample.Metadata = metaCopy
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.byOp[sample.Operation]; !exists {
		c.order = append(c.order, sample.Operation)
	}
	c.byOp[sample.Operation] = append(c.byOp[sample.Operation], sample)
	c.samples = append(c.samples, sample)
}

// Measure executes fn, records its wall-clock duration as a BenchmarkSample, and returns fn's error.
func (c *BenchmarkCollector) Measure(operation string, iteration int, payloadBytes int64, meta map[string]string, fn func() error) error {
	start := time.Now()
	err := fn()
	elapsedMs := float64(time.Since(start)) / float64(time.Millisecond)

	sample := BenchmarkSample{
		Operation:  operation,
		Iteration:  iteration,
		DurationMs: elapsedMs,
		Bytes:      payloadBytes,
		Success:    err == nil,
		Timestamp:  time.Now().UTC(),
		Metadata:   meta,
	}
	if err != nil {
		sample.Error = err.Error()
	}
	c.Record(sample)
	return err
}

// BuildReport computes per-operation statistics and SLO evaluations for all recorded samples.
func (c *BenchmarkCollector) BuildReport() BenchmarkSuiteReport {
	c.mu.Lock()
	defer c.mu.Unlock()

	envCopy := make(map[string]string, len(c.env))
	for k, v := range c.env {
		envCopy[k] = v
	}

	samplesCopy := make([]BenchmarkSample, len(c.samples))
	copy(samplesCopy, c.samples)

	report := BenchmarkSuiteReport{
		SuiteName:   c.suiteName,
		GeneratedAt: time.Now().UTC(),
		Environment: envCopy,
		Summaries:   make([]OperationStats, 0, len(c.order)),
		Samples:     samplesCopy,
	}

	for _, op := range c.order {
		var sloPtr *SLOTarget
		if target, ok := c.slos[op]; ok {
			tCopy := target
			sloPtr = &tCopy
		}
		stats := computeOperationStats(op, c.byOp[op], sloPtr)
		switch stats.Status {
		case SLOStatusPass:
			report.PassedCount++
		case SLOStatusWarn:
			report.WarnCount++
		case SLOStatusFail:
			report.FailedCount++
		default:
			report.InfoCount++
		}
		report.Summaries = append(report.Summaries, stats)
	}

	return report
}

// computeOperationStats calculates min, p50, p95, max, mean (using nearest-rank percentiles),
// aggregate throughput, and SLO evaluation for a slice of samples belonging to one operation.
func computeOperationStats(operation string, samples []BenchmarkSample, slo *SLOTarget) OperationStats {
	stats := OperationStats{
		Operation: operation,
		Count:     len(samples),
		SLO:       slo,
		Status:    SLOStatusInfo,
	}
	if len(samples) == 0 {
		stats.EvaluationReason = "no samples recorded"
		return stats
	}

	successDurations := make([]float64, 0, len(samples))
	allDurations := make([]float64, 0, len(samples))
	var totalSuccessMs float64
	var totalBytes int64

	for _, s := range samples {
		allDurations = append(allDurations, s.DurationMs)
		if s.Success {
			stats.SuccessCount++
			successDurations = append(successDurations, s.DurationMs)
			totalSuccessMs += s.DurationMs
			if s.Bytes > 0 {
				totalBytes += s.Bytes
			}
		} else {
			stats.FailureCount++
		}
	}

	durations := successDurations
	if len(durations) == 0 {
		durations = allDurations
	}
	sort.Float64s(durations)

	var sum float64
	for _, d := range durations {
		sum += d
	}

	stats.MinMs = durations[0]
	stats.MaxMs = durations[len(durations)-1]
	stats.MeanMs = sum / float64(len(durations))
	stats.P50Ms = percentileNearestRank(durations, 0.50)
	stats.P95Ms = percentileNearestRank(durations, 0.95)
	stats.TotalBytes = totalBytes

	if totalBytes > 0 && totalSuccessMs > 0 {
		stats.ThroughputMBps = (float64(totalBytes) / (1024.0 * 1024.0)) / (totalSuccessMs / 1000.0)
	}

	stats.Status, stats.EvaluationReason = evaluateSLO(stats)
	return stats
}

// percentileNearestRank computes the p-quantile (0 < p <= 1) of a sorted ascending slice
// using the nearest-rank method: index = ceil(p * N) - 1.
func percentileNearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// evaluateSLO determines PASS, WARN, FAIL, or INFO from latency percentiles and failure counts.
func evaluateSLO(stats OperationStats) (string, string) {
	if stats.Count == 0 {
		return SLOStatusInfo, "no samples recorded"
	}
	if stats.SuccessCount == 0 {
		return SLOStatusFail, fmt.Sprintf("all %d samples failed", stats.FailureCount)
	}
	if stats.SLO == nil || stats.SLO.TargetMs <= 0 {
		if stats.FailureCount > 0 {
			return SLOStatusWarn, fmt.Sprintf("%d/%d samples failed (no latency SLO)", stats.FailureCount, stats.Count)
		}
		return SLOStatusInfo, "informational metric (no latency SLO)"
	}

	target := stats.SLO.TargetMs
	warn := stats.SLO.WarnMs

	if stats.FailureCount > 0 {
		if stats.P95Ms <= target || (warn > target && stats.P95Ms <= warn) || stats.P50Ms <= target {
			return SLOStatusWarn, fmt.Sprintf("%d/%d samples failed; p95=%.1fms (target < %.1fms)", stats.FailureCount, stats.Count, stats.P95Ms, target)
		}
		return SLOStatusFail, fmt.Sprintf("%d/%d samples failed and p50=%.1fms > target %.1fms", stats.FailureCount, stats.Count, stats.P50Ms, target)
	}

	if stats.P95Ms <= target {
		return SLOStatusPass, fmt.Sprintf("p95 %.1fms <= target %.1fms", stats.P95Ms, target)
	}
	if stats.P50Ms <= target {
		return SLOStatusWarn, fmt.Sprintf("p50 %.1fms <= target %.1fms, but p95 %.1fms exceeds target", stats.P50Ms, target, stats.P95Ms)
	}
	if warn > target && stats.P95Ms <= warn {
		return SLOStatusWarn, fmt.Sprintf("p95 %.1fms exceeds target %.1fms within warn limit %.1fms", stats.P95Ms, target, warn)
	}
	return SLOStatusFail, fmt.Sprintf("p50 %.1fms and p95 %.1fms exceed target %.1fms", stats.P50Ms, stats.P95Ms, target)
}

// RenderJSON serializes the BenchmarkSuiteReport as indented JSON.
func (r BenchmarkSuiteReport) RenderJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// RenderMarkdown formats the BenchmarkSuiteReport as a human-readable Markdown document
// with environment details, summary latency table, and SLO counts.
func (r BenchmarkSuiteReport) RenderMarkdown() string {
	var b strings.Builder

	title := r.SuiteName
	if title == "" {
		title = "CubePod K8s Kata Performance Benchmark"
	}
	b.WriteString("# " + title + "\n\n")
	if !r.GeneratedAt.IsZero() {
		fmt.Fprintf(&b, "- **Generated At**: `%s`\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "- **SLO Summary**: %d PASS, %d WARN, %d FAIL, %d INFO\n",
		r.PassedCount, r.WarnCount, r.FailedCount, r.InfoCount)

	if len(r.Environment) > 0 {
		keys := make([]string, 0, len(r.Environment))
		for k := range r.Environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n## Environment\n\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "- **%s**: `%s`\n", k, r.Environment[k])
		}
	}

	b.WriteString("\n## Latency & Throughput Summary\n\n")
	b.WriteString("| Operation | Samples (OK/Total) | Min (ms) | p50 (ms) | p95 (ms) | Max (ms) | Mean (ms) | Throughput | SLO Target | Status |\n")
	b.WriteString("| :--- | :---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :---: |\n")

	for _, s := range r.Summaries {
		throughput := "-"
		if s.ThroughputMBps > 0 {
			throughput = fmt.Sprintf("%.2f MiB/s", s.ThroughputMBps)
		}
		sloCell := "-"
		if s.SLO != nil && s.SLO.TargetMs > 0 {
			sloCell = fmt.Sprintf("< %.0f ms", s.SLO.TargetMs)
		}
		fmt.Fprintf(&b, "| `%s` | %d/%d | %.2f | %.2f | %.2f | %.2f | %.2f | %s | %s | **%s** |\n",
			s.Operation,
			s.SuccessCount,
			s.Count,
			s.MinMs,
			s.P50Ms,
			s.P95Ms,
			s.MaxMs,
			s.MeanMs,
			throughput,
			sloCell,
			s.Status,
		)
	}

	return b.String()
}
