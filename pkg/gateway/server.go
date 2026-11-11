// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

// SandboxRecord represents a managed sandbox exposed over the E2B-compatible API.
type SandboxRecord struct {
	SandboxID        string            `json:"sandboxID"`
	TemplateID       string            `json:"templateID"`
	State            string            `json:"state"`
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
	mux.HandleFunc("POST /sandboxes/{sandboxID}/lease", s.handleLeaseSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/reset-rootfs", s.handleResetRootfs)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/fork", s.handleForkSandbox)
	mux.HandleFunc("DELETE /sandboxes/{sandboxID}", s.handleDeleteSandbox)

	// Data-Plane Execution & File routes
	mux.HandleFunc("POST /sandboxes/{sandboxID}/commands/run", s.handleRunCommand)
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

	// Panel Token Mint & Reverse Proxy routes
	mux.HandleFunc("POST /sandboxes/{sandboxID}/endpoints/{port}", s.handleMintEndpointToken)
	mux.HandleFunc("/panel/{token}/{rest...}", s.handlePanelProxy)
	mux.HandleFunc("/panel/{token}", s.handlePanelProxy)

	// Zero-Trust Egress Exchange route
	mux.HandleFunc("POST /internal/v1/egress/exchange", s.handleEgressExchange)

	return s.withMetrics(mux)
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
