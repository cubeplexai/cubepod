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
	// K8sVolumeMounts enables volumeMounts on live Kubernetes.
	K8sVolumeMounts bool
	// SharedPVCName is the cluster-wide RWX PVC shared between gateway and Kata pods.
	SharedPVCName string
}

// VolumeMount is the runtime attachment for one sandbox mount.
// HostPath is what the Docker daemon should bind. DockerVolume, when set, is
// a named volume (cpod-test-vol-<id>) backed by that same directory.
type VolumeMount struct {
	VolumeName    string `json:"volumeName"`
	VolumeID      string `json:"volumeID"`
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
	active := make(map[string]struct{}, len(s.sandboxes))
	for id := range s.sandboxes {
		active[id] = struct{}{}
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
				if box.Metadata.Name != "" {
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	vol, err := st.Create(key, req.Name)
	if err != nil {
		if errors.Is(err, volume.ErrLimitExceeded) {
			s.VolumeQuotaRejectionsTotal.Add(1)
		}
		writeVolumeMgmtError(w, err, req.Name)
		return
	}
	s.ensureVolumePVC(r.Context(), st, vol.VolumeID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"volumeID": vol.VolumeID,
		"name":     vol.Name,
		"token":    vol.Token,
	})
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
		out = append(out, map[string]string{
			"volumeID": v.VolumeID,
			"name":     v.Name,
		})
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"volumeID": vol.VolumeID,
		"name":     vol.Name,
		"token":    vol.Token,
	})
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
	if s.reconciler != nil && s.reconciler.KubeClient() != nil {
		boxes, listErr := s.reconciler.KubeClient().ListPodBoxes(r.Context(), s.reconciler.Namespace())
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to verify active PodBox mounts")
			return
		}
		for _, box := range boxes {
			for _, m := range box.Spec.VolumeMounts {
				if m.VolumeID == id {
					writeError(w, http.StatusConflict, "volume is mounted by a sandbox")
					return
				}
			}
		}
	}
	vol, err := st.Delete(key, id)
	if err != nil {
		writeVolumeMgmtError(w, err, "")
		return
	}
	s.deleteVolumePVC(r.Context(), vol.PVCNamespace, vol.PVCName, vol.VolumeID)
	w.WriteHeader(http.StatusNoContent)
}

func writeVolumeMgmtError(w http.ResponseWriter, err error, name string) {
	switch {
	case errors.Is(err, volume.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid volume name")
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

func (s *Server) deleteVolumePVC(ctx context.Context, namespace, pvcName, volumeID string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	if pvcName == "" || volumeID == "" {
		return
	}
	if shared := s.sharedVolumePVC(); shared != "" && pvcName == shared {
		return
	}
	if pvcName != volumePVCPrefix+volumeID || !strings.HasPrefix(pvcName, volumePVCPrefix) {
		return
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
	if s.isKubeVolumeMountsUnsupported() {
		s.VolumeMountErrorsTotal.Add(1)
		return nil, http.StatusNotImplemented, "volumeMounts are not supported on the Kubernetes runtime"
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
