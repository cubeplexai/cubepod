package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

// volumePVCPrefix is the PVC name prefix reserved for CubePod volume claims.
// Kubernetes volumeMounts are gated behind FeatureGateK8sVolumeMounts and require
// a shared RWX PVC (SharedPVCName) for multi-pod / fork safety.
const (
	volumePVCPrefix           = "vol-"
	defaultMaxVolumeFileBytes = 64 << 20 // 64 MiB
)

// VolumeConfig points the gateway at a durable volume directory.
type VolumeConfig struct {
	// Dir is the gateway-local store root. Required.
	Dir string
	// HostDir is the Docker daemon's view of Dir. Empty means HostPath == local path.
	HostDir string
	// DockerVolumes provisions cpod-test-vol-<id> bind volumes via the Docker API.
	DockerVolumes bool
	// DockerPrefix defaults to cpod-test-vol- and must start with cpod-test-.
	DockerPrefix string
	// DockerHost is DOCKER_HOST. Empty reads the environment inside the store.
	DockerHost string
	// MaxFileSizeBytes caps PUT /volumecontent/{id}/file uploads (default 64 MiB).
	MaxFileSizeBytes int64
	// MaxVolumesPerTenant caps volumes per API key tenant (default 64).
	MaxVolumesPerTenant int
	// MaxBytesPerTenant caps total stored file bytes across all volumes of a tenant (<=0 means unlimited).
	MaxBytesPerTenant int64
	// KMS optionally envelope-encrypts volume content tokens in meta.json.
	KMS volume.KMSProvider
	// K8sVolumeMounts enables shared RWX volumeMounts on live Kubernetes.
	K8sVolumeMounts bool
	// SharedPVCName is the cluster-wide RWX PVC shared between gateway and Kata pods.
	SharedPVCName string
	// AllowLazyLegacyClaim controls whether unmigrated legacy extension.volumeKey PVCs
	// may be lazily adopted on first attach (defaults to true when nil).
	AllowLazyLegacyClaim *bool
	// AttachLeaseGracePeriod controls how long a NotFound PVC attach lease is protected
	// before being treated as orphaned (defaults to 30s).
	AttachLeaseGracePeriod time.Duration
}

// VolumeMount is the runtime attachment for one sandbox mount.
// HostPath is what the Docker daemon should bind. DockerVolume, when set, is
// a named volume (cpod-test-vol-<id>) backed by that same directory.
type VolumeMount struct {
	VolumeName    string `json:"volumeName"`
	VolumeID      string `json:"volumeID"`
	Type          string `json:"type,omitempty"`
	ContainerPath string `json:"containerPath"`
	HostPath      string `json:"hostPath"`
	DockerVolume  string `json:"dockerVolume,omitempty"`
}

// ConfigureVolumes opens (or reopens) the durable volume store.
func (s *Server) ConfigureVolumes(cfg VolumeConfig) error {
	if cfg.Dir == "" {
		return errors.New("volume dir is required")
	}
	vcfg := volume.Config{
		HostDir:             cfg.HostDir,
		DockerVolumes:       cfg.DockerVolumes,
		DockerPrefix:        cfg.DockerPrefix,
		DockerHost:          cfg.DockerHost,
		MaxVolumesPerTenant: cfg.MaxVolumesPerTenant,
		MaxBytesPerTenant:   cfg.MaxBytesPerTenant,
		KMS:                 cfg.KMS,
	}
	st, err := volume.Open(cfg.Dir, vcfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.volumesDir = cfg.Dir
	s.volumeCfg = vcfg
	s.volumeMaxFileBytes = cfg.MaxFileSizeBytes
	s.k8sVolumeMounts = cfg.K8sVolumeMounts
	if cfg.K8sVolumeMounts {
		if s.featureGates == nil {
			s.featureGates = DefaultFeatureGates()
		}
		s.featureGates[FeatureGateK8sVolumeMounts] = true
	}
	s.volumesSharedPVC = strings.TrimSpace(cfg.SharedPVCName)
	if cfg.AllowLazyLegacyClaim != nil {
		v := *cfg.AllowLazyLegacyClaim
		s.allowLazyLegacyVolumeClaim = &v
	}
	if cfg.AttachLeaseGracePeriod > 0 {
		s.attachLeaseGracePeriod = cfg.AttachLeaseGracePeriod
	}
	active := make(map[string]struct{}, len(s.sandboxes))
	for id, rec := range s.sandboxes {
		if rec != nil && rec.State != string(v1alpha1.PodBoxPhaseTerminated) && rec.State != string(v1alpha1.PodBoxPhaseFailed) {
			active[id] = struct{}{}
		}
	}
	reconciler := s.reconciler
	s.mu.Unlock()

	shouldReconcile := true
	if reconciler != nil && reconciler.KubeClient() != nil {
		boxes, listErr := reconciler.KubeClient().ListPodBoxes(context.Background(), reconciler.Namespace())
		if listErr != nil {
			// Avoid wiping persisted mounts.json if the API server is temporarily unreachable on boot.
			slog.Warn("skipping volume mount reconciliation: failed to list PodBoxes", "err", listErr)
			shouldReconcile = false
		} else {
			for _, box := range boxes {
				if box.Metadata.Name != "" &&
					box.Status.Phase != v1alpha1.PodBoxPhaseTerminated &&
					box.Status.Phase != v1alpha1.PodBoxPhaseFailed {
					active[box.Metadata.Name] = struct{}{}
				}
			}
		}
	}
	if shouldReconcile {
		st.ReconcileMounts(active)
	}
	s.volumeStore.Store(st)
	return nil
}

func (s *Server) maxVolumeFileBytes() int64 {
	s.mu.RLock()
	n := s.volumeMaxFileBytes
	s.mu.RUnlock()
	if n <= 0 {
		return defaultMaxVolumeFileBytes
	}
	return n
}

func (s *Server) sharedVolumePVC() string {
	s.mu.RLock()
	pvc := s.volumesSharedPVC
	s.mu.RUnlock()
	return pvc
}

func (s *Server) isKubeVolumeMountsUnsupported() bool {
	s.mu.RLock()
	k8sMounts := s.k8sVolumeMounts
	if s.featureGates != nil {
		k8sMounts = s.featureGates[FeatureGateK8sVolumeMounts]
	}
	sharedPVC := s.volumesSharedPVC
	live := s.livePodRouting
	s.mu.RUnlock()
	return live && s.runtime == nil && (!k8sMounts || sharedPVC == "")
}

// VolumeMounts returns the volumes attached to a sandbox. The compose
// harness uses this to set Docker Mounts/Binds. An unknown sandbox returns nil.
func (s *Server) VolumeMounts(sandboxID string) []VolumeMount {
	st := s.volumeStore.Load()
	if st == nil || sandboxID == "" {
		return nil
	}
	specs := st.MountsForSandbox(sandboxID)
	if len(specs) == 0 {
		return nil
	}
	out := make([]VolumeMount, 0, len(specs))
	for _, m := range specs {
		out = append(out, VolumeMount{
			VolumeName:    m.VolumeName,
			VolumeID:      m.VolumeID,
			Type:          m.Type,
			ContainerPath: m.ContainerPath,
			HostPath:      m.HostPath,
			DockerVolume:  m.DockerVolume,
		})
	}
	return out
}

// VolumeDataDir returns the local data directory for a volume, for tests
// and operators. It is empty when the id is unknown.
func (s *Server) VolumeDataDir(volumeID string) string {
	st := s.volumeStore.Load()
	if st == nil {
		return ""
	}
	dir, ok := st.DataDir(volumeID)
	if !ok {
		return ""
	}
	return dir
}

func (s *Server) getVolumeStore() (*volume.Store, error) {
	if st := s.volumeStore.Load(); st != nil {
		return st, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.volumeStore.Load(); st != nil {
		return st, nil
	}
	dir := s.volumesDir
	if dir == "" {
		return nil, errors.New("volume store is not configured")
	}
	st, err := volume.Open(dir, s.volumeCfg)
	if err != nil {
		return nil, err
	}
	s.volumeStore.Store(st)
	return st, nil
}

func (s *Server) requireVolumeStore(w http.ResponseWriter) (*volume.Store, bool) {
	st, err := s.getVolumeStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "volume store unavailable")
		return nil, false
	}
	return st, true
}

func managementAPIKey(r *http.Request) string {
	// Canonical form covers X-API-KEY and X-API-Key.
	return strings.TrimSpace(r.Header.Get("X-API-KEY"))
}

func requireManagementKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := managementAPIKey(r)
	if key == "" {
		writeError(w, http.StatusUnauthorized, "API key is required")
		return "", false
	}
	return key, true
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func writeContentError(w http.ResponseWriter, status int, msg string) {
	// Volume content OpenAPI Error.code is a string. The management Error.code is an int.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    strconv.Itoa(status),
		"message": msg,
	})
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	volType := strings.TrimSpace(req.Type)
	if volType == "" {
		volType = v1alpha1.VolumeTypeShared
	}
	if volType == v1alpha1.VolumeTypeBlock && !s.FeatureGateEnabled(FeatureGateBlockWorkspaceVolumes) {
		writeError(w, http.StatusNotImplemented, "block workspace volumes feature gate is disabled")
		return
	}
	vol, err := st.CreateWithOptions(key, req.Name, volume.CreateOpts{
		Type: volType,
		Size: req.Size,
	})
	if err != nil {
		if errors.Is(err, volume.ErrLimitExceeded) {
			s.VolumeQuotaRejectionsTotal.Add(1)
		}
		writeVolumeMgmtError(w, err, req.Name)
		return
	}
	if vol.IsBlock() {
		if err := s.ensureBlockVolumePVC(r.Context(), st, vol); err != nil {
			_ = st.RollbackCreate(vol.VolumeID)
			writeError(w, http.StatusInternalServerError, "failed to provision block volume PVC: "+err.Error())
			return
		}
	} else {
		s.ensureVolumePVC(r.Context(), st, vol.VolumeID)
	}
	resp := map[string]string{
		"volumeID": vol.VolumeID,
		"name":     vol.Name,
		"type":     vol.EffectiveType(),
		"token":    vol.Token,
	}
	if vol.Size != "" {
		resp["size"] = vol.Size
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRotateVolumeKey(w http.ResponseWriter, r *http.Request) {
	newKey, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		PreviousAPIKey string `json:"previousApiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	prevKey := strings.TrimSpace(req.PreviousAPIKey)
	rotated, err := st.RotateAPIKey(prevKey, newKey)
	if err != nil {
		switch {
		case errors.Is(err, volume.ErrInvalidKey):
			writeError(w, http.StatusBadRequest, "invalid previousApiKey")
		case errors.Is(err, volume.ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "previous API key is unauthorized or already revoked")
		case errors.Is(err, volume.ErrConflict):
			writeError(w, http.StatusConflict, "target API key already belongs to another tenant")
		default:
			writeError(w, http.StatusInternalServerError, "volume key rotation failed")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{
		"rotatedVolumes": rotated,
	})
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return
	}
	vols, err := st.List(key)
	if err != nil {
		writeVolumeMgmtError(w, err, "")
		return
	}
	out := make([]map[string]string, 0, len(vols))
	for _, v := range vols {
		item := map[string]string{
			"volumeID": v.VolumeID,
			"name":     v.Name,
			"type":     v.EffectiveType(),
		}
		if v.Size != "" {
			item["size"] = v.Size
		}
		out = append(out, item)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleGetVolume(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return
	}
	vol, err := st.Get(key, r.PathValue("volumeID"))
	if err != nil {
		writeVolumeMgmtError(w, err, "")
		return
	}
	resp := map[string]string{
		"volumeID": vol.VolumeID,
		"name":     vol.Name,
		"type":     vol.EffectiveType(),
		"token":    vol.Token,
	}
	if vol.Size != "" {
		resp["size"] = vol.Size
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return
	}
	id := r.PathValue("volumeID")
	volMeta, err := st.Get(key, id)
	if err != nil {
		writeVolumeMgmtError(w, err, "")
		return
	}
	if s.reconciler != nil && s.reconciler.KubeClient() != nil {
		boxes, listErr := s.reconciler.KubeClient().ListPodBoxes(r.Context(), s.reconciler.Namespace())
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to verify active PodBox mounts")
			return
		}
		boxPhaseByName := make(map[string]v1alpha1.PodBoxPhase, len(boxes))
		for _, box := range boxes {
			boxPhaseByName[box.Metadata.Name] = box.Status.Phase
			if box.Status.Phase == v1alpha1.PodBoxPhaseTerminated || box.Status.Phase == v1alpha1.PodBoxPhaseFailed {
				continue
			}
			for _, m := range box.Spec.VolumeMounts {
				if m.VolumeID == id {
					if volMeta.IsBlock() {
						s.BlockVolumeAttachConflictsTotal.Add(1)
					}
					writeError(w, http.StatusConflict, "volume is mounted by a sandbox")
					return
				}
			}
			if volMeta.IsBlock() && volMeta.PVCName != "" {
				boxPVC := strings.TrimSpace(box.Status.WorkspacePVCName)
				if boxPVC == "" {
					boxPVC = controller.WorkspacePVCName(box.Metadata.Name, box.Spec)
				}
				if boxPVC == volMeta.PVCName {
					s.BlockVolumeAttachConflictsTotal.Add(1)
					writeError(w, http.StatusConflict, "volume is mounted by a sandbox")
					return
				}
			}
		}
		// Evict stale mounts.json entries whose owner PodBox is already Terminated/Failed.
		for _, owner := range st.ActiveMountOwners(id) {
			if ph, ok := boxPhaseByName[owner]; ok && (ph == v1alpha1.PodBoxPhaseTerminated || ph == v1alpha1.PodBoxPhaseFailed) {
				st.EvictMount(owner, id)
			}
		}
		// For managed block volumes, also verify the K8s PVC attach lease is free or reclaimable.
		if volMeta.IsBlock() && volMeta.PVCName != "" {
			if err := s.ensureBlockVolumeLeaseFreeForDelete(r.Context(), st, volMeta); err != nil {
				if errors.Is(err, volume.ErrInUse) {
					s.BlockVolumeAttachConflictsTotal.Add(1)
					writeError(w, http.StatusConflict, "volume is mounted by a sandbox")
					return
				}
				writeError(w, http.StatusInternalServerError, "failed to verify block volume lease: "+err.Error())
				return
			}
		}
	}
	vol, err := st.Delete(key, id)
	if err != nil {
		if errors.Is(err, volume.ErrInUse) && volMeta.IsBlock() {
			s.BlockVolumeAttachConflictsTotal.Add(1)
		}
		writeVolumeMgmtError(w, err, "")
		return
	}
	s.deleteVolumePVC(r.Context(), vol)
	w.WriteHeader(http.StatusNoContent)
}

func writeVolumeMgmtError(w http.ResponseWriter, err error, name string) {
	switch {
	case errors.Is(err, volume.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid volume name")
	case errors.Is(err, volume.ErrInvalidType):
		writeError(w, http.StatusBadRequest, "invalid volume type (must be 'shared' or 'block')")
	case errors.Is(err, volume.ErrInvalidSize):
		writeError(w, http.StatusBadRequest, "invalid volume size")
	case errors.Is(err, volume.ErrExists):
		writeError(w, http.StatusBadRequest, "Volume with name '"+name+"' already exists")
	case errors.Is(err, volume.ErrNotFound):
		writeError(w, http.StatusNotFound, "volume not found")
	case errors.Is(err, volume.ErrInUse):
		writeError(w, http.StatusConflict, "volume is mounted by a sandbox")
	case errors.Is(err, volume.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "API key is required")
	case errors.Is(err, volume.ErrLimitExceeded):
		writeError(w, http.StatusTooManyRequests, "tenant volume limit exceeded")
	default:
		writeError(w, http.StatusInternalServerError, "volume operation failed")
	}
}

func (s *Server) authVolumeContent(w http.ResponseWriter, r *http.Request) (*volume.Store, volume.Volume, bool) {
	tok := bearerToken(r)
	if tok == "" {
		writeContentError(w, http.StatusUnauthorized, "volume token is required")
		return nil, volume.Volume{}, false
	}
	st, ok := s.requireVolumeStore(w)
	if !ok {
		return nil, volume.Volume{}, false
	}
	vol, err := st.Authenticate(r.PathValue("volumeID"), tok)
	if err != nil {
		writeContentError(w, http.StatusUnauthorized, "invalid volume token")
		return nil, volume.Volume{}, false
	}
	if vol.IsBlock() {
		writeContentError(w, http.StatusBadRequest, volume.ErrUnsupportedType.Error())
		return nil, volume.Volume{}, false
	}
	return st, vol, true
}

func contentPath(r *http.Request) (string, bool) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", false
	}
	return p, true
}

func (s *Server) handleGetVolumeFile(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	f, err := st.OpenFile(vol.VolumeID, p)
	if err != nil {
		writeVolumeContentError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (s *Server) handlePutVolumeFile(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	opts, err := writeOptsFromQuery(r)
	if err != nil {
		writeContentError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxVolumeFileBytes())
	ent, err := st.WriteFile(vol.VolumeID, p, r.Body, opts)
	if err != nil {
		if errors.Is(err, volume.ErrLimitExceeded) {
			s.VolumeQuotaRejectionsTotal.Add(1)
		}
		writeVolumeContentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ent)
}

func (s *Server) handleGetVolumeDir(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	depth := 1
	if raw := r.URL.Query().Get("depth"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeContentError(w, http.StatusBadRequest, "invalid depth")
			return
		}
		depth = n
	}
	entries, err := st.ListDir(vol.VolumeID, p, depth)
	if err != nil {
		writeVolumeContentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (s *Server) handlePostVolumeDir(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	opts, err := writeOptsFromQuery(r)
	if err != nil {
		writeContentError(w, http.StatusBadRequest, err.Error())
		return
	}
	ent, err := st.MakeDir(vol.VolumeID, p, opts)
	if err != nil {
		writeVolumeContentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ent)
}

func (s *Server) handleGetVolumePath(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	ent, err := st.Stat(vol.VolumeID, p)
	if err != nil {
		writeVolumeContentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ent)
}

func (s *Server) handlePatchVolumePath(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	var body struct {
		UID  *int `json:"uid"`
		GID  *int `json:"gid"`
		Mode *int `json:"mode"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeContentError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	ent, err := st.Patch(vol.VolumeID, p, volume.WriteOpts{UID: body.UID, GID: body.GID, Mode: body.Mode})
	if err != nil {
		writeVolumeContentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ent)
}

func (s *Server) handleDeleteVolumePath(w http.ResponseWriter, r *http.Request) {
	st, vol, ok := s.authVolumeContent(w, r)
	if !ok {
		return
	}
	p, ok := contentPath(r)
	if !ok {
		writeContentError(w, http.StatusBadRequest, "path is required")
		return
	}
	// 2.64.0 Volume.remove sends only path. A recursive query is ignored.
	if err := st.Remove(vol.VolumeID, p); err != nil {
		writeVolumeContentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeVolumeContentError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr):
		writeContentError(w, http.StatusRequestEntityTooLarge, "file exceeds maximum allowed size")
	case errors.Is(err, volume.ErrLimitExceeded):
		writeContentError(w, http.StatusTooManyRequests, "tenant volume storage quota exceeded")
	case errors.Is(err, volume.ErrNotExist):
		writeContentError(w, http.StatusNotFound, "path not found")
	case errors.Is(err, volume.ErrUnsupportedType):
		writeContentError(w, http.StatusBadRequest, volume.ErrUnsupportedType.Error())
	case errors.Is(err, volume.ErrInvalidPath):
		writeContentError(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, volume.ErrNotDir):
		writeContentError(w, http.StatusBadRequest, "not a directory")
	case errors.Is(err, volume.ErrIsDir):
		writeContentError(w, http.StatusBadRequest, "is a directory")
	case errors.Is(err, volume.ErrConflict):
		writeContentError(w, http.StatusConflict, "path already exists with a different type")
	case errors.Is(err, volume.ErrUnauthorized), errors.Is(err, volume.ErrNotFound):
		writeContentError(w, http.StatusUnauthorized, "invalid volume token")
	default:
		writeContentError(w, http.StatusInternalServerError, "volume content operation failed")
	}
}

func writeOptsFromQuery(r *http.Request) (volume.WriteOpts, error) {
	var opts volume.WriteOpts
	var err error
	if opts.UID, err = queryOptInt(r, "uid"); err != nil {
		return opts, err
	}
	if opts.GID, err = queryOptInt(r, "gid"); err != nil {
		return opts, err
	}
	if opts.Mode, err = queryOptInt(r, "mode"); err != nil {
		return opts, err
	}
	opts.Force = queryBool(r, "force")
	return opts, nil
}

func queryOptInt(r *http.Request, key string) (*int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, errors.New("invalid " + key)
	}
	return &n, nil
}

func queryBool(r *http.Request, key string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return v == "true" || v == "1"
}

func (s *Server) ensureVolumePVC(ctx context.Context, st *volume.Store, volumeID string) {
	if s.isKubeVolumeMountsUnsupported() {
		return
	}
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || volumeID == "" {
		return
	}
	ns := s.reconciler.Namespace()
	if sharedPVC := s.sharedVolumePVC(); sharedPVC != "" {
		if err := st.NotePVC(volumeID, sharedPVC, ns); err != nil {
			slog.Warn("volume shared PVC note failed", "volumeID", volumeID, "pvc", sharedPVC, "err", err)
		}
		return
	}
	name := volumePVCPrefix + volumeID
	pvc := controller.BlockPVCManifest{
		Name:             name,
		Namespace:        ns,
		StorageClassName: s.reconciler.StorageClassName(),
		VolumeMode:       "Filesystem",
		Size:             "1Gi",
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"cubepod.io/volume-id":         volumeID,
			"app.kubernetes.io/managed-by": "cubepod-volumes",
		},
	}
	// Not the sandbox workspace PVC. The Kata pod is not updated to mount this
	// claim; see the package comment on volumePVCPrefix.
	if err := s.reconciler.KubeClient().EnsurePVC(ctx, pvc); err != nil {
		slog.Warn("volume filesystem PVC was not recorded", "volumeID", volumeID, "err", err)
		return
	}
	if err := st.NotePVC(volumeID, name, ns); err != nil {
		slog.Warn("volume PVC note failed", "volumeID", volumeID, "err", err)
	}
}

func (s *Server) ensureBlockVolumePVC(ctx context.Context, st *volume.Store, vol volume.Volume) error {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || vol.VolumeID == "" {
		return nil
	}
	ns := s.reconciler.Namespace()
	name := volumePVCPrefix + vol.VolumeID
	volMode := "Block"
	if strings.EqualFold(s.reconciler.VolumeMode(), "Filesystem") {
		volMode = "Filesystem"
	}
	size := vol.Size
	if size == "" {
		size = volume.DefaultBlockVolumeSize
	}
	pvc := controller.BlockPVCManifest{
		Name:             name,
		Namespace:        ns,
		StorageClassName: s.reconciler.StorageClassName(),
		VolumeMode:       volMode,
		Size:             size,
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"app.kubernetes.io/name":     "cubepod",
			"pod.cubeplex.ai/vol-type":   "workspace",
			controller.PVCLabelVolumeID:  vol.VolumeID,
			controller.PVCLabelManagedBy: controller.PVCLabelManagedByVolumes,
		},
	}
	if err := s.reconciler.KubeClient().EnsurePVC(ctx, pvc); err != nil {
		return err
	}
	return st.NotePVC(vol.VolumeID, name, ns)
}

func (s *Server) deleteVolumePVC(ctx context.Context, vol volume.Volume) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	pvcName := strings.TrimSpace(vol.PVCName)
	namespace := strings.TrimSpace(vol.PVCNamespace)
	if namespace == "" {
		namespace = s.reconciler.Namespace()
	}
	if pvcName == "" || vol.VolumeID == "" {
		return
	}
	if shared := s.sharedVolumePVC(); shared != "" && pvcName == shared {
		return
	}
	if vol.IsBlock() {
		// Allow deleting both canonical vol-<id> and Mode 1 in-place adopted legacy PVCs,
		// provided the PVC is verified to carry the matching cubepod-volumes ownership labels.
		pvc, err := s.reconciler.KubeClient().GetPVCManifest(ctx, namespace, pvcName)
		if err != nil {
			if !errors.Is(err, controller.ErrKubeNotFound) {
				slog.Warn("volume PVC ownership check failed", "pvc", pvcName, "err", err)
			}
			return
		}
		if pvc == nil || pvc.Labels[controller.PVCLabelManagedBy] != controller.PVCLabelManagedByVolumes ||
			pvc.Labels[controller.PVCLabelVolumeID] != vol.VolumeID {
			slog.Warn("skipping PVC delete: ownership labels do not match volume", "pvc", pvcName, "volumeID", vol.VolumeID)
			return
		}
	} else {
		if pvcName != volumePVCPrefix+vol.VolumeID || !strings.HasPrefix(pvcName, volumePVCPrefix) {
			return
		}
	}
	if err := s.reconciler.KubeClient().DeletePVC(ctx, namespace, pvcName); err != nil {
		slog.Warn("volume PVC delete failed", "pvc", pvcName, "err", err)
	}
}

// planSandboxVolumeMounts resolves volumeMounts for the caller's API key.
// An empty list is not an error and does not require a key (existing sandbox creates).
func (s *Server) planSandboxVolumeMounts(r *http.Request, mounts []sandboxVolumeMountReq) ([]volume.PlannedMount, int, string) {
	if len(mounts) == 0 {
		return nil, 0, ""
	}
	key := managementAPIKey(r)
	if key == "" {
		s.VolumeMountErrorsTotal.Add(1)
		return nil, http.StatusUnauthorized, "API key is required"
	}
	st, err := s.getVolumeStore()
	if err != nil {
		s.VolumeMountErrorsTotal.Add(1)
		return nil, http.StatusInternalServerError, "volume store unavailable"
	}
	reqs := make([]volume.MountRequest, len(mounts))
	for i, m := range mounts {
		reqs[i] = volume.MountRequest{Name: m.Name, Path: m.Path}
	}
	planned, err := st.LookupMounts(key, reqs)
	if err != nil {
		s.VolumeMountErrorsTotal.Add(1)
		switch {
		case errors.Is(err, volume.ErrUnauthorized):
			return nil, http.StatusUnauthorized, "API key is unauthorized or revoked"
		case errors.Is(err, volume.ErrInvalidName):
			return nil, http.StatusBadRequest, "invalid volume name"
		case errors.Is(err, volume.ErrInvalidPath):
			return nil, http.StatusBadRequest, "invalid volume mount path"
		case errors.Is(err, volume.ErrConflict):
			return nil, http.StatusBadRequest, "duplicate volume mount path"
		case errors.Is(err, volume.ErrNotFound):
			return nil, http.StatusNotFound, "volume not found"
		default:
			return nil, http.StatusInternalServerError, "volume mount lookup failed"
		}
	}
	// Check runtime capability per volume type:
	// - shared RWX mounts require K8sVolumeMounts + SharedPVCName on live K8s
	// - block /workspace mounts work on pure-block clusters without SharedPVCName, gated by BlockWorkspaceVolumes
	for _, p := range planned {
		if p.Type == v1alpha1.VolumeTypeBlock {
			if !s.FeatureGateEnabled(FeatureGateBlockWorkspaceVolumes) {
				s.VolumeMountErrorsTotal.Add(1)
				return nil, http.StatusNotImplemented, "block workspace volumes feature gate is disabled"
			}
		} else {
			if s.isKubeVolumeMountsUnsupported() {
				s.VolumeMountErrorsTotal.Add(1)
				return nil, http.StatusNotImplemented, "volumeMounts are not supported on the Kubernetes runtime"
			}
		}
	}
	return planned, 0, ""
}

func (s *Server) commitSandboxVolumeMounts(sandboxID, apiKey string, planned []volume.PlannedMount) error {
	if len(planned) == 0 {
		return nil
	}
	st := s.volumeStore.Load()
	if st == nil {
		s.VolumeMountErrorsTotal.Add(1)
		return errors.New("volume store unavailable")
	}
	if err := st.CommitMounts(sandboxID, apiKey, planned); err != nil {
		s.VolumeMountErrorsTotal.Add(1)
		return err
	}
	s.VolumeMountsTotal.Add(uint64(len(planned)))
	return nil
}

func (s *Server) releaseSandboxVolumeMounts(sandboxID string) {
	st := s.volumeStore.Load()
	if st == nil {
		return
	}
	st.UnmountSandbox(sandboxID)
}

func (s *Server) copySandboxVolumeMounts(fromID, toID string) {
	st := s.volumeStore.Load()
	if st == nil {
		return
	}
	if err := st.CopyMounts(fromID, toID); err != nil {
		s.VolumeMountErrorsTotal.Add(1)
		slog.Warn("copy volume mounts failed", "from", fromID, "to", toID, "err", err)
	}
}

func (s *Server) mapVolumeMountPath(sandboxID, userPath string) (string, bool, error) {
	st := s.volumeStore.Load()
	if st == nil {
		return "", false, nil
	}
	return st.MapSandboxPath(sandboxID, userPath)
}

// isPVCLeaseReclaimable determines whether an existing cubepod.io/attached-sandbox lease
// on a PVC may be preempted or cleared:
// - free (empty holder), OR
// - holder PodBox is Terminated or Failed, OR
// - holder PodBox is NotFound AND now - attached-at > AttachLeaseGracePeriod.
func (s *Server) isPVCLeaseReclaimable(ctx context.Context, pvc *controller.BlockPVCManifest) (reclaimable bool, holder string, err error) {
	if pvc == nil || pvc.Annotations == nil {
		return true, "", nil
	}
	holder = strings.TrimSpace(pvc.Annotations[controller.PVCAnnotationAttachedSandbox])
	if holder == "" {
		return true, "", nil
	}
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return false, holder, nil
	}
	box, getErr := s.reconciler.KubeClient().GetPodBox(ctx, s.reconciler.Namespace(), holder)
	if getErr != nil {
		if errors.Is(getErr, controller.ErrKubeNotFound) || strings.Contains(strings.ToLower(getErr.Error()), "not found") {
			attachedAtStr := strings.TrimSpace(pvc.Annotations[controller.PVCAnnotationAttachedAt])
			if attachedAtStr == "" {
				return true, holder, nil
			}
			attachedAt, parseErr := time.Parse(time.RFC3339Nano, attachedAtStr)
			if parseErr != nil {
				attachedAt, parseErr = time.Parse(time.RFC3339, attachedAtStr)
			}
			if parseErr != nil {
				return true, holder, nil
			}
			if time.Since(attachedAt) > s.AttachLeaseGracePeriod() {
				return true, holder, nil
			}
			return false, holder, nil
		}
		return false, holder, getErr
	}
	if box == nil || box.Status.Phase == v1alpha1.PodBoxPhaseTerminated || box.Status.Phase == v1alpha1.PodBoxPhaseFailed {
		return true, holder, nil
	}
	return false, holder, nil
}

// acquireBlockVolumePVCLease atomically claims cubepod.io/attached-sandbox on the block volume PVC
// via resourceVersion CAS. Returns volume.ErrInUse on contention.
func (s *Server) acquireBlockVolumePVCLease(ctx context.Context, st *volume.Store, sandboxID string, pm volume.PlannedMount) error {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || pm.Type != v1alpha1.VolumeTypeBlock {
		return nil
	}
	pvcName := pm.PVCName
	if pvcName == "" {
		pvcName = volumePVCPrefix + pm.VolumeID
	}
	ns := s.reconciler.Namespace()
	kc := s.reconciler.KubeClient()
	for attempt := 0; attempt < 3; attempt++ {
		pvc, err := kc.GetPVCManifest(ctx, ns, pvcName)
		if err != nil {
			return err
		}
		reclaimable, oldHolder, err := s.isPVCLeaseReclaimable(ctx, pvc)
		if err != nil {
			return err
		}
		if !reclaimable && oldHolder != sandboxID {
			return volume.ErrInUse
		}
		_, err = kc.UpdatePVCAnnotationsCAS(ctx, ns, pvcName, pvc.ResourceVersion, map[string]string{
			controller.PVCAnnotationAttachedSandbox: sandboxID,
			controller.PVCAnnotationAttachedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err == nil {
			if st != nil {
				if oldHolder != "" && oldHolder != sandboxID {
					st.EvictMount(oldHolder, pm.VolumeID)
				}
				// Also evict any stale mounts.json owner whose PVC lease was cleared
				// asynchronously (e.g. controller ReapIdle on PausedTTLSeconds expiration).
				for _, owner := range st.ActiveMountOwners(pm.VolumeID) {
					if owner == sandboxID {
						continue
					}
					box, getErr := kc.GetPodBox(ctx, ns, owner)
					if getErr != nil {
						if errors.Is(getErr, controller.ErrKubeNotFound) || strings.Contains(strings.ToLower(getErr.Error()), "not found") {
							st.EvictMount(owner, pm.VolumeID)
						}
						continue
					}
					if box == nil || box.Status.Phase == v1alpha1.PodBoxPhaseTerminated || box.Status.Phase == v1alpha1.PodBoxPhaseFailed {
						st.EvictMount(owner, pm.VolumeID)
					}
				}
			}
			return nil
		}
		if errors.Is(err, controller.ErrCASConflict) {
			continue
		}
		return err
	}
	return volume.ErrInUse
}

// ensureBlockVolumeLeaseFreeForDelete checks the PVC attach lease before DELETE /volumes/{id}
// and clears stale (Terminated/Failed/grace-expired NotFound) leases via CAS.
func (s *Server) ensureBlockVolumeLeaseFreeForDelete(ctx context.Context, st *volume.Store, vol volume.Volume) error {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || vol.PVCName == "" {
		return nil
	}
	ns := vol.PVCNamespace
	if ns == "" {
		ns = s.reconciler.Namespace()
	}
	pvc, err := s.reconciler.KubeClient().GetPVCManifest(ctx, ns, vol.PVCName)
	if err != nil {
		if errors.Is(err, controller.ErrKubeNotFound) {
			return nil
		}
		return err
	}
	reclaimable, holder, err := s.isPVCLeaseReclaimable(ctx, pvc)
	if err != nil {
		return err
	}
	if !reclaimable {
		return volume.ErrInUse
	}
	if holder != "" {
		if st != nil {
			st.EvictMount(holder, vol.VolumeID)
		}
		_, casErr := s.reconciler.KubeClient().UpdatePVCAnnotationsCAS(ctx, ns, vol.PVCName, pvc.ResourceVersion, map[string]string{
			controller.PVCAnnotationAttachedSandbox: "",
			controller.PVCAnnotationAttachedAt:      "",
		})
		if casErr != nil && !errors.Is(casErr, controller.ErrKubeNotFound) {
			return volume.ErrInUse
		}
	}
	return nil
}

// releaseSandboxBlockPVCLeases releases PVC attach leases for any block volumes held by sandboxID.
func (s *Server) releaseSandboxBlockPVCLeases(ctx context.Context, sandboxID string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || sandboxID == "" {
		return
	}
	st := s.volumeStore.Load()
	if st == nil {
		return
	}
	specs := st.MountsForSandbox(sandboxID)
	ns := s.reconciler.Namespace()
	kc := s.reconciler.KubeClient()
	for _, m := range specs {
		if m.Type != v1alpha1.VolumeTypeBlock {
			continue
		}
		vol, ok := st.GetByID(m.VolumeID)
		if !ok || vol.PVCName == "" {
			continue
		}
		pvc, err := kc.GetPVCManifest(ctx, ns, vol.PVCName)
		if err != nil || pvc == nil || pvc.Annotations == nil {
			continue
		}
		if strings.TrimSpace(pvc.Annotations[controller.PVCAnnotationAttachedSandbox]) != sandboxID {
			continue
		}
		_, _ = kc.UpdatePVCAnnotationsCAS(ctx, ns, vol.PVCName, pvc.ResourceVersion, map[string]string{
			controller.PVCAnnotationAttachedSandbox: "",
			controller.PVCAnnotationAttachedAt:      "",
		})
	}
}

// autoAdoptLegacyPodBoxVolumes idempotently registers active legacy spec.VolumeKey PodBoxes
// into volume.Store and backfills cubepod-volumes ownership labels onto their PVCs.
func (s *Server) autoAdoptLegacyPodBoxVolumes(ctx context.Context, boxes []v1alpha1.PodBox) {
	st := s.volumeStore.Load()
	if st == nil || s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	if !s.FeatureGateEnabled(FeatureGateBlockWorkspaceVolumes) {
		return
	}
	ns := s.reconciler.Namespace()
	kc := s.reconciler.KubeClient()
	for _, box := range boxes {
		if box.Spec.VolumeKey == "" || box.Spec.WorkspaceBlockMount() != nil {
			continue
		}
		ownerTenant := ""
		if box.Metadata.Annotations != nil {
			ownerTenant = strings.TrimSpace(box.Metadata.Annotations["cubepod.io/owner-tenant"])
		}
		if ownerTenant == "" {
			continue
		}
		pvcName := strings.TrimSpace(box.Status.WorkspacePVCName)
		if pvcName == "" {
			pvcName = controller.WorkspacePVCName(box.Metadata.Name, box.Spec)
		}
		size := volume.DefaultBlockVolumeSize
		if pvc, err := kc.GetPVCManifest(ctx, ns, pvcName); err == nil && pvc != nil && pvc.Size != "" {
			size = pvc.Size
		}
		vol, _, err := st.AdoptBlockVolume(volume.AdoptBlockVolumeOpts{
			OwnerTenantHash: ownerTenant,
			Name:            box.Spec.VolumeKey,
			PVCName:         pvcName,
			PVCNamespace:    ns,
			Size:            size,
			LegacyVolumeKey: box.Spec.VolumeKey,
		})
		if err != nil {
			slog.Warn("auto-adopt legacy volumeKey failed", "podBox", box.Metadata.Name, "pvc", pvcName, "err", err)
			continue
		}
		_ = kc.PatchPVCLabels(ctx, ns, pvcName, map[string]string{
			controller.PVCLabelVolumeID:  vol.VolumeID,
			controller.PVCLabelManagedBy: controller.PVCLabelManagedByVolumes,
		})
		if box.Status.Phase != v1alpha1.PodBoxPhaseTerminated && box.Status.Phase != v1alpha1.PodBoxPhaseFailed && box.Metadata.Name != "" {
			if curPVC, getErr := kc.GetPVCManifest(ctx, ns, pvcName); getErr == nil && curPVC != nil {
				holder := ""
				if curPVC.Annotations != nil {
					holder = strings.TrimSpace(curPVC.Annotations[controller.PVCAnnotationAttachedSandbox])
				}
				if holder == "" {
					_, _ = kc.UpdatePVCAnnotationsCAS(ctx, ns, pvcName, curPVC.ResourceVersion, map[string]string{
						controller.PVCAnnotationAttachedSandbox: box.Metadata.Name,
						controller.PVCAnnotationAttachedAt:      time.Now().UTC().Format(time.RFC3339Nano),
					})
				}
			}
		}
	}
}

// tryLazyAdoptLegacyVolumeKey implements the CEP-0030 lazy compatibility bridge for callers
// passing extension.volumeKey on POST /sandboxes when an existing legacy PVC is present.
// Returns (plannedMount, adopted, httpStatus, errMsg).
func (s *Server) tryLazyAdoptLegacyVolumeKey(ctx context.Context, apiKey, ownerTenant, volumeKey string) (volume.PlannedMount, bool, int, string) {
	if volumeKey == "" || apiKey == "" || !s.FeatureGateEnabled(FeatureGateBlockWorkspaceVolumes) || !s.AllowLazyLegacyVolumeClaim() {
		return volume.PlannedMount{}, false, 0, ""
	}
	st := s.volumeStore.Load()
	if st == nil || s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return volume.PlannedMount{}, false, 0, ""
	}
	ns := s.reconciler.Namespace()
	kc := s.reconciler.KubeClient()
	pvcName := controller.SanitizeVolumeKey(volumeKey) + "-workspace-pvc"

	// Check if already adopted in store.
	if existing, ok := st.FindByPVCName(pvcName); ok {
		if _, err := st.Get(apiKey, existing.VolumeID); err != nil {
			return volume.PlannedMount{}, false, http.StatusForbidden, "legacy workspace volume belongs to another tenant"
		}
		return volume.PlannedMount{
			VolumeID: existing.VolumeID,
			Name:     existing.Name,
			Type:     v1alpha1.VolumeTypeBlock,
			PVCName:  existing.PVCName,
			Path:     "/workspace",
		}, true, 0, ""
	}

	// Check whether the legacy PVC exists in Kubernetes.
	pvc, err := kc.GetPVCManifest(ctx, ns, pvcName)
	if err != nil {
		if errors.Is(err, controller.ErrKubeNotFound) {
			return volume.PlannedMount{}, false, 0, ""
		}
		return volume.PlannedMount{}, false, http.StatusInternalServerError, "failed to inspect legacy workspace PVC"
	}

	// Guard against cross-tenant takeover when an active PodBox owned by another tenant references this PVC.
	boxes, listErr := kc.ListPodBoxes(ctx, ns)
	if listErr == nil {
		for _, box := range boxes {
			if box.Status.Phase == v1alpha1.PodBoxPhaseTerminated || box.Status.Phase == v1alpha1.PodBoxPhaseFailed {
				continue
			}
			boxPVC := box.Status.WorkspacePVCName
			if boxPVC == "" {
				boxPVC = controller.WorkspacePVCName(box.Metadata.Name, box.Spec)
			}
			if boxPVC != pvcName {
				continue
			}
			boxTenant := ""
			if box.Metadata.Annotations != nil {
				boxTenant = strings.TrimSpace(box.Metadata.Annotations["cubepod.io/owner-tenant"])
			}
			if boxTenant != "" && ownerTenant != "" && boxTenant != ownerTenant {
				s.BlockVolumeAttachConflictsTotal.Add(1)
				return volume.PlannedMount{}, false, http.StatusForbidden, "legacy workspace PVC is actively owned by another tenant"
			}
			s.BlockVolumeAttachConflictsTotal.Add(1)
			return volume.PlannedMount{}, false, http.StatusConflict, "legacy workspace PVC is already attached to an active sandbox"
		}
	}

	size := volume.DefaultBlockVolumeSize
	if pvc != nil && pvc.Size != "" {
		size = pvc.Size
	}
	vol, _, err := st.AdoptBlockVolume(volume.AdoptBlockVolumeOpts{
		APIKey:          apiKey,
		Name:            volumeKey,
		PVCName:         pvcName,
		PVCNamespace:    ns,
		Size:            size,
		LegacyVolumeKey: volumeKey,
	})
	if err != nil {
		if errors.Is(err, volume.ErrConflict) {
			s.BlockVolumeAttachConflictsTotal.Add(1)
			return volume.PlannedMount{}, false, http.StatusForbidden, "legacy workspace volume belongs to another tenant"
		}
		return volume.PlannedMount{}, false, http.StatusInternalServerError, "failed to adopt legacy workspace volume"
	}
	_ = kc.PatchPVCLabels(ctx, ns, pvcName, map[string]string{
		controller.PVCLabelVolumeID:  vol.VolumeID,
		controller.PVCLabelManagedBy: controller.PVCLabelManagedByVolumes,
	})
	return volume.PlannedMount{
		VolumeID: vol.VolumeID,
		Name:     vol.Name,
		Type:     v1alpha1.VolumeTypeBlock,
		PVCName:  vol.PVCName,
		Path:     "/workspace",
	}, true, 0, ""
}
