// Package volume implements the official E2B 2.64.0 Volume control-plane
// records and content filesystem.
//
// Data and metadata live on disk under a store directory so they survive a
// gateway process restart. The store root and each {id} directory are mode
// 0710 (owner rwx, group exec) so cubepod-volume-nfs can traverse into
// {id}/data without listing siblings or reading 0600 metadata. Only {id}/data
// is mounted into a sandbox. When Docker volume
// provisioning is enabled, each volume is also exposed as a named volume
// whose local driver bind-mounts that data directory. The name must start
// with cubepod-vol- (product) or cpod-test- (tests) and must match this
// volume id.
//
// Callers attach the filesystem claim vol-<id> on the sandbox pod. This
// package records the claim. It does not talk to a live Kata cluster.
package volume

import (
	"context"
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
	"strconv"
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
	// the mounted data directory. The store root and per-volume dirs are
	// 0710 with group defaultDataGID so cubepod-volume-nfs (1000:1000)
	// can traverse into data/ without listing siblings or reading 0600 metadata.
	defaultDataUID = 1000
	defaultDataGID = 1000
	volumeDirPerm  = os.FileMode(0o710)
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
	// ErrUnauthorized is a bad, missing, or revoked API key or content token.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrInvalidKey is an empty or self-identical API key during key rotation.
	ErrInvalidKey = errors.New("invalid api key")
	// ErrLimitExceeded means the tenant has reached its volume or byte quota.
	ErrLimitExceeded = errors.New("volume limit exceeded")
	// ErrInvalidType means an unsupported volume type was requested.
	ErrInvalidType = errors.New("invalid volume type")
	// ErrInvalidSize means an invalid or out-of-range block volume size was requested.
	ErrInvalidSize = errors.New("invalid volume size")
	// ErrUnsupportedType means the operation is not supported for this volume type (e.g. /volumecontent on block).
	ErrUnsupportedType = errors.New("content API is only supported for shared volumes")

	volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	volumeIDRe   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

const (
	// DefaultBlockVolumeSize is the default size for type: "block" volumes when omitted.
	DefaultBlockVolumeSize = "20Gi"
	// MinBlockVolumeBytes is the minimum allowed size for a block volume (1 GiB).
	MinBlockVolumeBytes int64 = 1 << 30
	// MaxBlockVolumeBytes is the maximum allowed size for a block volume (1 TiB).
	MaxBlockVolumeBytes int64 = 1 << 40
)

// Config controls where volume bytes live, KMS token encryption, tenant quotas,
// and optional Docker volume provisioning.
type Config struct {
	// HostDir is the Docker-daemon view of the store directory. When empty,
	// HostPath in mount specs is the local data directory.
	HostDir string
	// DockerVolumes creates and deletes named volumes via the Docker API.
	DockerVolumes bool
	// DockerPrefix defaults to cpod-test-vol-. Product callers set cubepod-vol-.
	// The prefix must itself be an allowed Docker volume name.
	DockerPrefix string
	// DockerHost is DOCKER_HOST (tcp:// or unix://). Empty uses the environment.
	DockerHost string
	// MaxVolumesPerTenant caps volumes per API key tenant (default 64).
	MaxVolumesPerTenant int
	// MaxBytesPerTenant caps total regular-file bytes across all volumes of a tenant (<=0 means unlimited).
	MaxBytesPerTenant int64
	// KMS optionally envelope-encrypts volume content tokens in meta.json.
	KMS KMSProvider
}

// CapacityStats summarizes filesystem capacity of the store root and active volume count.
type CapacityStats struct {
	CapacityBytes  uint64 `json:"capacityBytes"`
	UsedBytes      uint64 `json:"usedBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
	VolumesTotal   int    `json:"volumesTotal"`
}

// Volume is the durable record returned to the management API.
type Volume struct {
	VolumeID        string    `json:"volumeID"`
	Name            string    `json:"name"`
	Type            string    `json:"type,omitempty"`
	Size            string    `json:"size,omitempty"`
	SizeBytes       int64     `json:"sizeBytes,omitempty"`
	LegacyVolumeKey string    `json:"legacyVolumeKey,omitempty"`
	OwnerTenantHash string    `json:"ownerTenantHash,omitempty"`
	Token           string    `json:"token,omitempty"`
	EncryptedToken  string    `json:"encryptedToken,omitempty"`
	KMSKeyID        string    `json:"kmsKeyID,omitempty"`
	Tenant          string    `json:"tenant"`
	CreatedAt       time.Time `json:"createdAt"`
	DockerVolume    string    `json:"dockerVolume,omitempty"`
	PVCName         string    `json:"pvcName,omitempty"`
	PVCNamespace    string    `json:"pvcNamespace,omitempty"`
	DataDir         string    `json:"-"`
	DaemonDataDir   string    `json:"-"`
}

// EffectiveType returns the normalized volume type ("shared" when empty).
func (v Volume) EffectiveType() string {
	if strings.EqualFold(strings.TrimSpace(v.Type), v1alpha1.VolumeTypeBlock) {
		return v1alpha1.VolumeTypeBlock
	}
	return v1alpha1.VolumeTypeShared
}

// IsBlock reports whether this volume is an exclusive RWO block workspace volume.
func (v Volume) IsBlock() bool {
	return v.EffectiveType() == v1alpha1.VolumeTypeBlock
}

// CreateOpts configures optional fields when creating a volume.
type CreateOpts struct {
	Type            string
	Size            string
	SizeBytes       int64
	LegacyVolumeKey string
}

// AdoptBlockVolumeOpts registers an existing legacy block PVC (or local directory)
// into the volume store in-place without copying data.
type AdoptBlockVolumeOpts struct {
	APIKey          string
	TenantHash      string
	OwnerTenantHash string
	Name            string
	VolumeID        string
	PVCName         string
	PVCNamespace    string
	Size            string
	SizeBytes       int64
	LegacyVolumeKey string
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
	Type     string
	PVCName  string
	Path     string
}

// MountSpec is what a container runtime needs to attach a volume.
type MountSpec struct {
	VolumeName    string
	VolumeID      string
	Type          string
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

type tenantKeyring struct {
	Version int               `json:"version"`
	Keys    map[string]string `json:"keys,omitempty"`
	Revoked map[string]string `json:"revoked,omitempty"`
}

// Store is a process-local, mutex-guarded view of an on-disk volume directory.
type Store struct {
	mu           sync.Mutex
	dir          string
	cfg          Config
	vols         map[string]*volRec
	byName       map[string]string
	mounts       map[string][]mountRec
	tenantKeys   map[string]string
	revokedKeys  map[string]string
	tenantsMtime int64
	tenantsSize  int64
	tenantQuota  map[string]*sync.Mutex
	docker       dockerClient
}

// tightenVolumeDir sets mode 0710 and, best-effort, gid defaultDataGID on a
// store or per-volume directory. Group search (+x) lets cubepod-volume-nfs
// reach <volumeID>/data; missing group-read keeps the directory unlistable.
// meta.json / attrs.json / mounts.json / tenants.json stay 0600.
func tightenVolumeDir(dir string) error {
	if err := os.Chmod(dir, volumeDirPerm); err != nil {
		return err
	}
	_ = os.Chown(dir, -1, defaultDataGID)
	return nil
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
	if err := os.MkdirAll(abs, volumeDirPerm); err != nil {
		return nil, err
	}
	if err := tightenVolumeDir(abs); err != nil {
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
		dir:         abs,
		cfg:         cfg,
		vols:        map[string]*volRec{},
		byName:      map[string]string{},
		mounts:      map[string][]mountRec{},
		tenantKeys:  map[string]string{},
		revokedKeys: map[string]string{},
		tenantQuota: map[string]*sync.Mutex{},
		docker:      dockerClient{host: cfg.DockerHost, enabled: cfg.DockerVolumes},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

// CapacityStats queries unix.Statfs on the store directory and returns capacity/used/available bytes plus active volume count.
func (s *Store) CapacityStats() (CapacityStats, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(s.dir, &st); err != nil {
		return CapacityStats{}, err
	}
	bsize := uint64(st.Frsize)
	if bsize == 0 && st.Bsize > 0 {
		bsize = uint64(st.Bsize)
	}
	capBytes := st.Blocks * bsize
	availBytes := st.Bavail * bsize
	if availBytes > capBytes {
		availBytes = capBytes
	}
	var usedBytes uint64
	if st.Blocks >= st.Bfree {
		usedBytes = (st.Blocks - st.Bfree) * bsize
	}
	s.mu.Lock()
	volCount := len(s.vols)
	s.mu.Unlock()
	return CapacityStats{
		CapacityBytes:  capBytes,
		UsedBytes:      usedBytes,
		AvailableBytes: availBytes,
		VolumesTotal:   volCount,
	}, nil
}

func rawKeyHash(apiKey string) string {
	sum := sha256.Sum256([]byte("cubepod-volume-v1\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

func ownerKeyHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

func (s *Store) promoteOwnerTenantLocked(tenant, apiKey string) {
	if tenant == "" || apiKey == "" {
		return
	}
	oh := ownerKeyHash(apiKey)
	for id, rec := range s.vols {
		if rec.meta.Tenant == "" && rec.meta.OwnerTenantHash == oh {
			delete(s.byName, nameKey("owner:"+oh, rec.meta.Name))
			name := rec.meta.Name
			if existingID, dup := s.byName[nameKey(tenant, name)]; dup && existingID != id {
				suffix := id
				if len(suffix) > 8 {
					suffix = suffix[:8]
				}
				if len(name)+1+len(suffix) > maxVolumeNameLen {
					name = name[:maxVolumeNameLen-1-len(suffix)]
				}
				name = name + "-" + suffix
				rec.meta.Name = name
			}
			rec.meta.Tenant = tenant
			s.byName[nameKey(tenant, rec.meta.Name)] = id
			_ = s.saveMetaUnlocked(rec)
		}
	}
}

// resolveTenantLocked resolves an API key to its canonical tenant ID, rejecting
// empty or rotated/revoked keys. Caller must hold s.mu.Lock().
func (s *Store) resolveTenantLocked(apiKey string) (string, error) {
	if apiKey == "" {
		return "", ErrUnauthorized
	}
	s.syncKeyringFromDiskLocked()
	kh := rawKeyHash(apiKey)
	if _, revoked := s.revokedKeys[kh]; revoked {
		return "", ErrUnauthorized
	}
	if tid, ok := s.tenantKeys[kh]; ok && tid != "" {
		return tid, nil
	}
	return kh, nil
}

func nameKey(tenant, name string) string { return tenant + "\x00" + name }

// ValidName reports whether name matches the 2.64.0 OpenAPI pattern.
func ValidName(name string) bool {
	return name != "" && len(name) <= maxVolumeNameLen && volumeNameRe.MatchString(name)
}

// ParseBlockSize parses a positive Kubernetes storage quantity (e.g. "20Gi", "1024Mi", "1Ti")
// within [MinBlockVolumeBytes, MaxBlockVolumeBytes]. Empty string defaults to DefaultBlockVolumeSize.
func ParseBlockSize(raw string) (string, int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultBlockVolumeSize, 20 << 30, nil
	}
	var numEnd int
	for numEnd < len(s) && s[numEnd] >= '0' && s[numEnd] <= '9' {
		numEnd++
	}
	if numEnd == 0 {
		return "", 0, ErrInvalidSize
	}
	val, err := strconv.ParseInt(s[:numEnd], 10, 64)
	if err != nil || val <= 0 {
		return "", 0, ErrInvalidSize
	}
	unit := s[numEnd:]
	var mult int64
	switch unit {
	case "Mi":
		mult = 1 << 20
	case "Gi":
		mult = 1 << 30
	case "Ti":
		mult = 1 << 40
	case "M":
		mult = 1_000_000
	case "G":
		mult = 1_000_000_000
	case "T":
		mult = 1_000_000_000_000
	default:
		return "", 0, ErrInvalidSize
	}
	if val > MaxBlockVolumeBytes/mult {
		return "", 0, ErrInvalidSize
	}
	bytes := val * mult
	if bytes < MinBlockVolumeBytes || bytes > MaxBlockVolumeBytes {
		return "", 0, ErrInvalidSize
	}
	return s, bytes, nil
}

// Create inserts a shared volume for the API key's tenant.
// KMS envelope wrapping runs outside Store.mu so external HTTP KMS calls never block other tenants.
func (s *Store) Create(apiKey, name string) (Volume, error) {
	return s.CreateWithOptions(apiKey, name, CreateOpts{Type: v1alpha1.VolumeTypeShared})
}

// CreateWithOptions inserts a shared or block volume for the API key's tenant.
func (s *Store) CreateWithOptions(apiKey, name string, opts CreateOpts) (Volume, error) {
	if apiKey == "" {
		return Volume{}, ErrUnauthorized
	}
	if !ValidName(name) {
		return Volume{}, ErrInvalidName
	}
	volType := strings.TrimSpace(opts.Type)
	if volType == "" {
		volType = v1alpha1.VolumeTypeShared
	}
	if volType != v1alpha1.VolumeTypeShared && volType != v1alpha1.VolumeTypeBlock {
		return Volume{}, ErrInvalidType
	}
	var sizeStr string
	var sizeBytes int64
	if volType == v1alpha1.VolumeTypeShared {
		if strings.TrimSpace(opts.Size) != "" || opts.SizeBytes > 0 {
			return Volume{}, ErrInvalidSize
		}
	} else {
		if opts.SizeBytes > 0 && strings.TrimSpace(opts.Size) != "" {
			sizeStr = strings.TrimSpace(opts.Size)
			sizeBytes = opts.SizeBytes
			if sizeBytes < MinBlockVolumeBytes || sizeBytes > MaxBlockVolumeBytes {
				return Volume{}, ErrInvalidSize
			}
		} else {
			var err error
			sizeStr, sizeBytes, err = ParseBlockSize(opts.Size)
			if err != nil {
				return Volume{}, err
			}
		}
	}

	s.mu.Lock()
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	s.promoteOwnerTenantLocked(tenant, apiKey)
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
	maxTenantBytes := s.cfg.MaxBytesPerTenant
	kms := s.cfg.KMS
	s.mu.Unlock()

	var qmu *sync.Mutex
	if volType == v1alpha1.VolumeTypeBlock && maxTenantBytes > 0 {
		qmu = s.tenantQuotaLock(tenant)
		qmu.Lock()
		defer qmu.Unlock()
		used, err := s.tenantUsedBytes(tenant)
		if err != nil {
			return Volume{}, err
		}
		if used+sizeBytes > maxTenantBytes {
			return Volume{}, ErrLimitExceeded
		}
	}

	id, err := newID()
	if err != nil {
		return Volume{}, err
	}
	token, err := newToken()
	if err != nil {
		return Volume{}, err
	}
	var encToken, kmsKeyID string
	if kms != nil {
		encToken, kmsKeyID, err = sealVolumeToken(context.Background(), kms, id, token)
		if err != nil {
			return Volume{}, err
		}
	}

	s.mu.Lock()
	tenant, err = s.resolveTenantLocked(apiKey)
	if err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	if _, ok := s.byName[nameKey(tenant, name)]; ok {
		s.mu.Unlock()
		return Volume{}, ErrExists
	}
	tenantCount = 0
	for _, r := range s.vols {
		if r.meta.Tenant == tenant {
			tenantCount++
		}
	}
	if tenantCount >= s.cfg.MaxVolumesPerTenant {
		s.mu.Unlock()
		return Volume{}, ErrLimitExceeded
	}
	rec := &volRec{
		meta: Volume{
			VolumeID:        id,
			Name:            name,
			Type:            volType,
			Size:            sizeStr,
			SizeBytes:       sizeBytes,
			LegacyVolumeKey: strings.TrimSpace(opts.LegacyVolumeKey),
			Token:           token,
			EncryptedToken:  encToken,
			KMSKeyID:        kmsKeyID,
			Tenant:          tenant,
			CreatedAt:       time.Now().UTC(),
			DataDir:         filepath.Join(s.dir, id, "data"),
			DaemonDataDir:   s.daemonDataPath(id),
		},
		attrs: map[string]fileAttr{},
	}
	volDir := filepath.Join(s.dir, id)
	// Parent holds metadata and stays 0710 (owner rwx, group --x) so a
	// sandbox user cannot list sibling volumes, while cubepod-volume-nfs
	// (gid 1000) can traverse into data/. Only data/ is mounted, owned by uid 1000.
	if err := os.MkdirAll(volDir, volumeDirPerm); err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	if err := tightenVolumeDir(volDir); err != nil {
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

	if dockerVolumes && volType == v1alpha1.VolumeTypeShared {
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

// RollbackCreate removes a freshly created volume when downstream provisioning (e.g. K8s EnsurePVC) fails.
func (s *Store) RollbackCreate(id string) error {
	if !volumeIDRe.MatchString(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	rec, ok := s.vols[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	volDir := filepath.Join(s.dir, id)
	if !s.withinRoot(volDir) {
		s.mu.Unlock()
		return ErrInvalidPath
	}
	_ = os.RemoveAll(volDir)
	delete(s.byName, nameKey(rec.meta.Tenant, rec.meta.Name))
	delete(s.vols, id)
	s.mu.Unlock()
	return nil
}

// DeterministicVolumeID derives a stable 32-char hex volume ID from a namespace/PVC pair
// so multi-replica syncFromKube auto-adoption is idempotent across replicas.
func DeterministicVolumeID(seed string) string {
	sum := sha256.Sum256([]byte("cubepod-adopt-v1:" + seed))
	return hex.EncodeToString(sum[:16])
}

// AdoptBlockVolume idempotently registers an existing legacy block PVC (or local directory)
// into the store under the specified APIKey or TenantHash without touching underlying data.
// Returns (volume, created, err).
func (s *Store) AdoptBlockVolume(opts AdoptBlockVolumeOpts) (Volume, bool, error) {
	cleanPVC := strings.TrimSpace(opts.PVCName)
	if cleanPVC == "" {
		return Volume{}, false, ErrInvalidName
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = sanitizeAdoptedName(cleanPVC)
	}
	if !ValidName(name) {
		name = sanitizeAdoptedName(name)
	}
	sizeStr := strings.TrimSpace(opts.Size)
	sizeBytes := opts.SizeBytes
	if sizeStr == "" {
		sizeStr = DefaultBlockVolumeSize
	}
	if sizeBytes <= 0 {
		if canonical, b, err := ParseBlockSize(sizeStr); err == nil {
			sizeStr = canonical
			sizeBytes = b
		} else {
			sizeStr = DefaultBlockVolumeSize
			sizeBytes = 20 << 30
		}
	}

	var out Volume
	var created bool
	err := s.withStoreFlock(true, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()

		tenant := strings.TrimSpace(opts.TenantHash)
		ownerHash := strings.TrimSpace(opts.OwnerTenantHash)
		nameTenant := tenant
		if tenant == "" && ownerHash == "" {
			if opts.APIKey == "" {
				return ErrUnauthorized
			}
			var err error
			tenant, err = s.resolveTenantLocked(opts.APIKey)
			if err != nil {
				return err
			}
			ownerHash = ownerKeyHash(opts.APIKey)
			nameTenant = tenant
		} else if tenant == "" && ownerHash != "" {
			nameTenant = "owner:" + ownerHash
		}

		// Idempotent check by PVCName first.
		for _, r := range s.vols {
			if r.meta.PVCName == cleanPVC && r.meta.IsBlock() {
				if (tenant != "" && r.meta.Tenant != "" && r.meta.Tenant != tenant) ||
					(ownerHash != "" && r.meta.OwnerTenantHash != "" && r.meta.OwnerTenantHash != ownerHash) {
					return ErrConflict
				}
				out = r.meta
				created = false
				return nil
			}
		}

		// Resolve unique name within tenant if there is a collision.
		finalName := name
		if existingID, ok := s.byName[nameKey(nameTenant, finalName)]; ok {
			if existing, exists := s.vols[existingID]; exists && existing.meta.PVCName == cleanPVC {
				out = existing.meta
				created = false
				return nil
			}
			suffix := DeterministicVolumeID(cleanPVC)[:8]
			if len(finalName)+1+len(suffix) > maxVolumeNameLen {
				finalName = finalName[:maxVolumeNameLen-1-len(suffix)]
			}
			finalName = finalName + "-" + suffix
		}

		id := strings.TrimSpace(opts.VolumeID)
		if id == "" || !volumeIDRe.MatchString(id) {
			id = DeterministicVolumeID(opts.PVCNamespace + "/" + cleanPVC)
		}
		if existing, exists := s.vols[id]; exists {
			if (tenant != "" && existing.meta.Tenant != "" && existing.meta.Tenant != tenant) ||
				(ownerHash != "" && existing.meta.OwnerTenantHash != "" && existing.meta.OwnerTenantHash != ownerHash) {
				return ErrConflict
			}
			out = existing.meta
			created = false
			return nil
		}

		token, err := newToken()
		if err != nil {
			return err
		}
		var encToken, kmsKeyID string
		if s.cfg.KMS != nil {
			encToken, kmsKeyID, err = sealVolumeToken(context.Background(), s.cfg.KMS, id, token)
			if err != nil {
				return err
			}
		}
		rec := &volRec{
			meta: Volume{
				VolumeID:        id,
				Name:            finalName,
				Type:            v1alpha1.VolumeTypeBlock,
				Size:            sizeStr,
				SizeBytes:       sizeBytes,
				LegacyVolumeKey: strings.TrimSpace(opts.LegacyVolumeKey),
				OwnerTenantHash: ownerHash,
				Token:           token,
				EncryptedToken:  encToken,
				KMSKeyID:        kmsKeyID,
				Tenant:          tenant,
				CreatedAt:       time.Now().UTC(),
				PVCName:         cleanPVC,
				PVCNamespace:    strings.TrimSpace(opts.PVCNamespace),
				DataDir:         filepath.Join(s.dir, id, "data"),
				DaemonDataDir:   s.daemonDataPath(id),
			},
			attrs: map[string]fileAttr{},
		}
		volDir := filepath.Join(s.dir, id)
		if err := os.MkdirAll(volDir, 0o700); err != nil {
			return err
		}
		_ = os.Chmod(volDir, 0o700)
		if err := os.MkdirAll(rec.meta.DataDir, 0o755); err != nil {
			_ = os.RemoveAll(volDir)
			return err
		}
		_ = ownPath(rec.meta.DataDir, WriteOpts{}, true)
		if err := s.saveMetaUnlocked(rec); err != nil {
			_ = os.RemoveAll(volDir)
			return err
		}
		s.vols[id] = rec
		s.byName[nameKey(nameTenant, finalName)] = id
		out = rec.meta
		created = true
		return nil
	})
	return out, created, err
}

func sanitizeAdoptedName(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "legacy-workspace"
	}
	if len(out) > maxVolumeNameLen {
		out = out[:maxVolumeNameLen]
	}
	return out
}

// GetByID returns volume metadata by ID without tenant filtering (for internal control-plane checks).
func (s *Store) GetByID(id string) (Volume, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok {
		return Volume{}, false
	}
	return rec.meta, true
}

// FindByPVCName looks up a managed block volume by its bound PVC name.
func (s *Store) FindByPVCName(pvcName string) (Volume, bool) {
	clean := strings.TrimSpace(pvcName)
	if clean == "" {
		return Volume{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.vols {
		if r.meta.PVCName == clean && r.meta.IsBlock() {
			return r.meta, true
		}
	}
	return Volume{}, false
}

// ActiveMountOwners returns sandbox IDs currently recorded in mounts.json for volumeID.
func (s *Store) ActiveMountOwners(volumeID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var owners []string
	for sb, ms := range s.mounts {
		for _, m := range ms {
			if m.VolumeID == volumeID {
				owners = append(owners, sb)
				break
			}
		}
	}
	sort.Strings(owners)
	return owners
}

// EvictMount removes a stale sandbox mount entry for volumeID (or all mounts for sandboxID if volumeID == "").
func (s *Store) EvictMount(sandboxID, volumeID string) bool {
	if sandboxID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.mounts[sandboxID]
	if !ok {
		return false
	}
	if volumeID == "" {
		delete(s.mounts, sandboxID)
		_ = s.saveMounts()
		return true
	}
	filtered := ms[:0]
	removed := false
	for _, m := range ms {
		if m.VolumeID == volumeID {
			removed = true
			continue
		}
		filtered = append(filtered, m)
	}
	if !removed {
		return false
	}
	if len(filtered) == 0 {
		delete(s.mounts, sandboxID)
	} else {
		s.mounts[sandboxID] = filtered
	}
	_ = s.saveMounts()
	return true
}

// BlockVolumeCount returns the number of managed type: "block" volumes across all tenants.
func (s *Store) BlockVolumeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.vols {
		if r.meta.IsBlock() {
			n++
		}
	}
	return n
}

// RotateAPIKey migrates all volumes and active mount ownership from oldAPIKey to
// newAPIKey, revokes oldAPIKey from accessing migrated volumes, and persists the
// keyring in tenants.json with flock + atomic rename for RWX safety.
func (s *Store) RotateAPIKey(oldAPIKey, newAPIKey string) (int, error) {
	if oldAPIKey == "" || newAPIKey == "" || oldAPIKey == newAPIKey {
		return 0, ErrInvalidKey
	}
	freshSuffix, err := newID()
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rotated := 0
	err = s.withStoreFlock(true, func() error {
		if loadErr := s.reloadKeyringAndMetaUnlocked(); loadErr != nil {
			return loadErr
		}
		oldHash := rawKeyHash(oldAPIKey)
		newHash := rawKeyHash(newAPIKey)

		if _, revoked := s.revokedKeys[oldHash]; revoked {
			return ErrUnauthorized
		}
		if _, revoked := s.revokedKeys[newHash]; revoked {
			return ErrConflict
		}

		oldTenant := s.tenantKeys[oldHash]
		if oldTenant == "" {
			oldTenant = oldHash
		}
		s.promoteOwnerTenantLocked(oldTenant, oldAPIKey)

		if existingTenant, ok := s.tenantKeys[newHash]; ok && existingTenant != "" && existingTenant != oldTenant {
			return ErrConflict
		}
		for _, r := range s.vols {
			if r.meta.Tenant == newHash && newHash != oldTenant {
				return ErrConflict
			}
		}
		for _, ms := range s.mounts {
			for _, m := range ms {
				if m.Tenant == newHash && newHash != oldTenant {
					return ErrConflict
				}
			}
		}

		targetTenant := oldTenant
		if !strings.HasPrefix(oldTenant, "tnt_") {
			targetTenant = "tnt_" + freshSuffix
		}

		type prevVolTenant struct {
			rec        *volRec
			prevTenant string
		}
		var migrated []prevVolTenant
		for _, r := range s.vols {
			if r.meta.Tenant != oldTenant && r.meta.Tenant != targetTenant {
				continue
			}
			rotated++
			if r.meta.Tenant != targetTenant {
				prev := r.meta.Tenant
				r.meta.Tenant = targetTenant
				if saveErr := s.saveMetaUnlocked(r); saveErr != nil {
					r.meta.Tenant = prev
					for _, m := range migrated {
						delete(s.byName, nameKey(targetTenant, m.rec.meta.Name))
						m.rec.meta.Tenant = m.prevTenant
						s.byName[nameKey(m.prevTenant, m.rec.meta.Name)] = m.rec.meta.VolumeID
						_ = s.saveMetaUnlocked(m.rec)
					}
					return saveErr
				}
				delete(s.byName, nameKey(prev, r.meta.Name))
				s.byName[nameKey(targetTenant, r.meta.Name)] = r.meta.VolumeID
				migrated = append(migrated, prevVolTenant{rec: r, prevTenant: prev})
			}
		}

		mountsChanged := false
		for sb, ms := range s.mounts {
			for i := range ms {
				if ms[i].Tenant == oldTenant && ms[i].Tenant != targetTenant {
					ms[i].Tenant = targetTenant
					mountsChanged = true
				}
			}
			s.mounts[sb] = ms
		}
		if mountsChanged {
			if saveErr := s.saveMountsUnlocked(); saveErr != nil {
				return saveErr
			}
		}

		delete(s.tenantKeys, oldHash)
		s.tenantKeys[newHash] = targetTenant
		s.revokedKeys[oldHash] = time.Now().UTC().Format(time.RFC3339Nano)
		return s.saveKeyringUnlocked()
	})
	if err != nil {
		return 0, err
	}
	return rotated, nil
}

// List returns volumes for this API key. Tokens are not included.
func (s *Store) List(apiKey string) ([]Volume, error) {
	if apiKey == "" {
		return nil, ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		return nil, err
	}
	s.promoteOwnerTenantLocked(tenant, apiKey)
	out := make([]Volume, 0)
	for _, rec := range s.vols {
		if rec.meta.Tenant != tenant {
			continue
		}
		cp := rec.meta
		cp.Token = ""
		cp.EncryptedToken = ""
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
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		return Volume{}, err
	}
	s.promoteOwnerTenantLocked(tenant, apiKey)
	rec, ok := s.vols[id]
	if !ok || rec.meta.Tenant != tenant {
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
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		s.mu.Unlock()
		return Volume{}, err
	}
	s.promoteOwnerTenantLocked(tenant, apiKey)
	rec, ok := s.vols[id]
	if !ok || rec.meta.Tenant != tenant {
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

// NotePVC records the filesystem PVC (per-volume or shared RWX) for this volume.
func (s *Store) NotePVC(id, pvcName, namespace string) error {
	cleanPVC := strings.TrimSpace(pvcName)
	if !volumeIDRe.MatchString(id) || cleanPVC == "" || len(cleanPVC) > 253 || strings.ContainsAny(cleanPVC, "/ \t\r\n") {
		return ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok {
		return ErrNotFound
	}
	rec.meta.PVCName = cleanPVC
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
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		return nil, err
	}
	s.promoteOwnerTenantLocked(tenant, apiKey)
	seenPath := map[string]struct{}{}
	blockCount := 0
	out := make([]PlannedMount, 0, len(reqs))
	for _, req := range reqs {
		if !ValidName(req.Name) {
			return nil, ErrInvalidName
		}
		id, ok := s.byName[nameKey(tenant, req.Name)]
		if !ok {
			// Preserve existing path-validation error ordering when volume is missing:
			// validate path syntax so invalid paths still return ErrInvalidPath.
			if _, perr := cleanMountPath(req.Path); perr != nil && path.Clean(strings.TrimSpace(req.Path)) != "/workspace" {
				return nil, perr
			}
			return nil, ErrNotFound
		}
		rec, exists := s.vols[id]
		if !exists {
			return nil, ErrNotFound
		}
		var mp string
		if rec.meta.IsBlock() {
			if req.Path == "" || strings.ContainsRune(req.Path, 0) || strings.Contains(req.Path, `\`) || !strings.HasPrefix(req.Path, "/") {
				return nil, ErrInvalidPath
			}
			for _, seg := range strings.Split(req.Path, "/") {
				if seg == ".." {
					return nil, ErrInvalidPath
				}
			}
			mp = path.Clean(req.Path)
			if mp != "/workspace" {
				return nil, ErrInvalidPath
			}
			blockCount++
			if blockCount > 1 {
				return nil, ErrConflict
			}
		} else {
			mp, err = cleanMountPath(req.Path)
			if err != nil {
				return nil, err
			}
		}
		if _, dup := seenPath[mp]; dup {
			return nil, ErrConflict
		}
		seenPath[mp] = struct{}{}
		out = append(out, PlannedMount{
			VolumeID: id,
			Name:     req.Name,
			Type:     rec.meta.EffectiveType(),
			PVCName:  rec.meta.PVCName,
			Path:     mp,
		})
	}
	return out, nil
}

// CommitMounts records sandbox mounts. The volume must still belong to tenantKey.
// For type: "block" volumes, single-writer exclusivity is enforced atomically under Store.mu.
func (s *Store) CommitMounts(sandboxID, apiKey string, planned []PlannedMount) error {
	if sandboxID == "" || apiKey == "" {
		return ErrInvalidPath
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, err := s.resolveTenantLocked(apiKey)
	if err != nil {
		return err
	}
	recs := make([]mountRec, 0, len(planned))
	for _, p := range planned {
		rec, ok := s.vols[p.VolumeID]
		if !ok || rec.meta.Tenant != tenant || rec.meta.Name != p.Name {
			return ErrNotFound
		}
		if rec.meta.IsBlock() {
			for otherSB, ms := range s.mounts {
				if otherSB == sandboxID {
					continue
				}
				for _, existing := range ms {
					if existing.VolumeID == p.VolumeID {
						return ErrInUse
					}
				}
			}
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

// CopyMounts duplicates shared mount records onto another sandbox (fork).
// Exclusive type: "block" mounts are intentionally skipped because forked sandboxes
// receive an independent CSI VolumeSnapshot clone of /workspace.
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
	var cp []mountRec
	for _, m := range src {
		if rec, ok := s.vols[m.VolumeID]; ok && rec.meta.IsBlock() {
			continue
		}
		cp = append(cp, m)
	}
	if len(cp) == 0 {
		return nil
	}
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
			Type:          rec.meta.EffectiveType(),
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

	maxTenantBytes := s.cfg.MaxBytesPerTenant

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
	var written int64
	if maxTenantBytes > 0 {
		written, err = io.Copy(tmpFile, io.LimitReader(r, maxTenantBytes+1))
		if err != nil {
			return Entry{}, err
		}
		if written > maxTenantBytes {
			return Entry{}, ErrLimitExceeded
		}
	} else {
		written, err = io.Copy(tmpFile, r)
		if err != nil {
			return Entry{}, err
		}
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

	if maxTenantBytes > 0 {
		tenantID, ok := s.volumeTenant(id)
		if !ok {
			return Entry{}, ErrNotFound
		}
		qmu := s.tenantQuotaLock(tenantID)
		qmu.Lock()
		defer qmu.Unlock()
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

	var existingSize int64
	var st unix.Stat_t
	if statErr := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFDIR || st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return Entry{}, ErrConflict
		}
		if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Size > 0 {
			existingSize = st.Size
		}
	} else if !errors.Is(statErr, unix.ENOENT) {
		return Entry{}, mapUnixErr(statErr)
	}

	if maxTenantBytes > 0 {
		tenantID, ok := s.volumeTenant(id)
		if !ok {
			return Entry{}, ErrNotFound
		}
		usedBytes, err := s.tenantUsedBytes(tenantID)
		if err != nil {
			return Entry{}, err
		}
		netBytes := usedBytes - existingSize + written
		if netBytes > maxTenantBytes {
			return Entry{}, ErrLimitExceeded
		}
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
	if rec.meta.IsBlock() {
		return nil, "", "", ErrUnsupportedType
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

func (s *Store) volumeTenant(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vols[id]
	if !ok {
		return "", false
	}
	return rec.meta.Tenant, true
}

func (s *Store) tenantQuotaLock(tenantID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenantQuota == nil {
		s.tenantQuota = map[string]*sync.Mutex{}
	}
	qmu, ok := s.tenantQuota[tenantID]
	if !ok {
		qmu = &sync.Mutex{}
		s.tenantQuota[tenantID] = qmu
	}
	return qmu
}

func (s *Store) tenantUsedBytes(tenantID string) (int64, error) {
	s.mu.Lock()
	dirs := make([]string, 0, len(s.vols))
	var blockBytes int64
	for _, r := range s.vols {
		if r.meta.Tenant == tenantID {
			if r.meta.IsBlock() {
				blockBytes += r.meta.SizeBytes
			} else {
				dirs = append(dirs, r.meta.DataDir)
			}
		}
	}
	s.mu.Unlock()

	total := blockBytes
	for _, d := range dirs {
		n, err := sumDataRootBytes(d)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) withStoreFlock(exclusive bool, fn func() error) error {
	lockPath := filepath.Join(s.dir, ".store.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	_ = f.Chmod(0o600)
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		lockErr := unix.Flock(int(f.Fd()), how)
		if lockErr == nil {
			break
		}
		if !errors.Is(lockErr, unix.EINTR) {
			_ = f.Close()
			return lockErr
		}
	}
	defer func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}()
	return fn()
}

func (s *Store) keyringPath() string {
	return filepath.Join(s.dir, "tenants.json")
}

func (s *Store) loadKeyringUnlocked() error {
	tp := s.keyringPath()
	fi, err := os.Stat(tp)
	if err != nil {
		if os.IsNotExist(err) {
			s.tenantKeys = map[string]string{}
			s.revokedKeys = map[string]string{}
			s.tenantsMtime = 0
			s.tenantsSize = 0
			return nil
		}
		return err
	}
	b, err := os.ReadFile(tp)
	if err != nil {
		return err
	}
	_ = os.Chmod(tp, 0o600)
	var kr tenantKeyring
	if len(b) > 0 {
		if err := json.Unmarshal(b, &kr); err != nil {
			return fmt.Errorf("tenants keyring: %w", err)
		}
	}
	if kr.Keys == nil {
		kr.Keys = map[string]string{}
	}
	if kr.Revoked == nil {
		kr.Revoked = map[string]string{}
	}
	s.tenantKeys = kr.Keys
	s.revokedKeys = kr.Revoked
	s.tenantsMtime = fi.ModTime().UnixNano()
	s.tenantsSize = fi.Size()
	return nil
}

func (s *Store) saveKeyringUnlocked() error {
	tp := s.keyringPath()
	kr := tenantKeyring{
		Version: 1,
		Keys:    s.tenantKeys,
		Revoked: s.revokedKeys,
	}
	if err := atomicWriteJSON(tp, kr); err != nil {
		return err
	}
	if fi, err := os.Stat(tp); err == nil {
		s.tenantsMtime = fi.ModTime().UnixNano()
		s.tenantsSize = fi.Size()
	}
	return nil
}

// reloadKeyringAndMetaUnlocked refreshes tenants.json and any rotated volume/mount
// tenant fields from disk while the caller holds both s.mu and the store flock.
func (s *Store) reloadKeyringAndMetaUnlocked() error {
	if err := s.loadKeyringUnlocked(); err != nil {
		return err
	}
	for id, rec := range s.vols {
		b, err := os.ReadFile(filepath.Join(s.dir, id, "meta.json"))
		if err != nil {
			continue
		}
		var disk Volume
		if json.Unmarshal(b, &disk) != nil || disk.Tenant == "" {
			continue
		}
		if disk.Tenant != rec.meta.Tenant {
			delete(s.byName, nameKey(rec.meta.Tenant, rec.meta.Name))
			rec.meta.Tenant = disk.Tenant
			s.byName[nameKey(rec.meta.Tenant, rec.meta.Name)] = id
		}
	}
	mp := filepath.Join(s.dir, "mounts.json")
	if b, err := os.ReadFile(mp); err == nil && len(b) > 0 {
		var raw map[string][]mountRec
		if json.Unmarshal(b, &raw) == nil {
			rebuilt := map[string][]mountRec{}
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
					rebuilt[sb] = kept
				}
			}
			s.mounts = rebuilt
		}
	}
	return nil
}

// syncKeyringFromDiskLocked checks if another RWX replica updated tenants.json
// and reloads keyring + tenant mappings when needed. Caller must hold s.mu.
func (s *Store) syncKeyringFromDiskLocked() {
	fi, err := os.Stat(s.keyringPath())
	if err != nil {
		return
	}
	if fi.ModTime().UnixNano() == s.tenantsMtime && fi.Size() == s.tenantsSize {
		return
	}
	_ = s.withStoreFlock(false, func() error {
		return s.reloadKeyringAndMetaUnlocked()
	})
}

func (s *Store) load() error {
	type rawVolDisk struct {
		id        string
		metaBytes []byte
		attrBytes []byte
	}
	var rawVols []rawVolDisk
	var mountsBytes []byte

	if err := s.withStoreFlock(false, func() error {
		if err := s.loadKeyringUnlocked(); err != nil {
			return err
		}
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
			ab, _ := os.ReadFile(filepath.Join(s.dir, id, "attrs.json"))
			rawVols = append(rawVols, rawVolDisk{id: id, metaBytes: b, attrBytes: ab})
		}
		mp := filepath.Join(s.dir, "mounts.json")
		if b, err := os.ReadFile(mp); err == nil {
			mountsBytes = b
		}
		return nil
	}); err != nil {
		return err
	}

	var migratedLegacy []*volRec
	for _, rv := range rawVols {
		id := rv.id
		var meta Volume
		if err := json.Unmarshal(rv.metaBytes, &meta); err != nil {
			return fmt.Errorf("volume %s meta: %w", id, err)
		}
		if meta.VolumeID != id || !ValidName(meta.Name) || meta.Tenant == "" {
			continue
		}
		wasLegacyPlaintext := false
		switch {
		case meta.EncryptedToken != "":
			plainToken, err := openVolumeToken(context.Background(), s.cfg.KMS, id, meta.EncryptedToken)
			if err != nil {
				return fmt.Errorf("volume %s token unwrap: %w", id, err)
			}
			meta.Token = plainToken
		case meta.Token != "":
			if s.cfg.KMS != nil {
				encTok, keyID, err := sealVolumeToken(context.Background(), s.cfg.KMS, id, meta.Token)
				if err != nil {
					return fmt.Errorf("volume %s legacy token KMS migration: %w", id, err)
				}
				meta.EncryptedToken = encTok
				meta.KMSKeyID = keyID
				wasLegacyPlaintext = true
			}
		default:
			continue
		}

		meta.DataDir = filepath.Join(s.dir, id, "data")
		meta.DaemonDataDir = s.daemonDataPath(id)
		volDir := filepath.Join(s.dir, id)
		metaPath := filepath.Join(volDir, "meta.json")
		if err := os.MkdirAll(meta.DataDir, 0o755); err != nil {
			return err
		}
		if err := tightenVolumeDir(volDir); err != nil {
			return err
		}
		if err := ownPath(meta.DataDir, WriteOpts{}, true); err != nil {
			return err
		}
		_ = os.Chmod(metaPath, 0o600)
		_ = os.Chmod(filepath.Join(volDir, "attrs.json"), 0o600)
		rec := &volRec{meta: meta, attrs: map[string]fileAttr{}}
		if len(rv.attrBytes) > 0 {
			_ = json.Unmarshal(rv.attrBytes, &rec.attrs)
			if rec.attrs == nil {
				rec.attrs = map[string]fileAttr{}
			}
		}
		s.vols[id] = rec
		idxTenant := meta.Tenant
		if idxTenant == "" && meta.OwnerTenantHash != "" {
			idxTenant = "owner:" + meta.OwnerTenantHash
		}
		s.byName[nameKey(idxTenant, meta.Name)] = id
		if wasLegacyPlaintext {
			migratedLegacy = append(migratedLegacy, rec)
		}
	}

	if len(migratedLegacy) > 0 {
		if err := s.withStoreFlock(true, func() error {
			for _, rec := range migratedLegacy {
				if err := s.saveMetaUnlocked(rec); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if len(mountsBytes) > 0 {
		var raw map[string][]mountRec
		if err := json.Unmarshal(mountsBytes, &raw); err != nil {
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
	_ = os.Chmod(filepath.Join(s.dir, "mounts.json"), 0o600)
	_ = os.Chmod(s.keyringPath(), 0o600)
	if err := tightenVolumeDir(s.dir); err != nil {
		return err
	}
	return nil
}

func (s *Store) saveMetaUnlocked(rec *volRec) error {
	disk := rec.meta
	if disk.EncryptedToken != "" {
		// Never persist plaintext content token on disk when envelope encryption is active.
		disk.Token = ""
	}
	return atomicWriteJSON(filepath.Join(s.dir, rec.meta.VolumeID, "meta.json"), disk)
}

func (s *Store) saveMeta(rec *volRec) error {
	return s.withStoreFlock(true, func() error {
		return s.saveMetaUnlocked(rec)
	})
}

func (s *Store) saveAttrs(rec *volRec) error {
	if rec.attrs == nil {
		rec.attrs = map[string]fileAttr{}
	}
	return atomicWriteJSON(filepath.Join(s.dir, rec.meta.VolumeID, "attrs.json"), rec.attrs)
}

func (s *Store) saveMountsUnlocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "mounts.json"), s.mounts)
}

func (s *Store) saveMounts() error {
	return s.withStoreFlock(true, func() error {
		return s.saveMountsUnlocked()
	})
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
	if err := os.MkdirAll(dir, volumeDirPerm); err != nil {
		return err
	}
	if err := tightenVolumeDir(dir); err != nil {
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
	if c == "/" || c == "." || strings.HasPrefix(c, "../") || !v1alpha1.ValidVolumeContainerPath(c) {
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
