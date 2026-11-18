package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/secret"
)

var defaultDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type labelPair struct {
	first  string
	second string
}

type labeledCounterMap struct {
	mu     sync.RWMutex
	counts map[labelPair]uint64
}

func (c *labeledCounterMap) Inc(first, second string) {
	c.Add(first, second, 1)
}

func (c *labeledCounterMap) Add(first, second string, delta uint64) {
	c.mu.Lock()
	if c.counts == nil {
		c.counts = make(map[labelPair]uint64)
	}
	c.counts[labelPair{first: first, second: second}] += delta
	c.mu.Unlock()
}

func (c *labeledCounterMap) Snapshot() []struct {
	key labelPair
	val uint64
} {
	c.mu.RLock()
	out := make([]struct {
		key labelPair
		val uint64
	}, 0, len(c.counts))
	for k, v := range c.counts {
		out = append(out, struct {
			key labelPair
			val uint64
		}{key: k, val: v})
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.first != out[j].key.first {
			return out[i].key.first < out[j].key.first
		}
		return out[i].key.second < out[j].key.second
	})
	return out
}

type promHistogram struct {
	mu      sync.Mutex
	buckets []float64
	counts  []uint64
	sum     float64
	total   uint64
}

func (h *promHistogram) Observe(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	h.mu.Lock()
	if h.buckets == nil {
		h.buckets = defaultDurationBuckets
		h.counts = make([]uint64, len(h.buckets))
	}
	for i, upper := range h.buckets {
		if seconds <= upper {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.total++
	h.mu.Unlock()
}

func (h *promHistogram) WritePrometheus(buf *bytes.Buffer, name, help string) {
	h.mu.Lock()
	buckets := h.buckets
	if buckets == nil {
		buckets = defaultDurationBuckets
	}
	counts := make([]uint64, len(buckets))
	if len(h.counts) == len(buckets) {
		copy(counts, h.counts)
	}
	sum := h.sum
	total := h.total
	h.mu.Unlock()

	fmt.Fprintf(buf, "# HELP %s %s\n", name, help)
	fmt.Fprintf(buf, "# TYPE %s histogram\n", name)
	for i, upper := range buckets {
		fmt.Fprintf(buf, "%s_bucket{le=\"%g\"} %d\n", name, upper, counts[i])
	}
	fmt.Fprintf(buf, "%s_bucket{le=\"+Inf\"} %d\n", name, total)
	fmt.Fprintf(buf, "%s_sum %g\n", name, sum)
	fmt.Fprintf(buf, "%s_count %d\n", name, total)
}

func (h *promHistogram) Snapshot() (uint64, float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total, h.sum
}

// Metrics encapsulates Prometheus counters, gauges, and histograms for CubePod Gateway observability.
type Metrics struct {
	HTTPRequestsTotal             atomic.Uint64
	SandboxesCreatedTotal         atomic.Uint64
	AutoresumeTotal               atomic.Uint64
	CommandsExecutedTotal         atomic.Uint64
	DesktopActivationsOK          atomic.Uint64
	DesktopActivationsUnsupported atomic.Uint64
	DesktopActivationsError       atomic.Uint64
	PanelActiveSessions           atomic.Int64
	E2BVolumeMountsTotal          atomic.Uint64
	E2BVolumeMountErrorsTotal     atomic.Uint64
	CRDResyncsTotal               atomic.Uint64
	WatchStreamsActive            atomic.Int64
	WatchEventsTotal              atomic.Uint64
	WatchOverflowsTotal           atomic.Uint64

	e2bRequests        labeledCounterMap
	e2bRequestDuration promHistogram
	secretOperations   labeledCounterMap
	sandboxForks       labeledCounterMap
	sandboxForkDur     promHistogram

	SandboxMetricsSamplesOK    atomic.Uint64
	SandboxMetricsSamplesError atomic.Uint64

	sandboxMetricsOnce sync.Once
	sandboxMetrics     *sandboxMetricsStore
}

func (m *Metrics) sandboxMetricsStore() *sandboxMetricsStore {
	m.sandboxMetricsOnce.Do(func() {
		m.sandboxMetrics = newSandboxMetricsStore(defaultSandboxMetricHistoryCap)
	})
	return m.sandboxMetrics
}

// RecordE2BRequest records an E2B API request count and latency observation.
func (m *Metrics) RecordE2BRequest(endpoint, status string, dur time.Duration) {
	m.e2bRequests.Inc(endpoint, status)
	m.e2bRequestDuration.Observe(dur.Seconds())
}

// RecordSecretOperation records a secret management API operation count.
func (m *Metrics) RecordSecretOperation(op, status string) {
	m.secretOperations.Inc(op, status)
}

// RecordE2BVolumeMount increments the E2B volume mount counter.
func (m *Metrics) RecordE2BVolumeMount(n uint64) {
	m.E2BVolumeMountsTotal.Add(n)
}

// RecordE2BVolumeMountError increments the E2B volume mount error counter.
func (m *Metrics) RecordE2BVolumeMountError() {
	m.E2BVolumeMountErrorsTotal.Add(1)
}

// RecordSandboxFork records a sandbox fork operation count and duration observation.
func (m *Metrics) RecordSandboxFork(mode, result string, dur time.Duration) {
	m.sandboxForks.Inc(mode, result)
	m.sandboxForkDur.Observe(dur.Seconds())
}

// RecordCRDResync increments the CRD resync counter.
func (m *Metrics) RecordCRDResync() {
	m.CRDResyncsTotal.Add(1)
}

// Metrics returns the server's metrics counters.
func (s *Server) Metrics() *Metrics {
	if s.metrics == nil {
		s.metrics = &Metrics{}
	}
	return s.metrics
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.statusCode = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.statusCode = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func sanitizeHTTPMethodLabel(method string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return strings.ToUpper(strings.TrimSpace(method)), true
	default:
		return "", false
	}
}

func allowlistedSegment(seg string, allowed ...string) string {
	for _, a := range allowed {
		if seg == a {
			return seg
		}
	}
	return "unknown"
}

func classifyE2BEndpoint(method, cleanPath string) (string, bool) {
	if cleanPath == "" {
		return "", false
	}
	m, ok := sanitizeHTTPMethodLabel(method)
	if !ok {
		return "", false
	}
	parts := strings.Split(cleanPath, "/")
	switch parts[0] {
	case "sandboxes":
		if len(parts) == 1 {
			return m + " /sandboxes", true
		}
		if len(parts) == 2 {
			if parts[1] == "metrics" {
				return m + " /sandboxes/metrics", true
			}
			return m + " /sandboxes/{sandboxID}", true
		}
		switch parts[2] {
		case "metrics", "pause", "connect", "timeout", "refreshes", "lease", "fork", "reset-rootfs", "events":
			return m + " /sandboxes/{sandboxID}/" + parts[2], true
		case "commands":
			return m + " /sandboxes/{sandboxID}/commands", true
		case "code", "execute":
			return m + " /sandboxes/{sandboxID}/code/execute", true
		case "files":
			return m + " /sandboxes/{sandboxID}/files", true
		case "endpoints":
			return m + " /sandboxes/{sandboxID}/endpoints/{port}", true
		case "desktop":
			return m + " /sandboxes/{sandboxID}/desktop", true
		case "process.Process":
			if len(parts) >= 4 {
				rpc := allowlistedSegment(parts[3], "Start", "SendSignal", "Connect", "List", "SendInput", "Update", "StreamInput", "CloseStdin")
				return m + " /process.Process/" + rpc, true
			}
			return m + " /process.Process", true
		case "filesystem.Filesystem":
			if len(parts) >= 4 {
				rpc := allowlistedSegment(parts[3], "Read", "Write", "List", "ListDir", "Stat", "Remove", "MakeDir", "CreateWatcher", "GetWatcherEvents", "RemoveWatcher", "WatchDir")
				return m + " /filesystem.Filesystem/" + rpc, true
			}
			return m + " /filesystem.Filesystem", true
		default:
			return m + " /sandboxes/{sandboxID}", true
		}
	case "v2":
		if len(parts) >= 2 && parts[1] == "sandboxes" {
			if len(parts) >= 4 && parts[3] == "connect" {
				return m + " /sandboxes/{sandboxID}/connect", true
			}
			return m + " /sandboxes", true
		}
		if len(parts) >= 2 && parts[1] == "templates" {
			return m + " /templates", true
		}
	case "v1":
		if len(parts) >= 4 && parts[1] == "sandboxes" && parts[3] == "desktop" {
			return m + " /sandboxes/{sandboxID}/desktop", true
		}
	case "v3":
		if len(parts) >= 2 && parts[1] == "templates" {
			return m + " /templates", true
		}
	case "templates":
		return m + " /templates", true
	case "volumes":
		if len(parts) == 1 {
			return m + " /volumes", true
		}
		return m + " /volumes/{volumeID}", true
	case "volumecontent":
		if len(parts) >= 3 {
			op := allowlistedSegment(parts[2], "file", "dir", "path")
			return m + " /volumecontent/{volumeID}/" + op, true
		}
		return m + " /volumecontent", true
	case "secrets":
		if len(parts) == 1 {
			return m + " /secrets", true
		}
		return m + " /secrets/{secretID}", true
	case "webhooks":
		if len(parts) == 1 {
			return m + " /webhooks", true
		}
		return m + " /webhooks/{webhookID}", true
	case "events":
		return m + " /events", true
	case "files":
		return m + " /files", true
	case "execute":
		return m + " /execute", true
	case "process.Process":
		if len(parts) >= 2 {
			rpc := allowlistedSegment(parts[1], "Start", "SendSignal", "Connect", "List", "SendInput", "Update", "StreamInput", "CloseStdin")
			return m + " /process.Process/" + rpc, true
		}
		return m + " /process.Process", true
	case "filesystem.Filesystem":
		if len(parts) >= 2 {
			rpc := allowlistedSegment(parts[1], "Read", "Write", "List", "ListDir", "Stat", "Remove", "MakeDir", "CreateWatcher", "GetWatcherEvents", "RemoveWatcher", "WatchDir")
			return m + " /filesystem.Filesystem/" + rpc, true
		}
		return m + " /filesystem.Filesystem", true
	}
	return "", false
}

func classifySecretOp(method string, parts []string) (string, bool) {
	if len(parts) == 0 || parts[0] != "secrets" {
		return "", false
	}
	if len(parts) == 1 {
		switch method {
		case http.MethodPost:
			return "create", true
		case http.MethodGet:
			return "list", true
		}
		return "", false
	}
	if len(parts) == 2 {
		switch method {
		case http.MethodGet:
			return "get", true
		case http.MethodPost:
			return "update", true
		case http.MethodDelete:
			return "delete", true
		}
	}
	return "", false
}

// withMetrics wraps an http.Handler to record metrics on incoming requests,
// sandbox creations, command executions, E2B endpoints, forks, secrets, and auto-resume transitions.
func (s *Server) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m := s.Metrics()
		m.HTTPRequestsTotal.Add(1)

		cleanPath := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(cleanPath, "/")

		var beforeResumes int
		var trackedSandboxID string
		if len(parts) >= 2 && parts[0] == "sandboxes" && parts[1] != "metrics" {
			trackedSandboxID = parts[1]
			s.mu.RLock()
			if sb, ok := s.sandboxes[trackedSandboxID]; ok {
				beforeResumes = sb.AutoResumeCount
			}
			s.mu.RUnlock()
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)

		if endpoint, isE2B := classifyE2BEndpoint(r.Method, cleanPath); isE2B {
			m.RecordE2BRequest(endpoint, strconv.Itoa(rec.statusCode), dur)
		}

		// Check if a sandbox was created (POST /sandboxes or POST /v2/sandboxes with 201 Created)
		if r.Method == http.MethodPost && rec.statusCode == http.StatusCreated &&
			((len(parts) == 1 && parts[0] == "sandboxes") || (len(parts) == 2 && parts[0] == "v2" && parts[1] == "sandboxes")) {
			m.SandboxesCreatedTotal.Add(1)
		}

		// Record secret HTTP error when no Store processed the request (store tracks its own ops).
		if op, ok := classifySecretOp(r.Method, parts); ok && (s.secrets.Load() == nil || rec.statusCode == http.StatusUnauthorized || rec.statusCode == http.StatusNotImplemented || rec.statusCode == http.StatusBadRequest) {
			status := "ok"
			if rec.statusCode >= 400 {
				status = "error"
			}
			m.RecordSecretOperation(op, status)
		}

		// Check if a sandbox fork occurred (POST /sandboxes/{id}/fork)
		if r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "sandboxes" && parts[2] == "fork" {
			mode := "loopback"
			if s.LivePodRouting() {
				mode = "pvc_clone"
			}
			result := "ok"
			if rec.statusCode >= 400 {
				result = "error"
			}
			m.RecordSandboxFork(mode, result, dur)
		}

		// Check if a command was executed (POST /sandboxes/{id}/commands/run with 200 OK)
		if r.Method == http.MethodPost && len(parts) >= 4 && parts[0] == "sandboxes" && parts[2] == "commands" && parts[3] == "run" && rec.statusCode == http.StatusOK {
			m.CommandsExecutedTotal.Add(1)
		}

		// Check if autoresume occurred on tracked sandbox
		if trackedSandboxID != "" {
			s.mu.RLock()
			if sb, ok := s.sandboxes[trackedSandboxID]; ok {
				diff := sb.AutoResumeCount - beforeResumes
				if diff > 0 {
					m.AutoresumeTotal.Add(uint64(diff))
				}
			}
			s.mu.RUnlock()
		}
	})
}

// handleMetrics serves Prometheus text exposition format (version 0.0.4) at GET /metrics.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.Metrics()

	s.mu.RLock()
	var runningCount int
	var pausedCount int
	var totalAutoResumes int
	for _, sb := range s.sandboxes {
		switch sb.State {
		case string(v1alpha1.PodBoxPhaseRunning), "running":
			runningCount++
		case string(v1alpha1.PodBoxPhasePaused), "paused":
			pausedCount++
		}
		totalAutoResumes += sb.AutoResumeCount
	}
	s.mu.RUnlock()

	// Guarantee autoresume counter reflects at least the observed sandbox auto-resumes
	if uint64(totalAutoResumes) > m.AutoresumeTotal.Load() {
		m.AutoresumeTotal.Store(uint64(totalAutoResumes))
	}

	warmpoolsTotal := 0
	if s.reconciler != nil {
		warmpoolsTotal = len(s.reconciler.GetWarmPoolSpecs())
	}

	watchStreamsActive := m.WatchStreamsActive.Load()
	if watchStreamsActive < 0 {
		watchStreamsActive = 0
	}
	watchersActive := s.activePollWatchers()
	secretsStoreCount := 0
	var secretOpsSnapshot map[string]map[string]uint64
	var secretLatencySnapshot map[string]secret.LatencySnapshot
	if secStore := s.secrets.Load(); secStore != nil {
		secretsStoreCount = secStore.Count()
		secretOpsSnapshot = secStore.OperationsSnapshot()
		secretLatencySnapshot = secStore.OperationsLatencySnapshot()
	}
	volumeMountsTotal := s.E2BVolumeMountsTotal.Load() + m.E2BVolumeMountsTotal.Load()
	volumeMountErrorsTotal := s.E2BVolumeMountErrorsTotal.Load() + m.E2BVolumeMountErrorsTotal.Load()
	crdResyncsTotal := s.crdResyncsTotal.Load() + m.CRDResyncsTotal.Load()

	e2bEnabled := s.E2BTemplateAPIEnabled()
	var templateStatusCounts map[string]int
	if e2bEnabled {
		templateStatusCounts = map[string]int{
			v1alpha1.BuildStatusReady:    0,
			v1alpha1.BuildStatusBuilding: 0,
			v1alpha1.BuildStatusWaiting:  0,
			v1alpha1.BuildStatusError:    0,
		}
		if s.reconciler != nil && s.reconciler.KubeClient() != nil {
			s.syncTemplates(r.Context())
			if list, err := s.reconciler.KubeClient().ListPodTemplates(r.Context(), ""); err == nil {
				for _, tpl := range list {
					crStatus := tpl.Status.BuildStatus
					if crStatus == "" {
						if tpl.Status.ReadyImage != "" {
							crStatus = v1alpha1.BuildStatusReady
						} else {
							crStatus = v1alpha1.BuildStatusWaiting
						}
					}
					wireStatus := controller.WireBuildStatus(crStatus)
					templateStatusCounts[wireStatus]++
				}
			}
		}
	}
	templatesTotal := len(defaultTemplates)

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		e2bCount, e2bSum := m.e2bRequestDuration.Snapshot()
		forkCount, forkSum := m.sandboxForkDur.Snapshot()
		payload := map[string]interface{}{
			"cubepod_gateway_http_requests_total":      m.HTTPRequestsTotal.Load(),
			"cubepod_gateway_requests_total":           m.HTTPRequestsTotal.Load(),
			"cubepod_gateway_sandboxes_created_total":  m.SandboxesCreatedTotal.Load(),
			"cubepod_gateway_autoresume_total":         m.AutoresumeTotal.Load(),
			"cubepod_gateway_auto_resumes_total":       m.AutoresumeTotal.Load(),
			"cubepod_gateway_commands_executed_total":  m.CommandsExecutedTotal.Load(),
			"cubepod_gateway_sandboxes_active_running": runningCount,
			"cubepod_gateway_sandboxes_active_paused":  pausedCount,
			"cubepod_gateway_warmpools_total":          warmpoolsTotal,
			"cubepod_panel_active_sessions":            m.PanelActiveSessions.Load(),
			"cubepod_secrets_store_count":              secretsStoreCount,
			"cubepod_watch_streams_active":             watchStreamsActive,
			"cubepod_watchers_active":                  watchersActive,
			"cubepod_watch_events_total":               m.WatchEventsTotal.Load(),
			"cubepod_watch_overflows_total":            m.WatchOverflowsTotal.Load(),
			"cubepod_e2b_volume_mounts_total":          volumeMountsTotal,
			"cubepod_e2b_volume_mount_errors_total":    volumeMountErrorsTotal,
			"cubepod_gateway_crd_resyncs_total":        crdResyncsTotal,
			"cubepod_e2b_request_duration_seconds": map[string]interface{}{
				"count": e2bCount,
				"sum":   e2bSum,
			},
			"cubepod_sandbox_fork_duration_seconds": map[string]interface{}{
				"count": forkCount,
				"sum":   forkSum,
			},
			"cubepod_sandbox_metrics_samples_total": map[string]uint64{
				"ok":    m.SandboxMetricsSamplesOK.Load(),
				"error": m.SandboxMetricsSamplesError.Load(),
			},
			"cubepod_desktop_activations_total": map[string]uint64{
				"ok":          m.DesktopActivationsOK.Load(),
				"unsupported": m.DesktopActivationsUnsupported.Load(),
				"error":       m.DesktopActivationsError.Load(),
			},
		}
		if e2bEnabled {
			payload["cubepod_templates"] = templateStatusCounts
		} else {
			payload["cubepod_gateway_templates_total"] = templatesTotal
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var buf bytes.Buffer

	// Counters
	fmt.Fprintf(&buf, "# HELP cubepod_gateway_http_requests_total Total HTTP requests handled by the gateway\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_http_requests_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_http_requests_total %d\n", m.HTTPRequestsTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_requests_total Total HTTP requests handled by the gateway (CEP-0004)\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_requests_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_requests_total %d\n", m.HTTPRequestsTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_sandboxes_created_total Total sandboxes created\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_sandboxes_created_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_created_total %d\n", m.SandboxesCreatedTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_autoresume_total Total sandbox auto-resume events\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_autoresume_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_autoresume_total %d\n", m.AutoresumeTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_auto_resumes_total Total sandbox auto-resume events (CEP-0004)\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_auto_resumes_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_auto_resumes_total %d\n", m.AutoresumeTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_commands_executed_total Total commands executed in sandboxes\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_commands_executed_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_commands_executed_total %d\n", m.CommandsExecutedTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_e2b_requests_total Total E2B-compatible API requests by endpoint and status\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_e2b_requests_total counter\n")
	for _, entry := range m.e2bRequests.Snapshot() {
		fmt.Fprintf(&buf, "cubepod_e2b_requests_total{endpoint=%q,status=%q} %d\n", entry.key.first, entry.key.second, entry.val)
	}

	m.e2bRequestDuration.WritePrometheus(&buf, "cubepod_e2b_request_duration_seconds", "Latency histogram of E2B-compatible API requests in seconds")

	fmt.Fprintf(&buf, "# HELP cubepod_secrets_operations_total Total E2B secret store operations by operation and status\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_secrets_operations_total counter\n")
	mergedSecretOps := make(map[labelPair]uint64)
	for op, statuses := range secretOpsSnapshot {
		for st, cnt := range statuses {
			mergedSecretOps[labelPair{first: op, second: st}] += cnt
		}
	}
	for _, entry := range m.secretOperations.Snapshot() {
		mergedSecretOps[entry.key] += entry.val
	}
	secretOpsList := make([]struct {
		key labelPair
		val uint64
	}, 0, len(mergedSecretOps))
	for k, v := range mergedSecretOps {
		secretOpsList = append(secretOpsList, struct {
			key labelPair
			val uint64
		}{key: k, val: v})
	}
	sort.Slice(secretOpsList, func(i, j int) bool {
		if secretOpsList[i].key.first != secretOpsList[j].key.first {
			return secretOpsList[i].key.first < secretOpsList[j].key.first
		}
		return secretOpsList[i].key.second < secretOpsList[j].key.second
	})
	for _, entry := range secretOpsList {
		fmt.Fprintf(&buf, "cubepod_secrets_operations_total{op=%q,status=%q} %d\n", entry.key.first, entry.key.second, entry.val)
	}

	fmt.Fprintf(&buf, "# HELP cubepod_e2b_volume_mounts_total Total E2B volume mounts attached to sandboxes\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_e2b_volume_mounts_total counter\n")
	fmt.Fprintf(&buf, "cubepod_e2b_volume_mounts_total %d\n", volumeMountsTotal)

	fmt.Fprintf(&buf, "# HELP cubepod_e2b_volume_mount_errors_total Total E2B volume mount planning or attachment errors\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_e2b_volume_mount_errors_total counter\n")
	fmt.Fprintf(&buf, "cubepod_e2b_volume_mount_errors_total %d\n", volumeMountErrorsTotal)

	fmt.Fprintf(&buf, "# HELP cubepod_sandbox_forks_total Total sandbox CoW fork operations by mode and result\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_sandbox_forks_total counter\n")
	for _, entry := range m.sandboxForks.Snapshot() {
		fmt.Fprintf(&buf, "cubepod_sandbox_forks_total{mode=%q,result=%q} %d\n", entry.key.first, entry.key.second, entry.val)
	}

	m.sandboxForkDur.WritePrometheus(&buf, "cubepod_sandbox_fork_duration_seconds", "Latency histogram of sandbox CoW fork operations in seconds")

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_crd_resyncs_total Total PodBox/PodWarmPool CRD resyncs executed by the gateway\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_crd_resyncs_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_crd_resyncs_total %d\n", crdResyncsTotal)

	fmt.Fprintf(&buf, "# HELP cubepod_desktop_activations_total Total desktop stack activations by result\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_desktop_activations_total counter\n")
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"ok\"} %d\n", m.DesktopActivationsOK.Load())
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"unsupported\"} %d\n", m.DesktopActivationsUnsupported.Load())
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"error\"} %d\n", m.DesktopActivationsError.Load())

	if evMgr := s.events.Load(); evMgr != nil {
		fmt.Fprintf(&buf, "# HELP cubepod_events_recorded_total Total sandbox lifecycle events recorded\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_events_recorded_total counter\n")
		fmt.Fprintf(&buf, "cubepod_events_recorded_total %d\n", evMgr.EventsRecordedTotal.Load())

		fmt.Fprintf(&buf, "# HELP cubepod_webhook_deliveries_total Total webhook delivery attempts by result\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_webhook_deliveries_total counter\n")
		fmt.Fprintf(&buf, "cubepod_webhook_deliveries_total{result=\"ok\"} %d\n", evMgr.WebhookDeliveriesOK.Load())
		fmt.Fprintf(&buf, "cubepod_webhook_deliveries_total{result=\"failed\"} %d\n", evMgr.WebhookDeliveriesFailed.Load())
		fmt.Fprintf(&buf, "cubepod_webhook_deliveries_total{result=\"dropped\"} %d\n", evMgr.WebhookDeliveriesDrop.Load())

		fmt.Fprintf(&buf, "# HELP cubepod_webhook_queue_depth Current number of queued webhook deliveries\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_webhook_queue_depth gauge\n")
		fmt.Fprintf(&buf, "cubepod_webhook_queue_depth %d\n", evMgr.QueueDepth())
	}

	writeSandboxMetricsSamplerProm(&buf, m)

	// Gauges
	fmt.Fprintf(&buf, "# HELP cubepod_gateway_sandboxes_active Current number of active sandboxes by state\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_sandboxes_active gauge\n")
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_active{state=\"Running\"} %d\n", runningCount)
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_active{state=\"Paused\"} %d\n", pausedCount)

	fmt.Fprintf(&buf, "# HELP cubepod_panel_active_sessions Current number of active panel proxy HTTP/WebSocket sessions\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_panel_active_sessions gauge\n")
	fmt.Fprintf(&buf, "cubepod_panel_active_sessions %d\n", m.PanelActiveSessions.Load())

	if len(secretLatencySnapshot) > 0 {
		fmt.Fprintf(&buf, "# HELP cubepod_secrets_operation_duration_seconds Latency of E2B secret store operations by operation\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_secrets_operation_duration_seconds histogram\n")
		ops := make([]string, 0, len(secretLatencySnapshot))
		for op := range secretLatencySnapshot {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			snap := secretLatencySnapshot[op]
			for i, upper := range snap.Buckets {
				fmt.Fprintf(&buf, "cubepod_secrets_operation_duration_seconds_bucket{operation=%q,le=\"%g\"} %d\n", op, upper, snap.Counts[i])
			}
			fmt.Fprintf(&buf, "cubepod_secrets_operation_duration_seconds_bucket{operation=%q,le=\"+Inf\"} %d\n", op, snap.Count)
			fmt.Fprintf(&buf, "cubepod_secrets_operation_duration_seconds_sum{operation=%q} %g\n", op, snap.Sum)
			fmt.Fprintf(&buf, "cubepod_secrets_operation_duration_seconds_count{operation=%q} %d\n", op, snap.Count)
		}
	}

	fmt.Fprintf(&buf, "# HELP cubepod_watch_streams_active Current number of live WatchDir streams\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_watch_streams_active gauge\n")
	fmt.Fprintf(&buf, "cubepod_watch_streams_active %d\n", watchStreamsActive)
	fmt.Fprintf(&buf, "# HELP cubepod_watchers_active Current number of active poll watchers\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_watchers_active gauge\n")
	fmt.Fprintf(&buf, "cubepod_watchers_active %d\n", watchersActive)
	fmt.Fprintf(&buf, "# HELP cubepod_watch_events_total Total filesystem watch events emitted by the gateway\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_watch_events_total counter\n")
	fmt.Fprintf(&buf, "cubepod_watch_events_total %d\n", m.WatchEventsTotal.Load())
	fmt.Fprintf(&buf, "# HELP cubepod_watch_overflows_total Total filesystem watch resource_exhausted overflows\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_watch_overflows_total counter\n")
	fmt.Fprintf(&buf, "cubepod_watch_overflows_total %d\n", m.WatchOverflowsTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_secrets_store_count Current number of stored E2B secrets across tenants\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_secrets_store_count gauge\n")
	fmt.Fprintf(&buf, "cubepod_secrets_store_count %d\n", secretsStoreCount)

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_warmpools_total Total configured warm pools\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_warmpools_total gauge\n")
	fmt.Fprintf(&buf, "cubepod_gateway_warmpools_total %d\n", warmpoolsTotal)

	if e2bEnabled {
		fmt.Fprintf(&buf, "# HELP cubepod_templates Current number of templates by build status\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_templates gauge\n")
		for _, status := range []string{
			v1alpha1.BuildStatusReady,
			v1alpha1.BuildStatusBuilding,
			v1alpha1.BuildStatusWaiting,
			v1alpha1.BuildStatusError,
		} {
			fmt.Fprintf(&buf, "cubepod_templates{status=%q} %d\n", status, templateStatusCounts[status])
		}
	} else {
		fmt.Fprintf(&buf, "# HELP cubepod_gateway_templates_total Total registered or available templates\n")
		fmt.Fprintf(&buf, "# TYPE cubepod_gateway_templates_total gauge\n")
		fmt.Fprintf(&buf, "cubepod_gateway_templates_total %d\n", templatesTotal)
	}

	_, _ = w.Write(buf.Bytes())
}
