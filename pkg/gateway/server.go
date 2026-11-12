// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

var subdomainHostRegex = regexp.MustCompile(`^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$`)

// SandboxRecord represents a managed sandbox exposed over the E2B-compatible API.
type SandboxRecord struct {
	SandboxID        string            `json:"sandboxID"`
	TemplateID       string            `json:"templateID"`
	State            string            `json:"state"`
	ClientID         string            `json:"clientID"`
	EnvdVersion      string            `json:"envdVersion"`
	StartedAt        time.Time         `json:"startedAt"`
	EndAt            time.Time         `json:"endAt"`
	CPUCount         int               `json:"cpuCount"`
	MemoryMB         int               `json:"memoryMB"`
	DiskSizeMB       int               `json:"diskSizeMB"`
	EnvdPort         int               `json:"envdPort"`
	PoddURL          string            `json:"poddURL,omitempty"`
	RootfsGeneration int64             `json:"rootfsGeneration"`
	VolumeKey        string            `json:"volumeKey,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	TimeoutSeconds   int               `json:"timeoutSeconds,omitempty"`
	LeaseSeconds     int               `json:"leaseSeconds,omitempty"`
	LeaseExpiresAt   *time.Time        `json:"leaseExpiresAt,omitempty"`
	LastActivityAt   *time.Time        `json:"lastActivityAt,omitempty"`
	AutoResumeCount  int               `json:"autoResumeCount"`
	WorkspaceDir     string            `json:"-"`
}

type commandEntry struct {
	id       string
	cmdID    string
	pid      int
	running  bool
	exitCode int
	buf      *bytes.Buffer
	mu       sync.RWMutex
	cancel   context.CancelFunc
}

type lockedWriter struct {
	entry *commandEntry
}

func (w *lockedWriter) Write(p []byte) (n int, err error) {
	w.entry.mu.Lock()
	defer w.entry.mu.Unlock()
	return w.entry.buf.Write(p)
}

// Server serves the CubePod Control Plane and Panel Proxy routes.
type Server struct {
	reconciler       *controller.Reconciler
	mu               sync.RWMutex
	sandboxes        map[string]*SandboxRecord
	commands         map[string]*commandEntry
	endpoints        map[string]map[int]string
	workspaceBaseDir string
	panelSecret      []byte
	vault            *egress.Vault
	ca               *egress.CA
	livePodRouting   bool
	metrics          *Metrics
}

// NewServer constructs a new Gateway Server.
func NewServer(reconciler *controller.Reconciler) *Server {
	v := egress.NewVault()
	ca, _ := egress.NewCA()
	return &Server{
		reconciler:       reconciler,
		sandboxes:        make(map[string]*SandboxRecord),
		commands:         make(map[string]*commandEntry),
		endpoints:        make(map[string]map[int]string),
		workspaceBaseDir: filepath.Join(os.TempDir(), "cubepod-sandboxes"),
		vault:            v,
		ca:               ca,
		metrics:          &Metrics{},
	}
}

// SetLivePodRouting enables or disables automatic routing to the live Kubernetes PodIP.
func (s *Server) SetLivePodRouting(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.livePodRouting = enabled
}

// LivePodRouting returns whether live pod routing is enabled.
func (s *Server) LivePodRouting() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.livePodRouting
}

// SetWorkspaceBaseDir overrides the base directory for local sandbox workspaces.
func (s *Server) SetWorkspaceBaseDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaceBaseDir = dir
}

// SetPoddURL registers the live podd daemon upstream URL for a sandbox.
func (s *Server) SetPoddURL(sandboxID, poddURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.sandboxes[sandboxID]; ok {
		rec.PoddURL = poddURL
	}
}

// RegisterPoddURL is an alias for SetPoddURL.
func (s *Server) RegisterPoddURL(sandboxID, poddURL string) {
	s.SetPoddURL(sandboxID, poddURL)
}

// Vault returns the internal Vault instance.
func (s *Server) Vault() *egress.Vault {
	return s.vault
}

// CA returns the internal CA instance.
func (s *Server) CA() *egress.CA {
	return s.ca
}

// Handler returns the HTTP router for the CubePod Gateway.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Control-Plane & Lifecycle routes
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	mux.HandleFunc("GET /sandboxes", s.handleListSandboxes)
	mux.HandleFunc("GET /sandboxes/{sandboxID}", s.handleGetSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/pause", s.handlePauseSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/connect", s.handleConnectSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/timeout", s.handleTimeoutSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/refreshes", s.handleRefreshSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/lease", s.handleLeaseSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/reset-rootfs", s.handleResetRootfs)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/fork", s.handleForkSandbox)
	mux.HandleFunc("DELETE /sandboxes/{sandboxID}", s.handleDeleteSandbox)

	// Data-Plane Execution & File routes
	mux.HandleFunc("POST /sandboxes/{sandboxID}/commands", s.handleRunCommand)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/commands/run", s.handleRunCommand)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/code/execute", s.handleCodeExecute)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/execute", s.handleCodeExecute)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("GET /sandboxes/{sandboxID}/commands/{cmdID}/logs", s.handleGetCommandLogs)
	mux.HandleFunc("GET /sandboxes/{sandboxID}/commands/{cmdID}/status", s.handleGetCommandStatus)
	mux.HandleFunc("GET /sandboxes/{sandboxID}/commands/{cmdID}/stream", s.handleStreamCommandLogs)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/commands/{cmdID}/kill", s.handleKillCommand)
	mux.HandleFunc("PUT /sandboxes/{sandboxID}/files", s.handlePutFile)
	mux.HandleFunc("GET /sandboxes/{sandboxID}/files", s.handleGetFile)
	mux.HandleFunc("DELETE /sandboxes/{sandboxID}/files", s.handleDeleteFile)
	mux.HandleFunc("GET /sandboxes/{sandboxID}/files/list", s.handleListFiles)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/files/archive-sync", s.handleArchiveSync)

	// WarmPool & Template routes
	mux.HandleFunc("POST /warmpools", s.handleCreateWarmPool)
	mux.HandleFunc("GET /warmpools", s.handleListWarmPools)
	mux.HandleFunc("GET /templates", s.handleListTemplates)
	mux.HandleFunc("GET /openapi.yaml", s.handleGetOpenAPI)

	// Host Endpoint & Panel Reverse Proxy routes
	mux.HandleFunc("GET /sandboxes/{sandboxID}/endpoints/{port}", s.handleGetEndpoint)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/endpoints/{port}", s.handleMintEndpointToken)
	mux.HandleFunc("/panel/{token}/{rest...}", s.handlePanelProxy)
	mux.HandleFunc("/panel/{token}", s.handlePanelProxy)

	// Zero-Trust Egress Exchange route
	mux.HandleFunc("POST /internal/v1/egress/exchange", s.handleEgressExchange)

	hostRouter := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Subdomain Host Router: inspect r.Host for regex ^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$,
		// extracting targetPort and sandboxID. When matched, reverse-proxy the request
		// to the target sandbox on targetPort (e.g. 49999 for Jupyter, 49983 for envd/podd, 8080 for web).
		matches := subdomainHostRegex.FindStringSubmatch(r.Host)
		if len(matches) == 4 {
			port, err := strconv.Atoi(matches[1])
			if err == nil && port > 0 && port <= 65535 {
				s.handleSubdomainProxy(w, r, matches[2], port)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	return s.withMetrics(hostRouter)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	totalResumes := 0
	for _, rec := range s.sandboxes {
		totalResumes += rec.AutoResumeCount
	}
	count := len(s.sandboxes)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"service":         "cubepod-gateway",
		"sandboxes":       count,
		"autoResumeCount": totalResumes,
	})
}

func (s *Server) handleEgressExchange(w http.ResponseWriter, r *http.Request) {
	h := egress.VaultExchangeHandler(s.vault, s.ca)
	h(w, r)
}

func (s *Server) forwardToPodd(w http.ResponseWriter, r *http.Request, targetURL string) {
	if r.URL.RawQuery != "" {
		if strings.Contains(targetURL, "?") {
			targetURL += "&" + r.URL.RawQuery
		} else {
			targetURL += "?" + r.URL.RawQuery
		}
	}
	var bodyReader io.Reader
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bodyReader)
	if err != nil {
		http.Error(w, "failed to create forward request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "podd forwarding failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type createWarmPoolReq struct {
	Name        string `json:"name"`
	PoolName    string `json:"poolName"`
	TemplateRef string `json:"templateRef"`
	TemplateID  string `json:"templateID"`
	Replicas    int32  `json:"replicas"`
}

func (s *Server) handleCreateWarmPool(w http.ResponseWriter, r *http.Request) {
	var req createWarmPoolReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	name := req.Name
	if name == "" {
		name = req.PoolName
	}
	if name == "" {
		name = "default-pool"
	}
	tpl := req.TemplateRef
	if tpl == "" {
		tpl = req.TemplateID
	}
	if tpl == "" {
		tpl = "cubeplex-ubuntu24-desktop"
	}
	replicas := req.Replicas
	if replicas <= 0 {
		replicas = 2
	}

	spec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: tpl,
		Replicas:    replicas,
	}

	status, err := s.reconciler.ReconcileWarmPool(r.Context(), name, spec)
	if err != nil {
		http.Error(w, "failed to reconcile warm pool: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"name":              name,
		"poolName":          name,
		"templateRef":       tpl,
		"replicas":          replicas,
		"readyReplicas":     status.ReadyReplicas,
		"availableReplicas": status.AvailableReplicas,
	})
}

func (s *Server) handleListWarmPools(w http.ResponseWriter, _ *http.Request) {
	specs := s.reconciler.GetWarmPoolSpecs()
	readyCount := s.reconciler.WarmPoolReadyCount()

	type poolInfo struct {
		Name              string `json:"name"`
		PoolName          string `json:"poolName"`
		TemplateRef       string `json:"templateRef"`
		Replicas          int32  `json:"replicas"`
		ReadyReplicas     int32  `json:"readyReplicas"`
		AvailableReplicas int32  `json:"availableReplicas"`
	}

	pools := make([]poolInfo, 0, len(specs))
	for name, spec := range specs {
		st, _ := s.reconciler.GetWarmPoolStatus(name)
		var ready, avail int32
		if st != nil {
			ready = st.ReadyReplicas
			avail = st.AvailableReplicas
		}
		pools = append(pools, poolInfo{
			Name:              name,
			PoolName:          name,
			TemplateRef:       spec.TemplateRef,
			Replicas:          spec.Replicas,
			ReadyReplicas:     ready,
			AvailableReplicas: avail,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"warmPools":          pools,
		"warmPoolReadyCount": readyCount,
		"totalReady":         readyCount,
	})
}

// TemplateInfo describes a sandbox template exposed by the gateway.
type TemplateInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	Description string `json:"description"`
}

var defaultTemplates = []TemplateInfo{
	{
		ID:          "cubeplex-ubuntu24-desktop",
		Name:        "CubePlex Ubuntu 24.04 Desktop",
		Image:       "cubeplex-ubuntu24-desktop:latest",
		Description: "Full desktop environment with XFCE, browser, WebRTC panel, and dev tools",
	},
	{
		ID:          "ubuntu:24.04",
		Name:        "Ubuntu 24.04 Standard",
		Image:       "ubuntu:24.04",
		Description: "Standard headless Linux agent sandbox",
	},
	{
		ID:          "debian:12",
		Name:        "Debian 12 Bookworm",
		Image:       "debian:12",
		Description: "Minimal Debian 12 sandbox",
	},
}

func (s *Server) handleListTemplates(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": defaultTemplates,
	})
}

// handleGetOpenAPI serves the OpenAPI specification.
func (s *Server) handleGetOpenAPI(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAPISpec(w, r)
}

// handleOpenAPISpec serves the OpenAPI specification file.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	candidates := []string{
		"api/openapi/cubepod-v1.yaml",
		"../../api/openapi/cubepod-v1.yaml",
		"/tmp/cubepod-wt-cep12/api/openapi/cubepod-v1.yaml",
	}
	var data []byte
	var err error
	for _, cand := range candidates {
		data, err = os.ReadFile(cand)
		if err == nil {
			break
		}
	}
	if err != nil {
		http.Error(w, "openapi specification not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleSubdomainProxy reverse-proxies inbound requests matching ^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$
// to the target sandbox on targetPort (e.g. 49999 for Jupyter, 49983 for envd/podd, 8080 for web).
func (s *Server) handleSubdomainProxy(w http.ResponseWriter, r *http.Request, sandboxID string, port int) {
	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	// Auto-resume sandbox if currently Paused
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		slog.Error("failed to auto-resume sandbox for subdomain proxy", "sandboxID", sandboxID, "port", port, "err", err)
		http.Error(w, "failed to auto-resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	targetURLStr := s.GetRegisteredPort(sandboxID, port)
	if targetURLStr == "" && rec != nil {
		if s.livePodRouting {
			if boxStatus, ok := s.reconciler.GetBox(sandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
				targetURLStr = fmt.Sprintf("http://%s:%d", boxStatus.PodIP, port)
			}
		} else if rec.PoddURL != "" {
			if u, err := url.Parse(rec.PoddURL); err == nil && u.Hostname() != "" && u.Hostname() != "10.244.0.42" {
				targetURLStr = fmt.Sprintf("http://%s:%d", u.Hostname(), port)
			}
		}
	}

	// Fallback to local code runner if port 49999 is targeted without a live upstream daemon
	if targetURLStr == "" && port == 49999 && (r.URL.Path == "/execute" || r.URL.Path == "/sandboxes/"+sandboxID+"/code/execute") {
		s.executeCodeLocally(w, r, rec)
		return
	}

	if targetURLStr == "" {
		http.Error(w, fmt.Sprintf("no upstream registered for sandbox %s port %d", sandboxID, port), http.StatusBadGateway)
		return
	}

	targetURL, err := url.Parse(targetURLStr)
	if err != nil {
		http.Error(w, "invalid target URL: "+err.Error(), http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host

			targetPath := strings.TrimSuffix(targetURL.Path, "/")
			reqPath := req.URL.Path
			if !strings.HasPrefix(reqPath, "/") {
				reqPath = "/" + reqPath
			}
			req.URL.Path = targetPath + reqPath

			if targetURL.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = targetURL.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("subdomain reverse proxy connection failed", "target", targetURLStr, "err", err)
			http.Error(w, fmt.Sprintf("reverse proxy error: %v", err), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// handleGetEndpoint returns the public ingress host and URL for a sandbox port (E2B get_host compatible).
func (s *Server) handleGetEndpoint(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	portStr := r.PathValue("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port parameter", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	_, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	hostDomain := r.Host
	if hostDomain == "" {
		hostDomain = "cubepod.local"
	}
	subdomainHost := fmt.Sprintf("%d-%s.%s", port, sandboxID, hostDomain)
	urlPath := fmt.Sprintf("http://%s", subdomainHost)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"host":      subdomainHost,
		"url":       urlPath,
		"endpoint":  subdomainHost,
		"port":      port,
		"sandboxID": sandboxID,
	})
}

// handleCodeExecute routes POST /sandboxes/{sandboxID}/code/execute or /execute to in-guest port 49999 or local runner.
func (s *Server) handleCodeExecute(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandboxID")
	}
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandbox_id")
	}
	if sandboxID == "" {
		sandboxID = r.Header.Get("X-Sandbox-ID")
	}
	if sandboxID == "" {
		http.Error(w, "sandboxID is required", http.StatusBadRequest)
		return
	}

	s.executeCodeForSandbox(w, r, sandboxID)
}

// handleExecute handles direct POST /execute routing with sandboxID via query param or header.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.URL.Query().Get("sandboxID")
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandbox_id")
	}
	if sandboxID == "" {
		sandboxID = r.Header.Get("X-Sandbox-ID")
	}
	if sandboxID == "" {
		http.Error(w, "sandboxID is required", http.StatusBadRequest)
		return
	}

	s.executeCodeForSandbox(w, r, sandboxID)
}

func (s *Server) executeCodeForSandbox(w http.ResponseWriter, r *http.Request, sandboxID string) {
	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	// Check if in-guest port 49999 has a target upstream registered
	targetURLStr := s.GetRegisteredPort(sandboxID, 49999)
	if targetURLStr == "" && rec != nil {
		if s.livePodRouting {
			if boxStatus, ok := s.reconciler.GetBox(sandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
				targetURLStr = fmt.Sprintf("http://%s:49999", boxStatus.PodIP)
			}
		} else if rec.PoddURL != "" {
			if u, err := url.Parse(rec.PoddURL); err == nil && u.Hostname() != "" && u.Hostname() != "10.244.0.42" {
				targetURLStr = fmt.Sprintf("http://%s:49999", u.Hostname())
			}
		}
	}

	if targetURLStr != "" {
		s.proxyToCodeInterpreter(w, r, targetURLStr)
		return
	}

	// Local runner fallback
	s.executeCodeLocally(w, r, rec)
}

func (s *Server) proxyToCodeInterpreter(w http.ResponseWriter, r *http.Request, targetURLStr string) {
	targetURL, err := url.Parse(targetURLStr)
	if err != nil {
		http.Error(w, "invalid target URL: "+err.Error(), http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host

			targetPath := strings.TrimSuffix(targetURL.Path, "/")
			targetPath = strings.TrimSuffix(targetPath, "/execute")
			req.URL.Path = targetPath + "/execute"
			if targetURL.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = targetURL.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("code interpreter reverse proxy failed", "target", targetURLStr, "err", err)
			http.Error(w, fmt.Sprintf("code interpreter reverse proxy error: %v", err), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

func (s *Server) executeCodeLocally(w http.ResponseWriter, r *http.Request, rec *SandboxRecord) {
	var req struct {
		Code      string            `json:"code"`
		Command   string            `json:"command"`
		ContextID any               `json:"context_id,omitempty"`
		Language  string            `json:"language,omitempty"`
		EnvVars   map[string]string `json:"env_vars,omitempty"`
		Envs      map[string]string `json:"envs,omitempty"`
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err == nil && len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &req)
	}

	code := req.Code
	if code == "" {
		code = req.Command
	}
	if code == "" && len(bodyBytes) > 0 {
		code = string(bodyBytes)
	}
	if strings.TrimSpace(code) == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	workdir := rec.WorkspaceDir
	_ = os.MkdirAll(workdir, 0755)

	envs := req.EnvVars
	if envs == nil {
		envs = req.Envs
	}

	cmd := exec.CommandContext(r.Context(), "python3", "-c", code)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")
	for k, v := range envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	now := time.Now().UnixMilli()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	writeJSONLine := func(val any) {
		line, _ := json.Marshal(val)
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	if stdoutBuf.Len() > 0 {
		writeJSONLine(map[string]any{
			"type":      "stdout",
			"text":      stdoutBuf.String(),
			"timestamp": now,
		})
	}

	if stderrBuf.Len() > 0 {
		writeJSONLine(map[string]any{
			"type":      "stderr",
			"text":      stderrBuf.String(),
			"timestamp": now,
		})
	}

	if runErr != nil {
		exitCode := 1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		writeJSONLine(map[string]any{
			"type":      "error",
			"name":      "ExecutionError",
			"value":     runErr.Error(),
			"traceback": stderrBuf.String(),
			"exitCode":  exitCode,
		})
	}

	writeJSONLine(map[string]any{
		"type":            "number_of_executions",
		"execution_count": 1,
	})
}
