package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

var internalHex64Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ControllerManager orchestrates the background reconciliation loop (reaping idle/paused sandboxes,
// replenishing warmpools, sweeping template contexts) and serves HTTP /livez, /healthz, /metrics,
// and internal template-build endpoints.
type ControllerManager struct {
	reconciler *Reconciler
	addr       string
	interval   time.Duration
	server     *http.Server
	mu         sync.Mutex
}

// NewControllerManager creates a new ControllerManager instance.
func NewControllerManager(reconciler *Reconciler, addr string, interval time.Duration) *ControllerManager {
	if addr == "" {
		addr = ":8081"
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ControllerManager{
		reconciler: reconciler,
		addr:       addr,
		interval:   interval,
	}
}

// Handler returns the HTTP mux for /livez, /healthz, /metrics, and internal template-build endpoints.
func (m *ControllerManager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", m.handleLivez)
	mux.HandleFunc("GET /healthz", m.handleHealthz)
	mux.HandleFunc("GET /metrics", m.handleMetrics)
	// Combine HEAD .../context/{hash} and GET .../{buildID}/logs under one GET pattern
	// (GET matches HEAD in net/http.ServeMux and avoids pattern ambiguity on /context/logs).
	mux.HandleFunc("GET /internal/v1/template-builds/{seg1}/{seg2}", func(w http.ResponseWriter, r *http.Request) {
		seg1 := r.PathValue("seg1")
		seg2 := r.PathValue("seg2")
		if r.Method == http.MethodHead && seg1 == "context" {
			r.SetPathValue("hash", seg2)
			m.handleInternalHeadContext(w, r)
			return
		}
		if r.Method == http.MethodGet && seg2 == "logs" {
			r.SetPathValue("buildID", seg1)
			m.handleInternalGetBuildLogs(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("PUT /internal/v1/template-builds/context/{hash}", m.handleInternalPutContext)
	mux.HandleFunc("POST /internal/v1/template-builds/{templateID}/{buildID}/start", m.handleInternalStartBuild)
	return mux
}

func writeControllerJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": msg,
	})
}

func (m *ControllerManager) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"component": "cubepod-controller",
	})
}

func (m *ControllerManager) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if m.reconciler != nil && m.reconciler.TemplateBuildEnabled() {
		if err := m.reconciler.PingBuilder(r.Context()); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":    "unhealthy",
				"component": "cubepod-controller",
				"error":     err.Error(),
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"component": "cubepod-controller",
	})
}

func (m *ControllerManager) templateStatusCounts() (map[string]int, bool) {
	counts := map[string]int{
		v1alpha1.BuildStatusReady:    0,
		v1alpha1.BuildStatusBuilding: 0,
		v1alpha1.BuildStatusWaiting:  0,
		v1alpha1.BuildStatusError:    0,
	}
	if m.reconciler == nil {
		return counts, false
	}
	m.reconciler.mu.RLock()
	defer m.reconciler.mu.RUnlock()
	show := m.reconciler.e2bTemplateAPI || len(m.reconciler.templateObjects) > 0
	for _, tpl := range m.reconciler.templateObjects {
		crStatus := tpl.Status.BuildStatus
		if crStatus == "" {
			if tpl.Status.ReadyImage != "" {
				crStatus = v1alpha1.BuildStatusReady
			} else {
				crStatus = v1alpha1.BuildStatusWaiting
			}
		}
		counts[WireBuildStatus(crStatus)]++
	}
	return counts, show
}

func (m *ControllerManager) handleMetrics(w http.ResponseWriter, r *http.Request) {
	active := m.reconciler.ActiveSandboxesCount()
	paused := m.reconciler.PausedSandboxesCount()
	warmReady := m.reconciler.WarmPoolReadyCount()
	tplCounts, showTemplates := m.templateStatusCounts()
	buildMetrics := m.reconciler.TemplateBuildMetrics()
	forkMetrics := m.reconciler.ForkMetrics()
	phaseMetrics := m.reconciler.ReconcilePhaseMetrics()

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		phaseSummary := make(map[string]map[string]any)
		for _, s := range phaseMetrics {
			if phaseSummary[s.Operation] == nil {
				phaseSummary[s.Operation] = make(map[string]any)
			}
			phaseSummary[s.Operation][s.Phase] = map[string]any{
				"count": s.Count,
				"sum":   s.Sum,
			}
		}
		payload := map[string]any{
			"cubepod_active_sandboxes":                      active,
			"cubepod_paused_sandboxes":                      paused,
			"cubepod_warmpool_ready":                        warmReady,
			"cubepod_warmpool_available_replicas":           warmReady,
			"cubepod_template_builds_total":                 buildMetrics.TemplateBuildsTotal,
			"cubepod_template_build_duration_seconds_sum":   buildMetrics.TemplateBuildDurationSum,
			"cubepod_template_build_duration_seconds_count": buildMetrics.TemplateBuildDurationCount,
			"cubepod_sandbox_forks_total":                   forkMetrics.ForksTotal,
			"cubepod_sandbox_fork_duration_seconds_sum":     forkMetrics.ForkDurationSum,
			"cubepod_sandbox_fork_duration_seconds_count":   forkMetrics.ForkDurationCount,
			"cubepod_reconcile_phase_duration_seconds":      phaseSummary,
		}
		if showTemplates {
			payload["cubepod_templates"] = tplCounts
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP cubepod_active_sandboxes Current active running sandboxes\n")
	fmt.Fprintf(w, "# TYPE cubepod_active_sandboxes gauge\n")
	fmt.Fprintf(w, "cubepod_active_sandboxes %d\n", active)
	fmt.Fprintf(w, "# HELP cubepod_paused_sandboxes Current paused sandboxes\n")
	fmt.Fprintf(w, "# TYPE cubepod_paused_sandboxes gauge\n")
	fmt.Fprintf(w, "cubepod_paused_sandboxes %d\n", paused)
	fmt.Fprintf(w, "# HELP cubepod_warmpool_ready Ready warm standby sandboxes in warmpools\n")
	fmt.Fprintf(w, "# TYPE cubepod_warmpool_ready gauge\n")
	fmt.Fprintf(w, "cubepod_warmpool_ready %d\n", warmReady)
	fmt.Fprintf(w, "# HELP cubepod_warmpool_available_replicas Available unclaimed standby replicas across warmpools\n")
	fmt.Fprintf(w, "# TYPE cubepod_warmpool_available_replicas gauge\n")
	fmt.Fprintf(w, "cubepod_warmpool_available_replicas %d\n", warmReady)

	if showTemplates {
		fmt.Fprintf(w, "# HELP cubepod_templates Current number of templates by build status\n")
		fmt.Fprintf(w, "# TYPE cubepod_templates gauge\n")
		for _, st := range []string{
			v1alpha1.BuildStatusReady,
			v1alpha1.BuildStatusBuilding,
			v1alpha1.BuildStatusWaiting,
			v1alpha1.BuildStatusError,
		} {
			fmt.Fprintf(w, "cubepod_templates{status=%q} %d\n", st, tplCounts[st])
		}
	}

	fmt.Fprintf(w, "# HELP cubepod_template_builds_total Total template builds completed by result\n")
	fmt.Fprintf(w, "# TYPE cubepod_template_builds_total counter\n")
	for _, res := range []string{v1alpha1.BuildStatusReady, v1alpha1.BuildStatusError} {
		fmt.Fprintf(w, "cubepod_template_builds_total{result=%q} %d\n", res, buildMetrics.TemplateBuildsTotal[res])
	}

	fmt.Fprintf(w, "# HELP cubepod_template_build_duration_seconds Duration of template image builds in seconds\n")
	fmt.Fprintf(w, "# TYPE cubepod_template_build_duration_seconds histogram\n")
	for _, b := range []struct {
		key string
		le  string
	}{
		{"le_5", "5"},
		{"le_15", "15"},
		{"le_60", "60"},
		{"le_300", "300"},
		{"le_inf", "+Inf"},
	} {
		fmt.Fprintf(w, "cubepod_template_build_duration_seconds_bucket{le=%q} %d\n", b.le, buildMetrics.TemplateBuildDurationBuckets[b.key])
	}
	fmt.Fprintf(w, "cubepod_template_build_duration_seconds_sum %g\n", buildMetrics.TemplateBuildDurationSum)
	fmt.Fprintf(w, "cubepod_template_build_duration_seconds_count %d\n", buildMetrics.TemplateBuildDurationCount)

	fmt.Fprintf(w, "# HELP cubepod_sandbox_forks_total Total sandbox CoW forks attempted by mode and result\n")
	fmt.Fprintf(w, "# TYPE cubepod_sandbox_forks_total counter\n")
	for _, mode := range []string{"pvc_clone", "snapshot"} {
		for _, res := range []string{"ok", "error"} {
			var val int64
			if forkMetrics.ForksTotal != nil && forkMetrics.ForksTotal[mode] != nil {
				val = forkMetrics.ForksTotal[mode][res]
			}
			fmt.Fprintf(w, "cubepod_sandbox_forks_total{mode=%q,result=%q} %d\n", mode, res, val)
		}
	}

	fmt.Fprintf(w, "# HELP cubepod_sandbox_fork_duration_seconds Duration of sandbox CoW fork provisioning in seconds\n")
	fmt.Fprintf(w, "# TYPE cubepod_sandbox_fork_duration_seconds histogram\n")
	for _, b := range []struct {
		key string
		le  string
	}{
		{"le_0_1", "0.1"},
		{"le_0_5", "0.5"},
		{"le_1", "1"},
		{"le_5", "5"},
		{"le_15", "15"},
		{"le_60", "60"},
		{"le_inf", "+Inf"},
	} {
		fmt.Fprintf(w, "cubepod_sandbox_fork_duration_seconds_bucket{le=%q} %d\n", b.le, forkMetrics.ForkDurationBuckets[b.key])
	}
	fmt.Fprintf(w, "cubepod_sandbox_fork_duration_seconds_sum %g\n", forkMetrics.ForkDurationSum)
	fmt.Fprintf(w, "cubepod_sandbox_fork_duration_seconds_count %d\n", forkMetrics.ForkDurationCount)

	if len(phaseMetrics) > 0 {
		fmt.Fprintf(w, "# HELP cubepod_reconcile_phase_duration_seconds Latency histogram of controller reconciliation phases in seconds\n")
		fmt.Fprintf(w, "# TYPE cubepod_reconcile_phase_duration_seconds histogram\n")
		for _, s := range phaseMetrics {
			for i, upper := range s.Buckets {
				fmt.Fprintf(w, "cubepod_reconcile_phase_duration_seconds_bucket{operation=%q,phase=%q,le=\"%g\"} %d\n", s.Operation, s.Phase, upper, s.Counts[i])
			}
			fmt.Fprintf(w, "cubepod_reconcile_phase_duration_seconds_bucket{operation=%q,phase=%q,le=\"+Inf\"} %d\n", s.Operation, s.Phase, s.Count)
			fmt.Fprintf(w, "cubepod_reconcile_phase_duration_seconds_sum{operation=%q,phase=%q} %g\n", s.Operation, s.Phase, s.Sum)
			fmt.Fprintf(w, "cubepod_reconcile_phase_duration_seconds_count{operation=%q,phase=%q} %d\n", s.Operation, s.Phase, s.Count)
		}
	}
}

func (m *ControllerManager) handleInternalHeadContext(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !internalHex64Regex.MatchString(hash) {
		writeControllerJSONError(w, http.StatusBadRequest, "hash must be 64 lowercase hex characters")
		return
	}
	if m.reconciler == nil || m.reconciler.ContextStore() == nil {
		writeControllerJSONError(w, http.StatusInternalServerError, "context store is not configured")
		return
	}
	if m.reconciler.ContextStore().IsPresent(hash) {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (m *ControllerManager) handleInternalPutContext(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !internalHex64Regex.MatchString(hash) {
		writeControllerJSONError(w, http.StatusBadRequest, "hash must be 64 lowercase hex characters")
		return
	}
	if m.reconciler == nil || m.reconciler.ContextStore() == nil {
		writeControllerJSONError(w, http.StatusInternalServerError, "context store is not configured")
		return
	}
	if err := m.reconciler.ContextStore().Store(hash, r.Body); err != nil {
		switch {
		case errors.Is(err, ErrContextTooLarge):
			writeControllerJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, ErrTarMismatch):
			writeControllerJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrInvalidContextTar), errors.Is(err, ErrPathTraversal):
			writeControllerJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeControllerJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "stored"})
}

func (m *ControllerManager) handleInternalStartBuild(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	buildID := r.PathValue("buildID")
	if templateID == "" || buildID == "" {
		writeControllerJSONError(w, http.StatusBadRequest, "templateID and buildID are required")
		return
	}
	var req StartBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeControllerJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if m.reconciler == nil {
		writeControllerJSONError(w, http.StatusInternalServerError, "reconciler is not configured")
		return
	}
	if m.reconciler.KubeClient() != nil {
		_ = m.reconciler.SyncTemplatesFromKube(r.Context())
	}
	if err := m.reconciler.StartBuildAsync(context.WithoutCancel(r.Context()), templateID, buildID, req); err != nil {
		switch {
		case errors.Is(err, ErrTemplateBuildDisabled) || strings.HasPrefix(err.Error(), "501"):
			writeControllerJSONError(w, http.StatusNotImplemented, "template build is not enabled")
		case errors.Is(err, ErrTemplateNotFound) || strings.HasPrefix(err.Error(), "404"):
			writeControllerJSONError(w, http.StatusNotFound, "template not found")
		case errors.Is(err, ErrTemplateNotReady) || errors.Is(err, ErrTemplateConflict) || strings.HasPrefix(err.Error(), "409"):
			writeControllerJSONError(w, http.StatusConflict, err.Error())
		case strings.HasPrefix(err.Error(), "400") || strings.Contains(err.Error(), "exactly one") || strings.Contains(err.Error(), "unknown step type") || strings.Contains(err.Error(), "ARG"):
			writeControllerJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeControllerJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "building"})
}

func (m *ControllerManager) handleInternalGetBuildLogs(w http.ResponseWriter, r *http.Request) {
	buildID := r.PathValue("buildID")
	if buildID == "" {
		writeControllerJSONError(w, http.StatusBadRequest, "buildID is required")
		return
	}
	cursor := 0
	if cursorStr := r.URL.Query().Get("cursor"); cursorStr != "" {
		parsed, err := strconv.Atoi(cursorStr)
		if err != nil || parsed < 0 {
			writeControllerJSONError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
			return
		}
		cursor = parsed
	}
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			writeControllerJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}
	direction := r.URL.Query().Get("direction")
	level := r.URL.Query().Get("level")

	if m.reconciler == nil || m.reconciler.ContextStore() == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": []BuildLogEntry{}})
		return
	}

	store := m.reconciler.ContextStore()
	var entries []BuildLogEntry
	var err error
	if strings.EqualFold(direction, "backward") {
		all, readErr := store.ReadLogs(buildID, 0, 0, level)
		if readErr != nil {
			writeControllerJSONError(w, http.StatusInternalServerError, readErr.Error())
			return
		}
		if len(all) > 0 {
			end := len(all)
			if cursor > 0 && cursor < end {
				end = cursor
			}
			start := 0
			if limit > 0 && end-limit > 0 {
				start = end - limit
			}
			entries = make([]BuildLogEntry, end-start)
			copy(entries, all[start:end])
		}
	} else {
		entries, err = store.ReadLogs(buildID, cursor, limit, level)
		if err != nil {
			writeControllerJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if entries == nil {
		entries = []BuildLogEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"logs": entries})
}

// ReconcileOnce executes one pass of ReapIdle, WarmPool replenishment, and context TTL sweep.
func (m *ControllerManager) ReconcileOnce(ctx context.Context) {
	// Sync state from Kubernetes (PodBoxes, PodWarmPools, and PodTemplates)
	if _, err := m.reconciler.SyncFromKube(ctx); err != nil {
		slog.Debug("sync from kube failed", "err", err)
	}

	now := time.Now().UTC()
	if err := m.reconciler.SweepTemplateContexts(now); err != nil {
		slog.Warn("sweep template contexts failed", "err", err)
	}

	if promoted := m.reconciler.ReconcileProvisioning(ctx, now); len(promoted) > 0 {
		slog.Info("promoted provisioning sandboxes to running", "count", len(promoted), "names", promoted)
	}

	paused, terminated := m.reconciler.ReapIdle(ctx, now)
	if len(paused) > 0 || len(terminated) > 0 {
		slog.Info("reap cycle executed", "paused", len(paused), "terminated", len(terminated))
	}

	pools := m.reconciler.GetWarmPoolSpecs()
	if len(pools) > 0 {
		var wg sync.WaitGroup
		wg.Add(len(pools))
		for poolName, poolSpec := range pools {
			go func(name string, spec v1alpha1.PodWarmPoolSpec) {
				defer wg.Done()
				if _, err := m.reconciler.ReconcileWarmPool(ctx, name, spec); err != nil {
					slog.Warn("warmpool replenishment failed", "pool", name, "err", err)
				}
			}(poolName, poolSpec)
		}
		wg.Wait()
	}
}

func (m *ControllerManager) replenishWarmPool(ctx context.Context, poolName string) {
	if m.reconciler == nil || poolName == "" || ctx.Err() != nil {
		return
	}
	specs := m.reconciler.GetWarmPoolSpecs()
	poolSpec, ok := specs[poolName]
	if !ok {
		if err := m.reconciler.SyncWarmPoolsFromKube(ctx, ""); err == nil {
			specs = m.reconciler.GetWarmPoolSpecs()
			poolSpec, ok = specs[poolName]
		}
	}
	if !ok {
		return
	}
	if _, err := m.reconciler.ReconcileWarmPool(ctx, poolName, poolSpec); err != nil && ctx.Err() == nil {
		slog.Warn("async warmpool replenishment failed", "pool", poolName, "err", err)
	}
}

func (m *ControllerManager) watchWarmPoolPodsLoop(ctx context.Context) {
	if m.reconciler == nil {
		return
	}
	type podWatcher interface {
		WatchPods(ctx context.Context, namespace, labelSelector, resourceVersion string, onEvent func(eventType string, pod PodManifest)) (string, error)
	}
	watcher, ok := m.reconciler.KubeClient().(podWatcher)
	if !ok || watcher == nil {
		return
	}
	ns := m.reconciler.Namespace()
	var rv string
	for ctx.Err() == nil {
		nextRV, err := watcher.WatchPods(ctx, ns, "cubepod.io/warmpool", rv, func(eventType string, pod PodManifest) {
			if pod.Labels == nil {
				return
			}
			poolName := pod.Labels["cubepod.io/warmpool"]
			if poolName == "" {
				return
			}
			if eventType == "DELETED" || (eventType == "MODIFIED" && pod.Labels["cubepod.io/claimed"] == "true") {
				_ = m.reconciler.SyncWarmPoolsFromKube(ctx, "")
				m.reconciler.notifyWarmPoolReplenish(poolName)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			rv = ""
			timer := time.NewTimer(m.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		rv = nextRV
	}
}

// Start launches the background ticker loop and HTTP server, blocking until ctx is cancelled.
func (m *ControllerManager) Start(ctx context.Context) error {
	if m.reconciler != nil {
		if _, err := m.reconciler.SyncFromKube(ctx); err != nil {
			slog.Warn("initial sync from kube failed", "err", err)
		}
		m.reconciler.RecoverInFlightBuilds(ctx)
	}

	m.mu.Lock()
	m.server = &http.Server{
		Addr:    m.addr,
		Handler: m.Handler(),
	}
	m.mu.Unlock()

	errCh := make(chan error, 1)

	// Start HTTP server in a separate goroutine.
	go func() {
		slog.Info("starting controller HTTP server", "addr", m.addr)
		if err := m.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Background ticker for ReapIdle and WarmPool replenishment.
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// Initial reconciliation pass.
	m.ReconcileOnce(ctx)

	var workersWG sync.WaitGroup
	var replenishCh <-chan string
	if m.reconciler != nil {
		replenishCh = m.reconciler.WarmPoolReplenishChan()
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			m.watchWarmPoolPodsLoop(ctx)
		}()
	}

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m.mu.Lock()
			srv := m.server
			m.mu.Unlock()
			if srv != nil {
				_ = srv.Shutdown(shutdownCtx)
			}
			workersWG.Wait()
			return nil
		case err := <-errCh:
			workersWG.Wait()
			return err
		case poolName := <-replenishCh:
			pending := map[string]struct{}{poolName: {}}
		drainLoop:
			for {
				select {
				case extra := <-replenishCh:
					if extra != "" {
						pending[extra] = struct{}{}
					}
				default:
					break drainLoop
				}
			}
			for name := range pending {
				if name == "" {
					continue
				}
				workersWG.Add(1)
				go func(p string) {
					defer workersWG.Done()
					m.replenishWarmPool(ctx, p)
				}(name)
			}
		case <-ticker.C:
			m.ReconcileOnce(ctx)
		}
	}
}
