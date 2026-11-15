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

// e2bPVCPrefix is the only PVC name prefix this feature creates or deletes.
// It is not the sandbox workspace PVC (that one still ends in -workspace-pvc).
//
// Live Kata does not mount this claim into the sandbox pod. MemoryKubeClient
// tests record a volumeMode=Filesystem PVC. Compose/runc mounts the host data
// directory (or the cpod-test-vol-<id> named volume) instead.
const e2bPVCPrefix = "e2bvol-"

// E2BVolumeConfig points the gateway at a durable volume directory.
type E2BVolumeConfig struct {
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
}

// E2BVolumeMount is the runtime attachment for one sandbox mount.
// HostPath is what the Docker daemon should bind. DockerVolume, when set, is
// a named volume (cpod-test-vol-<id>) backed by that same directory.
type E2BVolumeMount struct {
	VolumeName    string `json:"volumeName"`
	VolumeID      string `json:"volumeID"`
	ContainerPath string `json:"containerPath"`
	HostPath      string `json:"hostPath"`
	DockerVolume  string `json:"dockerVolume,omitempty"`
}

// ConfigureE2BVolumes opens (or reopens) the durable volume store.
func (s *Server) ConfigureE2BVolumes(cfg E2BVolumeConfig) error {
	if cfg.Dir == "" {
		return errors.New("e2b volume dir is required")
	}
	vcfg := volume.Config{
		HostDir:       cfg.HostDir,
		DockerVolumes: cfg.DockerVolumes,
		DockerPrefix:  cfg.DockerPrefix,
		DockerHost:    cfg.DockerHost,
	}
	st, err := volume.Open(cfg.Dir, vcfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.e2bDir = cfg.Dir
	s.e2bCfg = vcfg
	s.mu.Unlock()
	s.e2b.Store(st)
	return nil
}

// E2BVolumeMounts returns the volumes attached to a sandbox. The compose
// harness uses this to set Docker Mounts/Binds. An unknown sandbox returns nil.
func (s *Server) E2BVolumeMounts(sandboxID string) []E2BVolumeMount {
	st := s.e2b.Load()
	if st == nil || sandboxID == "" {
		return nil
	}
	specs := st.MountsForSandbox(sandboxID)
	if len(specs) == 0 {
		return nil
	}
	out := make([]E2BVolumeMount, 0, len(specs))
	for _, m := range specs {
		out = append(out, E2BVolumeMount{
			VolumeName:    m.VolumeName,
			VolumeID:      m.VolumeID,
			ContainerPath: m.ContainerPath,
			HostPath:      m.HostPath,
			DockerVolume:  m.DockerVolume,
		})
	}
	return out
}

// E2BVolumeDataDir returns the local data directory for a volume, for tests
// and operators. It is empty when the id is unknown.
func (s *Server) E2BVolumeDataDir(volumeID string) string {
	st := s.e2b.Load()
	if st == nil {
		return ""
	}
	dir, ok := st.DataDir(volumeID)
	if !ok {
		return ""
	}
	return dir
}

func (s *Server) e2bStore() (*volume.Store, error) {
	if st := s.e2b.Load(); st != nil {
		return st, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.e2b.Load(); st != nil {
		return st, nil
	}
	dir := s.e2bDir
	if dir == "" {
		return nil, errors.New("e2b volume store is not configured")
	}
	st, err := volume.Open(dir, s.e2bCfg)
	if err != nil {
		return nil, err
	}
	s.e2b.Store(st)
	return st, nil
}

func (s *Server) requireE2BStore(w http.ResponseWriter) (*volume.Store, bool) {
	st, err := s.e2bStore()
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
	st, ok := s.requireE2BStore(w)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	vol, err := st.Create(key, req.Name)
	if err != nil {
		writeVolumeMgmtError(w, err, req.Name)
		return
	}
	s.ensureE2BVolumePVC(r.Context(), st, vol.VolumeID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"volumeID": vol.VolumeID,
		"name":     vol.Name,
		"token":    vol.Token,
	})
}

func (s *Server) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireE2BStore(w)
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
	st, ok := s.requireE2BStore(w)
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
	st, ok := s.requireE2BStore(w)
	if !ok {
		return
	}
	id := r.PathValue("volumeID")
	vol, err := st.Delete(key, id)
	if err != nil {
		writeVolumeMgmtError(w, err, "")
		return
	}
	s.deleteE2BVolumePVC(r.Context(), vol.PVCNamespace, vol.PVCName, vol.VolumeID)
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
	st, ok := s.requireE2BStore(w)
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
	ent, err := st.WriteFile(vol.VolumeID, p, r.Body, opts)
	if err != nil {
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
	switch {
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

func (s *Server) ensureE2BVolumePVC(ctx context.Context, st *volume.Store, volumeID string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil || volumeID == "" {
		return
	}
	name := e2bPVCPrefix + volumeID
	if !strings.HasPrefix(name, e2bPVCPrefix) {
		return
	}
	ns := s.reconciler.Namespace()
	pvc := controller.BlockPVCManifest{
		Name:             name,
		Namespace:        ns,
		StorageClassName: s.reconciler.StorageClassName(),
		VolumeMode:       "Filesystem",
		Size:             "1Gi",
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"cubepod.io/e2b-volume-id":     volumeID,
			"app.kubernetes.io/managed-by": "cubepod-e2b-volume",
		},
	}
	// Not the sandbox workspace PVC. The Kata pod is not updated to mount this
	// claim; see the package comment on e2bPVCPrefix.
	if err := s.reconciler.KubeClient().EnsurePVC(ctx, pvc); err != nil {
		slog.Warn("e2b volume filesystem PVC was not recorded", "volumeID", volumeID, "err", err)
		return
	}
	if err := st.NotePVC(volumeID, name, ns); err != nil {
		slog.Warn("e2b volume PVC note failed", "volumeID", volumeID, "err", err)
	}
}

func (s *Server) deleteE2BVolumePVC(ctx context.Context, namespace, pvcName, volumeID string) {
	if s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return
	}
	if pvcName == "" || volumeID == "" {
		return
	}
	if pvcName != e2bPVCPrefix+volumeID || !strings.HasPrefix(pvcName, e2bPVCPrefix) {
		return
	}
	if err := s.reconciler.KubeClient().DeletePVC(ctx, namespace, pvcName); err != nil {
		slog.Warn("e2b volume PVC delete failed", "pvc", pvcName, "err", err)
	}
}

// planSandboxVolumeMounts resolves official volumeMounts for the caller's API key.
// An empty list is not an error and does not require a key (existing sandbox creates).
func (s *Server) planSandboxVolumeMounts(r *http.Request, mounts []sandboxVolumeMountReq) ([]volume.PlannedMount, int, string) {
	if len(mounts) == 0 {
		return nil, 0, ""
	}
	key := managementAPIKey(r)
	if key == "" {
		return nil, http.StatusUnauthorized, "API key is required"
	}
	st, err := s.e2bStore()
	if err != nil {
		return nil, http.StatusInternalServerError, "volume store unavailable"
	}
	reqs := make([]volume.MountRequest, len(mounts))
	for i, m := range mounts {
		reqs[i] = volume.MountRequest{Name: m.Name, Path: m.Path}
	}
	planned, err := st.LookupMounts(key, reqs)
	if err != nil {
		switch {
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
	st := s.e2b.Load()
	if st == nil {
		return errors.New("volume store unavailable")
	}
	return st.CommitMounts(sandboxID, apiKey, planned)
}

func (s *Server) releaseSandboxVolumeMounts(sandboxID string) {
	st := s.e2b.Load()
	if st == nil {
		return
	}
	st.UnmountSandbox(sandboxID)
}

func (s *Server) copySandboxVolumeMounts(fromID, toID string) {
	st := s.e2b.Load()
	if st == nil {
		return
	}
	if err := st.CopyMounts(fromID, toID); err != nil {
		slog.Warn("copy e2b volume mounts failed", "from", fromID, "to", toID, "err", err)
	}
}

func (s *Server) mapVolumeMountPath(sandboxID, userPath string) (string, bool, error) {
	st := s.e2b.Load()
	if st == nil {
		return "", false, nil
	}
	return st.MapSandboxPath(sandboxID, userPath)
}
