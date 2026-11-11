package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

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

func (s *Server) recordActivity(rec *SandboxRecord) {
	if rec == nil {
		return
	}
	now := time.Now().UTC()
	rec.LastActivityAt = &now
}

func waitForPoddReady(ctx context.Context, poddURL string, timeout time.Duration) error {
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

func pollPoddHealth(ctx context.Context, poddURL string, timeout time.Duration) error {
	return waitForPoddReady(ctx, poddURL, timeout)
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
		_ = waitForPoddReady(r.Context(), rec.PoddURL, 15*time.Second)
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
		rec.PoddURL = ""
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
		if s.livePodRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)
			_ = waitForPoddReady(r.Context(), rec.PoddURL, 30*time.Second)
		}
		s.recordActivity(rec)
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
	s.handleSetTimeout(w, r)
}

func (s *Server) handleSetTimeout(w http.ResponseWriter, r *http.Request) {
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
	s.recordActivity(rec)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

type leaseReq struct {
	LeaseSeconds int `json:"leaseSeconds"`
}

func (s *Server) handleLeaseSandbox(w http.ResponseWriter, r *http.Request) {
	s.handleAcquireLease(w, r)
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
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
	s.recordActivity(rec)
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
		if s.livePodRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)
			_ = waitForPoddReady(r.Context(), rec.PoddURL, 30*time.Second)
		}
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
	deleteVolumes := r.URL.Query().Get("deleteVolumes") == "true"
	_ = s.reconciler.DeleteBox(r.Context(), id, deleteVolumes)

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

	s.recordActivity(rec)

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
			_ = waitForPoddReady(ctx, rec.PoddURL, 15*time.Second)
		}
	} else if s.livePodRouting && rec.PoddURL == "" {
		if boxStatus, ok := s.reconciler.GetBox(rec.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", boxStatus.PodIP, rec.EnvdPort)
			_ = waitForPoddReady(ctx, rec.PoddURL, 15*time.Second)
		}
	}
	return nil
}
