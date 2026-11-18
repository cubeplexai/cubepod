// Package secret implements the tenant-isolated, durable secret store
// for official E2B /secrets management (CEP-0027).
package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxSecretsPerTenant caps secrets per tenant API key.
	DefaultMaxSecretsPerTenant = 100
	// MaxSecretValueBytes is the maximum UTF-8 byte length of a secret value (64 KiB).
	MaxSecretValueBytes = 64 << 10
	// DefaultListLimit is the default page size for List.
	DefaultListLimit = 100

	secretsFileName = "secrets.enc.json"
	kekFileName     = ".kek"
)

var (
	// ErrUnauthorized is returned when the API key is missing or empty.
	ErrUnauthorized = errors.New("API key is required")
	// ErrInvalidName is returned when a secret name does not match ^[a-zA-Z0-9_.-]{1,128}$.
	ErrInvalidName = errors.New("invalid secret name")
	// ErrInvalidValue is returned when a secret value is empty or exceeds 64 KiB.
	ErrInvalidValue = errors.New("invalid secret value")
	// ErrExists is returned when creating a secret whose name already exists for the tenant.
	ErrExists = errors.New("secret already exists")
	// ErrNotFound is returned when the secret does not exist or belongs to another tenant.
	ErrNotFound = errors.New("secret not found")
	// ErrLimitExceeded is returned when the tenant reaches MaxSecretsPerTenant.
	ErrLimitExceeded = errors.New("tenant secret limit exceeded")
	// ErrInvalidCursor is returned when nextToken cannot be decoded.
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	validSecretName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)
)

// SecretValue wraps a plaintext secret and redacts it in fmt and slog output.
type SecretValue string

// String implements fmt.Stringer without revealing the underlying secret.
func (SecretValue) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer without revealing the underlying secret.
func (SecretValue) GoString() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer without revealing the underlying secret.
func (SecretValue) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// Raw returns the underlying secret value for controlled vault binding only.
func (v SecretValue) Raw() string { return string(v) }

// SecretInfo is the write-only metadata returned by all /secrets endpoints.
type SecretInfo struct {
	ID        string            `json:"secretID"`
	Name      string            `json:"name"`
	Version   int               `json:"currentVersion"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// StoredSecret represents a durable secret record persisted by a Backend.
type StoredSecret struct {
	TenantHash string     `json:"tenantHash"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	Value      string     `json:"value"`
}

// Backend abstracts durable persistence for E2B secrets (local encrypted file or K8s Secrets).
type Backend interface {
	LoadAll(ctx context.Context) ([]StoredSecret, error)
	Put(ctx context.Context, rec StoredSecret) error
	Delete(ctx context.Context, id string) error
}

// Config configures the durable secret store.
type Config struct {
	// EncryptionKey is an optional 32-byte AES-256 key for FileBackend. If empty, a key is
	// loaded from or generated at <dir>/.kek with 0600 permissions.
	EncryptionKey []byte
	// MaxSecretsPerTenant caps secrets per tenant (default 100).
	MaxSecretsPerTenant int
	// OnUpdate is invoked when Sync detects a secret value or version update from another replica.
	OnUpdate func(tenantHash, secretID string, value SecretValue)
	// OnRevoke is invoked when Sync detects a secret deletion from another replica.
	OnRevoke func(tenantHash, secretID string)
}

type encryptedEnvelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type diskState struct {
	Secrets []StoredSecret `json:"secrets"`
}

// FileBackend persists all secrets in an AES-256-GCM encrypted JSON envelope under dir.
type FileBackend struct {
	mu      sync.Mutex
	dir     string
	key     []byte
	records map[string]StoredSecret
}

// NewFileBackend initializes an AES-256-GCM encrypted file backend under dir.
func NewFileBackend(dir string, encryptionKey []byte) (*FileBackend, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("secret store dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secret dir: %w", err)
	}
	key, err := resolveKEK(dir, encryptionKey)
	if err != nil {
		return nil, err
	}
	return &FileBackend{
		dir:     dir,
		key:     key,
		records: make(map[string]StoredSecret),
	}, nil
}

// LoadAll reads and decrypts all secrets from secrets.enc.json.
func (b *FileBackend) LoadAll(_ context.Context) ([]StoredSecret, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	path := filepath.Join(b.dir, secretsFileName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		b.records = make(map[string]StoredSecret)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read secrets store: %w", err)
	}

	var env encryptedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode secrets envelope: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode secrets nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode secrets ciphertext: %w", err)
	}

	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt secrets store: %w", err)
	}

	var state diskState
	if err := json.Unmarshal(plain, &state); err != nil {
		return nil, fmt.Errorf("decode secrets state: %w", err)
	}

	next := make(map[string]StoredSecret, len(state.Secrets))
	for _, rec := range state.Secrets {
		next[rec.ID] = rec
	}
	b.records = next
	return state.Secrets, nil
}

// Put writes or updates a secret and atomically persists the encrypted envelope.
func (b *FileBackend) Put(_ context.Context, rec StoredSecret) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	prev, existed := b.records[rec.ID]
	b.records[rec.ID] = rec
	if err := b.saveLocked(); err != nil {
		if existed {
			b.records[rec.ID] = prev
		} else {
			delete(b.records, rec.ID)
		}
		return err
	}
	return nil
}

// Delete removes a secret and atomically persists the encrypted envelope.
func (b *FileBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	prev, existed := b.records[id]
	if !existed {
		return nil
	}
	delete(b.records, id)
	if err := b.saveLocked(); err != nil {
		b.records[id] = prev
		return err
	}
	return nil
}

func (b *FileBackend) saveLocked() error {
	state := diskState{
		Secrets: make([]StoredSecret, 0, len(b.records)),
	}
	for _, rec := range b.records {
		state.Secrets = append(state.Secrets, rec)
	}
	sort.Slice(state.Secrets, func(i, j int) bool {
		return state.Secrets[i].ID < state.Secrets[j].ID
	})

	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(b.key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)

	env := encryptedEnvelope{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	path := filepath.Join(b.dir, secretsFileName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LatencyBuckets are the upper bounds (seconds) of the per-operation latency histograms.
var LatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// LatencySnapshot is a point-in-time copy of one operation's latency histogram.
// Counts are cumulative per bucket (Prometheus "le" semantics); Count is the +Inf bucket.
type LatencySnapshot struct {
	Buckets []float64
	Counts  []uint64
	Sum     float64
	Count   uint64
}

type opCounterPair struct {
	ok  atomic.Uint64
	err atomic.Uint64

	latMu     sync.Mutex
	latCounts [11]uint64
	latSum    float64
	latTotal  uint64
}

// track starts timing an operation; call the returned func (via defer) with the final error.
func (p *opCounterPair) track(errp *error) func() {
	start := time.Now()
	return func() {
		p.record(*errp)
		p.observe(time.Since(start).Seconds())
	}
}

func (p *opCounterPair) observe(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	p.latMu.Lock()
	for i, upper := range LatencyBuckets {
		if seconds <= upper {
			p.latCounts[i]++
		}
	}
	p.latSum += seconds
	p.latTotal++
	p.latMu.Unlock()
}

func (p *opCounterPair) latency() LatencySnapshot {
	p.latMu.Lock()
	defer p.latMu.Unlock()
	return LatencySnapshot{
		Buckets: append([]float64(nil), LatencyBuckets...),
		Counts:  append([]uint64(nil), p.latCounts[:]...),
		Sum:     p.latSum,
		Count:   p.latTotal,
	}
}

func (p *opCounterPair) record(err error) {
	if err != nil {
		p.err.Add(1)
	} else {
		p.ok.Add(1)
	}
}

type storeMetrics struct {
	create  opCounterPair
	list    opCounterPair
	get     opCounterPair
	update  opCounterPair
	delete  opCounterPair
	resolve opCounterPair
}

func (m *storeMetrics) snapshot() map[string]map[string]uint64 {
	return map[string]map[string]uint64{
		"create":  {"ok": m.create.ok.Load(), "error": m.create.err.Load()},
		"list":    {"ok": m.list.ok.Load(), "error": m.list.err.Load()},
		"get":     {"ok": m.get.ok.Load(), "error": m.get.err.Load()},
		"update":  {"ok": m.update.ok.Load(), "error": m.update.err.Load()},
		"delete":  {"ok": m.delete.ok.Load(), "error": m.delete.err.Load()},
		"resolve": {"ok": m.resolve.ok.Load(), "error": m.resolve.err.Load()},
	}
}

func (m *storeMetrics) latencySnapshot() map[string]LatencySnapshot {
	return map[string]LatencySnapshot{
		"create":  m.create.latency(),
		"list":    m.list.latency(),
		"get":     m.get.latency(),
		"update":  m.update.latency(),
		"delete":  m.delete.latency(),
		"resolve": m.resolve.latency(),
	}
}

// Store manages tenant-isolated E2B secrets backed by a durable Backend.
type Store struct {
	mu        sync.RWMutex
	syncMu    sync.Mutex
	backend   Backend
	maxPerTen int
	byID      map[string]*StoredSecret
	byTenName map[string]string // tenantHash + "\x00" + name -> id
	nowFn     func() time.Time
	writeSeq  atomic.Uint64
	metrics   storeMetrics
	onUpdate  func(tenantHash, secretID string, value SecretValue)
	onRevoke  func(tenantHash, secretID string)
}

// TenantHash derives a stable opaque tenant identifier from an API key.
func TenantHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

// Open opens or initializes an AES-256-GCM encrypted file-backed secret store under dir.
func Open(dir string, cfg Config) (*Store, error) {
	backend, err := NewFileBackend(dir, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return OpenWithBackend(backend, cfg)
}

// OpenWithBackend initializes a secret Store backed by the provided Backend and performs an initial load.
func OpenWithBackend(backend Backend, cfg Config) (*Store, error) {
	if backend == nil {
		return nil, errors.New("secret backend is required")
	}
	maxPerTen := cfg.MaxSecretsPerTenant
	if maxPerTen <= 0 {
		maxPerTen = DefaultMaxSecretsPerTenant
	}
	st := &Store{
		backend:   backend,
		maxPerTen: maxPerTen,
		byID:      make(map[string]*StoredSecret),
		byTenName: make(map[string]string),
		nowFn:     func() time.Time { return time.Now().UTC() },
		onUpdate:  cfg.OnUpdate,
		onRevoke:  cfg.OnRevoke,
	}
	if err := st.loadInitial(context.Background()); err != nil {
		return nil, err
	}
	return st, nil
}

func resolveKEK(dir string, explicit []byte) ([]byte, error) {
	if len(explicit) > 0 {
		if len(explicit) != 32 {
			return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(explicit))
		}
		cp := make([]byte, 32)
		copy(cp, explicit)
		return cp, nil
	}

	kekPath := filepath.Join(dir, kekFileName)
	raw, err := os.ReadFile(kekPath)
	if err == nil {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("corrupt local KEK at %s", kekPath)
		}
		return decoded, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read local KEK: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate local KEK: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	tmp := kekPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write local KEK: %w", err)
	}
	if err := os.Rename(tmp, kekPath); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("commit local KEK: %w", err)
	}
	slog.Warn("CUBEPOD_SECRETS_KEY unset; generated local secret encryption key", "path", kekPath)
	return key, nil
}

func (s *Store) loadInitial(ctx context.Context) error {
	records, err := s.backend.LoadAll(ctx)
	if err != nil {
		return err
	}
	for i := range records {
		cp := records[i]
		if cp.ID == "" {
			continue
		}
		s.byID[cp.ID] = &cp
		s.byTenName[tenNameKey(cp.TenantHash, cp.Name)] = cp.ID
	}
	return nil
}

// SetSyncCallbacks configures callbacks invoked when Sync observes updates or deletions from other replicas.
func (s *Store) SetSyncCallbacks(onUpdate func(tenantHash, secretID string, value SecretValue), onRevoke func(tenantHash, secretID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onUpdate = onUpdate
	s.onRevoke = onRevoke
}

type syncedUpdate struct {
	tenantHash string
	secretID   string
	value      SecretValue
}

type syncedRevoke struct {
	tenantHash string
	secretID   string
}

// Sync refreshes the in-memory store from the underlying Backend and invokes
// update/revoke callbacks for any rotations or deletions made by other replicas.
func (s *Store) Sync(ctx context.Context) error {
	if s.backend == nil {
		return nil
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	seqBefore := s.writeSeq.Load()
	records, err := s.backend.LoadAll(ctx)
	if err != nil {
		return err
	}

	nextByID := make(map[string]*StoredSecret, len(records))
	nextByTenName := make(map[string]string, len(records))
	for i := range records {
		cp := records[i]
		if cp.ID == "" {
			continue
		}
		nextByID[cp.ID] = &cp
		nextByTenName[tenNameKey(cp.TenantHash, cp.Name)] = cp.ID
	}

	s.mu.Lock()
	if s.writeSeq.Load() != seqBefore {
		s.mu.Unlock()
		return nil
	}
	var updates []syncedUpdate
	var revokes []syncedRevoke
	for id, oldRec := range s.byID {
		newRec, exists := nextByID[id]
		if !exists {
			revokes = append(revokes, syncedRevoke{tenantHash: oldRec.TenantHash, secretID: oldRec.ID})
			continue
		}
		if newRec.Version != oldRec.Version || newRec.Value != oldRec.Value {
			updates = append(updates, syncedUpdate{
				tenantHash: newRec.TenantHash,
				secretID:   newRec.ID,
				value:      SecretValue(newRec.Value),
			})
		}
	}
	s.byID = nextByID
	s.byTenName = nextByTenName
	onUpdate := s.onUpdate
	onRevoke := s.onRevoke
	s.mu.Unlock()

	for _, u := range updates {
		if onUpdate != nil {
			onUpdate(u.tenantHash, u.secretID, u.value)
		}
	}
	for _, r := range revokes {
		if onRevoke != nil {
			onRevoke(r.tenantHash, r.secretID)
		}
	}
	return nil
}

// StartSync runs a periodic Sync loop in the background until ctx is cancelled.
func (s *Store) StartSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncCtx, cancel := context.WithTimeout(ctx, interval)
				if err := s.Sync(syncCtx); err != nil && ctx.Err() == nil {
					slog.Warn("E2B secret store sync failed", "err", err)
				}
				cancel()
			}
		}
	}()
}

func tenNameKey(tenantHash, name string) string {
	return tenantHash + "\x00" + name
}

func validateNameAndValue(name, value string, checkName bool) error {
	if checkName && !validSecretName.MatchString(name) {
		return ErrInvalidName
	}
	if value == "" || len(value) > MaxSecretValueBytes {
		return ErrInvalidValue
	}
	return nil
}

func requireTenantHash(apiKey string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", ErrUnauthorized
	}
	return TenantHash(apiKey), nil
}

func newSecretID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sec_" + hex.EncodeToString(b[:]), nil
}

func toInfo(rec *StoredSecret) SecretInfo {
	updated := rec.CreatedAt
	if rec.UpdatedAt != nil {
		updated = *rec.UpdatedAt
	}
	return SecretInfo{
		ID:        rec.ID,
		Name:      rec.Name,
		Version:   rec.Version,
		Metadata:  map[string]string{},
		CreatedAt: rec.CreatedAt,
		UpdatedAt: updated,
	}
}

// Create stores a new secret for the tenant identified by apiKey.
func (s *Store) Create(apiKey, name, value string) (info SecretInfo, err error) {
	defer s.metrics.create.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}
	if err := validateNameAndValue(name, value, true); err != nil {
		return SecretInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byTenName[tenNameKey(tenant, name)]; exists {
		return SecretInfo{}, ErrExists
	}

	count := 0
	for _, rec := range s.byID {
		if rec.TenantHash == tenant {
			count++
		}
	}
	if count >= s.maxPerTen {
		return SecretInfo{}, ErrLimitExceeded
	}

	id, err := newSecretID()
	if err != nil {
		return SecretInfo{}, err
	}

	rec := &StoredSecret{
		TenantHash: tenant,
		ID:         id,
		Name:       name,
		Version:    1,
		CreatedAt:  s.nowFn(),
		Value:      value,
	}
	s.byID[id] = rec
	s.byTenName[tenNameKey(tenant, name)] = id

	if err := s.backend.Put(context.Background(), *rec); err != nil {
		delete(s.byID, id)
		delete(s.byTenName, tenNameKey(tenant, name))
		return SecretInfo{}, err
	}
	s.writeSeq.Add(1)
	return toInfo(rec), nil
}

// List returns paginated secret metadata (never values) for the tenant.
func (s *Store) List(apiKey string, limit int, nextToken string) (items []SecretInfo, next string, err error) {
	defer s.metrics.list.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > DefaultListLimit {
		limit = DefaultListLimit
	}

	offset := 0
	if nextToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(nextToken)
		if err != nil {
			return nil, "", ErrInvalidCursor
		}
		parsed, err := strconv.Atoi(string(raw))
		if err != nil || parsed < 0 {
			return nil, "", ErrInvalidCursor
		}
		offset = parsed
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, rec := range s.byID {
		if rec.TenantHash == tenant {
			items = append(items, toInfo(rec))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	if offset >= len(items) {
		return []SecretInfo{}, "", nil
	}
	end := offset + limit
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	} else {
		end = len(items)
	}
	return items[offset:end], next, nil
}

func (s *Store) findLocked(tenant, idOrName string) (*StoredSecret, bool) {
	if rec, ok := s.byID[idOrName]; ok && rec.TenantHash == tenant {
		return rec, true
	}
	if id, ok := s.byTenName[tenNameKey(tenant, idOrName)]; ok {
		if rec, ok := s.byID[id]; ok && rec.TenantHash == tenant {
			return rec, true
		}
	}
	return nil, false
}

// Get returns secret metadata (never value) for secretID or name owned by the tenant.
func (s *Store) Get(apiKey, secretID string) (info SecretInfo, err error) {
	defer s.metrics.get.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.findLocked(tenant, secretID)
	if !ok {
		return SecretInfo{}, ErrNotFound
	}
	return toInfo(rec), nil
}

// Update rotates an existing secret's value and increments its version.
func (s *Store) Update(apiKey, secretID, value string) (info SecretInfo, err error) {
	defer s.metrics.update.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}
	if err := validateNameAndValue("", value, false); err != nil {
		return SecretInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.findLocked(tenant, secretID)
	if !ok {
		return SecretInfo{}, ErrNotFound
	}

	prevVal := rec.Value
	prevVer := rec.Version
	prevUpd := rec.UpdatedAt

	now := s.nowFn()
	rec.Value = value
	rec.Version++
	rec.UpdatedAt = &now

	if err := s.backend.Put(context.Background(), *rec); err != nil {
		rec.Value = prevVal
		rec.Version = prevVer
		rec.UpdatedAt = prevUpd
		return SecretInfo{}, err
	}
	s.writeSeq.Add(1)
	return toInfo(rec), nil
}

// Delete removes a secret owned by the tenant.
func (s *Store) Delete(apiKey, secretID string) (info SecretInfo, err error) {
	defer s.metrics.delete.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.findLocked(tenant, secretID)
	if !ok {
		return SecretInfo{}, ErrNotFound
	}
	info = toInfo(rec)
	delete(s.byID, rec.ID)
	delete(s.byTenName, tenNameKey(tenant, rec.Name))

	if err := s.backend.Delete(context.Background(), rec.ID); err != nil {
		s.byID[rec.ID] = rec
		s.byTenName[tenNameKey(tenant, rec.Name)] = rec.ID
		return SecretInfo{}, err
	}
	s.writeSeq.Add(1)
	return info, nil
}

// ResolveByName looks up a secret by name for the tenant and returns its metadata and redacted SecretValue.
func (s *Store) ResolveByName(apiKey, name string) (info SecretInfo, val SecretValue, err error) {
	defer s.metrics.resolve.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	id, ok := s.byTenName[tenNameKey(tenant, name)]
	if !ok {
		return SecretInfo{}, "", ErrNotFound
	}
	rec, ok := s.byID[id]
	if !ok || rec.TenantHash != tenant {
		return SecretInfo{}, "", ErrNotFound
	}
	return toInfo(rec), SecretValue(rec.Value), nil
}

// ResolveByID looks up a secret by ID for the tenant and returns its metadata and redacted SecretValue.
func (s *Store) ResolveByID(apiKey, secretID string) (info SecretInfo, val SecretValue, err error) {
	defer s.metrics.resolve.track(&err)()

	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.byID[secretID]
	if !ok || rec.TenantHash != tenant {
		return SecretInfo{}, "", ErrNotFound
	}
	return toInfo(rec), SecretValue(rec.Value), nil
}

// ResolveByTenantID looks up a secret by pre-hashed tenantHash and secretID
// (used for HA Vault re-hydration from PodBox SecretRefBindings).
func (s *Store) ResolveByTenantID(tenantHash, secretID string) (info SecretInfo, val SecretValue, err error) {
	defer s.metrics.resolve.track(&err)()

	if strings.TrimSpace(tenantHash) == "" || strings.TrimSpace(secretID) == "" {
		return SecretInfo{}, "", ErrNotFound
	}

	s.mu.RLock()
	rec, ok := s.byID[secretID]
	if ok && rec.TenantHash == tenantHash {
		info = toInfo(rec)
		val = SecretValue(rec.Value)
		s.mu.RUnlock()
		return info, val, nil
	}
	s.mu.RUnlock()

	return SecretInfo{}, "", ErrNotFound
}

// Count returns the total number of secrets stored across all tenants.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// OperationsSnapshot returns a thread-safe copy of secret operation counts by operation and status ("ok"/"error").
func (s *Store) OperationsSnapshot() map[string]map[string]uint64 {
	return s.metrics.snapshot()
}

// OperationsLatencySnapshot returns a thread-safe copy of per-operation latency histograms
// (create, list, get, update, delete, resolve).
func (s *Store) OperationsLatencySnapshot() map[string]LatencySnapshot {
	return s.metrics.latencySnapshot()
}
