package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

type createSandboxReq struct {
	TemplateID  string            `json:"templateID"`
	Template_ID string            `json:"template_id,omitempty"`
	WarmPoolRef string            `json:"warmPoolRef,omitempty"`
	Timeout     int               `json:"timeout,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	EnvVars     map[string]string `json:"envVars,omitempty"`
	Env_Vars    map[string]string `json:"env_vars,omitempty"`
	AutoPause   *bool             `json:"autoPause,omitempty"`
	AutoResume  any               `json:"autoResume,omitempty"`
	Extension   struct {
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

	var cpuCount = 2
	var memoryMB = 2048
	var diskSizeMB = 10240
	var resolvedReadyCmd string

	if s.E2BTemplateAPIEnabled() {
		if s.reconciler.KubeClient() != nil {
			_ = s.reconciler.SyncTemplatesFromKube(r.Context())
		}
		rawTemplateID := req.TemplateID
		if rawTemplateID == "" && req.Template_ID != "" {
			rawTemplateID = req.Template_ID
		}
		resolved, err := s.reconciler.ResolveTemplate(rawTemplateID)
		if err != nil {
			if errors.Is(err, controller.ErrTemplateNotReady) {
				writeError(w, http.StatusConflict, "template is not ready")
				return
			}
			if errors.Is(err, controller.ErrTemplateNotFound) {
				writeError(w, http.StatusNotFound, "template not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		cpuCount = int(resolved.CPUCount)
		memoryMB = int(resolved.MemoryMB)
		diskSizeMB = int(resolved.DiskSizeMB)
		resolvedReadyCmd = resolved.ReadyCmd
		if rawTemplateID == "" {
			req.TemplateID = "base"
		} else {
			req.TemplateID = rawTemplateID
		}
	} else {
		if req.TemplateID == "" && req.Template_ID != "" {
			req.TemplateID = req.Template_ID
		}
		if req.TemplateID == "" || req.TemplateID == "base" || req.TemplateID == "code-interpreter" || req.TemplateID == "code-interpreter-v1" || req.TemplateID == "python" {
			req.TemplateID = "cubeplex-ubuntu24-desktop"
		}
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
		WarmPoolRef:      req.WarmPoolRef,
	}

	// Automatic WarmPool Claim: check if there is an available warmpool for this template if not explicitly specified
	if spec.WarmPoolRef == "" {
		warmPoolName := s.reconciler.FindAvailableWarmPool(req.TemplateID)
		if warmPoolName == "" {
			// Run a quick SyncFromKube to discover any newly added standby pods from the controller before falling back to cold start
			_, _ = s.reconciler.SyncFromKube(r.Context())
			warmPoolName = s.reconciler.FindAvailableWarmPool(req.TemplateID)
		}
		if warmPoolName != "" {
			spec.WarmPoolRef = warmPoolName
		}
	}

	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil && spec.WarmPoolRef != "" {
		slog.Warn("warmpool claim failed, refreshing standbys from Kubernetes via SyncFromKube and retrying", "sandboxID", id, "pool", spec.WarmPoolRef, "err", err)
		if _, syncErr := s.reconciler.SyncFromKube(r.Context()); syncErr != nil {
			slog.Warn("SyncFromKube failed during claim retry", "err", syncErr)
			// do not fall through to find on stale map; fall back to cold start immediately
			spec.WarmPoolRef = ""
			status, err = s.reconciler.Reconcile(r.Context(), id, spec)
		} else {
			if req.WarmPoolRef == "" {
				spec.WarmPoolRef = s.reconciler.FindAvailableWarmPool(req.TemplateID)
			}
			if spec.WarmPoolRef != "" {
				status, err = s.reconciler.Reconcile(r.Context(), id, spec)
			}
			if err != nil || spec.WarmPoolRef == "" {
				slog.Warn("warmpool claim retry failed, falling back to cold start", "sandboxID", id, "pool", spec.WarmPoolRef, "err", err)
				spec.WarmPoolRef = ""
				status, err = s.reconciler.Reconcile(r.Context(), id, spec)
			}
		}
	}
	if err != nil {
		if errors.Is(err, controller.ErrTemplateNotReady) {
			writeError(w, http.StatusConflict, "template is not ready")
			return
		}
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.E2BTemplateAPIEnabled() {
		s.reconciler.RecordTemplateSpawn(r.Context(), req.TemplateID)
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
	endAt := now.Add(time.Duration(timeoutSecs) * time.Second)
	rec := &SandboxRecord{
		SandboxID:        id,
		TemplateID:       req.TemplateID,
		ClientID:         "cpod-client-01",
		EnvdVersion:      "1.1.0",
		StartedAt:        now,
		EndAt:            endAt,
		CPUCount:         cpuCount,
		MemoryMB:         memoryMB,
		DiskSizeMB:       diskSizeMB,
		State:            string(status.Phase),
		Reason:           status.Reason,
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
		if _, _, splitErr := net.SplitHostPort(status.PodIP); splitErr == nil {
			rec.PoddURL = "http://" + status.PodIP
		} else {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", status.PodIP, rec.EnvdPort)
		}
		healthTimeout := 15 * time.Second
		if probeTimeout := s.ProbeTimeout(); probeTimeout > 0 && probeTimeout < healthTimeout {
			healthTimeout = probeTimeout
		}
		_ = waitForPoddReady(r.Context(), rec.PoddURL, healthTimeout)
	}

	if s.E2BTemplateAPIEnabled() && resolvedReadyCmd != "" && status.Reason != "WarmPoolClaimed" && rec.PoddURL != "" {
		if err := s.waitForTemplateReadyCmd(r.Context(), rec.PoddURL, resolvedReadyCmd, s.ProbeTimeout()); err != nil {
			rec.State = string(v1alpha1.PodBoxPhaseFailed)
			rec.Reason = "TemplateReadyProbeTimedOut"
			s.mu.Lock()
			s.sandboxes[id] = rec
			s.mu.Unlock()
			s.markPodBoxFailed(r.Context(), id, "TemplateReadyProbeTimedOut")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":      http.StatusServiceUnavailable,
				"message":   "template ready command timed out",
				"sandboxID": rec.SandboxID,
				"logPath":   "/var/log/cubepod/start-cmd.log",
			})
			return
		}
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

type refreshReq struct {
	Duration int `json:"duration"`
}

func (s *Server) handleRefreshSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	var req refreshReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	secs := req.Duration
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
	rec.TimeoutSeconds = secs
	now := time.Now().UTC()
	rec.LastActivityAt = &now
	rec.EndAt = now.Add(time.Duration(secs) * time.Second)
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
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

	s.copyResolvedImageAnnotation(r.Context(), parentID, childID)

	workspaceDir := filepath.Join(s.workspaceBaseDir, childID, "workspace")
	_ = os.MkdirAll(workspaceDir, 0755)

	cpuCount := parent.CPUCount
	if cpuCount <= 0 {
		cpuCount = 2
	}
	memoryMB := parent.MemoryMB
	if memoryMB <= 0 {
		memoryMB = 2048
	}
	diskSizeMB := parent.DiskSizeMB
	if diskSizeMB <= 0 {
		diskSizeMB = 10240
	}

	now := time.Now().UTC()
	endAt := now.Add(time.Duration(parent.TimeoutSeconds) * time.Second)
	child := &SandboxRecord{
		SandboxID:        childID,
		TemplateID:       parent.TemplateID,
		ClientID:         "cpod-client-01",
		EnvdVersion:      "1.1.0",
		StartedAt:        now,
		EndAt:            endAt,
		CPUCount:         cpuCount,
		MemoryMB:         memoryMB,
		DiskSizeMB:       diskSizeMB,
		State:            string(status.Phase),
		Reason:           status.Reason,
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
			_ = waitForPoddReady(ctx, rec.PoddURL, 45*time.Second)
		}
	} else if s.livePodRouting && rec.PoddURL == "" {
		if boxStatus, ok := s.reconciler.GetBox(rec.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			rec.PoddURL = fmt.Sprintf("http://%s:%d", boxStatus.PodIP, rec.EnvdPort)
			_ = waitForPoddReady(ctx, rec.PoddURL, 45*time.Second)
		}
	}
	return nil
}

func (s *Server) waitForTemplateReadyCmd(ctx context.Context, poddURL, readyCmd string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	interval := 500 * time.Millisecond
	if timeout < interval {
		interval = 25 * time.Millisecond
	}

	payload, err := json.Marshal(map[string]string{"command": readyCmd})
	if err != nil {
		return err
	}
	runURL := strings.TrimRight(poddURL, "/") + "/v1/commands/run"
	client := &http.Client{Timeout: 5 * time.Second}

	tryOnce := func() bool {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, runURL, bytes.NewReader(payload))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var out struct {
			ExitCode      *int `json:"exit_code"`
			CamelExitCode *int `json:"exitCode"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return false
		}
		if out.ExitCode != nil && *out.ExitCode == 0 {
			return true
		}
		if out.CamelExitCode != nil && *out.CamelExitCode == 0 {
			return true
		}
		return false
	}

	if tryOnce() {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-probeCtx.Done():
			return probeCtx.Err()
		case <-ticker.C:
			if tryOnce() {
				return nil
			}
		}
	}
}

func (s *Server) markPodBoxFailed(ctx context.Context, boxName, reason string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	kc := s.reconciler.KubeClient()
	boxes, err := kc.ListPodBoxes(ctx, "")
	if err != nil {
		return
	}
	for _, box := range boxes {
		if box.Metadata.Name != boxName {
			continue
		}
		box.Status.Phase = v1alpha1.PodBoxPhaseFailed
		box.Status.Reason = reason
		type podBoxStatusUpdater interface {
			UpdatePodBoxStatus(context.Context, v1alpha1.PodBox) error
		}
		if u, ok := kc.(podBoxStatusUpdater); ok {
			_ = u.UpdatePodBoxStatus(ctx, box)
			return
		}
		_ = kc.UpsertPodBox(ctx, box)
		return
	}
}

func (s *Server) copyResolvedImageAnnotation(ctx context.Context, parentID, childID string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	kc := s.reconciler.KubeClient()
	boxes, err := kc.ListPodBoxes(ctx, "")
	if err != nil {
		return
	}
	var parentResolvedImage string
	var childBox *v1alpha1.PodBox
	for i := range boxes {
		if boxes[i].Metadata.Name == parentID && boxes[i].Metadata.Annotations != nil {
			parentResolvedImage = boxes[i].Metadata.Annotations["cubepod.io/resolved-image"]
		}
		if boxes[i].Metadata.Name == childID {
			b := boxes[i]
			childBox = &b
		}
	}
	if parentResolvedImage == "" || childBox == nil {
		return
	}
	if childBox.Metadata.Annotations == nil {
		childBox.Metadata.Annotations = map[string]string{}
	}
	childBox.Metadata.Annotations["cubepod.io/resolved-image"] = parentResolvedImage
	_ = kc.UpsertPodBox(ctx, *childBox)
}
