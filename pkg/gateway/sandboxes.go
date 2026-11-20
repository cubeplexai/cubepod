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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/events"
	"github.com/cubeplexai/cubepod/pkg/secret"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

type createSandboxReq struct {
	TemplateID    string                      `json:"templateID"`
	Template_ID   string                      `json:"template_id,omitempty"`
	WarmPoolRef   string                      `json:"warmPoolRef,omitempty"`
	Timeout       int                         `json:"timeout,omitempty"`
	Metadata      map[string]string           `json:"metadata,omitempty"`
	VolumeMounts  []sandboxVolumeMountReq     `json:"volumeMounts,omitempty"`
	EnvVars       map[string]string           `json:"envVars,omitempty"`
	Env_Vars      map[string]string           `json:"env_vars,omitempty"`
	AutoPause     *bool                       `json:"autoPause,omitempty"`
	AutoResume    any                         `json:"autoResume,omitempty"`
	Network       *e2bNetworkSpec             `json:"network,omitempty"`
	NetworkPolicy *v1alpha1.NetworkPolicySpec `json:"networkPolicy,omitempty"`
	Extension     struct {
		VolumeKey      string                      `json:"volumeKey,omitempty"`
		NetworkPolicy  *v1alpha1.NetworkPolicySpec `json:"networkPolicy,omitempty"`
		SecretBindings []struct {
			EnvName        string   `json:"envName"`
			RawSecret      string   `json:"rawSecret"`
			SecretName     string   `json:"secretName,omitempty"`
			SecretID       string   `json:"secretID,omitempty"`
			AllowedHosts   []string `json:"allowedHosts"`
			AllowedHeaders []string `json:"allowedHeaders"`
		} `json:"secretBindings,omitempty"`
	} `json:"extension,omitempty"`
}

// sandboxVolumeMountReq is the official SandboxVolumeMount wire object.
// name is the volume name, path is the absolute path inside the sandbox.
type sandboxVolumeMountReq struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func cloneNetworkPolicy(p *v1alpha1.NetworkPolicySpec) *v1alpha1.NetworkPolicySpec {
	if p == nil {
		return nil
	}
	cp := *p
	switch strings.ToLower(strings.TrimSpace(string(cp.DefaultAction))) {
	case "allow":
		cp.DefaultAction = v1alpha1.NetworkDefaultAllow
	case "deny":
		cp.DefaultAction = v1alpha1.NetworkDefaultDeny
	}
	if len(p.AllowHosts) > 0 {
		cp.AllowHosts = append([]string(nil), p.AllowHosts...)
	}
	if len(p.DenyHosts) > 0 {
		cp.DenyHosts = append([]string(nil), p.DenyHosts...)
	}
	if len(p.Egress) > 0 {
		cp.Egress = make([]v1alpha1.NetworkEgressRule, len(p.Egress))
		for i, rule := range p.Egress {
			switch strings.ToLower(strings.TrimSpace(rule.Action)) {
			case "allow":
				rule.Action = "allow"
			case "deny":
				rule.Action = "deny"
			}
			cp.Egress[i] = rule
		}
	}
	if len(p.SecretBindings) > 0 {
		cp.SecretBindings = make([]v1alpha1.SecretBinding, len(p.SecretBindings))
		for i, sb := range p.SecretBindings {
			sbCopy := sb
			if len(sb.AllowedHosts) > 0 {
				sbCopy.AllowedHosts = append([]string(nil), sb.AllowedHosts...)
			}
			if len(sb.AllowedHeaders) > 0 {
				sbCopy.AllowedHeaders = append([]string(nil), sb.AllowedHeaders...)
			}
			cp.SecretBindings[i] = sbCopy
		}
	}
	if len(p.HeaderTransforms) > 0 {
		cp.HeaderTransforms = make([]v1alpha1.HeaderTransformRule, len(p.HeaderTransforms))
		for i, ht := range p.HeaderTransforms {
			htCopy := v1alpha1.HeaderTransformRule{Host: ht.Host}
			if ht.Headers != nil {
				htCopy.Headers = make(map[string]string, len(ht.Headers))
				for k, v := range ht.Headers {
					htCopy.Headers[k] = v
				}
			}
			cp.HeaderTransforms[i] = htCopy
		}
	}
	if len(p.SecretRefBindings) > 0 {
		cp.SecretRefBindings = make([]v1alpha1.SecretRefBinding, len(p.SecretRefBindings))
		for i, rb := range p.SecretRefBindings {
			rbCopy := rb
			if len(rb.AllowedHosts) > 0 {
				rbCopy.AllowedHosts = append([]string(nil), rb.AllowedHosts...)
			}
			if len(rb.AllowedHeaders) > 0 {
				rbCopy.AllowedHeaders = append([]string(nil), rb.AllowedHeaders...)
			}
			cp.SecretRefBindings[i] = rbCopy
		}
	}
	return &cp
}

func (s *Server) workspaceDirForSandbox(sandboxID, volumeKey string) string {
	if volumeKey != "" {
		return filepath.Join(s.workspaceBaseDir, "volumes", controller.SanitizeVolumeKey(volumeKey), "workspace")
	}
	return filepath.Join(s.workspaceBaseDir, sandboxID, "workspace")
}

func formatPoddURL(podIP string, envdPort int) string {
	if podIP == "" || podIP == "10.244.0.42" {
		return ""
	}
	if envdPort <= 0 {
		envdPort = 49983
	}
	if _, _, err := net.SplitHostPort(podIP); err == nil {
		return "http://" + podIP
	}
	return fmt.Sprintf("http://%s:%d", podIP, envdPort)
}

func (s *Server) lookupPodBox(ctx context.Context, sandboxID string) (*v1alpha1.PodBox, bool, error) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return nil, false, nil
	}
	kc := s.reconciler.KubeClient()
	if box, err := kc.GetPodBox(ctx, "", sandboxID); err == nil && box != nil {
		return box, true, nil
	}
	boxes, err := kc.ListPodBoxes(ctx, "")
	if err != nil {
		return nil, false, err
	}
	for i := range boxes {
		if boxes[i].Metadata.Name == sandboxID {
			b := boxes[i]
			return &b, true, nil
		}
	}
	return nil, false, nil
}

const defaultPodBoxSyncInterval = 5 * time.Second

func (s *Server) retainSandboxLock(sandboxID string) *sandboxOpLock {
	s.sandboxMu.Lock()
	if s.sandboxLocks == nil {
		s.sandboxLocks = make(map[string]*sandboxOpLock)
	}
	entry := s.sandboxLocks[sandboxID]
	if entry == nil {
		entry = &sandboxOpLock{}
		s.sandboxLocks[sandboxID] = entry
	}
	entry.refs++
	s.sandboxMu.Unlock()
	return entry
}

func (s *Server) releaseSandboxLockRef(sandboxID string, entry *sandboxOpLock) {
	s.sandboxMu.Lock()
	entry.refs--
	if entry.refs == 0 && s.sandboxLocks[sandboxID] == entry {
		delete(s.sandboxLocks, sandboxID)
	}
	s.sandboxMu.Unlock()
}

func (s *Server) acquireSandboxLock(sandboxID string) func() {
	entry := s.retainSandboxLock(sandboxID)
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.releaseSandboxLockRef(sandboxID, entry)
	}
}

func (s *Server) tryAcquireSandboxLock(sandboxID string) (func(), bool) {
	entry := s.retainSandboxLock(sandboxID)
	if !entry.mu.TryLock() {
		s.releaseSandboxLockRef(sandboxID, entry)
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		s.releaseSandboxLockRef(sandboxID, entry)
	}, true
}

func cloneSandboxRecord(rec *SandboxRecord) *SandboxRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.NetworkPolicy = cloneNetworkPolicy(rec.NetworkPolicy)
	if rec.Metadata != nil {
		cp.Metadata = make(map[string]string, len(rec.Metadata))
		for k, v := range rec.Metadata {
			cp.Metadata[k] = v
		}
	}
	if rec.LeaseExpiresAt != nil {
		t := *rec.LeaseExpiresAt
		cp.LeaseExpiresAt = &t
	}
	if rec.LastActivityAt != nil {
		t := *rec.LastActivityAt
		cp.LastActivityAt = &t
	}
	return &cp
}

func (s *Server) syncFromKube(ctx context.Context) (int, error) {
	if s.reconciler == nil {
		return 0, nil
	}
	s.crdResyncsTotal.Add(1)
	return s.reconciler.SyncFromKube(ctx)
}

// CRDResyncsTotal returns the number of Kubernetes CRD resyncs performed by the gateway.
func (s *Server) CRDResyncsTotal() uint64 {
	return s.crdResyncsTotal.Load()
}

// SetPodBoxSyncInterval configures the periodic Kubernetes PodBox sync interval and
// restarts the background worker if livePodRouting is active.
func (s *Server) SetPodBoxSyncInterval(interval time.Duration) {
	s.syncMu.Lock()
	s.syncInterval = interval
	s.syncMu.Unlock()
	s.reconcileSyncWorker()
}

func (s *Server) reconcileSyncWorker() {
	s.mu.RLock()
	live := s.livePodRouting
	hasKube := s.reconciler != nil && s.reconciler.KubeClient() != nil
	s.mu.RUnlock()

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if s.syncCancel != nil {
		s.syncCancel()
		s.syncCancel = nil
		s.syncWG.Wait()
	}

	if !live || !hasKube {
		return
	}

	interval := s.syncInterval
	if interval <= 0 {
		interval = defaultPodBoxSyncInterval
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.syncCancel = cancel
	s.syncWG.Add(1)
	go s.runPodBoxSyncLoop(ctx, interval)
}

func (s *Server) runPodBoxSyncLoop(ctx context.Context, interval time.Duration) {
	defer s.syncWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncPodBoxesOnce(ctx)
		}
	}
}

// Close stops background gateway workers (PodBox sync and webhook delivery).
func (s *Server) Close() {
	s.syncMu.Lock()
	if s.syncCancel != nil {
		s.syncCancel()
		s.syncCancel = nil
	}
	s.syncWG.Wait()
	s.syncMu.Unlock()
	s.CloseEvents()
}

// SyncPodBoxesOnce reconciles cached sandboxes against current Kubernetes PodBoxes
// and emits lifecycle events for asynchronous idle-reaper transitions.
func (s *Server) SyncPodBoxesOnce(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.RLock()
	live := s.livePodRouting
	r := s.reconciler
	s.mu.RUnlock()
	if !live || r == nil || r.KubeClient() == nil {
		return
	}

	s.mu.RLock()
	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}
	s.mu.RUnlock()

	boxes, err := r.KubeClient().ListPodBoxes(ctx, "")
	if err != nil {
		return
	}
	_, _ = s.syncFromKube(ctx)

	byName := make(map[string]v1alpha1.PodBox, len(boxes))
	for _, box := range boxes {
		if box.Metadata.Name != "" {
			byName[box.Metadata.Name] = box
		}
	}

	for _, id := range ids {
		unlock, ok := s.tryAcquireSandboxLock(id)
		if !ok {
			continue
		}
		box, found := byName[id]
		if !found {
			// Re-verify under the per-sandbox lock so a concurrent create/fork is never
			// evicted due to an older ListPodBoxes snapshot.
			if liveBox, getErr := r.KubeClient().GetPodBox(ctx, r.Namespace(), id); getErr == nil && liveBox != nil {
				box = *liveBox
				found = true
			} else if getErr != nil && !isBoxNotFoundErr(getErr) {
				unlock()
				continue
			}
		}
		s.reconcileCachedSandboxWithPodBox(id, box, found)
		unlock()
	}
}

func isBoxNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

// observedTransitionKey derives a replica-independent dedupe key for a PodBox phase
// transition so every gateway replica computes the same deterministic event ID.
func observedTransitionKey(box v1alpha1.PodBox) string {
	key := string(box.Status.Phase) + "|" + box.Status.Reason
	if box.Status.PausedAt != nil {
		key += "|" + strconv.FormatInt(box.Status.PausedAt.UTC().UnixNano(), 10)
	}
	return key
}

func (s *Server) reconcileCachedSandboxWithPodBox(sandboxID string, box v1alpha1.PodBox, found bool) (*SandboxRecord, bool) {
	if !found {
		var wireRec *SandboxRecord
		var tenantHash string
		var emitKilled bool

		s.mu.Lock()
		rec, ok := s.sandboxes[sandboxID]
		if ok && rec != nil {
			wireRec = cloneSandboxRecord(rec)
			if !rec.explicitlyDeleted && rec.lastEmittedLifecycleState != events.EventKilled {
				rec.lastEmittedLifecycleState = events.EventKilled
				emitKilled = true
				tenantHash = rec.OwnerTenantHash
			}
			delete(s.sandboxes, sandboxID)
			delete(s.endpoints, sandboxID)
		}
		s.mu.Unlock()

		if ok {
			s.releaseSandboxVolumeMounts(sandboxID)
			s.releaseLocalSandbox(sandboxID)
			s.Metrics().sandboxMetricsStore().delete(sandboxID)
			if s.vault != nil {
				s.vault.RevokeSandbox(sandboxID)
			}
			if wireRec != nil && wireRec.WorkspaceDir != "" && wireRec.VolumeKey == "" {
				_ = os.RemoveAll(filepath.Dir(wireRec.WorkspaceDir))
			}
		}
		if emitKilled && wireRec != nil {
			s.emitObservedSandboxEvent(tenantHash, events.EventKilled, wireRec, "podbox-deleted", &events.EventData{
				Execution: executionDataForRecord(wireRec),
			})
		}
		return nil, false
	}

	var wireRec *SandboxRecord
	var tenantHash string
	var emitPaused bool
	var emitKilled bool

	s.mu.Lock()
	rec, ok := s.sandboxes[sandboxID]
	if !ok || rec == nil {
		s.mu.Unlock()
		return nil, false
	}
	prevState := rec.State
	if box.Status.Phase != "" {
		rec.State = string(box.Status.Phase)
	}
	rec.Reason = box.Status.Reason
	if box.Spec.VolumeKey != "" && rec.VolumeKey == "" {
		rec.VolumeKey = box.Spec.VolumeKey
	}
	if box.Spec.NetworkPolicy != nil && rec.NetworkPolicy == nil {
		rec.NetworkPolicy = cloneNetworkPolicy(box.Spec.NetworkPolicy)
	}
	if box.Status.ObservedRootfsGen > 0 {
		rec.RootfsGeneration = box.Status.ObservedRootfsGen
	}
	if box.Status.Phase == v1alpha1.PodBoxPhasePaused && box.Status.PodIP == "" {
		rec.PoddURL = ""
	} else if poddURL := formatPoddURL(box.Status.PodIP, rec.EnvdPort); poddURL != "" {
		rec.PoddURL = poddURL
	}

	switch box.Status.Phase {
	case v1alpha1.PodBoxPhasePaused:
		if !rec.explicitlyPaused && rec.lastEmittedLifecycleState != events.EventPaused {
			rec.lastEmittedLifecycleState = events.EventPaused
			emitPaused = true
			wireRec = cloneSandboxRecord(rec)
			tenantHash = rec.OwnerTenantHash
		}
	case v1alpha1.PodBoxPhaseTerminated:
		if !rec.explicitlyDeleted && rec.lastEmittedLifecycleState != events.EventKilled {
			rec.lastEmittedLifecycleState = events.EventKilled
			emitKilled = true
			wireRec = cloneSandboxRecord(rec)
			tenantHash = rec.OwnerTenantHash
		}
	case v1alpha1.PodBoxPhaseRunning:
		if prevState == string(v1alpha1.PodBoxPhasePaused) || prevState == "Paused" {
			rec.lastEmittedLifecycleState = events.EventResumed
		}
	}
	s.mu.Unlock()

	if emitPaused && wireRec != nil {
		s.emitObservedSandboxEvent(tenantHash, events.EventPaused, wireRec, observedTransitionKey(box), &events.EventData{
			PauseMode: "filesystem",
			Execution: executionDataForRecord(wireRec),
		})
	}
	if emitKilled && wireRec != nil {
		s.emitObservedSandboxEvent(tenantHash, events.EventKilled, wireRec, "podbox-terminated", &events.EventData{
			Execution: executionDataForRecord(wireRec),
		})
	}
	return rec, true
}

// resolveSandbox returns a cached SandboxRecord or reconstructs it from Kubernetes PodBox state
// on cache miss (CEP-0022 stateless multi-replica HA). When livePodRouting is active and the
// PodBox has been deleted in Kubernetes, stale cached entries are evicted.
func (s *Server) resolveSandbox(ctx context.Context, sandboxID string) (*SandboxRecord, bool) {
	if sandboxID == "" {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	liveRouting := s.livePodRouting
	s.mu.RUnlock()

	if ok {
		if liveRouting && s.reconciler != nil && s.reconciler.KubeClient() != nil {
			box, found, err := s.lookupPodBox(ctx, sandboxID)
			if err == nil {
				var boxVal v1alpha1.PodBox
				if box != nil {
					boxVal = *box
				}
				return s.reconcileCachedSandboxWithPodBox(sandboxID, boxVal, found)
			}
		}
		return rec, true
	}

	if !s.FeatureGateEnabled(FeatureGateStatelessGatewayHA) {
		return nil, false
	}

	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return nil, false
	}

	box, found, err := s.lookupPodBox(ctx, sandboxID)
	if err != nil || !found {
		return nil, false
	}

	_, _ = s.syncFromKube(ctx)

	templateID := box.Spec.TemplateRef
	timeoutSecs := 1800
	volumeKey := box.Spec.VolumeKey
	var metadata map[string]string
	var ownerTenant string
	if box.Metadata.Annotations != nil {
		if ot := box.Metadata.Annotations["cubepod.io/owner-tenant"]; ot != "" {
			ownerTenant = ot
		}
		if templateID == "" {
			templateID = box.Metadata.Annotations["cubepod.io/template-id"]
		}
		if rawTimeout := box.Metadata.Annotations["cubepod.io/timeout-seconds"]; rawTimeout != "" {
			if parsed, convErr := strconv.Atoi(rawTimeout); convErr == nil && parsed > 0 {
				timeoutSecs = parsed
			}
		}
		if volumeKey == "" {
			volumeKey = box.Metadata.Annotations["cubepod.io/volume-key"]
		}
		for k, v := range box.Metadata.Annotations {
			if strings.HasPrefix(k, "cubepod.io/metadata-") {
				if metadata == nil {
					metadata = make(map[string]string)
				}
				metadata[strings.TrimPrefix(k, "cubepod.io/metadata-")] = v
			}
		}
	}
	if templateID == "" {
		templateID = "base"
	}

	now := time.Now().UTC()
	createdAt := now
	if box.Status.LastActivityAt != nil && !box.Status.LastActivityAt.IsZero() {
		createdAt = *box.Status.LastActivityAt
	}
	rootfsGen := box.Status.ObservedRootfsGen
	if rootfsGen == 0 {
		rootfsGen = box.Spec.RootfsGeneration
	}
	if rootfsGen == 0 {
		rootfsGen = 1
	}

	initialLifecycle := events.EventCreated
	switch box.Status.Phase {
	case v1alpha1.PodBoxPhasePaused:
		initialLifecycle = events.EventPaused
	case v1alpha1.PodBoxPhaseTerminated:
		initialLifecycle = events.EventKilled
	}

	reconstructed := &SandboxRecord{
		SandboxID:                 sandboxID,
		TemplateID:                templateID,
		ClientID:                  "cpod-client-01",
		EnvdVersion:               "1.1.0",
		StartedAt:                 createdAt,
		EndAt:                     createdAt.Add(time.Duration(timeoutSecs) * time.Second),
		CPUCount:                  2,
		MemoryMB:                  2048,
		DiskSizeMB:                10240,
		State:                     string(box.Status.Phase),
		Reason:                    box.Status.Reason,
		EnvdPort:                  49983,
		PoddURL:                   formatPoddURL(box.Status.PodIP, 49983),
		RootfsGeneration:          rootfsGen,
		VolumeKey:                 volumeKey,
		NetworkPolicy:             cloneNetworkPolicy(box.Spec.NetworkPolicy),
		Metadata:                  metadata,
		CreatedAt:                 createdAt,
		LastActivityAt:            &now,
		TimeoutSeconds:            timeoutSecs,
		WorkspaceDir:              s.workspaceDirForSandbox(sandboxID, volumeKey),
		OwnerTenantHash:           ownerTenant,
		lastEmittedLifecycleState: initialLifecycle,
	}

	s.mu.Lock()
	if existing, exists := s.sandboxes[sandboxID]; exists {
		s.mu.Unlock()
		return existing, true
	}
	s.sandboxes[sandboxID] = reconstructed
	s.mu.Unlock()
	return reconstructed, true
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

	if !s.FeatureGateEnabled(FeatureGateTransparentEgress) && (req.Network != nil || req.NetworkPolicy != nil || req.Extension.NetworkPolicy != nil || len(req.Extension.SecretBindings) > 0) {
		writeError(w, http.StatusNotImplemented, "transparent egress feature gate is disabled")
		return
	}
	if !s.FeatureGateEnabled(FeatureGateE2BSecretsAPI) {
		for _, sb := range req.Extension.SecretBindings {
			if sb.SecretName != "" || sb.SecretID != "" {
				writeError(w, http.StatusNotImplemented, "E2B secrets API feature gate is disabled")
				return
			}
		}
	}

	volumeKey := req.Extension.VolumeKey
	plannedMounts, mountStatus, mountMsg := s.planSandboxVolumeMounts(r, req.VolumeMounts)
	if mountStatus != 0 {
		writeError(w, mountStatus, mountMsg)
		return
	}
	netPolicy := cloneNetworkPolicy(req.Extension.NetworkPolicy)
	if netPolicy == nil {
		netPolicy = cloneNetworkPolicy(req.NetworkPolicy)
	}

	id := fmt.Sprintf("cpod-%d", time.Now().UnixNano())
	resolvedNetPolicy, secretEnvVars, secErr := s.resolveSandboxSecretsAndNetwork(managementAPIKey(r), id, &req, netPolicy)
	if secErr != nil {
		if s.vault != nil {
			s.vault.RevokeSandbox(id)
		}
		switch {
		case errors.Is(secErr, secret.ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "API key is required")
		case errors.Is(secErr, secret.ErrNotFound):
			writeError(w, http.StatusNotFound, "secret not found")
		default:
			writeError(w, http.StatusBadRequest, secErr.Error())
		}
		return
	}
	netPolicy = resolvedNetPolicy

	spec := v1alpha1.PodBoxSpec{
		TemplateRef:      req.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		WarmPoolRef:      req.WarmPoolRef,
		VolumeKey:        volumeKey,
		E2BVolumeMounts:  s.plannedToPodBoxMounts(plannedMounts),
		NetworkPolicy:    cloneNetworkPolicy(netPolicy),
	}

	// Official E2B volume mounts or per-sandbox network/secret policies must be
	// present on the pod at create time, so a pre-warmed standby cannot be claimed.
	if len(plannedMounts) > 0 || netPolicy != nil {
		spec.WarmPoolRef = ""
	}

	// Automatic WarmPool Claim: skip when deterministic VolumeKey, official mounts,
	// or custom network/secret policies are requested.
	if spec.WarmPoolRef == "" && volumeKey == "" && len(plannedMounts) == 0 && netPolicy == nil {
		warmPoolName := s.reconciler.FindAvailableWarmPool(req.TemplateID)
		if warmPoolName == "" {
			// Run a quick SyncFromKube to discover any newly added standby pods from the controller before falling back to cold start
			_, _ = s.syncFromKube(r.Context())
			warmPoolName = s.reconciler.FindAvailableWarmPool(req.TemplateID)
		}
		if warmPoolName != "" {
			spec.WarmPoolRef = warmPoolName
		}
	}

	if err := s.commitSandboxVolumeMounts(id, managementAPIKey(r), plannedMounts); err != nil {
		if s.vault != nil {
			s.vault.RevokeSandbox(id)
		}
		if errors.Is(err, volume.ErrNotFound) {
			writeError(w, http.StatusNotFound, "volume not found")
			return
		}
		if errors.Is(err, volume.ErrInvalidPath) {
			writeError(w, http.StatusBadRequest, "invalid volume mount path")
			return
		}
		writeError(w, http.StatusConflict, "volume mount failed")
		return
	}

	ownerTenant := ""
	if key := managementAPIKey(r); key != "" {
		ownerTenant = events.TenantHash(key)
	}
	if ownerTenant != "" && s.reconciler != nil {
		s.reconciler.SetBoxAnnotations(id, map[string]string{"cubepod.io/owner-tenant": ownerTenant})
	}

	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil && spec.WarmPoolRef != "" {
		slog.Warn("warmpool claim failed, refreshing standbys from Kubernetes via SyncFromKube and retrying", "sandboxID", id, "pool", spec.WarmPoolRef, "err", err)
		if _, syncErr := s.syncFromKube(r.Context()); syncErr != nil {
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
		s.rollbackCreatedSandbox(r.Context(), id)
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

	workspaceDir := s.workspaceDirForSandbox(id, volumeKey)
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		s.rollbackCreatedSandbox(r.Context(), id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// ownerTenant already computed above before Reconcile
	now := time.Now().UTC()
	endAt := now.Add(time.Duration(timeoutSecs) * time.Second)
	rec := &SandboxRecord{
		SandboxID:                 id,
		TemplateID:                req.TemplateID,
		ClientID:                  "cpod-client-01",
		EnvdVersion:               "1.1.0",
		StartedAt:                 now,
		EndAt:                     endAt,
		CPUCount:                  cpuCount,
		MemoryMB:                  memoryMB,
		DiskSizeMB:                diskSizeMB,
		State:                     string(status.Phase),
		Reason:                    status.Reason,
		EnvdPort:                  49983,
		RootfsGeneration:          status.ObservedRootfsGen,
		VolumeKey:                 volumeKey,
		NetworkPolicy:             cloneNetworkPolicy(netPolicy),
		Metadata:                  req.Metadata,
		CreatedAt:                 now,
		LastActivityAt:            &now,
		TimeoutSeconds:            timeoutSecs,
		WorkspaceDir:              workspaceDir,
		OwnerTenantHash:           ownerTenant,
		lastEmittedLifecycleState: events.EventCreated,
	}

	if s.LivePodRouting() && status.PodIP != "" && status.PodIP != "10.244.0.42" {
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

	mergedEnv := mergeSandboxEnv(req)
	if len(secretEnvVars) > 0 {
		if mergedEnv == nil {
			mergedEnv = make(map[string]string, len(secretEnvVars))
		}
		for k, v := range secretEnvVars {
			mergedEnv[k] = v
		}
	}
	if err := s.startRuntimeSandbox(r.Context(), rec, "", mergedEnv); err != nil {
		s.rollbackCreatedSandbox(r.Context(), id)
		writeError(w, http.StatusInternalServerError, "failed to start sandbox runtime")
		return
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
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventCreated, wireRec, nil)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(wireRec)
}

func (s *Server) listSandboxesFiltered(ctx context.Context, stateFilter, templateFilter string, lowercaseState bool) []*SandboxRecord {
	if s.LivePodRouting() && s.FeatureGateEnabled(FeatureGateStatelessGatewayHA) && s.reconciler != nil && s.reconciler.KubeClient() != nil {
		if boxes, err := s.reconciler.KubeClient().ListPodBoxes(ctx, ""); err == nil {
			for i := range boxes {
				if boxes[i].Metadata.Name != "" {
					_, _ = s.resolveSandbox(ctx, boxes[i].Metadata.Name)
				}
			}
		}
	}

	s.mu.RLock()
	list := make([]*SandboxRecord, 0, len(s.sandboxes))
	for _, rec := range s.sandboxes {
		if stateFilter != "" && !strings.EqualFold(rec.State, stateFilter) {
			continue
		}
		if templateFilter != "" && rec.TemplateID != templateFilter {
			continue
		}
		wireRec := cloneSandboxRecord(rec)
		if lowercaseState {
			wireRec.State = strings.ToLower(wireRec.State)
		}
		list = append(list, wireRec)
	}
	s.mu.RUnlock()
	return list
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	list := s.listSandboxesFiltered(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("templateID"), false)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleListSandboxesV2(w http.ResponseWriter, r *http.Request) {
	list := s.listSandboxesFiltered(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("templateID"), true)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.mu.RLock()
	wireRec := cloneSandboxRecord(rec)
	s.mu.RUnlock()
	wireRec.State = strings.ToLower(wireRec.State)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
}

func (s *Server) handlePauseSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	if rec.State == string(v1alpha1.PodBoxPhaseFailed) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is failed")
		return
	}
	poddURL := rec.PoddURL
	templateID := rec.TemplateID
	rootfsGen := rec.RootfsGeneration
	volumeKey := rec.VolumeKey
	netPolicy := cloneNetworkPolicy(rec.NetworkPolicy)
	rec.explicitlyPaused = true
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

	spec := s.withOfficialMounts(v1alpha1.PodBoxSpec{
		TemplateRef:      templateID,
		DesiredState:     v1alpha1.DesiredStatePaused,
		RootfsGeneration: rootfsGen,
		VolumeKey:        volumeKey,
		NetworkPolicy:    netPolicy,
	}, id)
	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil {
		s.mu.Lock()
		rec.explicitlyPaused = false
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status.Phase == v1alpha1.PodBoxPhaseTerminated || status.Phase == v1alpha1.PodBoxPhaseFailed {
		s.mu.Lock()
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		rec.explicitlyPaused = false
		s.mu.Unlock()
		if status.Phase == v1alpha1.PodBoxPhaseFailed {
			writeError(w, http.StatusConflict, "sandbox is failed")
		} else {
			writeError(w, http.StatusConflict, "sandbox is terminated")
		}
		return
	}
	s.pauseRuntimeSandbox(r.Context(), id)

	s.mu.Lock()
	rec.State = string(status.Phase)
	rec.Reason = status.Reason
	rec.PoddURL = ""
	rec.explicitlyPaused = false
	shouldEmit := rec.lastEmittedLifecycleState != events.EventPaused
	rec.lastEmittedLifecycleState = events.EventPaused
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	if shouldEmit {
		s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventPaused, wireRec, &events.EventData{
			PauseMode: "filesystem",
			Execution: executionDataForRecord(wireRec),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
}

func (s *Server) handleConnectSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.mu.RLock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.RUnlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	if rec.State == string(v1alpha1.PodBoxPhaseFailed) {
		s.mu.RUnlock()
		writeError(w, http.StatusConflict, "sandbox is failed")
		return
	}
	wasPaused := rec.State == "Paused" || rec.State == string(v1alpha1.PodBoxPhasePaused)
	recCopy := cloneSandboxRecord(rec)
	liveRouting := s.livePodRouting
	s.mu.RUnlock()

	// Auto-resume from Paused if needed (E2B connect semantics).
	spec := s.withOfficialMounts(v1alpha1.PodBoxSpec{
		TemplateRef:      recCopy.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: recCopy.RootfsGeneration,
		VolumeKey:        recCopy.VolumeKey,
		NetworkPolicy:    cloneNetworkPolicy(recCopy.NetworkPolicy),
	}, id)
	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status.Phase == v1alpha1.PodBoxPhaseTerminated || status.Phase == v1alpha1.PodBoxPhaseFailed {
		s.mu.Lock()
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		s.mu.Unlock()
		if status.Phase == v1alpha1.PodBoxPhaseFailed {
			writeError(w, http.StatusConflict, "sandbox is failed")
		} else {
			writeError(w, http.StatusConflict, "sandbox is terminated")
		}
		return
	}

	if liveRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
		recCopy.PoddURL = formatPoddURL(status.PodIP, recCopy.EnvdPort)
		_ = waitForPoddReady(r.Context(), recCopy.PoddURL, 30*time.Second)
	}
	if err := s.startRuntimeSandbox(r.Context(), recCopy, "", nil); err != nil {
		s.mu.Lock()
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		if wasPaused && (status.Phase == v1alpha1.PodBoxPhaseRunning || strings.EqualFold(string(status.Phase), "running")) {
			rec.lastEmittedLifecycleState = events.EventResumed
		}
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	rec.State = string(status.Phase)
	rec.Reason = status.Reason
	if wasPaused {
		rec.AutoResumeCount++
		rec.lastEmittedLifecycleState = events.EventResumed
	}
	if recCopy.PoddURL != "" {
		rec.PoddURL = recCopy.PoddURL
	}
	s.recordActivity(rec)
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	if wasPaused {
		s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventResumed, wireRec, &events.EventData{
			ResumeMode: "reboot",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
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

	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	if s.reconciler != nil {
		if box, ok := s.reconciler.GetBox(id); ok && box != nil && box.Phase == v1alpha1.PodBoxPhaseTerminated {
			rec.State = string(box.Phase)
			rec.Reason = box.Reason
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "sandbox is terminated")
			return
		}
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

	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	if s.reconciler != nil {
		if box, ok := s.reconciler.GetBox(id); ok && box != nil && box.Phase == v1alpha1.PodBoxPhaseTerminated {
			rec.State = string(box.Phase)
			rec.Reason = box.Reason
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "sandbox is terminated")
			return
		}
	}
	rec.TimeoutSeconds = secs
	s.recordActivity(rec)
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventUpdated, wireRec, nil)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
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

	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	state := rec.State
	if s.reconciler != nil {
		if box, ok := s.reconciler.GetBox(id); ok && box != nil && box.Phase != "" {
			if box.Phase == v1alpha1.PodBoxPhaseTerminated || box.Phase == v1alpha1.PodBoxPhaseFailed || box.Phase == v1alpha1.PodBoxPhasePaused {
				rec.State = string(box.Phase)
				rec.Reason = box.Reason
				state = rec.State
			}
		}
	}
	switch state {
	case string(v1alpha1.PodBoxPhaseTerminated):
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	case string(v1alpha1.PodBoxPhaseFailed):
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is failed")
		return
	case string(v1alpha1.PodBoxPhasePaused):
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is paused")
		return
	}
	rec.LeaseSeconds = secs
	exp := time.Now().UTC().Add(time.Duration(secs) * time.Second)
	rec.LeaseExpiresAt = &exp
	s.recordActivity(rec)
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	if s.reconciler != nil {
		_ = s.reconciler.AcquireLease(id, exp)
	}

	s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventUpdated, wireRec, &events.EventData{
		Reason: "LeaseAcquired",
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
}

func (s *Server) handleResetRootfs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	unlock := s.acquireSandboxLock(id)
	defer unlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.mu.RLock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.RUnlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	wasPaused := rec.State == "Paused" || rec.State == string(v1alpha1.PodBoxPhasePaused)
	recCopy := cloneSandboxRecord(rec)
	recCopy.RootfsGeneration++
	liveRouting := s.livePodRouting
	s.mu.RUnlock()

	spec := s.withOfficialMounts(v1alpha1.PodBoxSpec{
		TemplateRef:      recCopy.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: recCopy.RootfsGeneration,
		VolumeKey:        recCopy.VolumeKey,
		NetworkPolicy:    cloneNetworkPolicy(recCopy.NetworkPolicy),
	}, id)
	status, err := s.reconciler.Reconcile(r.Context(), id, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status.Phase == v1alpha1.PodBoxPhaseTerminated {
		s.mu.Lock()
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "sandbox is terminated")
		return
	}
	if liveRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
		recCopy.PoddURL = formatPoddURL(status.PodIP, recCopy.EnvdPort)
		_ = waitForPoddReady(r.Context(), recCopy.PoddURL, 30*time.Second)
	}
	if err := s.startRuntimeSandbox(r.Context(), recCopy, "", nil); err != nil {
		s.mu.Lock()
		rec.RootfsGeneration = recCopy.RootfsGeneration
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		if wasPaused && (status.Phase == v1alpha1.PodBoxPhaseRunning || strings.EqualFold(string(status.Phase), "running")) {
			rec.lastEmittedLifecycleState = events.EventResumed
		}
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	rec.RootfsGeneration = recCopy.RootfsGeneration
	rec.State = string(status.Phase)
	rec.Reason = status.Reason
	if wasPaused {
		rec.lastEmittedLifecycleState = events.EventResumed
	}
	if recCopy.PoddURL != "" {
		rec.PoddURL = recCopy.PoddURL
	}
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventUpdated, wireRec, &events.EventData{
		Reason: "RootfsReset",
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wireRec)
}

func (s *Server) handleForkSandbox(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("sandboxID")
	unlockParent := s.acquireSandboxLock(parentID)
	defer unlockParent()

	parent, ok := s.resolveSandbox(r.Context(), parentID)
	if !ok {
		http.Error(w, "parent sandbox not found", http.StatusNotFound)
		return
	}

	s.mu.RLock()
	parentCopy := cloneSandboxRecord(parent)
	liveRouting := s.livePodRouting
	s.mu.RUnlock()

	childID := fmt.Sprintf("cpod-fork-%d", time.Now().UnixNano())
	spec := s.withOfficialMounts(v1alpha1.PodBoxSpec{
		TemplateRef:      parentCopy.TemplateID,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: parentID},
		NetworkPolicy:    cloneNetworkPolicy(parentCopy.NetworkPolicy),
	}, parentID)
	if len(spec.E2BVolumeMounts) == 0 && s.reconciler != nil && s.reconciler.KubeClient() != nil {
		if parentBox, getErr := s.reconciler.KubeClient().GetPodBox(r.Context(), s.reconciler.Namespace(), parentID); getErr == nil && parentBox != nil && len(parentBox.Spec.E2BVolumeMounts) > 0 {
			spec.E2BVolumeMounts = append([]v1alpha1.E2BVolumeMount(nil), parentBox.Spec.E2BVolumeMounts...)
		}
	}
	if len(spec.E2BVolumeMounts) > 0 && liveRouting && s.runtime == nil {
		sharedPVC := s.sharedE2BPVC()
		for _, m := range spec.E2BVolumeMounts {
			if sharedPVC == "" || m.PVCName != sharedPVC {
				s.E2BVolumeMountErrorsTotal.Add(1)
				writeError(w, http.StatusConflict, "cannot fork sandbox with RWO per-volume E2B PVC on Kubernetes")
				return
			}
		}
	}
	s.copySandboxVolumeMounts(parentID, childID)
	if parentCopy.OwnerTenantHash != "" && s.reconciler != nil {
		s.reconciler.SetBoxAnnotations(childID, map[string]string{"cubepod.io/owner-tenant": parentCopy.OwnerTenantHash})
	}
	status, err := s.reconciler.Reconcile(r.Context(), childID, spec)
	if err != nil {
		s.releaseSandboxVolumeMounts(childID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.copyResolvedImageAnnotation(r.Context(), parentID, childID)

	workspaceDir := filepath.Join(s.workspaceBaseDir, childID, "workspace")
	_ = os.MkdirAll(workspaceDir, 0755)

	cpuCount := parentCopy.CPUCount
	if cpuCount <= 0 {
		cpuCount = 2
	}
	memoryMB := parentCopy.MemoryMB
	if memoryMB <= 0 {
		memoryMB = 2048
	}
	diskSizeMB := parentCopy.DiskSizeMB
	if diskSizeMB <= 0 {
		diskSizeMB = 10240
	}

	now := time.Now().UTC()
	endAt := now.Add(time.Duration(parentCopy.TimeoutSeconds) * time.Second)
	child := &SandboxRecord{
		SandboxID:                 childID,
		TemplateID:                parentCopy.TemplateID,
		ClientID:                  "cpod-client-01",
		EnvdVersion:               "1.1.0",
		StartedAt:                 now,
		EndAt:                     endAt,
		CPUCount:                  cpuCount,
		MemoryMB:                  memoryMB,
		DiskSizeMB:                diskSizeMB,
		State:                     string(status.Phase),
		Reason:                    status.Reason,
		EnvdPort:                  49983,
		RootfsGeneration:          status.ObservedRootfsGen,
		NetworkPolicy:             cloneNetworkPolicy(parentCopy.NetworkPolicy),
		CreatedAt:                 now,
		LastActivityAt:            &now,
		TimeoutSeconds:            parentCopy.TimeoutSeconds,
		WorkspaceDir:              workspaceDir,
		OwnerTenantHash:           parentCopy.OwnerTenantHash,
		lastEmittedLifecycleState: events.EventCreated,
	}

	if s.runtime != nil {
		info, err := s.runtime.Fork(r.Context(), parentID, childID)
		if err != nil {
			if err2 := s.startRuntimeSandbox(r.Context(), child, "", nil); err2 != nil {
				s.rollbackCreatedSandbox(r.Context(), childID)
				http.Error(w, err2.Error(), http.StatusInternalServerError)
				return
			}
		} else if info != nil && info.PoddURL != "" {
			child.PoddURL = info.PoddURL
		}
	}

	s.mu.Lock()
	s.sandboxes[childID] = child
	wireChild := cloneSandboxRecord(child)
	s.mu.Unlock()

	s.emitSandboxEvent(wireChild.OwnerTenantHash, events.EventForked, wireChild, nil)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(wireChild)
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	deleteVolumes := r.URL.Query().Get("deleteVolumes") == "true"

	unlock := s.acquireSandboxLock(id)
	defer unlock()

	s.mu.RLock()
	prevRec := cloneSandboxRecord(s.sandboxes[id])
	s.mu.RUnlock()

	rec, ok := s.resolveSandbox(r.Context(), id)
	var alreadyEmittedKilled bool
	s.mu.Lock()
	if cached, hasCached := s.sandboxes[id]; hasCached {
		rec = cached
		ok = true
	}
	if rec != nil {
		alreadyEmittedKilled = rec.lastEmittedLifecycleState == events.EventKilled
		rec.explicitlyDeleted = true
		rec.lastEmittedLifecycleState = events.EventKilled
	}
	s.mu.Unlock()

	s.deleteRuntimeSandbox(r.Context(), id)
	_ = s.reconciler.DeleteBox(r.Context(), id, deleteVolumes)

	s.mu.Lock()
	delete(s.sandboxes, id)
	delete(s.endpoints, id)
	wireRec := cloneSandboxRecord(rec)
	s.mu.Unlock()

	s.releaseSandboxVolumeMounts(id)
	s.releaseLocalSandbox(id)
	s.Metrics().sandboxMetricsStore().delete(id)

	// Revoke secrets associated with this sandbox
	s.vault.RevokeSandbox(id)

	if ok && wireRec != nil && !alreadyEmittedKilled {
		s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventKilled, wireRec, &events.EventData{
			Execution: executionDataForRecord(wireRec),
		})
	}

	cleanupRec := wireRec
	if cleanupRec == nil {
		cleanupRec = prevRec
	}
	if cleanupRec != nil && cleanupRec.WorkspaceDir != "" && (cleanupRec.VolumeKey == "" || deleteVolumes) {
		_ = os.RemoveAll(filepath.Dir(cleanupRec.WorkspaceDir))
	}
	w.WriteHeader(http.StatusNoContent)
}

// releaseLocalSandbox closes local PTY masters and stops directory watches for a sandbox.
func (s *Server) releaseLocalSandbox(sandboxID string) {
	s.mu.Lock()
	seen := map[*commandEntry]struct{}{}
	var entries []*commandEntry
	for id, e := range s.commands {
		if e.sandboxID == sandboxID {
			if _, ok := seen[e]; !ok {
				seen[e] = struct{}{}
				entries = append(entries, e)
			}
			delete(s.commands, id)
		}
	}
	var watchers []*dirWatcher
	for id, w := range s.watchers {
		if w.sandboxID == sandboxID {
			watchers = append(watchers, w)
			delete(s.watchers, id)
		}
	}
	s.mu.Unlock()

	for _, e := range entries {
		e.mu.Lock()
		if e.cancel != nil {
			e.cancel()
		}
		if e.masterFile != nil {
			_ = e.masterFile.Close()
			e.masterFile = nil
		}
		e.running = false
		pid := e.pid
		e.mu.Unlock()
		if pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for _, w := range watchers {
		w.stop()
	}
}

// ensureRunning resumes a Paused sandbox to Running, incrementing AutoResumeCount.
func (s *Server) ensureRunning(ctx context.Context, rec *SandboxRecord) error {
	if rec == nil {
		return nil
	}
	unlock := s.acquireSandboxLock(rec.SandboxID)
	defer unlock()

	s.mu.Lock()
	if rec.State == string(v1alpha1.PodBoxPhaseTerminated) {
		s.mu.Unlock()
		return fmt.Errorf("sandbox %s is terminated", rec.SandboxID)
	}
	s.recordActivity(rec)
	recCopy := cloneSandboxRecord(rec)
	liveRouting := s.livePodRouting
	s.mu.Unlock()

	resumed := false
	var wireRec *SandboxRecord
	if recCopy.State == string(v1alpha1.PodBoxPhasePaused) || recCopy.State == "Paused" {
		spec := s.withOfficialMounts(v1alpha1.PodBoxSpec{
			TemplateRef:      recCopy.TemplateID,
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: recCopy.RootfsGeneration,
			VolumeKey:        recCopy.VolumeKey,
			NetworkPolicy:    cloneNetworkPolicy(recCopy.NetworkPolicy),
		}, recCopy.SandboxID)
		status, err := s.reconciler.Reconcile(ctx, recCopy.SandboxID, spec)
		if err != nil {
			return err
		}
		if status.Phase == v1alpha1.PodBoxPhaseTerminated {
			s.mu.Lock()
			rec.State = string(status.Phase)
			rec.Reason = status.Reason
			s.mu.Unlock()
			return fmt.Errorf("sandbox %s is terminated", recCopy.SandboxID)
		}
		if liveRouting && status.PodIP != "" && status.PodIP != "10.244.0.42" {
			recCopy.PoddURL = formatPoddURL(status.PodIP, recCopy.EnvdPort)
			_ = waitForPoddReady(ctx, recCopy.PoddURL, 45*time.Second)
		}
		if err := s.startRuntimeSandbox(ctx, recCopy, "", nil); err != nil {
			s.mu.Lock()
			rec.State = string(status.Phase)
			rec.Reason = status.Reason
			if status.Phase == v1alpha1.PodBoxPhaseRunning || strings.EqualFold(string(status.Phase), "running") {
				rec.lastEmittedLifecycleState = events.EventResumed
			}
			s.mu.Unlock()
			return err
		}
		s.mu.Lock()
		rec.State = string(status.Phase)
		rec.Reason = status.Reason
		rec.AutoResumeCount++
		rec.lastEmittedLifecycleState = events.EventResumed
		if recCopy.PoddURL != "" {
			rec.PoddURL = recCopy.PoddURL
		}
		s.recordActivity(rec)
		wireRec = cloneSandboxRecord(rec)
		s.mu.Unlock()
		resumed = true
	} else if liveRouting && recCopy.PoddURL == "" {
		if boxStatus, ok := s.reconciler.GetBox(recCopy.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			nextPoddURL := formatPoddURL(boxStatus.PodIP, recCopy.EnvdPort)
			_ = waitForPoddReady(ctx, nextPoddURL, 45*time.Second)
			s.mu.Lock()
			rec.PoddURL = nextPoddURL
			s.mu.Unlock()
		}
	}

	if resumed && wireRec != nil {
		s.emitSandboxEvent(wireRec.OwnerTenantHash, events.EventResumed, wireRec, &events.EventData{
			ResumeMode: "reboot",
		})
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
	if s.reconciler == nil {
		return
	}
	_ = s.reconciler.MarkFailed(ctx, boxName, reason, "")
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

func mergeSandboxEnv(req createSandboxReq) map[string]string {
	out := map[string]string{}
	for k, v := range req.Env_Vars {
		out[k] = v
	}
	for k, v := range req.EnvVars {
		out[k] = v
	}
	return out
}
