package volume

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultHTTPKMSTimeout = 10 * time.Second
	maxKMSResponseBytes   = 1 << 20 // 1 MiB
	localKEKAAD           = "cubepod-e2b-vol-kek-wrap-v1"
	envelopeTokenAADPref  = "cubepod-e2b-vol-token-v1\x00"
)

// ErrKMS indicates a KMS configuration, wrapping, or unwrapping failure.
var ErrKMS = errors.New("volume kms error")

// KMSProvider wraps and unwraps per-volume data encryption keys (DEKs) for
// envelope encryption of volume content tokens in meta.json.
type KMSProvider interface {
	KeyID() string
	Wrap(ctx context.Context, plaintext []byte) ([]byte, error)
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
}

// ParseKEK parses a 32-byte AES-256 key-encryption key from either a 64-char
// hex string or a base64-encoded string. Raw key material is never echoed in errors.
func ParseKEK(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty KEK", ErrKMS)
	}
	if len(trimmed) == 64 {
		if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(trimmed); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%w: KEK must be a 32-byte base64 or 64-char hex key", ErrKMS)
}

// LocalKEKProvider implements KMSProvider using local AES-256-GCM key wrapping.
type LocalKEKProvider struct {
	keyID string
	aead  cipher.AEAD
}

// NewLocalKEKProvider constructs an AES-256-GCM LocalKEKProvider from a 32-byte key.
func NewLocalKEKProvider(keyID string, key []byte) (*LocalKEKProvider, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: local KEK must be 32 bytes (got %d)", ErrKMS, len(key))
	}
	keyCopy := make([]byte, 32)
	copy(keyCopy, key)
	defer clear(keyCopy)

	block, err := aes.NewCipher(keyCopy)
	if err != nil {
		return nil, fmt.Errorf("%w: init AES cipher: %v", ErrKMS, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: init AES-GCM: %v", ErrKMS, err)
	}
	id := strings.TrimSpace(keyID)
	if id == "" {
		sum := sha256.Sum256(append([]byte("cubepod-e2b-volumes-kek-id\x00"), keyCopy...))
		id = "local:" + hex.EncodeToString(sum[:8])
	}
	return &LocalKEKProvider{
		keyID: id,
		aead:  aead,
	}, nil
}

// NewLocalKEKProviderFromString constructs a LocalKEKProvider from a base64 or 64-hex KEK string.
func NewLocalKEKProviderFromString(keyID, rawKey string) (*LocalKEKProvider, error) {
	key, err := ParseKEK(rawKey)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return NewLocalKEKProvider(keyID, key)
}

// KeyID returns the identifier of this KEK.
func (p *LocalKEKProvider) KeyID() string { return p.keyID }

// String redacts internal key state.
func (p *LocalKEKProvider) String() string { return fmt.Sprintf("LocalKEKProvider(%s)", p.keyID) }

// GoString redacts internal key state.
func (p *LocalKEKProvider) GoString() string { return p.String() }

// LogValue redacts internal key state in structured logs.
func (p *LocalKEKProvider) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Wrap encrypts plaintext (typically a 32-byte DEK) under the local AES-256-GCM KEK.
func (p *LocalKEKProvider) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("%w: cannot wrap empty plaintext", ErrKMS)
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: generate nonce: %v", ErrKMS, err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+p.aead.Overhead())
	out = append(out, nonce...)
	out = p.aead.Seal(out, nonce, plaintext, []byte(localKEKAAD))
	return out, nil
}

// Unwrap decrypts wrapped key material under the local AES-256-GCM KEK.
func (p *LocalKEKProvider) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	ns := p.aead.NonceSize()
	if len(wrapped) <= ns {
		return nil, fmt.Errorf("%w: wrapped payload too short", ErrKMS)
	}
	pt, err := p.aead.Open(nil, wrapped[:ns], wrapped[ns:], []byte(localKEKAAD))
	if err != nil {
		return nil, fmt.Errorf("%w: unwrap failed", ErrKMS)
	}
	return pt, nil
}

// HTTPKMSConfig configures HTTPKMSProvider for external KMS wrap/unwrap endpoints.
type HTTPKMSConfig struct {
	// URL is the base KMS endpoint URL (POST <URL>/wrap and POST <URL>/unwrap).
	// Remote endpoints must use https:// unless AllowInsecureHTTP is explicitly set;
	// loopback addresses (127.0.0.1, ::1, localhost) allow http:// for local KMS sidecars.
	URL string
	// BearerToken is an optional Authorization Bearer token (never logged).
	BearerToken string
	// KeyID identifies the external KMS key (defaults to "http-kms").
	KeyID string
	// Timeout bounds each KMS HTTP call (defaults to 10s).
	Timeout time.Duration
	// TLSConfig customizes TLS verification (enforces TLS 1.2+ minimum).
	TLSConfig *tls.Config
	// HTTPClient optionally overrides the underlying transport; redirect blocking and
	// timeout hardening are still enforced.
	HTTPClient *http.Client
	// AllowInsecureHTTP permits plaintext http:// to non-loopback hosts (test-only).
	AllowInsecureHTTP bool
}

// HTTPKMSProvider implements KMSProvider over an external HTTP/HTTPS KMS service.
type HTTPKMSProvider struct {
	baseURL     string
	keyID       string
	bearerToken string
	client      *http.Client
}

// NewHTTPKMSProvider validates cfg and builds a hardened HTTP KMS client.
func NewHTTPKMSProvider(cfg HTTPKMSConfig) (*HTTPKMSProvider, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: KMS URL is required", ErrKMS)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%w: invalid KMS URL", ErrKMS)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("%w: unsupported KMS URL scheme %q", ErrKMS, parsed.Scheme)
	}
	if scheme == "http" && !cfg.AllowInsecureHTTP && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("%w: remote KMS URL must use https:// (got host %q)", ErrKMS, parsed.Hostname())
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: credentials must not be embedded in KMS URL", ErrKMS)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPKMSTimeout
	}

	var tlsCfg *tls.Config
	if cfg.TLSConfig != nil {
		tlsCfg = cfg.TLSConfig.Clone()
	} else {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}

	var client *http.Client
	if cfg.HTTPClient != nil {
		cp := *cfg.HTTPClient
		if cp.Timeout <= 0 {
			cp.Timeout = timeout
		}
		cp.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &cp
	} else {
		transport := &http.Transport{
			TLSClientConfig:       tlsCfg,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
		}
		client = &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	keyID := strings.TrimSpace(cfg.KeyID)
	if keyID == "" {
		keyID = "http-kms"
	}

	return &HTTPKMSProvider{
		baseURL:     strings.TrimRight(parsed.String(), "/"),
		keyID:       keyID,
		bearerToken: strings.TrimSpace(cfg.BearerToken),
		client:      client,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// KeyID returns the configured external KMS key identifier.
func (p *HTTPKMSProvider) KeyID() string { return p.keyID }

// String redacts any bearer token.
func (p *HTTPKMSProvider) String() string {
	return fmt.Sprintf("HTTPKMSProvider(url=%s,keyID=%s)", p.baseURL, p.keyID)
}

// GoString redacts any bearer token.
func (p *HTTPKMSProvider) GoString() string { return p.String() }

// LogValue redacts any bearer token in structured logs.
func (p *HTTPKMSProvider) LogValue() slog.Value { return slog.StringValue(p.String()) }

type kmsWrapRequest struct {
	KeyID     string `json:"keyId"`
	Plaintext string `json:"plaintext"`
}

type kmsWrapResponse struct {
	KeyID      string `json:"keyId,omitempty"`
	Ciphertext string `json:"ciphertext"`
	Wrapped    string `json:"wrapped,omitempty"`
}

type kmsUnwrapRequest struct {
	KeyID      string `json:"keyId"`
	Ciphertext string `json:"ciphertext"`
}

type kmsUnwrapResponse struct {
	Plaintext string `json:"plaintext"`
}

// Wrap sends plaintext (typically a 32-byte DEK) to POST <URL>/wrap and returns wrapped bytes.
func (p *HTTPKMSProvider) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("%w: cannot wrap empty plaintext", ErrKMS)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reqPayload := kmsWrapRequest{
		KeyID:     p.keyID,
		Plaintext: base64.StdEncoding.EncodeToString(plaintext),
	}
	var respPayload kmsWrapResponse
	if err := p.doJSON(ctx, "/wrap", reqPayload, &respPayload); err != nil {
		return nil, err
	}
	rawCipher := respPayload.Ciphertext
	if rawCipher == "" {
		rawCipher = respPayload.Wrapped
	}
	if rawCipher == "" {
		return nil, fmt.Errorf("%w: KMS wrap response missing ciphertext", ErrKMS)
	}
	wrapped, err := base64.StdEncoding.DecodeString(rawCipher)
	if err != nil || len(wrapped) == 0 {
		return nil, fmt.Errorf("%w: invalid base64 ciphertext from KMS wrap", ErrKMS)
	}
	return wrapped, nil
}

// Unwrap sends wrapped bytes to POST <URL>/unwrap and returns the decrypted plaintext bytes.
func (p *HTTPKMSProvider) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	if len(wrapped) == 0 {
		return nil, fmt.Errorf("%w: cannot unwrap empty ciphertext", ErrKMS)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reqPayload := kmsUnwrapRequest{
		KeyID:      p.keyID,
		Ciphertext: base64.StdEncoding.EncodeToString(wrapped),
	}
	var respPayload kmsUnwrapResponse
	if err := p.doJSON(ctx, "/unwrap", reqPayload, &respPayload); err != nil {
		return nil, err
	}
	if respPayload.Plaintext == "" {
		return nil, fmt.Errorf("%w: KMS unwrap response missing plaintext", ErrKMS)
	}
	pt, err := base64.StdEncoding.DecodeString(respPayload.Plaintext)
	if err != nil || len(pt) == 0 {
		return nil, fmt.Errorf("%w: invalid base64 plaintext from KMS unwrap", ErrKMS)
	}
	return pt, nil
}

func (p *HTTPKMSProvider) doJSON(ctx context.Context, suffix string, reqBody any, respBody any) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("%w: marshal request: %v", ErrKMS, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+suffix, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: build KMS request: %v", ErrKMS, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearerToken)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// Sanitize error so URL/header details or tokens never leak unexpectedly.
		return fmt.Errorf("%w: KMS HTTP %s request failed: %v", ErrKMS, suffix, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxKMSResponseBytes))
	if readErr != nil {
		return fmt.Errorf("%w: read KMS %s response: %v", ErrKMS, suffix, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: KMS %s returned status %d", ErrKMS, suffix, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, respBody); err != nil {
		return fmt.Errorf("%w: decode KMS %s response: %v", ErrKMS, suffix, err)
	}
	return nil
}

type tokenEnvelope struct {
	Version    int    `json:"v"`
	WrappedDEK string `json:"dek"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ct"`
}

// sealVolumeToken performs envelope encryption on a volume content token:
// 1. Generates a random 32-byte DEK.
// 2. Encrypts token with AES-256-GCM bound to volumeID as AAD.
// 3. Wraps DEK using kms.Wrap(ctx, dek).
func sealVolumeToken(ctx context.Context, kms KMSProvider, volumeID, plaintextToken string) (encryptedToken, kmsKeyID string, err error) {
	if kms == nil {
		return "", "", fmt.Errorf("%w: nil KMSProvider", ErrKMS)
	}
	if volumeID == "" || plaintextToken == "" {
		return "", "", fmt.Errorf("%w: volumeID and token are required", ErrKMS)
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return "", "", fmt.Errorf("%w: generate DEK: %v", ErrKMS, err)
	}
	defer clear(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", "", fmt.Errorf("%w: init DEK cipher: %v", ErrKMS, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("%w: init DEK GCM: %v", ErrKMS, err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("%w: generate DEK nonce: %v", ErrKMS, err)
	}
	aad := []byte(envelopeTokenAADPref + volumeID)
	ct := aead.Seal(nil, nonce, []byte(plaintextToken), aad)

	wrappedDEK, err := kms.Wrap(ctx, dek)
	if err != nil {
		return "", "", err
	}
	env := tokenEnvelope{
		Version:    1,
		WrappedDEK: base64.StdEncoding.EncodeToString(wrappedDEK),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}
	rawEnv, err := json.Marshal(env)
	if err != nil {
		return "", "", fmt.Errorf("%w: encode token envelope: %v", ErrKMS, err)
	}
	return base64.StdEncoding.EncodeToString(rawEnv), kms.KeyID(), nil
}

// openVolumeToken reverses sealVolumeToken, unwrapping the DEK via kms.Unwrap and
// verifying the AES-256-GCM AAD binding to volumeID.
func openVolumeToken(ctx context.Context, kms KMSProvider, volumeID, encryptedToken string) (string, error) {
	if kms == nil {
		return "", fmt.Errorf("%w: volume %s has encryptedToken but no KMSProvider is configured", ErrKMS, volumeID)
	}
	rawEnv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encryptedToken))
	if err != nil {
		return "", fmt.Errorf("%w: decode token envelope for volume %s", ErrKMS, volumeID)
	}
	var env tokenEnvelope
	if err := json.Unmarshal(rawEnv, &env); err != nil || env.Version != 1 {
		return "", fmt.Errorf("%w: invalid token envelope version for volume %s", ErrKMS, volumeID)
	}
	wrappedDEK, err := base64.StdEncoding.DecodeString(env.WrappedDEK)
	if err != nil || len(wrappedDEK) == 0 {
		return "", fmt.Errorf("%w: invalid wrapped DEK in volume %s", ErrKMS, volumeID)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return "", fmt.Errorf("%w: invalid envelope nonce in volume %s", ErrKMS, volumeID)
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("%w: invalid envelope ciphertext in volume %s", ErrKMS, volumeID)
	}

	dek, err := kms.Unwrap(ctx, wrappedDEK)
	if err != nil {
		return "", err
	}
	defer clear(dek)
	if len(dek) != 32 {
		return "", fmt.Errorf("%w: unwrapped DEK must be 32 bytes (got %d)", ErrKMS, len(dek))
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", fmt.Errorf("%w: init DEK cipher: %v", ErrKMS, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%w: init DEK GCM: %v", ErrKMS, err)
	}
	if len(nonce) != aead.NonceSize() {
		return "", fmt.Errorf("%w: invalid DEK nonce size in volume %s", ErrKMS, volumeID)
	}
	aad := []byte(envelopeTokenAADPref + volumeID)
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", fmt.Errorf("%w: envelope authentication failed for volume %s", ErrKMS, volumeID)
	}
	return string(pt), nil
}
