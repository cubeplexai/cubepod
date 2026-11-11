// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
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

	return mux
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

type createSandboxReq struct {
	TemplateID string            `json:"templateID"`
	Timeout    int               `json:"timeout,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Extension  struct {
		VolumeKey      string                      `json:"volumeKey,omitempty"`
		NetworkPolicy  *v1alpha1.NetworkPolicySpec `json:"networkPolicy,omitempty"`
		SecretBindings []struct {
			EnvName        string   `json:"envName"`
			RawSecret      string   `json:"rawSecret"`
			AllowedHosts   []string `json:"allowedHosts"`
			AllowedHeaders []string `json:"allowedHeaders"`
		} `json:"secretBindings,omitempty"`
	} `json:"extension,omitempty"`
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.TemplateID == "" {
		req.TemplateID = "cubeplex-ubuntu24-desktop"
	}
	timeoutSecs := req.Timeout
	if timeoutSecs <= 0 {
		timeoutSecs = 1800
	}

	id := fmt.Sprintf("cpod-%d", time.Now().UnixNano())
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:      req.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	workspaceDir := filepath.Join(s.workspaceBaseDir, id, "workspace")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Register any secret bindings with Vault
	for _, sb := range req.Extension.SecretBindings {
		if sb.RawSecret != "" {
			_, _ = s.vault.BindSecret(id, sb.EnvName, sb.RawSecret, sb.AllowedHosts, sb.AllowedHeaders)
		}
	}

	now := time.Now().UTC()
	rec := &SandboxRecord{
		SandboxID:        id,
		TemplateID:       req.TemplateID,
		State:            string(status.Phase),
		EnvdPort:         49983,
		RootfsGeneration: status.ObservedRootfsGen,
		VolumeKey:        req.Extension.VolumeKey,
		Metadata:         req.Metadata,
		CreatedAt:        now,
		LastActivityAt:   &now,
		TimeoutSeconds:   timeoutSecs,
		WorkspaceDir:     workspaceDir,
	}

	if s.livePodRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
		rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)
		_ = pollPoddHealth(r.Context(), rec.PoddURL, 15*time.Second)
	}

	s.mu.Lock()
	s.sandboxes[id] = rec
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	stateFilter := r.URL.Query().Get("state")
	templateFilter := r.URL.Query().Get("templateID")

	s.mu.RLock()
	list := make([]*SandboxRecord, 0, len(s.sandboxes))
	for _, rec := range s.sandboxes {
		if stateFilter != "" && rec.State != stateFilter {
			continue
		}
		if templateFilter != "" && rec.TemplateID != templateFilter {
			continue
		}
		list = append(list, rec)
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handlePauseSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	poddURL := rec.PoddURL
	s.mu.Unlock()

	// Call POST <PoddURL>/v1/lifecycle/prepare-stop before pausing if podd is running
	if poddURL != "" {
		stopURL := strings.TrimSuffix(poddURL, "/") + "/v1/lifecycle/prepare-stop"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, stopURL, nil)
		if err == nil {
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}

	s.mu.Lock()
	status, err := s.reconciler.Reconcile(r.Context(), id, v1alpha1.PodBoxSpec{
		TemplateRef:      rec.TemplateID,
		DesiredState:     v1alpha1.DesiredStatePaused,
		RootfsGeneration: rec.RootfsGeneration,
	})
	if err == nil {
		rec.State = string(status.Phase)
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleConnectSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	wasPaused := rec.State == "Paused" || rec.State == string(v1alpha1.PodBoxPhasePaused)
	// Auto-resume from Paused if needed (E2B connect semantics).
	status, err := s.reconciler.Reconcile(r.Context(), id, v1alpha1.PodBoxSpec{
		TemplateRef:      rec.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: rec.RootfsGeneration,
	})
	if err == nil {
		rec.State = string(status.Phase)
		if wasPaused {
			rec.AutoResumeCount++
		}
		now := time.Now().UTC()
		rec.LastActivityAt = &now
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

type timeoutReq struct {
	TimeoutSeconds int `json:"timeoutSeconds"`
	Timeout        int `json:"timeout"`
}

func (s *Server) handleTimeoutSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	var req timeoutReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	secs := req.TimeoutSeconds
	if secs <= 0 {
		secs = req.Timeout
	}
	if secs <= 0 {
		secs = 1800
	}

	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	rec.TimeoutSeconds = secs
	now := time.Now().UTC()
	rec.LastActivityAt = &now
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

type leaseReq struct {
	LeaseSeconds int `json:"leaseSeconds"`
}

func (s *Server) handleLeaseSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	var req leaseReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	secs := req.LeaseSeconds
	if secs <= 0 {
		secs = 300
	}

	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	rec.LeaseSeconds = secs
	exp := time.Now().UTC().Add(time.Duration(secs) * time.Second)
	rec.LeaseExpiresAt = &exp
	now := time.Now().UTC()
	rec.LastActivityAt = &now
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleResetRootfs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	rec.RootfsGeneration++
	status, err := s.reconciler.Reconcile(r.Context(), id, v1alpha1.PodBoxSpec{
		TemplateRef:      rec.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: rec.RootfsGeneration,
	})
	if err == nil {
		rec.State = string(status.Phase)
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleForkSandbox(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("sandboxID")
	s.mu.RLock()
	parent, ok := s.sandboxes[parentID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "parent sandbox not found", http.StatusNotFound)
		return
	}

	childID := fmt.Sprintf("cpod-fork-%d", time.Now().UnixNano())
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:      parent.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: parentID},
	}
	status, err := s.reconciler.Reconcile(r.Context(), childID, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	workspaceDir := filepath.Join(s.workspaceBaseDir, childID, "workspace")
	_ = os.MkdirAll(workspaceDir, 0755)

	now := time.Now().UTC()
	child := &SandboxRecord{
		SandboxID:        childID,
		TemplateID:       parent.TemplateID,
		State:            string(status.Phase),
		EnvdPort:         49983,
		RootfsGeneration: status.ObservedRootfsGen,
		CreatedAt:        now,
		LastActivityAt:   &now,
		TimeoutSeconds:   parent.TimeoutSeconds,
		WorkspaceDir:     workspaceDir,
	}
	s.mu.Lock()
	s.sandboxes[childID] = child
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(child)
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.Lock()
	rec, ok := s.sandboxes[id]
	delete(s.sandboxes, id)
	delete(s.endpoints, id)
	s.mu.Unlock()

	// Revoke secrets associated with this sandbox
	s.vault.RevokeSandbox(id)

	if ok && rec.WorkspaceDir != "" {
		_ = os.RemoveAll(filepath.Dir(rec.WorkspaceDir))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ensureRunning resumes a Paused sandbox to Running, incrementing AutoResumeCount.
func (s *Server) ensureRunning(ctx context.Context, rec *SandboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	rec.LastActivityAt = &now

	if rec.State == string(v1alpha1.PodBoxPhasePaused) || rec.State == "Paused" {
		status, err := s.reconciler.Reconcile(ctx, rec.SandboxID, v1alpha1.PodBoxSpec{
			TemplateRef:      rec.TemplateID,
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: rec.RootfsGeneration,
		})
		if err != nil {
			return err
		}
		rec.State = string(status.Phase)
		rec.AutoResumeCount++
		if s.livePodRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)
			_ = pollPoddHealth(ctx, rec.PoddURL, 15*time.Second)
		}
	} else if s.livePodRouting && rec.PoddURL == "" {
		if boxStatus, ok := s.reconciler.GetBox(rec.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", boxStatus.PodIP, rec.EnvdPort)
			_ = pollPoddHealth(ctx, rec.PoddURL, 15*time.Second)
		}
	}
	return nil
}

func (s *Server) resolveFilePath(rec *SandboxRecord, userPath string) (string, error) {
	if userPath == "" {
		return "", fmt.Errorf("empty file path")
	}
	baseDir := rec.WorkspaceDir
	if baseDir == "" {
		baseDir = filepath.Join(s.workspaceBaseDir, rec.SandboxID, "workspace")
		_ = os.MkdirAll(baseDir, 0755)
		rec.WorkspaceDir = baseDir
	}

	p := path.Clean("/" + userPath)
	var rel string
	if strings.HasPrefix(p, "/workspace/") {
		rel = strings.TrimPrefix(p, "/workspace/")
	} else if p == "/workspace" {
		rel = ""
	} else {
		rel = strings.TrimPrefix(p, "/")
	}

	fullPath := filepath.Join(baseDir, filepath.FromSlash(rel))
	cleanBase := filepath.Clean(baseDir)
	cleanFull := filepath.Clean(fullPath)
	if cleanFull != cleanBase && !strings.HasPrefix(cleanFull, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid path traversal attempt")
	}
	return cleanFull, nil
}

type runCommandReq struct {
	Cmd        string            `json:"cmd"`
	Command    string            `json:"command"`
	Workdir    string            `json:"workdir"`
	Envs       map[string]string `json:"envs"`
	Background bool              `json:"background"`
}

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/commands/run"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback execution
	var req runCommandReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	cmdStr := req.Cmd
	if cmdStr == "" {
		cmdStr = req.Command
	}
	if cmdStr == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	workdir := rec.WorkspaceDir
	if req.Workdir != "" && req.Workdir != "/workspace" {
		if strings.HasPrefix(req.Workdir, "/workspace/") {
			rel := strings.TrimPrefix(req.Workdir, "/workspace/")
			workdir = filepath.Join(rec.WorkspaceDir, rel)
		} else {
			workdir = req.Workdir
		}
	}
	_ = os.MkdirAll(workdir, 0755)

	cmdID := fmt.Sprintf("cmd-%d", time.Now().UnixNano())

	if req.Background {
		ctx, cancel := context.WithCancel(context.Background())
		execCmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		execCmd.Dir = workdir
		execCmd.Env = os.Environ()
		for k, v := range req.Envs {
			execCmd.Env = append(execCmd.Env, fmt.Sprintf("%s=%s", k, v))
		}

		entry := &commandEntry{
			id:      cmdID,
			cmdID:   cmdID,
			running: true,
			buf:     &bytes.Buffer{},
			cancel:  cancel,
		}
		writer := &lockedWriter{entry: entry}
		execCmd.Stdout = writer
		execCmd.Stderr = writer

		if err := execCmd.Start(); err != nil {
			cancel()
			http.Error(w, "failed to start command: "+err.Error(), http.StatusInternalServerError)
			return
		}

		entry.pid = execCmd.Process.Pid
		s.mu.Lock()
		s.commands[cmdID] = entry
		s.mu.Unlock()

		go func() {
			waitErr := execCmd.Wait()
			entry.mu.Lock()
			entry.running = false
			if waitErr != nil {
				if exitErr, ok := waitErr.(*exec.ExitError); ok {
					entry.exitCode = exitErr.ExitCode()
				} else {
					entry.exitCode = 1
				}
			} else {
				entry.exitCode = 0
			}
			entry.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      cmdID,
			"cmdID":   cmdID,
			"running": true,
			"pid":     entry.pid,
		})
		return
	}

	// Synchronous / Foreground Execution
	execCmd := exec.CommandContext(r.Context(), "sh", "-c", cmdStr)
	execCmd.Dir = workdir
	execCmd.Env = os.Environ()
	for k, v := range req.Envs {
		execCmd.Env = append(execCmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	err := execCmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	combined := append(stdout.Bytes(), stderr.Bytes()...)
	entry := &commandEntry{
		id:       cmdID,
		cmdID:    cmdID,
		running:  false,
		exitCode: exitCode,
		buf:      bytes.NewBuffer(combined),
	}
	s.mu.Lock()
	s.commands[cmdID] = entry
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       cmdID,
		"cmdID":    cmdID,
		"exitCode": exitCode,
		"stdout":   stdout.String(),
		"stderr":   stderr.String(),
	})
}

func (s *Server) handleGetCommandLogs(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

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

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/logs", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	cursorStr := r.URL.Query().Get("cursor")
	cursor := 0
	if cursorStr != "" {
		if c, err := strconv.Atoi(cursorStr); err == nil {
			cursor = c
		}
	}

	entry.mu.RLock()
	all := entry.buf.Bytes()
	running := entry.running
	exitCode := entry.exitCode

	var content string
	nextCursor := len(all)
	if cursor < 0 {
		cursor = 0
	}
	if cursor < len(all) {
		content = string(all[cursor:])
	}
	entry.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       cmdID,
		"cmdID":    cmdID,
		"content":  content,
		"cursor":   nextCursor,
		"running":  running,
		"exitCode": exitCode,
	})
}

func (s *Server) handleKillCommand(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

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

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/kill", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	if entry.running {
		if entry.cancel != nil {
			entry.cancel()
		}
		entry.running = false
		entry.exitCode = -1
	}
	entry.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     cmdID,
		"cmdID":  cmdID,
		"killed": true,
	})
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
	}

	var contentBytes []byte
	var fileJSON struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if len(bodyBytes) > 0 && bodyBytes[0] == '{' && json.Unmarshal(bodyBytes, &fileJSON) == nil && (fileJSON.Path != "" || fileJSON.Content != "") {
		if targetPath == "" {
			targetPath = fileJSON.Path
		}
		contentBytes = []byte(fileJSON.Content)
	} else {
		contentBytes = bodyBytes
	}

	if targetPath == "" {
		http.Error(w, "path parameter or json field is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(fullPath, contentBytes, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"path":   targetPath,
		"size":   len(contentBytes),
	})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("format") == "json" || r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"path":    targetPath,
			"content": string(data),
			"size":    len(data),
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleArchiveSync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files/archive-sync"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	if r.Body == nil {
		http.Error(w, "empty request body", http.StatusBadRequest)
		return
	}

	dest := r.URL.Query().Get("destination")
	baseDir := rec.WorkspaceDir
	if dest != "" {
		resolvedDest, err := s.resolveFilePath(rec, dest)
		if err != nil {
			http.Error(w, "invalid destination: "+err.Error(), http.StatusBadRequest)
			return
		}
		baseDir = resolvedDest
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove uninstalled paths if specified
	for _, p := range r.URL.Query()["remove_paths"] {
		if resolved, err := s.resolveFilePath(rec, p); err == nil {
			_ = os.RemoveAll(resolved)
		}
	}
	for _, p := range r.URL.Query()["removePaths"] {
		if resolved, err := s.resolveFilePath(rec, p); err == nil {
			_ = os.RemoveAll(resolved)
		}
	}

	br := bufio.NewReader(r.Body)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		http.Error(w, "failed to read archive: "+err.Error(), http.StatusBadRequest)
		return
	}

	var tr *tar.Reader
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gzr, err := gzip.NewReader(br)
		if err != nil {
			http.Error(w, "invalid gzip data: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gzr.Close()
		tr = tar.NewReader(gzr)
	} else {
		tr = tar.NewReader(br)
	}

	var extractedCount int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "tar read error: "+err.Error(), http.StatusBadRequest)
			return
		}

		cleanName := path.Clean("/" + hdr.Name)
		var rel string
		if strings.HasPrefix(cleanName, "/workspace/") {
			rel = strings.TrimPrefix(cleanName, "/workspace/")
		} else {
			rel = strings.TrimPrefix(cleanName, "/")
		}
		if rel == "" || rel == "." {
			continue
		}

		target := filepath.Join(baseDir, filepath.FromSlash(rel))
		cleanTarget := filepath.Clean(target)
		cleanBase := filepath.Clean(baseDir)
		if cleanTarget != cleanBase && !strings.HasPrefix(cleanTarget, cleanBase+string(filepath.Separator)) {
			http.Error(w, "directory traversal in tar entry: "+hdr.Name, http.StatusBadRequest)
			return
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanTarget, 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(cleanTarget), 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			mode := os.FileMode(hdr.Mode & 0777)
			if mode == 0 {
				mode = 0644
			}
			f, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = f.Close()
			extractedCount++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"extracted": extractedCount,
	})
}

func pollPoddHealth(ctx context.Context, poddURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	healthURL := strings.TrimSuffix(poddURL, "/") + "/health"
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out for %s", poddURL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	if err := os.RemoveAll(fullPath); err != nil {
		http.Error(w, "failed to delete file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"deleted": targetPath,
	})
}

type fileEntryInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files/list"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		targetPath = "/workspace"
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fi, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "directory not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !fi.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		http.Error(w, "failed to read directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]fileEntryInfo, 0, len(entries))
	for _, entry := range entries {
		info, _ := entry.Info()
		var size int64
		var mode string
		if info != nil {
			size = info.Size()
			mode = info.Mode().String()
		}
		entryPath := path.Join(targetPath, entry.Name())
		result = append(result, fileEntryInfo{
			Name:  entry.Name(),
			Path:  entryPath,
			IsDir: entry.IsDir(),
			Size:  size,
			Mode:  mode,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"path":    targetPath,
		"entries": result,
	})
}

func (s *Server) handleGetCommandStatus(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

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

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/status", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	entry.mu.RLock()
	running := entry.running
	exitCode := entry.exitCode
	cursor := entry.buf.Len()
	pid := entry.pid
	entry.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":        cmdID,
		"cmdID":     cmdID,
		"pid":       pid,
		"running":   running,
		"exit_code": exitCode,
		"exitCode":  exitCode,
		"cursor":    cursor,
	})
}

func (s *Server) handleStreamCommandLogs(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

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

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	cursor := 0
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		var content string
		var nextCursor int
		var running bool
		var exitCode int

		if poddURL != "" {
			reqURL := fmt.Sprintf("%s/v1/commands/%s/logs?cursor=%d", strings.TrimSuffix(poddURL, "/"), cmdID, cursor)
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, reqURL, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			var logResp struct {
				ID       string `json:"id"`
				Content  string `json:"content"`
				Cursor   int    `json:"cursor"`
				Running  bool   `json:"running"`
				ExitCode *int   `json:"exit_code"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&logResp)
			_ = resp.Body.Close()

			content = logResp.Content
			nextCursor = logResp.Cursor
			running = logResp.Running
			if logResp.ExitCode != nil {
				exitCode = *logResp.ExitCode
			}
		} else {
			s.mu.RLock()
			entry, ok := s.commands[cmdID]
			s.mu.RUnlock()
			if !ok {
				return
			}

			entry.mu.RLock()
			all := entry.buf.Bytes()
			running = entry.running
			exitCode = entry.exitCode
			nextCursor = len(all)
			if cursor < len(all) {
				content = string(all[cursor:])
			}
			entry.mu.RUnlock()
		}

		cursor = nextCursor

		frameData, _ := json.Marshal(map[string]interface{}{
			"id":        cmdID,
			"cmdID":     cmdID,
			"content":   content,
			"cursor":    cursor,
			"running":   running,
			"exit_code": exitCode,
			"exitCode":  exitCode,
		})

		_, _ = fmt.Fprintf(w, "data: %s\n\n", frameData)
		flusher.Flush()

		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
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

func (s *Server) handleListTemplates(w http.ResponseWriter, _ *http.Request) {
	type templateInfo struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Image       string `json:"image"`
		Description string `json:"description"`
	}

	templates := []templateInfo{
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
	})
}

func (s *Server) handleGetOpenAPI(w http.ResponseWriter, _ *http.Request) {
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
