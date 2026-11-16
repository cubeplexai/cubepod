package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/pkg/events"
)

const (
	defaultSandboxMetricHistoryCap = 120
	poddMetricsTimeout             = 5 * time.Second
)

// SandboxMetric represents an E2B-compatible point-in-time resource utilization
// snapshot for a sandbox.
type SandboxMetric struct {
	Timestamp     string  `json:"timestamp"`
	TimestampUnix int64   `json:"timestampUnix"`
	CPUCount      int     `json:"cpuCount"`
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	MemUsed       uint64  `json:"memUsed"`
	MemTotal      uint64  `json:"memTotal"`
	MemCache      uint64  `json:"memCache"`
	DiskUsed      uint64  `json:"diskUsed"`
	DiskTotal     uint64  `json:"diskTotal"`
}

type sandboxMetricRing struct {
	cap     int
	samples []SandboxMetric
}

func (r *sandboxMetricRing) push(m SandboxMetric) {
	if r.cap <= 0 {
		r.cap = defaultSandboxMetricHistoryCap
	}
	if len(r.samples) >= r.cap {
		copy(r.samples, r.samples[1:])
		r.samples[len(r.samples)-1] = m
		return
	}
	r.samples = append(r.samples, m)
}

func (r *sandboxMetricRing) query(startUnix, endUnix *int64) []SandboxMetric {
	out := make([]SandboxMetric, 0, len(r.samples))
	for _, s := range r.samples {
		if startUnix != nil && s.TimestampUnix < *startUnix {
			continue
		}
		if endUnix != nil && s.TimestampUnix > *endUnix {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *sandboxMetricRing) latest() (SandboxMetric, bool) {
	if len(r.samples) == 0 {
		return SandboxMetric{}, false
	}
	return r.samples[len(r.samples)-1], true
}

type sandboxMetricsStore struct {
	mu    sync.RWMutex
	cap   int
	rings map[string]*sandboxMetricRing
}

func newSandboxMetricsStore(capPerSandbox int) *sandboxMetricsStore {
	if capPerSandbox <= 0 {
		capPerSandbox = defaultSandboxMetricHistoryCap
	}
	return &sandboxMetricsStore{
		cap:   capPerSandbox,
		rings: make(map[string]*sandboxMetricRing),
	}
}

func (st *sandboxMetricsStore) record(sandboxID string, m SandboxMetric) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ring, ok := st.rings[sandboxID]
	if !ok {
		ring = &sandboxMetricRing{cap: st.cap}
		st.rings[sandboxID] = ring
	}
	ring.push(m)
}

func (st *sandboxMetricsStore) query(sandboxID string, startUnix, endUnix *int64) []SandboxMetric {
	st.mu.RLock()
	defer st.mu.RUnlock()
	ring, ok := st.rings[sandboxID]
	if !ok {
		return []SandboxMetric{}
	}
	return ring.query(startUnix, endUnix)
}

func (st *sandboxMetricsStore) latest(sandboxID string) (SandboxMetric, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	ring, ok := st.rings[sandboxID]
	if !ok {
		return SandboxMetric{}, false
	}
	return ring.latest()
}

func (st *sandboxMetricsStore) delete(sandboxID string) {
	if sandboxID == "" {
		return
	}
	st.mu.Lock()
	delete(st.rings, sandboxID)
	st.mu.Unlock()
}

func parseMetricTimestamp(raw string) (*int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	if sec, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return &sec, nil
	}
	if ts, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		sec := ts.UTC().Unix()
		return &sec, nil
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		sec := ts.UTC().Unix()
		return &sec, nil
	}
	return nil, fmt.Errorf("invalid timestamp %q: expected unix seconds or RFC3339", raw)
}

func (s *Server) fetchPoddMetric(ctx context.Context, poddURL string) (SandboxMetric, error) {
	if poddURL == "" {
		return SandboxMetric{}, errors.New("empty poddURL")
	}
	reqCtx, cancel := context.WithTimeout(ctx, poddMetricsTimeout)
	defer cancel()

	target := strings.TrimRight(poddURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return SandboxMetric{}, err
	}
	req.Header.Set("Accept", "application/json")

	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: poddMetricsTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return SandboxMetric{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return SandboxMetric{}, fmt.Errorf("podd /metrics returned status %d", resp.StatusCode)
	}

	var m SandboxMetric
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return SandboxMetric{}, err
	}
	now := time.Now().UTC()
	if m.Timestamp == "" && m.TimestampUnix == 0 {
		m.Timestamp = now.Format(time.RFC3339)
		m.TimestampUnix = now.Unix()
	} else if m.Timestamp == "" {
		m.Timestamp = time.Unix(m.TimestampUnix, 0).UTC().Format(time.RFC3339)
	} else if m.TimestampUnix == 0 {
		if parsed, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
			m.TimestampUnix = parsed.UTC().Unix()
		}
	}
	return m, nil
}

func (s *Server) sampleLocalSandboxMetric(rec *SandboxRecord) SandboxMetric {
	now := time.Now().UTC()
	cpuCount := 2
	var memLimitBytes uint64 = 2048 * 1024 * 1024
	diskSizeMB := 10240
	var workdir string

	s.mu.RLock()
	baseDir := s.workspaceBaseDir
	if rec != nil {
		if rec.CPUCount > 0 {
			cpuCount = rec.CPUCount
		}
		if rec.MemoryMB > 0 {
			memLimitBytes = uint64(rec.MemoryMB) * 1024 * 1024
		}
		if rec.DiskSizeMB > 0 {
			diskSizeMB = rec.DiskSizeMB
		}
		workdir = rec.WorkspaceDir
	}
	s.mu.RUnlock()

	cpuPct := sampleHostCPUPct(cpuCount)
	memUsed, memCache := sampleHostMemoryCapped(memLimitBytes)
	diskUsed, diskTotal := sampleWorkspaceDisk(workdir, baseDir, diskSizeMB)

	return SandboxMetric{
		Timestamp:     now.Format(time.RFC3339),
		TimestampUnix: now.Unix(),
		CPUCount:      cpuCount,
		CPUUsedPct:    cpuPct,
		MemUsed:       memUsed,
		MemTotal:      memLimitBytes,
		MemCache:      memCache,
		DiskUsed:      diskUsed,
		DiskTotal:     diskTotal,
	}
}

func sampleHostCPUPct(cpuCount int) float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(load1) || math.IsInf(load1, 0) || load1 < 0 {
		return 0
	}
	if cpuCount <= 0 {
		cpuCount = 1
	}
	pct := (load1 / float64(cpuCount)) * 100.0
	if pct > 100 {
		pct = 100
	}
	return math.Round(pct*100) / 100
}

func sampleHostMemoryCapped(limitBytes uint64) (usedBytes, cacheBytes uint64) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return limitBytes / 8, limitBytes / 16
	}
	var totalKB, availKB, cachedKB, buffersKB uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, convErr := strconv.ParseUint(fields[1], 10, 64)
		if convErr != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB = val
		case "MemAvailable":
			availKB = val
		case "Cached":
			cachedKB = val
		case "Buffers":
			buffersKB = val
		}
	}
	if totalKB == 0 {
		return limitBytes / 8, limitBytes / 16
	}
	hostTotal := totalKB * 1024
	hostAvail := availKB * 1024
	hostUsed := uint64(0)
	if hostTotal > hostAvail {
		hostUsed = hostTotal - hostAvail
	}
	hostCache := (cachedKB + buffersKB) * 1024
	if hostUsed > limitBytes {
		hostUsed = limitBytes
	}
	if hostCache > limitBytes {
		hostCache = limitBytes
	}
	return hostUsed, hostCache
}

func sampleWorkspaceDisk(workdir, baseDir string, diskSizeMB int) (usedBytes, totalBytes uint64) {
	candidates := []string{workdir, baseDir, os.TempDir(), "/"}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := st.Blocks * bsize
		free := st.Bfree * bsize
		used := uint64(0)
		if total > free {
			used = total - free
		}
		if diskSizeMB > 0 {
			specTotal := uint64(diskSizeMB) * 1024 * 1024
			if total == 0 {
				total = specTotal
			}
		}
		if used > total {
			used = total
		}
		return used, total
	}
	specTotal := uint64(diskSizeMB) * 1024 * 1024
	return 0, specTotal
}

func (s *Server) collectSandboxMetric(ctx context.Context, rec *SandboxRecord) SandboxMetric {
	s.mu.RLock()
	poddURL := ""
	if rec != nil {
		poddURL = rec.PoddURL
	}
	s.mu.RUnlock()

	if poddURL != "" {
		if m, err := s.fetchPoddMetric(ctx, poddURL); err == nil {
			return m
		}
	}
	return s.sampleLocalSandboxMetric(rec)
}

func (s *Server) authorizeSandboxMetricsAccess(r *http.Request, rec *SandboxRecord) (authorized bool, missingAPIKey bool) {
	if rec == nil {
		return false, false
	}
	s.mu.RLock()
	ownerTenant := rec.OwnerTenantHash
	s.mu.RUnlock()
	if ownerTenant == "" {
		return true, false
	}
	key := managementAPIKey(r)
	if key == "" {
		return false, true
	}
	return events.TenantHash(key) == ownerTenant, false
}

// handleGetSandboxMetrics implements E2B GET /sandboxes/{sandboxID}/metrics.
func (s *Server) handleGetSandboxMetrics(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), sandboxID)
	if !ok {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if allowed, missingKey := s.authorizeSandboxMetricsAccess(r, rec); !allowed {
		if missingKey {
			writeError(w, http.StatusUnauthorized, "API key is required")
		} else {
			writeError(w, http.StatusNotFound, "sandbox not found")
		}
		return
	}

	startUnix, err := parseMetricTimestamp(r.URL.Query().Get("start"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	endUnix, err := parseMetricTimestamp(r.URL.Query().Get("end"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if startUnix != nil && endUnix != nil && *startUnix > *endUnix {
		writeError(w, http.StatusBadRequest, "start timestamp must not be after end timestamp")
		return
	}

	nowUnix := time.Now().UTC().Unix()
	historicalOnly := endUnix != nil && *endUnix < nowUnix
	autoResume := !historicalOnly && !strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("autoResume")), "false")
	if autoResume {
		if err := s.ensureRunning(r.Context(), rec); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resume sandbox: "+err.Error())
			return
		}
	}

	store := s.Metrics().sandboxMetricsStore()

	s.mu.RLock()
	state := rec.State
	s.mu.RUnlock()

	if !historicalOnly && !strings.EqualFold(state, "Paused") && !strings.EqualFold(state, "Terminated") {
		sample := s.collectSandboxMetric(r.Context(), rec)
		store.record(sandboxID, sample)
	}

	series := store.query(sandboxID, startUnix, endUnix)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(series)
}

// handleBatchSandboxMetrics implements E2B GET /sandboxes/metrics.
func (s *Server) handleBatchSandboxMetrics(w http.ResponseWriter, r *http.Request) {
	var requestedIDs []string
	seen := make(map[string]struct{})
	for _, rawParam := range r.URL.Query()["sandboxIds"] {
		for _, part := range strings.Split(rawParam, ",") {
			id := strings.TrimSpace(part)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; !exists {
				seen[id] = struct{}{}
				requestedIDs = append(requestedIDs, id)
			}
		}
	}

	if len(requestedIDs) == 0 {
		if s.LivePodRouting() && s.FeatureGateEnabled(FeatureGateStatelessGatewayHA) && s.reconciler != nil && s.reconciler.KubeClient() != nil {
			if boxes, err := s.reconciler.KubeClient().ListPodBoxes(r.Context(), ""); err == nil {
				for i := range boxes {
					if boxes[i].Metadata.Name != "" {
						_, _ = s.resolveSandbox(r.Context(), boxes[i].Metadata.Name)
					}
				}
			}
		}
		s.mu.RLock()
		for id := range s.sandboxes {
			requestedIDs = append(requestedIDs, id)
		}
		s.mu.RUnlock()
	}

	store := s.Metrics().sandboxMetricsStore()
	sandboxesMap := make(map[string]SandboxMetric, len(requestedIDs))
	for _, id := range requestedIDs {
		rec, ok := s.resolveSandbox(r.Context(), id)
		if !ok {
			continue
		}
		if allowed, _ := s.authorizeSandboxMetricsAccess(r, rec); !allowed {
			continue
		}
		s.mu.RLock()
		state := rec.State
		s.mu.RUnlock()

		if !strings.EqualFold(state, "Paused") && !strings.EqualFold(state, "Terminated") {
			sample := s.collectSandboxMetric(r.Context(), rec)
			store.record(id, sample)
			sandboxesMap[id] = sample
		} else if cached, hasCached := store.latest(id); hasCached {
			sandboxesMap[id] = cached
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sandboxes": sandboxesMap,
	})
}
