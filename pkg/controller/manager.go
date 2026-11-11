package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ControllerManager orchestrates the background reconciliation loop (reaping idle/paused sandboxes,
// replenishing warmpools) and serves HTTP /healthz and /metrics.
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

// Handler returns the HTTP mux for /healthz and /metrics.
func (m *ControllerManager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", m.handleHealthz)
	mux.HandleFunc("GET /metrics", m.handleMetrics)
	return mux
}

func (m *ControllerManager) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"component": "cubepod-controller",
	})
}

func (m *ControllerManager) handleMetrics(w http.ResponseWriter, r *http.Request) {
	active := m.reconciler.ActiveSandboxesCount()
	paused := m.reconciler.PausedSandboxesCount()
	warmReady := m.reconciler.WarmPoolReadyCount()

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{
			"cubepod_active_sandboxes": active,
			"cubepod_paused_sandboxes": paused,
			"cubepod_warmpool_ready":   warmReady,
		})
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
}

// ReconcileOnce executes one pass of ReapIdle and WarmPool replenishment.
func (m *ControllerManager) ReconcileOnce(ctx context.Context) {
	now := time.Now().UTC()
	paused, terminated := m.reconciler.ReapIdle(ctx, now)
	if len(paused) > 0 || len(terminated) > 0 {
		slog.Info("reap cycle executed", "paused", len(paused), "terminated", len(terminated))
	}

	pools := m.reconciler.GetWarmPoolSpecs()
	for poolName, poolSpec := range pools {
		if _, err := m.reconciler.ReconcileWarmPool(ctx, poolName, poolSpec); err != nil {
			slog.Warn("warmpool replenishment failed", "pool", poolName, "err", err)
		}
	}
}

// Start launches the background ticker loop and HTTP server, blocking until ctx is cancelled.
func (m *ControllerManager) Start(ctx context.Context) error {
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
			return nil
		case err := <-errCh:
			return err
		case <-ticker.C:
			m.ReconcileOnce(ctx)
		}
	}
}
