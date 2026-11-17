package egress

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	// ErrUnknownPlaceholder is returned when the given placeholder does not exist in the vault.
	ErrUnknownPlaceholder = errors.New("unknown placeholder")
	// ErrSandboxMismatch is returned when the requesting sandboxID does not match the bound sandboxID.
	ErrSandboxMismatch = errors.New("sandbox mismatch")
	// ErrHostNotAllowed is returned when the destination SNI host is not allowed by the secret binding policy.
	ErrHostNotAllowed = errors.New("host not allowed")
	// ErrHeaderNotAllowed is returned when the target HTTP header is not allowed by the secret binding policy.
	ErrHeaderNotAllowed = errors.New("header not allowed")
)

// SecretBinding represents an opaque secret bound to a sandbox, destination hosts, and headers.
type SecretBinding struct {
	SandboxID      string
	TenantHash     string
	SecretID       string
	EnvName        string
	RawSecret      string
	AllowedHosts   []string
	AllowedHeaders []string
}

type vaultKey struct {
	sandboxID   string
	placeholder string
}

// Vault manages in-memory secret bindings and zero-trust placeholder exchanges.
type Vault struct {
	mu       sync.RWMutex
	bindings map[vaultKey]*SecretBinding
}

// NewVault constructs a new Secret Vault.
func NewVault() *Vault {
	return &Vault{
		bindings: make(map[vaultKey]*SecretBinding),
	}
}

// DefaultPlaceholderPrefix is the prefix for opaque sandbox secret references.
const DefaultPlaceholderPrefix = "cpodref_"

// BindSecret mints a unique cpodref_<32-char-base32> placeholder bound to the given sandbox,
// environment variable name, raw secret, allowed SNI hosts, and allowed HTTP headers.
func (v *Vault) BindSecret(sandboxID, envName, rawSecret string, allowedHosts, allowedHeaders []string) (string, error) {
	return v.BindManagedSecret(sandboxID, "", "", envName, rawSecret, allowedHosts, allowedHeaders)
}

// BindManagedSecret mints a unique cpodref_<32-char-base32> placeholder linked to an optional
// durable tenant secret (tenantHash, secretID) so rotations and revocations propagate in real time.
func (v *Vault) BindManagedSecret(sandboxID, tenantHash, secretID, envName, rawSecret string, allowedHosts, allowedHeaders []string) (string, error) {
	if sandboxID == "" {
		return "", errors.New("sandboxID cannot be empty")
	}
	if rawSecret == "" {
		return "", errors.New("rawSecret cannot be empty")
	}

	// Generate 20 random bytes -> 32 characters in unpadded Base32
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate random bytes for placeholder: %w", err)
	}

	placeholder := DefaultPlaceholderPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)

	hostsCopy := make([]string, len(allowedHosts))
	copy(hostsCopy, allowedHosts)

	headersCopy := make([]string, len(allowedHeaders))
	copy(headersCopy, allowedHeaders)

	v.mu.Lock()
	if v.bindings == nil {
		v.bindings = make(map[vaultKey]*SecretBinding)
	}
	v.bindings[vaultKey{sandboxID: sandboxID, placeholder: placeholder}] = &SecretBinding{
		SandboxID:      sandboxID,
		TenantHash:     tenantHash,
		SecretID:       secretID,
		EnvName:        envName,
		RawSecret:      rawSecret,
		AllowedHosts:   hostsCopy,
		AllowedHeaders: headersCopy,
	}
	v.mu.Unlock()

	return placeholder, nil
}

// Exchange validates the requesting sandbox identity, destination SNI host, and HTTP header name
// with fail-closed semantics, returning the real secret value upon verification.
func (v *Vault) Exchange(sandboxID, placeholder, sniHost, headerName string) (string, error) {
	v.mu.RLock()
	ptr, ok := v.bindings[vaultKey{sandboxID: sandboxID, placeholder: placeholder}]
	if !ok {
		for k := range v.bindings {
			if k.placeholder == placeholder {
				v.mu.RUnlock()
				return "", ErrSandboxMismatch
			}
		}
		v.mu.RUnlock()
		return "", ErrUnknownPlaceholder
	}
	binding := *ptr
	v.mu.RUnlock()

	// 1. Sandbox identity verification
	if binding.SandboxID != sandboxID {
		return "", ErrSandboxMismatch
	}

	// 2. SNI host verification (fail-closed)
	if sniHost == "" || len(binding.AllowedHosts) == 0 {
		return "", ErrHostNotAllowed
	}
	hostAllowed := false
	for _, pattern := range binding.AllowedHosts {
		if MatchHost(sniHost, pattern) {
			hostAllowed = true
			break
		}
	}
	if !hostAllowed {
		return "", ErrHostNotAllowed
	}

	// 3. Header name verification (case-insensitive, fail-closed)
	if headerName == "" || len(binding.AllowedHeaders) == 0 {
		return "", ErrHeaderNotAllowed
	}
	headerAllowed := false
	for _, h := range binding.AllowedHeaders {
		if h == "*" || strings.EqualFold(h, headerName) {
			headerAllowed = true
			break
		}
	}
	if !headerAllowed {
		return "", ErrHeaderNotAllowed
	}

	return binding.RawSecret, nil
}

// RevokeSandbox immediately revokes all secret bindings associated with sandboxID.
func (v *Vault) RevokeSandbox(sandboxID string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for k := range v.bindings {
		if k.sandboxID == sandboxID {
			delete(v.bindings, k)
		}
	}
}

// RevokeSecret revokes a specific secret binding by placeholder.
func (v *Vault) RevokeSecret(placeholder string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k := range v.bindings {
		if k.placeholder == placeholder {
			delete(v.bindings, k)
		}
	}
}

// UpdateSecretByRef rotates RawSecret for all active bindings referencing (tenantHash, secretID).
func (v *Vault) UpdateSecretByRef(tenantHash, secretID, newRawSecret string) int {
	if tenantHash == "" || secretID == "" || newRawSecret == "" {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	updated := 0
	for k, binding := range v.bindings {
		if binding.TenantHash == tenantHash && binding.SecretID == secretID {
			cp := *binding
			cp.RawSecret = newRawSecret
			v.bindings[k] = &cp
			updated++
		}
	}
	return updated
}

// RevokeSecretByRef revokes all active bindings referencing (tenantHash, secretID).
func (v *Vault) RevokeSecretByRef(tenantHash, secretID string) int {
	if tenantHash == "" || secretID == "" {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	revoked := 0
	for k, binding := range v.bindings {
		if binding.TenantHash == tenantHash && binding.SecretID == secretID {
			delete(v.bindings, k)
			revoked++
		}
	}
	return revoked
}

// Count returns the total number of active secret bindings in the vault.
func (v *Vault) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.bindings)
}
