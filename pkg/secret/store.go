// Package secret implements the tenant-isolated, AES-256-GCM encrypted
// durable store for official E2B /secrets management (CEP-0027).
package secret

import (
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
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Version   int        `json:"version"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// Config configures the encrypted durable secret store.
type Config struct {
	// EncryptionKey is an optional 32-byte AES-256 key. If empty, a key is
	// loaded from or generated at <dir>/.kek with 0600 permissions.
	EncryptionKey []byte
	// MaxSecretsPerTenant caps secrets per tenant (default 100).
	MaxSecretsPerTenant int
}

type storedSecret struct {
	TenantHash string     `json:"tenantHash"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	Value      string     `json:"value"`
}

type encryptedEnvelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type diskState struct {
	Secrets []storedSecret `json:"secrets"`
}

// Store manages tenant-isolated E2B secrets encrypted at rest with AES-256-GCM.
type Store struct {
	mu        sync.RWMutex
	dir       string
	key       []byte
	maxPerTen int
	byID      map[string]*storedSecret
	byTenName map[string]string // tenantHash + "\x00" + name -> id
	nowFn     func() time.Time
}

// TenantHash derives a stable opaque tenant identifier from an API key.
func TenantHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

// Open opens or initializes an encrypted secret store under dir.
func Open(dir string, cfg Config) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("secret store dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create secret dir: %w", err)
	}

	key, err := resolveKEK(dir, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	maxPerTen := cfg.MaxSecretsPerTenant
	if maxPerTen <= 0 {
		maxPerTen = DefaultMaxSecretsPerTenant
	}

	st := &Store{
		dir:       dir,
		key:       key,
		maxPerTen: maxPerTen,
		byID:      make(map[string]*storedSecret),
		byTenName: make(map[string]string),
		nowFn:     func() time.Time { return time.Now().UTC() },
	}
	if err := st.load(); err != nil {
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

func (s *Store) load() error {
	path := filepath.Join(s.dir, secretsFileName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read secrets store: %w", err)
	}

	var env encryptedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode secrets envelope: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return fmt.Errorf("decode secrets nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return fmt.Errorf("decode secrets ciphertext: %w", err)
	}

	block, err := aes.NewCipher(s.key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("decrypt secrets store: %w", err)
	}

	var state diskState
	if err := json.Unmarshal(plain, &state); err != nil {
		return fmt.Errorf("decode secrets state: %w", err)
	}
	for i := range state.Secrets {
		rec := state.Secrets[i]
		cp := rec
		s.byID[cp.ID] = &cp
		s.byTenName[tenNameKey(cp.TenantHash, cp.Name)] = cp.ID
	}
	return nil
}

func (s *Store) saveLocked() error {
	state := diskState{
		Secrets: make([]storedSecret, 0, len(s.byID)),
	}
	for _, rec := range s.byID {
		state.Secrets = append(state.Secrets, *rec)
	}
	sort.Slice(state.Secrets, func(i, j int) bool {
		return state.Secrets[i].ID < state.Secrets[j].ID
	})

	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}

	block, err := aes.NewCipher(s.key)
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

	path := filepath.Join(s.dir, secretsFileName)
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

func toInfo(rec *storedSecret) SecretInfo {
	var updatedCopy *time.Time
	if rec.UpdatedAt != nil {
		t := *rec.UpdatedAt
		updatedCopy = &t
	}
	return SecretInfo{
		ID:        rec.ID,
		Name:      rec.Name,
		Version:   rec.Version,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: updatedCopy,
	}
}

// Create stores a new secret for the tenant identified by apiKey.
func (s *Store) Create(apiKey, name, value string) (SecretInfo, error) {
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

	rec := &storedSecret{
		TenantHash: tenant,
		ID:         id,
		Name:       name,
		Version:    1,
		CreatedAt:  s.nowFn(),
		Value:      value,
	}
	s.byID[id] = rec
	s.byTenName[tenNameKey(tenant, name)] = id

	if err := s.saveLocked(); err != nil {
		delete(s.byID, id)
		delete(s.byTenName, tenNameKey(tenant, name))
		return SecretInfo{}, err
	}
	return toInfo(rec), nil
}

// List returns paginated secret metadata (never values) for the tenant.
func (s *Store) List(apiKey string, limit int, nextToken string) ([]SecretInfo, string, error) {
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

	var items []SecretInfo
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
	var outNext string
	if end < len(items) {
		outNext = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	} else {
		end = len(items)
	}
	return items[offset:end], outNext, nil
}

// Get returns secret metadata (never value) for secretID owned by the tenant.
func (s *Store) Get(apiKey, secretID string) (SecretInfo, error) {
	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.byID[secretID]
	if !ok || rec.TenantHash != tenant {
		return SecretInfo{}, ErrNotFound
	}
	return toInfo(rec), nil
}

// Update rotates an existing secret's value and increments its version.
func (s *Store) Update(apiKey, secretID, value string) (SecretInfo, error) {
	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}
	if err := validateNameAndValue("", value, false); err != nil {
		return SecretInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[secretID]
	if !ok || rec.TenantHash != tenant {
		return SecretInfo{}, ErrNotFound
	}

	prevVal := rec.Value
	prevVer := rec.Version
	prevUpd := rec.UpdatedAt

	now := s.nowFn()
	rec.Value = value
	rec.Version++
	rec.UpdatedAt = &now

	if err := s.saveLocked(); err != nil {
		rec.Value = prevVal
		rec.Version = prevVer
		rec.UpdatedAt = prevUpd
		return SecretInfo{}, err
	}
	return toInfo(rec), nil
}

// Delete removes a secret owned by the tenant.
func (s *Store) Delete(apiKey, secretID string) (SecretInfo, error) {
	tenant, err := requireTenantHash(apiKey)
	if err != nil {
		return SecretInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[secretID]
	if !ok || rec.TenantHash != tenant {
		return SecretInfo{}, ErrNotFound
	}
	info := toInfo(rec)
	delete(s.byID, secretID)
	delete(s.byTenName, tenNameKey(tenant, rec.Name))

	if err := s.saveLocked(); err != nil {
		s.byID[secretID] = rec
		s.byTenName[tenNameKey(tenant, rec.Name)] = secretID
		return SecretInfo{}, err
	}
	return info, nil
}

// ResolveByName looks up a secret by name for the tenant and returns its metadata and redacted SecretValue.
func (s *Store) ResolveByName(apiKey, name string) (SecretInfo, SecretValue, error) {
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
func (s *Store) ResolveByID(apiKey, secretID string) (SecretInfo, SecretValue, error) {
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

// Count returns the total number of secrets stored across all tenants.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}
