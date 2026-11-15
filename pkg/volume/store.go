// Package volume implements the official E2B 2.64.0 Volume control-plane
// records and content filesystem.
//
// Data and metadata live on disk under a store directory so they survive a
// gateway process restart. When Docker volume provisioning is enabled, each
// volume is also exposed as a named volume cpod-test-vol-<id> whose local
// driver bind-mounts that same data directory. The named volume is only
// created or deleted when its name has the cpod-test- prefix and matches
// this volume id.
//
// Kubernetes: callers may record a filesystem PersistentVolumeClaim per
// volume (see the gateway). This package does not attach that claim to a
// Kata pod. Live Kata / block-device mounting is not implemented here.
package volume

import (
	"crypto/rand"
	"crypto/sha256"
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
	"syscall"
	"time"
)

const (
	defaultDockerPrefix = "cpod-test-vol-"
	dockerPrefixGuard   = "cpod-test-"
	maxVolumeNameLen    = 128
	maxMountsPerSandbox = 32
	maxListDepth        = 64
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
	// DockerPrefix defaults to cpod-test-vol-. It must start with cpod-test-.
	DockerPrefix string
	// DockerHost is DOCKER_HOST (tcp:// or unix://). Empty uses the environment.
	DockerHost string
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
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	if cfg.DockerPrefix == "" {
		cfg.DockerPrefix = defaultDockerPrefix
	}
	if cfg.DockerVolumes && !strings.HasPrefix(cfg.DockerPrefix, dockerPrefixGuard) {
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
	defer s.mu.Unlock()

	tenant := tenantOf(apiKey)
	if _, ok := s.byName[nameKey(tenant, name)]; ok {
		return Volume{}, ErrExists
	}
	id, err := newID()
	if err != nil {
		return Volume{}, err
	}
	token, err := newToken()
	if err != nil {
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
	// 0777 so a sandbox user (often uid 1000, not the gateway root) can
	// write after the volume is mounted. umask is applied by MkdirAll, so
	// chmod follows.
	if err := os.MkdirAll(rec.meta.DataDir, 0o777); err != nil {
		return Volume{}, err
	}
	if err := os.Chmod(rec.meta.DataDir, 0o777); err != nil {
		_ = os.RemoveAll(volDir)
		return Volume{}, err
	}
	if err := s.saveMeta(rec); err != nil {
		_ = os.RemoveAll(volDir)
		return Volume{}, err
	}
	if s.cfg.DockerVolumes {
		if dockerName, err := s.ensureDockerVolume(id, rec.meta.DaemonDataDir); err == nil {
			rec.meta.DockerVolume = dockerName
			_ = s.saveMeta(rec)
		}
	}
	s.vols[id] = rec
	s.byName[nameKey(tenant, name)] = id
	return rec.meta, nil
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
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok || rec.meta.Tenant != tenantOf(apiKey) {
		return Volume{}, ErrNotFound
	}
	for _, ms := range s.mounts {
		for _, m := range ms {
			if m.VolumeID == id {
				return Volume{}, ErrInUse
			}
		}
	}
	volDir := filepath.Join(s.dir, id)
	if !s.withinRoot(volDir) {
		return Volume{}, ErrInvalidPath
	}
	dockerName := rec.meta.DockerVolume
	if err := os.RemoveAll(volDir); err != nil {
		return Volume{}, err
	}
	delete(s.byName, nameKey(rec.meta.Tenant, rec.meta.Name))
	delete(s.vols, id)
	cp := rec.meta
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

// MountsForSandbox returns runtime mount instructions. It retries Docker
// volume creation when provisioning is enabled and the named volume is missing.
func (s *Store) MountsForSandbox(sandboxID string) []MountSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.mounts[sandboxID]
	if len(src) == 0 {
		return nil
	}
	out := make([]MountSpec, 0, len(src))
	for _, m := range src {
		rec, ok := s.vols[m.VolumeID]
		if !ok {
			continue
		}
		if s.cfg.DockerVolumes && rec.meta.DockerVolume == "" {
			if name, err := s.ensureDockerVolume(m.VolumeID, s.daemonDataPath(m.VolumeID)); err == nil {
				rec.meta.DockerVolume = name
				_ = s.saveMeta(rec)
			}
		}
		out = append(out, MountSpec{
			VolumeName:    m.Name,
			VolumeID:      m.VolumeID,
			ContainerPath: m.Path,
			HostPath:      s.daemonDataPath(m.VolumeID),
			LocalPath:     filepath.Join(s.dir, m.VolumeID, "data"),
			DockerVolume:  rec.meta.DockerVolume,
		})
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
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	return s.statFull(rec, abs, full)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, ErrNotDir
	}
	var out []Entry
	if err := s.walk(rec, abs, full, depth, &out); err != nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if abs == "/" {
		return s.statFull(rec, abs, full)
	}
	if err := validateModeOwner(opts); err != nil {
		return Entry{}, err
	}
	if fi, err := os.Lstat(full); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return Entry{}, ErrInvalidPath
		}
		if !fi.IsDir() {
			return Entry{}, ErrConflict
		}
	} else if !os.IsNotExist(err) {
		return Entry{}, err
	} else if opts.Force {
		if err := os.MkdirAll(full, 0o777); err != nil {
			return Entry{}, err
		}
		_ = os.Chmod(full, 0o777)
	} else {
		parent := filepath.Dir(full)
		pfi, err := os.Lstat(parent)
		if err != nil {
			if os.IsNotExist(err) {
				return Entry{}, ErrNotExist
			}
			return Entry{}, err
		}
		if !pfi.IsDir() {
			return Entry{}, ErrNotDir
		}
		if err := os.Mkdir(full, 0o777); err != nil {
			if os.IsExist(err) {
				return s.statFull(rec, abs, full)
			}
			return Entry{}, err
		}
		if err := os.Chmod(full, 0o777); err != nil {
			return Entry{}, err
		}
	}
	if err := applyMeta(full, opts); err != nil {
		return Entry{}, err
	}
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return s.statFull(rec, abs, full)
}

// WriteFile creates or overwrites a file. Force creates missing parents.
// Existing files are overwritten even when Force is false (2.64.0 write_file).
func (s *Store) WriteFile(id, userPath string, r io.Reader, opts WriteOpts) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if abs == "/" {
		return Entry{}, ErrInvalidPath
	}
	if err := validateModeOwner(opts); err != nil {
		return Entry{}, err
	}
	parent := filepath.Dir(full)
	pfi, err := os.Lstat(parent)
	if err != nil {
		if !os.IsNotExist(err) {
			return Entry{}, err
		}
		if !opts.Force {
			return Entry{}, ErrNotExist
		}
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return Entry{}, err
		}
		_ = os.Chmod(parent, 0o777)
	} else if pfi.Mode()&os.ModeSymlink != 0 || !pfi.IsDir() {
		return Entry{}, ErrNotDir
	}
	if fi, err := os.Lstat(full); err == nil {
		if fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return Entry{}, ErrConflict
		}
	} else if !os.IsNotExist(err) {
		return Entry{}, err
	}
	// Default 0666 (then chmod, to beat umask) so the mounted sandbox user
	// can overwrite files the content API created as gateway root.
	mode := 0o666
	if opts.Mode != nil {
		mode = *opts.Mode & 0o777
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, os.FileMode(mode))
	if err != nil {
		return Entry{}, err
	}
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return Entry{}, copyErr
	}
	if closeErr != nil {
		return Entry{}, closeErr
	}
	if err := os.Chmod(full, os.FileMode(mode)); err != nil {
		return Entry{}, err
	}
	if err := applyMeta(full, opts); err != nil {
		return Entry{}, err
	}
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return s.statFull(rec, abs, full)
}

// OpenFile opens a regular file for reading without following a symlink.
func (s *Store) OpenFile(id, userPath string) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return nil, err
	}
	if abs == "/" {
		return nil, ErrIsDir
	}
	fi, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	if fi.IsDir() {
		return nil, ErrIsDir
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidPath
	}
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	return f, nil
}

// Patch updates uid, gid, and mode. Nil fields are left unchanged.
func (s *Store) Patch(id, userPath string, opts WriteOpts) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return Entry{}, err
	}
	if err := validateModeOwner(opts); err != nil {
		return Entry{}, err
	}
	fi, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotExist
		}
		return Entry{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Entry{}, ErrInvalidPath
	}
	if err := applyMeta(full, opts); err != nil {
		return Entry{}, err
	}
	s.putAttr(rec, abs, opts)
	if err := s.saveAttrs(rec); err != nil {
		return Entry{}, err
	}
	return s.statFull(rec, abs, full)
}

// Remove deletes a file, symlink, or directory. Directories are removed with
// their children because 2.64.0 Volume.remove sends no recursive flag.
// The volume root itself cannot be removed.
func (s *Store) Remove(id, userPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, abs, full, err := s.prepare(id, userPath)
	if err != nil {
		return err
	}
	if abs == "/" {
		return ErrInvalidPath
	}
	fi, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotExist
		}
		return err
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		s.dropAttrs(rec, abs, true)
	} else {
		if err := os.Remove(full); err != nil {
			return err
		}
		s.dropAttrs(rec, abs, false)
	}
	return s.saveAttrs(rec)
}

func (s *Store) prepare(id, userPath string) (*volRec, string, string, error) {
	if !volumeIDRe.MatchString(id) {
		return nil, "", "", ErrNotFound
	}
	rec, ok := s.vols[id]
	if !ok {
		return nil, "", "", ErrNotFound
	}
	abs, err := cleanUserPath(userPath)
	if err != nil {
		return nil, "", "", err
	}
	rel := strings.TrimPrefix(abs, "/")
	full, err := secureJoin(rec.meta.DataDir, rel)
	if err != nil {
		return nil, "", "", err
	}
	return rec, abs, full, nil
}

func (s *Store) statFull(rec *volRec, abs, full string) (Entry, error) {
	fi, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotExist
		}
		return Entry{}, err
	}
	ent := entryFrom(abs, fi)
	if ent.Type == "symlink" {
		if t, err := os.Readlink(full); err == nil {
			ent.Target = t
		}
	}
	if a, ok := rec.attrs[abs]; ok {
		if a.UID != nil {
			ent.UID = *a.UID
		}
		if a.GID != nil {
			ent.GID = *a.GID
		}
		if a.Mode != nil {
			ent.Mode = *a.Mode
		}
	}
	return ent, nil
}

func (s *Store) walk(rec *volRec, abs, full string, depth int, out *[]Entry) error {
	if depth < 1 {
		return nil
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		childAbs := path.Join(abs, name)
		childFull := filepath.Join(full, name)
		fi, err := os.Lstat(childFull)
		if err != nil {
			return err
		}
		ent := entryFrom(childAbs, fi)
		if ent.Type == "symlink" {
			if t, err := os.Readlink(childFull); err == nil {
				ent.Target = t
			}
		}
		if a, ok := rec.attrs[childAbs]; ok {
			if a.UID != nil {
				ent.UID = *a.UID
			}
			if a.GID != nil {
				ent.GID = *a.GID
			}
			if a.Mode != nil {
				ent.Mode = *a.Mode
			}
		}
		*out = append(*out, ent)
		if ent.Type == "directory" && depth > 1 {
			if err := s.walk(rec, childAbs, childFull, depth-1, out); err != nil {
				return err
			}
		}
	}
	return nil
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
		v := *opts.Mode
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
		if err := os.MkdirAll(meta.DataDir, 0o755); err != nil {
			return err
		}
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
	if !strings.HasPrefix(name, dockerPrefixGuard) || !strings.HasSuffix(name, id) {
		return "", errors.New("refusing docker volume name")
	}
	if err := s.docker.ensureBindVolume(name, device); err != nil {
		return "", err
	}
	return name, nil
}

func (s *Store) removeDockerVolume(id, name string) error {
	if !strings.HasPrefix(name, dockerPrefixGuard) || name != s.cfg.DockerPrefix+id {
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
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".tmp-*")
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
	// Equal length, so a constant-time compare does not leak the token.
	gb := []byte(got)
	wb := []byte(want)
	diff := 0
	for i := range gb {
		diff |= int(gb[i] ^ wb[i])
	}
	return diff == 0
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
	if c == "/" || c == "." || strings.HasPrefix(c, "../") {
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
	if !strings.HasPrefix(c, "/") || strings.Contains(c, "..") {
		return "", ErrInvalidPath
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

func entryFrom(abs string, fi os.FileInfo) Entry {
	atime, mtime, ctime, uid, gid := statTimes(fi)
	name := path.Base(abs)
	if abs == "/" {
		name = "/"
	}
	mode := int(fi.Mode().Perm())
	return Entry{
		Name:  name,
		Type:  entryType(fi),
		Path:  abs,
		Size:  fi.Size(),
		Mode:  mode,
		UID:   uid,
		GID:   gid,
		Atime: atime,
		Mtime: mtime,
		Ctime: ctime,
	}
}

func entryType(fi os.FileInfo) string {
	m := fi.Mode()
	switch {
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m.IsDir():
		return "directory"
	case m.IsRegular():
		return "file"
	default:
		return "unknown"
	}
}

func statTimes(fi os.FileInfo) (atime, mtime, ctime time.Time, uid, gid int) {
	mtime = fi.ModTime().UTC()
	atime, ctime = mtime, mtime
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	atime = time.Unix(st.Atim.Sec, st.Atim.Nsec).UTC()
	mtime = time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC()
	ctime = time.Unix(st.Ctim.Sec, st.Ctim.Nsec).UTC()
	uid = int(st.Uid)
	gid = int(st.Gid)
	return
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

func applyMeta(full string, opts WriteOpts) error {
	if opts.Mode != nil {
		if err := os.Chmod(full, os.FileMode(*opts.Mode&0o7777)); err != nil {
			return err
		}
	}
	if opts.UID != nil || opts.GID != nil {
		uid, gid := -1, -1
		if opts.UID != nil {
			uid = *opts.UID
		}
		if opts.GID != nil {
			gid = *opts.GID
		}
		// Not running as root is fine: the stored override is what stat returns.
		_ = os.Chown(full, uid, gid)
	}
	return nil
}
