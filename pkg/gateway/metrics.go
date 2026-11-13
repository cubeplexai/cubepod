package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

// Metrics encapsulates Prometheus counters for CubePod Gateway observability.
type Metrics struct {
	HTTPRequestsTotal             atomic.Uint64
	SandboxesCreatedTotal         atomic.Uint64
	AutoresumeTotal               atomic.Uint64
	CommandsExecutedTotal         atomic.Uint64
	DesktopActivationsOK          atomic.Uint64
	DesktopActivationsUnsupported atomic.Uint64
	DesktopActivationsError       atomic.Uint64
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

// withMetrics wraps an http.Handler to record metrics on incoming requests,
// sandbox creations, command executions, and auto-resume transitions.
func (s *Server) withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := s.Metrics()
		m.HTTPRequestsTotal.Add(1)

		cleanPath := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(cleanPath, "/")

		var beforeResumes int
		var trackedSandboxID string
		if len(parts) >= 2 && parts[0] == "sandboxes" {
			trackedSandboxID = parts[1]
			s.mu.RLock()
			if sb, ok := s.sandboxes[trackedSandboxID]; ok {
				beforeResumes = sb.AutoResumeCount
			}
			s.mu.RUnlock()
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Check if a sandbox was created (POST /sandboxes or POST /v2/sandboxes with 201 Created)
		if r.Method == http.MethodPost && rec.statusCode == http.StatusCreated &&
			((len(parts) == 1 && parts[0] == "sandboxes") || (len(parts) == 2 && parts[0] == "v2" && parts[1] == "sandboxes")) {
			m.SandboxesCreatedTotal.Add(1)
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
		payload := map[string]interface{}{
			"cubepod_gateway_http_requests_total":      m.HTTPRequestsTotal.Load(),
			"cubepod_gateway_sandboxes_created_total":  m.SandboxesCreatedTotal.Load(),
			"cubepod_gateway_autoresume_total":         m.AutoresumeTotal.Load(),
			"cubepod_gateway_commands_executed_total":  m.CommandsExecutedTotal.Load(),
			"cubepod_gateway_sandboxes_active_running": runningCount,
			"cubepod_gateway_sandboxes_active_paused":  pausedCount,
			"cubepod_gateway_warmpools_total":          warmpoolsTotal,
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

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_sandboxes_created_total Total sandboxes created\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_sandboxes_created_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_created_total %d\n", m.SandboxesCreatedTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_autoresume_total Total sandbox auto-resume events\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_autoresume_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_autoresume_total %d\n", m.AutoresumeTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_gateway_commands_executed_total Total commands executed in sandboxes\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_commands_executed_total counter\n")
	fmt.Fprintf(&buf, "cubepod_gateway_commands_executed_total %d\n", m.CommandsExecutedTotal.Load())

	fmt.Fprintf(&buf, "# HELP cubepod_desktop_activations_total Total desktop stack activations by result\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_desktop_activations_total counter\n")
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"ok\"} %d\n", m.DesktopActivationsOK.Load())
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"unsupported\"} %d\n", m.DesktopActivationsUnsupported.Load())
	fmt.Fprintf(&buf, "cubepod_desktop_activations_total{result=\"error\"} %d\n", m.DesktopActivationsError.Load())

	// Gauges
	fmt.Fprintf(&buf, "# HELP cubepod_gateway_sandboxes_active Current number of active sandboxes by state\n")
	fmt.Fprintf(&buf, "# TYPE cubepod_gateway_sandboxes_active gauge\n")
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_active{state=\"Running\"} %d\n", runningCount)
	fmt.Fprintf(&buf, "cubepod_gateway_sandboxes_active{state=\"Paused\"} %d\n", pausedCount)

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
