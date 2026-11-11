// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

// SandboxRecord represents a managed sandbox exposed over the E2B-compatible API.
type SandboxRecord struct {
	SandboxID        string            `json:"sandboxID"`
	TemplateID       string            `json:"templateID"`
	State            string            `json:"state"`
	EnvdPort         int               `json:"envdPort"`
	RootfsGeneration int64             `json:"rootfsGeneration"`
	VolumeKey        string            `json:"volumeKey,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
}

// Server serves the CubePod Control Plane and Panel Proxy routes.
type Server struct {
	reconciler *controller.Reconciler
	mu         sync.RWMutex
	sandboxes  map[string]*SandboxRecord
}

// NewServer constructs a new Gateway Server.
func NewServer(reconciler *controller.Reconciler) *Server {
	return &Server{
		reconciler: reconciler,
		sandboxes:  make(map[string]*SandboxRecord),
	}
}

// Handler returns the HTTP router for the CubePod Gateway.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /sandboxes", s.handleCreateSandbox)
	mux.HandleFunc("GET /sandboxes/{sandboxID}", s.handleGetSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/pause", s.handlePauseSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/connect", s.handleConnectSandbox)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/reset-rootfs", s.handleResetRootfs)
	mux.HandleFunc("POST /sandboxes/{sandboxID}/fork", s.handleForkSandbox)
	mux.HandleFunc("DELETE /sandboxes/{sandboxID}", s.handleDeleteSandbox)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "cubepod-gateway"})
}

type createSandboxReq struct {
	TemplateID string            `json:"templateID"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Extension  struct {
		VolumeKey string `json:"volumeKey,omitempty"`
	} `json:"extension,omitempty"`
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.TemplateID == "" {
		req.TemplateID = "cubeplex-ubuntu24-desktop"
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

	rec := &SandboxRecord{
		SandboxID:        id,
		TemplateID:       req.TemplateID,
		State:            string(status.Phase),
		EnvdPort:         49983,
		RootfsGeneration: status.ObservedRootfsGen,
		VolumeKey:        req.Extension.VolumeKey,
		Metadata:         req.Metadata,
		CreatedAt:        time.Now().UTC(),
	}

	s.mu.Lock()
	s.sandboxes[id] = rec
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rec)
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
	// Auto-resume from Paused if needed (E2B connect semantics).
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

	child := &SandboxRecord{
		SandboxID:        childID,
		TemplateID:       parent.TemplateID,
		State:            string(status.Phase),
		EnvdPort:         49983,
		RootfsGeneration: status.ObservedRootfsGen,
		CreatedAt:        time.Now().UTC(),
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
	delete(s.sandboxes, id)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
