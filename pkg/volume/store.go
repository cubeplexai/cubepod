// Package volume implements the official E2B 2.64.0 Volume control-plane
// records and content filesystem.
//
// Data and metadata live on disk under a store directory so they survive a
// gateway process restart. The store root and each {id} directory are mode
// 0700. Only {id}/data is mounted into a sandbox. When Docker volume
// provisioning is enabled, each volume is also exposed as a named volume
// whose local driver bind-mounts that data directory. The name must start
// with cubepod-e2bvol- (product) or cpod-test- (tests) and must match this
// volume id.
//
// Callers attach the filesystem claim e2bvol-<id> on the sandbox pod. This
// package records the claim. It does not talk to a live Kata cluster.
package volume

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"golang.org/x/sys/unix"
)

const (
	defaultDockerPrefix        = "cpod-test-vol-"
	maxVolumeNameLen           = 128
	maxMountsPerSandbox        = 32
	maxListDepth               = 64
	defaultMaxVolumesPerTenant = 64
	// defaultDataUID is the sandbox user (ubuntu) that must be able to write
	// the mounted data directory. The store root stays root-owned and 0700.
	defaultDataUID = 1000
	defaultDataGID = 1000
)

var (
	// ErrInvalidName is an invalid E2B volume name.
	ErrInvalidName = errors.New("invalid volume name")
	// ErrExists is a duplicate name within the same tenant.
	ErrExists = errors.New("volume exists")
	// ErrNotFound is a missing volume, or a volume owned by another tenant.
	ErrNotFound = errors.New("volume not found")
	// ErrInUse means at least one sandbox still mounts the volume.
	ErrInUse = errors.New("volume in use")
	// ErrInvalidPath is a missing, escaping, or otherwise illegal content path.
	ErrInvalidPath = errors.New("invalid path")
	// ErrNotExist is a missing file or directory inside the volume.
	ErrNotExist = errors.New("path not found")
	// ErrNotDir is a list or mkdir target that is not a directory.
	ErrNotDir = errors.New("not a directory")
	// ErrIsDir is a file operation aimed at a directory.
	ErrIsDir = errors.New("is a directory")
	// ErrConflict is a type clash (file where a directory is required, or the reverse).
	ErrConflict = errors.New("conflict")
	// ErrUnauthorized is a bad or missing content token.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrLimitExceeded means the tenant has reached its volume quota.
	ErrLimitExceeded = errors.New("volume limit exceeded")

	volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	volumeIDRe   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// Config controls where volume bytes live and whether a test-prefixed Docker
// volume is provisioned for them.
type Config struct {
	// HostDir is the Docker-daemon view of the store directory. When empty,
	// HostPath in mount specs is the local data directory.
	HostDir string
	// DockerVolumes creates and deletes named volumes via the Docker API.
	DockerVolumes bool
	// DockerPrefix defaults to cpod-test-vol-. Product callers set cubepod-e2bvol-.
	// The prefix must itself be an allowed Docker volume name.
	DockerPrefix string
	// DockerHost is DOCKER_HOST (tcp:// or unix://). Empty uses the environment.
	DockerHost string
	// MaxVolumesPerTenant caps volumes per API key tenant (default 64).
	MaxVolumesPerTenant int
}

// Volume is the durable record returned to the management API.
type Volume struct {
	VolumeID      string    `json:"volumeID"`
	Name          string    `json:"name"`
	Token         string    `json:"token"`
	Tenant        string    `json:"tenant"`
	CreatedAt     time.Time `json:"createdAt"`
	DockerVolume  string    `json:"dockerVolume,omitempty"`
	PVCName       string    `json:"pvcName,omitempty"`
	PVCNamespace  string    `json:"pvcNamespace,omitempty"`
	DataDir       string    `json:"-"`
	DaemonDataDir string    `json:"-"`
}

// Entry is a VolumeEntryStat payload.
type Entry struct {
	Name   string    `json:"name"`
	Type   string    `json:"type"`
	Path   string    `json:"path"`
	Size   int64     `json:"size"`
	Mode   int       `json:"mode"`
	UID    int       `json:"uid"`
	GID    int       `json:"gid"`
	Atime  time.Time `json:"atime"`
	Mtime  time.Time `json:"mtime"`
	Ctime  time.Time `json:"ctime"`
	Target string    `json:"target,omitempty"`
}

// WriteOpts are the optional PUT /file and POST /dir query fields.
type WriteOpts struct {
	UID   *int
	GID   *int
	Mode  *int
	Force bool
}

// MountRequest is one sandbox mount the caller asked for, by volume name.
type MountRequest struct {
	Name string
	Path string
}

// PlannedMount is one sandbox mount already resolved to a volume id.
type PlannedMount struct {
	VolumeID string
	Name     string
	Path     string
}

// MountSpec is what a container runtime needs to attach a volume.
type MountSpec struct {
	VolumeName    string
	VolumeID      string
	ContainerPath string
	// HostPath is the path the Docker daemon should bind. It equals the
	// gateway data directory when HostDir is unset.
	HostPath string
	// LocalPath is the directory this process reads and writes.
	LocalPath string
	// DockerVolume is cpod-test-vol-<id> when provisioning succeeded.
	DockerVolume string
}

type fileAttr struct {
	UID  *int `json:"uid,omitempty"`
	GID  *int `json:"gid,omitempty"`
	Mode *int `json:"mode,omitempty"`
}

type volRec struct {
	mu    sync.Mutex
	meta  Volume
	attrs map[string]fileAttr
}

type mountRec struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Tenant   string `json:"tenant"`
}

// Store is a process-local, mutex-guarded view of an on-disk volume directory.
type Store struct {
	mu     sync.Mutex
	dir    string
	cfg    Config
	vols   map[string]*volRec
	byName map[string]string
	mounts map[string][]mountRec
	docker dockerClient
}

// Open loads an existing store directory or creates it.
func Open(dir string, cfg Config) (*Store, error) {
	if dir == "" {
		return nil, errors.New("volume store dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, err
	}
	if cfg.DockerPrefix == "" {
		cfg.DockerPrefix = defaultDockerPrefix
	}
	if cfg.DockerVolumes && !v1alpha1.AllowedDockerVolumeName(cfg.DockerPrefix) {
		cfg.DockerVolumes = false
	}
	if cfg.DockerHost == "" {
		cfg.DockerHost = os.Getenv("DOCKER_HOST")
	}
	if cfg.HostDir != "" {
		host, err := filepath.Abs(cfg.HostDir)
		if err != nil {
			return nil, err
		}
		cfg.HostDir = host
	}
	if cfg.MaxVolumesPerTenant <= 0 {
		cfg.MaxVolumesPerTenant = defaultMaxVolumesPerTenant
	}
	s := &Store{
		dir:    abs,
		cfg:    cfg,
		vols:   map[string]*volRec{},
		byName: map[string]string{},
		mounts: map[string][]mountRec{},
		docker: dockerClient{host: cfg.DockerHost, enabled: cfg.DockerVolumes},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

func tenantOf(apiKey string) string {
	sum := sha256.Sum256([]byte("cubepod-e2b-volume-v1\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

func nameKey(tenant, name string) string { return tenant + "\x00" + name }

// ValidName reports whether name matches the 2.64.0 OpenAPI pattern.
func ValidName(name string) bool {
	return name != "" && len(name) <= maxVolumeNameLen && volumeNameRe.MatchString(name)
}

// Create inserts a volume for the API key's tenant.
func (s *Store) Create(apiKey, name string) (Volume, error) {
	if apiKey == "" {
		return Volume{}, ErrUnauthorized
	}
	if !ValidName(name) {
		return Volume{}, ErrInvalidName
	}
	s.mu.Lock()

	tenant := tenantOf(apiKey)
	if _, ok := s.byName[nameKey(tenant, name)]; ok {
		s.mu.Unlock()
		return Volume{}, ErrExists
	}
	tenantCount := 0
	for _, r := range s.vols {
		if r.meta.Tenant == tenant {
			tenantCount++
		}
	}
	if tenantCount >= s.cfg.MaxVolumesPerTenant {
		s.mu.Unlock()
		return Volume{}, ErrLimitExceeded
	}
	id, err := newID()
	if err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	token, err := newToken()
	if err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	rec := &volRec{
		meta: Volume{
			VolumeID:      id,
			Name:          name,
			Token:         token,
			Tenant:        tenant,
			CreatedAt:     time.Now().UTC(),
			DataDir:       filepath.Join(s.dir, id, "data"),
			DaemonDataDir: s.daemonDataPath(id),
		},
		attrs: map[string]fileAttr{},
	}
	volDir := filepath.Join(s.dir, id)
	// Parent holds metadata and stays 0700 so a sandbox user cannot walk
	// sibling volumes. Only data/ is mounted, and it is owned by uid 1000.
	if err := os.MkdirAll(volDir, 0o700); err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	if err := os.Chmod(volDir, 0o700); err != nil {
		_ = os.RemoveAll(volDir)
		s.mu.Unlock()
		return Volume{}, err
	}
	if err := os.Mkdir(rec.meta.DataDir, 0o755); err != nil {
		_ = os.RemoveAll(volDir)
		s.mu.Unlock()
		return Volume{}, err
	}
	if err := ownPath(rec.meta.DataDir, WriteOpts{}, true); err != nil {
		_ = os.RemoveAll(volDir)
		s.mu.Unlock()
		return Volume{}, err
	}
	if err := s.saveMeta(rec); err != nil {
		_ = os.RemoveAll(volDir)
		s.mu.Unlock()
		return Volume{}, err
	}
	s.vols[id] = rec
	s.byName[nameKey(tenant, name)] = id
	out := rec.meta
	dockerVolumes := s.cfg.DockerVolumes
	daemonDataDir := rec.meta.DaemonDataDir
	s.mu.Unlock()

	if dockerVolumes {
		if dockerName, err := s.ensureDockerVolume(id, daemonDataDir); err == nil {
			s.mu.Lock()
			if cur, ok := s.vols[id]; ok {
				cur.meta.DockerVolume = dockerName
				_ = s.saveMeta(cur)
				out = cur.meta
			}
			s.mu.Unlock()
		}
	}
	return out, nil
}

// List returns volumes for this API key. Tokens are not included.
func (s *Store) List(apiKey string) ([]Volume, error) {
	if apiKey == "" {
		return nil, ErrUnauthorized
	}
	tenant := tenantOf(apiKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Volume, 0)
	for _, rec := range s.vols {
		if rec.meta.Tenant != tenant {
			continue
		}
		cp := rec.meta
		cp.Token = ""
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].VolumeID < out[j].VolumeID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Get returns one volume, including its content token, for this tenant.
func (s *Store) Get(apiKey, id string) (Volume, error) {
	if apiKey == "" {
		return Volume{}, ErrUnauthorized
	}
	if !volumeIDRe.MatchString(id) {
		return Volume{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok || rec.meta.Tenant != tenantOf(apiKey) {
		return Volume{}, ErrNotFound
	}
	return rec.meta, nil
}

// Authenticate checks a content bearer token against the volume id.
func (s *Store) Authenticate(id, token string) (Volume, error) {
	if token == "" || !volumeIDRe.MatchString(id) {
		return Volume{}, ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok || !tokenMatch(token, rec.meta.Token) {
		return Volume{}, ErrUnauthorized
	}
	return rec.meta, nil
}

// Delete removes a volume owned by this tenant. A mounted volume returns ErrInUse.
func (s *Store) Delete(apiKey, id string) (Volume, error) {
	if apiKey == "" {
		return Volume{}, ErrUnauthorized
	}
	if !volumeIDRe.MatchString(id) {
		return Volume{}, ErrNotFound
	}
	s.mu.Lock()
	rec, ok := s.vols[id]
	if !ok || rec.meta.Tenant != tenantOf(apiKey) {
		s.mu.Unlock()
		return Volume{}, ErrNotFound
	}
	for _, ms := range s.mounts {
		for _, m := range ms {
			if m.VolumeID == id {
				s.mu.Unlock()
				return Volume{}, ErrInUse
			}
		}
	}
	volDir := filepath.Join(s.dir, id)
	if !s.withinRoot(volDir) {
		s.mu.Unlock()
		return Volume{}, ErrInvalidPath
	}
	dockerName := rec.meta.DockerVolume
	if err := os.RemoveAll(volDir); err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	delete(s.byName, nameKey(rec.meta.Tenant, rec.meta.Name))
	delete(s.vols, id)
	cp := rec.meta
	s.mu.Unlock()

	if dockerName != "" {
		_ = s.removeDockerVolume(id, dockerName)
	}
	return cp, nil
}

// NotePVC records the filesystem PVC created for this volume.
func (s *Store) NotePVC(id, pvcName, namespace string) error {
	if !volumeIDRe.MatchString(id) || !strings.HasPrefix(pvcName, "e2bvol-") {
		return ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok {
		return ErrNotFound
	}
	rec.meta.PVCName = pvcName
	rec.meta.PVCNamespace = namespace
	return s.saveMeta(rec)
}

// LookupMounts resolves mount requests for a tenant without recording them.
func (s *Store) LookupMounts(apiKey string, reqs []MountRequest) ([]PlannedMount, error) {
	if apiKey == "" {
		return nil, ErrUnauthorized
	}
	if len(reqs) > maxMountsPerSandbox {
		return nil, ErrInvalidPath
	}
	tenant := tenantOf(apiKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	seenPath := map[string]struct{}{}
	out := make([]PlannedMount, 0, len(reqs))
	for _, req := range reqs {
		if !ValidName(req.Name) {
			return nil, ErrInvalidName
		}
		mp, err := cleanMountPath(req.Path)
		if err != nil {
			return nil, err
		}
		if _, dup := seenPath[mp]; dup {
			return nil, ErrConflict
		}
		seenPath[mp] = struct{}{}
		id, ok := s.byName[nameKey(tenant, req.Name)]
		if !ok {
			return nil, ErrNotFound
		}
		out = append(out, PlannedMount{VolumeID: id, Name: req.Name, Path: mp})
	}
	return out, nil
}

// CommitMounts records sandbox mounts. The volume must still belong to tenantKey.
func (s *Store) CommitMounts(sandboxID, apiKey string, planned []PlannedMount) error {
	if sandboxID == "" || apiKey == "" {
		return ErrInvalidPath
	}
	tenant := tenantOf(apiKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := make([]mountRec, 0, len(planned))
	for _, p := range planned {
		rec, ok := s.vols[p.VolumeID]
		if !ok || rec.meta.Tenant != tenant || rec.meta.Name != p.Name {
			return ErrNotFound
		}
		recs = append(recs, mountRec{VolumeID: p.VolumeID, Name: p.Name, Path: p.Path, Tenant: tenant})
	}
	if len(recs) == 0 {
		delete(s.mounts, sandboxID)
	} else {
		s.mounts[sandboxID] = recs
	}
	return s.saveMounts()
}

// CopyMounts duplicates mount records onto another sandbox (fork).
func (s *Store) CopyMounts(fromID, toID string) error {
	if fromID == "" || toID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.mounts[fromID]
	if len(src) == 0 {
		return nil
	}
	cp := append([]mountRec(nil), src...)
	s.mounts[toID] = cp
	return s.saveMounts()
}

// UnmountSandbox drops mount records for a deleted sandbox. Volume bytes stay.
func (s *Store) UnmountSandbox(sandboxID string) {
	if sandboxID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mounts[sandboxID]; !ok {
		return
	}
	delete(s.mounts, sandboxID)
	_ = s.saveMounts()
}

// ReconcileMounts drops persisted mount records for sandboxes that are no
// longer active, preventing stale records after a gateway restart from blocking
// volume deletion forever.
func (s *Store) ReconcileMounts(active map[string]struct{}) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for sb := range s.mounts {
		if _, ok := active[sb]; !ok {
			delete(s.mounts, sb)
			removed++
		}
	}
	if removed > 0 {
		_ = s.saveMounts()
	}
	return removed
}

// MountsForSandbox returns runtime mount instructions. It retries Docker
// volume creation when provisioning is enabled and the named volume is missing.
func (s *Store) MountsForSandbox(sandboxID string) []MountSpec {
	s.mu.Lock()
	src := s.mounts[sandboxID]
	if len(src) == 0 {
		s.mu.Unlock()
		return nil
	}
	out := make([]MountSpec, 0, len(src))
	type pendingDocker struct {
		idx           int
		volID         string
		daemonDataDir string
	}
	var pending []pendingDocker
	for _, m := range src {
		rec, ok := s.vols[m.VolumeID]
		if !ok {
			continue
		}
		host := s.daemonDataPath(m.VolumeID)
		local := filepath.Join(s.dir, m.VolumeID, "data")
		// Mount only the data directory. The store root and {id} hold metadata.
		if filepath.Base(host) != "data" || filepath.Base(local) != "data" {
			continue
		}
		idx := len(out)
		out = append(out, MountSpec{
			VolumeName:    m.Name,
			VolumeID:      m.VolumeID,
			ContainerPath: m.Path,
			HostPath:      host,
			LocalPath:     local,
			DockerVolume:  rec.meta.DockerVolume,
		})
		if s.cfg.DockerVolumes && rec.meta.DockerVolume == "" {
			pending = append(pending, pendingDocker{idx: idx, volID: m.VolumeID, daemonDataDir: host})
		}
	}
	s.mu.Unlock()

	for _, p := range pending {
		name, err := s.ensureDockerVolume(p.volID, p.daemonDataDir)
		if err != nil {
			continue
		}
		out[p.idx].DockerVolume = name
		s.mu.Lock()
		if rec, ok := s.vols[p.volID]; ok {
			rec.meta.DockerVolume = name
			_ = s.saveMeta(rec)
		}
		s.mu.Unlock()
	}
	return out
}

// DataDir returns the local data directory for a volume id, if it is loaded.
func (s *Store) DataDir(id string) (string, bool) {
	if !volumeIDRe.MatchString(id) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok {
		return "", false
	}
	return rec.meta.DataDir, true
}

// MapSandboxPath maps a sandbox filesystem path onto a mounted volume.
// ok is false when the path is not under any mount.
func (s *Store) MapSandboxPath(sandboxID, userPath string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.mounts[sandboxID]
	if len(ms) == 0 {
		return "", false, nil
	}
	cleaned, err := cleanUserPath(userPath)
	if err != nil {
		return "", false, err
	}
	var best *mountRec
	bestRel := ""
	for i := range ms {
		rel, ok := relativeToMount(ms[i].Path, cleaned)
		if !ok {
			continue
		}
		if best == nil || len(ms[i].Path) > len(best.Path) {
			m := ms[i]
			best = &m
			bestRel = rel
		}
	}
	if best == nil {
		return "", false, nil
	}
	rec, ok := s.vols[best.VolumeID]
	if !ok {
		return "", false, nil
	}
	full, err := secureJoin(rec.meta.DataDir, bestRel)
	if err != nil {
		return "", true, err
	}
	return full, true, nil
}

// Stat returns metadata for a path inside the volume.
func (s *Store) Stat(id, userPath string) (Entry, error) {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	return statBeneath(int(root.Fd()), abs, rel, rec.attrs)
}

// ListDir returns directory entries. depth is the number of levels below the
// directory to include (1 = immediate children). The directory itself is omitted.
func (s *Store) ListDir(id, userPath string, depth int) ([]Entry, error) {
	if depth < 0 || depth > maxListDepth {
		return nil, ErrInvalidPath
	}
	if depth == 0 {
		return []Entry{}, nil
	}
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := resolveDirFD(int(root.Fd()), rel, false, 0)
	if err != nil {
		if errors.Is(err, ErrInvalidPath) && rel != "" {
			// Check if the final component itself is a symlink so ListDir returns ErrNotDir.
			if ent, stErr := statBeneath(int(root.Fd()), abs, rel, rec.attrs); stErr == nil && ent.Type == "symlink" {
				return nil, ErrNotDir
			}
		}
		return nil, err
	}
	defer dir.Close()
	var out []Entry
	if err := walkDirFD(dir, abs, depth, rec.attrs, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Entry{}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// MakeDir creates a directory. Force creates missing parents.
func (s *Store) MakeDir(id, userPath string, opts WriteOpts) (Entry, error) {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if abs != "/" {
		if err := validateModeOwner(opts); err != nil {
			return Entry{}, err
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	if abs == "/" {
		return statBeneath(int(root.Fd()), abs, rel, rec.attrs)
	}

	dirMode := os.FileMode(0o755)
	if opts.Mode != nil {
		dirMode = os.FileMode(*opts.Mode & 0o777)
	}
	parentRel, base := splitRel(rel)
	var parent *os.File
	if opts.Force {
		parent, err = resolveDirFD(int(root.Fd()), parentRel, true, 0o755)
	} else {
		parent, err = resolveDirFD(int(root.Fd()), parentRel, false, 0)
	}
	if err != nil {
		return Entry{}, err
	}
	defer parent.Close()

	var st unix.Stat_t
	statErr := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW)
	if statErr == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return Entry{}, ErrInvalidPath
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return Entry{}, ErrConflict
		}
		if opts.Mode != nil {
			d, err := openFileBeneath(int(root.Fd()), rel, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			if err != nil {
				return Entry{}, err
			}
			chErr := d.Chmod(dirMode)
			_ = d.Close()
			if chErr != nil {
				return Entry{}, chErr
			}
		}
	} else if !errors.Is(statErr, unix.ENOENT) {
		return Entry{}, mapUnixErr(statErr)
	} else {
		if mkErr := unix.Mkdirat(int(parent.Fd()), base, uint32(dirMode)); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			return Entry{}, mapUnixErr(mkErr)
		}
		d, err := openFileBeneath(int(root.Fd()), rel, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return Entry{}, err
		}
		chErr := ownFileFD(d, dirMode)
		_ = d.Close()
		if chErr != nil {
			return Entry{}, chErr
		}
	}
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return statBeneath(int(root.Fd()), abs, rel, rec.attrs)
}

// WriteFile creates or overwrites a file. Force creates missing parents.
// Existing files are overwritten even when Force is false (2.64.0 write_file).
// Request body I/O is streamed to a private temp file outside any store/volume lock.
func (s *Store) WriteFile(id, userPath string, r io.Reader, opts WriteOpts) (Entry, error) {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if abs == "/" {
		return Entry{}, ErrInvalidPath
	}
	if err := validateModeOwner(opts); err != nil {
		return Entry{}, err
	}

	// Stage the upload into the volume's private 0700 directory (outside data/)
	// so no lock is held during network I/O and partial writes never touch data/.
	volDir := filepath.Join(s.dir, id)
	tmpFile, err := os.CreateTemp(volDir, ".upload-*")
	if err != nil {
		return Entry{}, err
	}
	tmpPath := tmpFile.Name()
	tmpName := filepath.Base(tmpPath)
	committed := false
	defer func() {
		if !committed {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmpFile, r); err != nil {
		return Entry{}, err
	}
	fileMode := os.FileMode(0o644)
	if opts.Mode != nil {
		fileMode = os.FileMode(*opts.Mode & 0o777)
	}
	if err := ownFileFD(tmpFile, fileMode); err != nil {
		return Entry{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return Entry{}, err
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()

	parentRel, base := splitRel(rel)
	var parent *os.File
	if opts.Force {
		parent, err = resolveDirFD(int(root.Fd()), parentRel, true, 0o755)
	} else {
		parent, err = resolveDirFD(int(root.Fd()), parentRel, false, 0)
	}
	if err != nil {
		return Entry{}, err
	}
	defer parent.Close()

	var st unix.Stat_t
	if statErr := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFDIR || st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return Entry{}, ErrConflict
		}
	} else if !errors.Is(statErr, unix.ENOENT) {
		return Entry{}, mapUnixErr(statErr)
	}

	volDirFile, err := os.Open(volDir)
	if err != nil {
		return Entry{}, err
	}
	defer volDirFile.Close()
	if err := unix.Renameat(int(volDirFile.Fd()), tmpName, int(parent.Fd()), base); err != nil {
		if !errors.Is(err, unix.EXDEV) {
			return Entry{}, mapUnixErr(err)
		}
		// Fallback across filesystems: open target with O_NOFOLLOW and copy.
		dstFD, openErr := unix.Openat(int(parent.Fd()), base, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(fileMode))
		if openErr != nil {
			return Entry{}, mapUnixErr(openErr)
		}
		dst := os.NewFile(uintptr(dstFD), base)
		src, srcErr := os.Open(tmpPath)
		if srcErr != nil {
			_ = dst.Close()
			return Entry{}, srcErr
		}
		_, cpErr := io.Copy(dst, src)
		_ = src.Close()
		if cpErr != nil {
			_ = dst.Close()
			return Entry{}, cpErr
		}
		if err := ownFileFD(dst, fileMode); err != nil {
			_ = dst.Close()
			return Entry{}, err
		}
		if err := dst.Close(); err != nil {
			return Entry{}, err
		}
		_ = os.Remove(tmpPath)
	}
	committed = true
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return statBeneath(int(root.Fd()), abs, rel, rec.attrs)
}

// OpenFile opens a regular file for reading without following a symlink.
func (s *Store) OpenFile(id, userPath string) (*os.File, error) {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return nil, err
	}
	if abs == "/" {
		return nil, ErrIsDir
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := openFileBeneath(int(root.Fd()), rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if fi.IsDir() {
		_ = f.Close()
		return nil, ErrIsDir
	}
	return f, nil
}

// Patch updates uid, gid, and mode metadata. On disk, only mode (masked to 0o777)
// is applied via fchmod; ownership stays defaultDataUID:defaultDataGID (1000:1000).
func (s *Store) Patch(id, userPath string, opts WriteOpts) (Entry, error) {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if err := validateModeOwner(opts); err != nil {
		return Entry{}, err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	f, err := openFileBeneath(int(root.Fd()), rel, unix.O_RDONLY, 0)
	if err != nil {
		return Entry{}, err
	}
	if opts.Mode != nil {
		if err := f.Chmod(os.FileMode(*opts.Mode & 0o777)); err != nil {
			_ = f.Close()
			return Entry{}, err
		}
	}
	_ = f.Close()
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return statBeneath(int(root.Fd()), abs, rel, rec.attrs)
}

// Remove deletes a file, symlink, or directory. Directories are removed with
// their children because 2.64.0 Volume.remove sends no recursive flag.
// The volume root itself cannot be removed.
func (s *Store) Remove(id, userPath string) error {
	rec, abs, rel, err := s.lookupVol(id, userPath)
	if err != nil {
		return err
	}
	if abs == "/" {
		return ErrInvalidPath
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	root, err := openDataRoot(rec.meta.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	isDir, err := removeBeneath(int(root.Fd()), rel)
	if err != nil {
		return err
	}
	s.dropAttrs(rec, abs, isDir)
	return s.saveAttrs(rec)
}

func (s *Store) lookupVol(id, userPath string) (*volRec, string, string, error) {
	if !volumeIDRe.MatchString(id) {
		return nil, "", "", ErrNotFound
	}
	abs, err := cleanUserPath(userPath)
	if err != nil {
		return nil, "", "", err
	}
	rel := strings.TrimPrefix(abs, "/")
	s.mu.Lock()
	rec, ok := s.vols[id]
	s.mu.Unlock()
	if !ok {
		return nil, "", "", ErrNotFound
	}
	return rec, abs, rel, nil
}

func (s *Store) putAttr(rec *volRec, abs string, opts WriteOpts) {
	if opts.UID == nil && opts.GID == nil && opts.Mode == nil {
		return
	}
	a := rec.attrs[abs]
	if opts.UID != nil {
		v := *opts.UID
		a.UID = &v
	}
	if opts.GID != nil {
		v := *opts.GID
		a.GID = &v
	}
	if opts.Mode != nil {
		v := *opts.Mode & 0o777
		a.Mode = &v
	}
	if rec.attrs == nil {
		rec.attrs = map[string]fileAttr{}
	}
	rec.attrs[abs] = a
}

func (s *Store) dropAttrs(rec *volRec, abs string, dir bool) {
	if !dir {
		delete(rec.attrs, abs)
		return
	}
	prefix := abs
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for k := range rec.attrs {
		if k == abs || strings.HasPrefix(k, prefix) {
			delete(rec.attrs, k)
		}
	}
}

func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !volumeIDRe.MatchString(e.Name()) {
			continue
		}
		id := e.Name()
		metaPath := filepath.Join(s.dir, id, "meta.json")
		b, err := os.ReadFile(metaPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		var meta Volume
		if err := json.Unmarshal(b, &meta); err != nil {
			return fmt.Errorf("volume %s meta: %w", id, err)
		}
		if meta.VolumeID != id || !ValidName(meta.Name) || meta.Tenant == "" || meta.Token == "" {
			continue
		}
		meta.DataDir = filepath.Join(s.dir, id, "data")
		meta.DaemonDataDir = s.daemonDataPath(id)
		volDir := filepath.Join(s.dir, id)
		if err := os.MkdirAll(meta.DataDir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(volDir, 0o700); err != nil {
			return err
		}
		if err := ownPath(meta.DataDir, WriteOpts{}, true); err != nil {
			return err
		}
		_ = os.Chmod(metaPath, 0o600)
		_ = os.Chmod(filepath.Join(s.dir, id, "attrs.json"), 0o600)
		rec := &volRec{meta: meta, attrs: map[string]fileAttr{}}
		if ab, err := os.ReadFile(filepath.Join(s.dir, id, "attrs.json")); err == nil {
			_ = json.Unmarshal(ab, &rec.attrs)
			if rec.attrs == nil {
				rec.attrs = map[string]fileAttr{}
			}
		}
		s.vols[id] = rec
		s.byName[nameKey(meta.Tenant, meta.Name)] = id
	}
	mp := filepath.Join(s.dir, "mounts.json")
	if b, err := os.ReadFile(mp); err == nil && len(b) > 0 {
		var raw map[string][]mountRec
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("mounts: %w", err)
		}
		for sb, ms := range raw {
			kept := make([]mountRec, 0, len(ms))
			for _, m := range ms {
				rec, ok := s.vols[m.VolumeID]
				if !ok || rec.meta.Tenant != m.Tenant {
					continue
				}
				kept = append(kept, m)
			}
			if len(kept) > 0 {
				s.mounts[sb] = kept
			}
		}
	}
	_ = os.Chmod(mp, 0o600)
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return err
	}
	return nil
}

func (s *Store) saveMeta(rec *volRec) error {
	return atomicWriteJSON(filepath.Join(s.dir, rec.meta.VolumeID, "meta.json"), rec.meta)
}

func (s *Store) saveAttrs(rec *volRec) error {
	if rec.attrs == nil {
		rec.attrs = map[string]fileAttr{}
	}
	return atomicWriteJSON(filepath.Join(s.dir, rec.meta.VolumeID, "attrs.json"), rec.attrs)
}

func (s *Store) saveMounts() error {
	return atomicWriteJSON(filepath.Join(s.dir, "mounts.json"), s.mounts)
}

func (s *Store) daemonDataPath(id string) string {
	if s.cfg.HostDir != "" {
		return filepath.Join(s.cfg.HostDir, id, "data")
	}
	return filepath.Join(s.dir, id, "data")
}

func (s *Store) withinRoot(p string) bool {
	rel, err := filepath.Rel(s.dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func (s *Store) ensureDockerVolume(id, device string) (string, error) {
	name := s.cfg.DockerPrefix + id
	if !v1alpha1.AllowedDockerVolumeName(name) || !strings.HasSuffix(name, id) {
		return "", errors.New("refusing docker volume name")
	}
	if err := s.docker.ensureBindVolume(name, device); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Store) removeDockerVolume(id, name string) error {
	if !v1alpha1.AllowedDockerVolumeName(name) || name != s.cfg.DockerPrefix+id {
		return errors.New("refusing to delete docker volume")
	}
	return s.docker.removeVolume(name)
}

func atomicWriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWrite(path, b)
}

func atomicWrite(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	ok = true
	return nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func tokenMatch(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func cleanMountPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return "", ErrInvalidPath
	}
	if !strings.HasPrefix(p, "/") {
		return "", ErrInvalidPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", ErrInvalidPath
		}
	}
	c := path.Clean(p)
	if c == "/" || c == "." || strings.HasPrefix(c, "../") || !v1alpha1.ValidE2BContainerPath(c) {
		return "", ErrInvalidPath
	}
	return c, nil
}

func cleanUserPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return "", ErrInvalidPath
	}
	if len(p) > 4096 {
		return "", ErrInvalidPath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", ErrInvalidPath
		}
	}
	c := path.Clean("/" + strings.TrimPrefix(p, "/"))
	if !strings.HasPrefix(c, "/") {
		return "", ErrInvalidPath
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return "", ErrInvalidPath
		}
	}
	return c, nil
}

func relativeToMount(mountPath, cleanedUser string) (string, bool) {
	if cleanedUser == mountPath {
		return "", true
	}
	if strings.HasPrefix(cleanedUser, mountPath+"/") {
		return strings.TrimPrefix(cleanedUser, mountPath+"/"), true
	}
	return "", false
}

func secureJoin(root, rel string) (string, error) {
	if rel == "" {
		return root, nil
	}
	cur := root
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", ErrInvalidPath
		}
		next := filepath.Join(cur, p)
		fi, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				rest := next
				if i+1 < len(parts) {
					rest = filepath.Join(append([]string{next}, parts[i+1:]...)...)
				}
				if !within(root, rest) {
					return "", ErrInvalidPath
				}
				for _, extra := range parts[i+1:] {
					if extra == "" || extra == "." || extra == ".." {
						return "", ErrInvalidPath
					}
				}
				return rest, nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// A symlink may be the final component so stat can report it.
			// Intermediate symlinks are never followed.
			if i != len(parts)-1 {
				return "", ErrInvalidPath
			}
			cur = next
			break
		}
		if i != len(parts)-1 && !fi.IsDir() {
			return "", ErrNotDir
		}
		cur = next
	}
	if !within(root, cur) {
		return "", ErrInvalidPath
	}
	return cur, nil
}

func within(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validateModeOwner(opts WriteOpts) error {
	if opts.Mode != nil && (*opts.Mode < 0 || *opts.Mode > 0o7777) {
		return ErrInvalidPath
	}
	if opts.UID != nil && *opts.UID < 0 {
		return ErrInvalidPath
	}
	if opts.GID != nil && *opts.GID < 0 {
		return ErrInvalidPath
	}
	return nil
}

// ownPath initializes the volume's data root directory with 1000:1000 ownership
// and 0755 mode via openDataRoot + fchown/fchmod.
func ownPath(full string, _ WriteOpts, _ bool) error {
	f, err := openDataRoot(full)
	if err != nil {
		return err
	}
	defer f.Close()
	return ownFileFD(f, 0o755)
}
