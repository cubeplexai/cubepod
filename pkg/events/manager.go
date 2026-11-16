// Package events implements tenant-isolated Sandbox Lifecycle Events and
// SSRF-safe, HMAC-SHA256 signed Webhook delivery (CEP-0028).
package events

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// DefaultMaxWebhooksPerTenant caps webhooks per API key tenant.
	DefaultMaxWebhooksPerTenant = 32
	// DefaultMaxEventsPerTenant caps retained lifecycle events per tenant.
	DefaultMaxEventsPerTenant = 1000
	// DefaultListLimit is the default page size for ListEvents.
	DefaultListLimit = 100
	// DefaultQueueSize bounds async webhook delivery jobs.
	DefaultQueueSize = 256

	webhooksFileName = "webhooks.enc.json"
	eventsFileName   = "events.json"
	kekFileName      = ".kek"
)

// Canonical sandbox lifecycle event types (E2B v1 compatible).
const (
	EventCreated = "sandbox.lifecycle.created"
	EventPaused  = "sandbox.lifecycle.paused"
	EventResumed = "sandbox.lifecycle.resumed"
	EventUpdated = "sandbox.lifecycle.updated"
	EventKilled  = "sandbox.lifecycle.killed"
	EventForked  = "sandbox.lifecycle.forked"
)

var (
	// ErrUnauthorized is returned when the API key is missing.
	ErrUnauthorized = errors.New("API key is required")
	// ErrInvalidWebhook is returned when webhook name, URL, or events are invalid.
	ErrInvalidWebhook = errors.New("invalid webhook configuration")
	// ErrNotFound is returned when the webhook does not exist or belongs to another tenant.
	ErrNotFound = errors.New("webhook not found")
	// ErrLimitExceeded is returned when the tenant reaches MaxWebhooksPerTenant.
	ErrLimitExceeded = errors.New("tenant webhook limit exceeded")
	// ErrInvalidCursor is returned when nextToken cannot be decoded.
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	validEventTypes = map[string]string{
		EventCreated: "create",
		EventPaused:  "pause",
		EventResumed: "resume",
		EventUpdated: "update",
		EventKilled:  "kill",
		EventForked:  "fork",
	}
	validWebhookName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)
)

// ExecutionData carries resource and runtime duration metadata in eventData.execution.
type ExecutionData struct {
	StartedAt     string `json:"started_at"`
	VCPUCount     int    `json:"vcpu_count"`
	MemoryMB      int    `json:"memory_mb"`
	ExecutionTime int64  `json:"execution_time"`
}

// EventData carries optional E2B lifecycle context.
type EventData struct {
	PauseMode  string         `json:"pause_mode,omitempty"`
	ResumeMode string         `json:"resume_mode,omitempty"`
	Execution  *ExecutionData `json:"execution,omitempty"`
	Reason     string         `json:"reason,omitempty"`
}

// LifecycleEvent matches E2B's v1 Sandbox Lifecycle Webhook payload schema.
type LifecycleEvent struct {
	Version            string     `json:"version"`
	ID                 string     `json:"id"`
	Type               string     `json:"type"`
	Timestamp          time.Time  `json:"timestamp"`
	EventCategory      string     `json:"eventCategory"`
	EventLabel         string     `json:"eventLabel"`
	SandboxID          string     `json:"sandboxId"`
	SandboxTeamID      string     `json:"sandboxTeamId"`
	SandboxTemplateID  string     `json:"sandboxTemplateId"`
	SandboxBuildID     string     `json:"sandboxBuildId"`
	SandboxExecutionID string     `json:"sandboxExecutionId"`
	EventData          *EventData `json:"eventData"`
}

// WebhookInfo is the public metadata for a webhook subscription (never includes SignatureKey).
type WebhookInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// WebhookWithSecret is returned only on creation or explicit secret rotation.
type WebhookWithSecret struct {
	WebhookInfo
	SignatureKey string `json:"signatureKey,omitempty"`
}

// CreateWebhookInput defines parameters for RegisterWebhook.
type CreateWebhookInput struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// UpdateWebhookInput defines mutable fields for UpdateWebhook.
type UpdateWebhookInput struct {
	URL          *string  `json:"url,omitempty"`
	Events       []string `json:"events,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
	RotateSecret bool     `json:"rotateSecret,omitempty"`
}

// Config configures the events and webhook manager.
type Config struct {
	EncryptionKey        []byte
	MaxWebhooksPerTenant int
	MaxEventsPerTenant   int
	AllowPrivateIPs      bool
	DeliveryTimeout      time.Duration
	MaxRetries           int
	RetryBaseDelay       time.Duration
}

type storedWebhook struct {
	TenantHash   string    `json:"tenantHash"`
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Events       []string  `json:"events"`
	Enabled      bool      `json:"enabled"`
	SignatureKey string    `json:"signatureKey"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type storedEvent struct {
	TenantHash string         `json:"tenantHash"`
	Event      LifecycleEvent `json:"event"`
}

type encryptedEnvelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type webhookDiskState struct {
	Webhooks []storedWebhook `json:"webhooks"`
}

type eventDiskState struct {
	Events []storedEvent `json:"events"`
}

type deliveryJob struct {
	webhookID    string
	url          string
	signatureKey string
	event        LifecycleEvent
	payload      []byte
}

// Manager manages tenant-isolated lifecycle events and async signed webhook delivery.
type Manager struct {
	mu           sync.RWMutex
	saveMu       sync.Mutex
	closeMu      sync.RWMutex
	dir          string
	key          []byte
	maxWebhooks  int
	maxEvents    int
	allowPrivate bool
	maxRetries   int
	retryBase    time.Duration
	webhooks     map[string]*storedWebhook
	events       []storedEvent
	httpClient   *http.Client
	queue        chan deliveryJob
	stopCh       chan struct{}
	wg           sync.WaitGroup
	closed       atomic.Bool
	nowFn        func() time.Time

	EventsRecordedTotal     atomic.Uint64
	WebhookDeliveriesOK     atomic.Uint64
	WebhookDeliveriesFailed atomic.Uint64
	WebhookDeliveriesDrop   atomic.Uint64
}

// TenantHash derives the opaque tenant identifier from an API key.
func TenantHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

// Open opens or initializes the events and webhook manager under dir.
func Open(dir string, cfg Config) (*Manager, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("events dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create events dir: %w", err)
	}

	key, err := resolveKEK(dir, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	maxWh := cfg.MaxWebhooksPerTenant
	if maxWh <= 0 {
		maxWh = DefaultMaxWebhooksPerTenant
	}
	maxEv := cfg.MaxEventsPerTenant
	if maxEv <= 0 {
		maxEv = DefaultMaxEventsPerTenant
	}
	timeout := cfg.DeliveryTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	retryBase := cfg.RetryBaseDelay
	if retryBase <= 0 {
		retryBase = 250 * time.Millisecond
	}

	m := &Manager{
		dir:          dir,
		key:          key,
		maxWebhooks:  maxWh,
		maxEvents:    maxEv,
		allowPrivate: cfg.AllowPrivateIPs,
		maxRetries:   maxRetries,
		retryBase:    retryBase,
		webhooks:     make(map[string]*storedWebhook),
		httpClient:   buildSSRFClient(timeout, cfg.AllowPrivateIPs),
		queue:        make(chan deliveryJob, DefaultQueueSize),
		stopCh:       make(chan struct{}),
		nowFn:        func() time.Time { return time.Now().UTC() },
	}
	if err := m.load(); err != nil {
		return nil, err
	}

	m.wg.Add(1)
	go m.worker()
	return m, nil
}

// Close drains pending webhook deliveries and stops the background worker.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeMu.Lock()
	if m.closed.Swap(true) {
		m.closeMu.Unlock()
		return
	}
	close(m.stopCh)
	m.closeMu.Unlock()
	m.wg.Wait()
}

// QueueDepth returns the current number of queued webhook delivery jobs.
func (m *Manager) QueueDepth() int {
	return len(m.queue)
}

func buildSSRFClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, c syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("invalid resolved IP %q: %w", host, err)
			}
			if isBlockedIP(addr) {
				return fmt.Errorf("webhook target IP %s is blocked by SSRF policy", addr)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var blockedCIDRs = func() []netip.Prefix {
	raw := []string{
		"100.64.0.0/10",  // RFC 6598 CGNAT
		"192.0.0.0/24",   // IETF Protocol Assignments
		"198.18.0.0/15",  // Benchmarking
		"240.0.0.0/4",    // Reserved
		"64:ff9b::/96",   // NAT64
		"64:ff9b:1::/48", // Local-Use NAT64
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}()

func isBlockedIP(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsGlobalUnicast() ||
		addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return true
	}
	for _, p := range blockedCIDRs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
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
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	tmp := kekPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, kekPath); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	slog.Warn("CUBEPOD_EVENTS_KEY unset; generated local webhook encryption key", "path", kekPath)
	return key, nil
}

func (m *Manager) load() error {
	whPath := filepath.Join(m.dir, webhooksFileName)
	if raw, err := os.ReadFile(whPath); err == nil {
		var env encryptedEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
		if err != nil {
			return err
		}
		ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
		if err != nil {
			return err
		}
		block, err := aes.NewCipher(m.key)
		if err != nil {
			return err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return err
		}
		plain, err := gcm.Open(nil, nonce, ct, nil)
		if err != nil {
			return fmt.Errorf("decrypt webhooks store: %w", err)
		}
		var state webhookDiskState
		if err := json.Unmarshal(plain, &state); err != nil {
			return err
		}
		for i := range state.Webhooks {
			cp := state.Webhooks[i]
			m.webhooks[cp.ID] = &cp
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	evPath := filepath.Join(m.dir, eventsFileName)
	if raw, err := os.ReadFile(evPath); err == nil {
		var state eventDiskState
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		m.events = state.Events
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *Manager) saveWebhooksLocked() error {
	state := webhookDiskState{Webhooks: make([]storedWebhook, 0, len(m.webhooks))}
	for _, w := range m.webhooks {
		state.Webhooks = append(state.Webhooks, *w)
	}
	sort.Slice(state.Webhooks, func(i, j int) bool {
		return state.Webhooks[i].ID < state.Webhooks[j].ID
	})
	plain, err := json.Marshal(state)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(m.key)
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
	data, err := json.Marshal(encryptedEnvelope{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(m.dir, webhooksFileName), data, 0o600)
}

func (m *Manager) saveEventsSnapshot(eventsCopy []storedEvent) {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	data, err := json.Marshal(eventDiskState{Events: eventsCopy})
	if err != nil {
		slog.Warn("failed to marshal events state", "err", err)
		return
	}
	if err := atomicWriteFile(filepath.Join(m.dir, eventsFileName), data, 0o600); err != nil {
		slog.Warn("failed to persist events state", "err", err)
	}
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newWebhookID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "whk_" + hex.EncodeToString(b[:])
}

func newSignatureKey() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b[:])
}

func (m *Manager) validateWebhookURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" || u.User != nil {
		return ErrInvalidWebhook
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(m.allowPrivate && scheme == "http") {
		return ErrInvalidWebhook
	}
	host := u.Hostname()
	if !m.allowPrivate {
		if ip, err := netip.ParseAddr(host); err == nil && isBlockedIP(ip) {
			return ErrInvalidWebhook
		}
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
			return ErrInvalidWebhook
		}
	}
	return nil
}

func validateEventList(events []string) ([]string, error) {
	if len(events) == 0 {
		return nil, ErrInvalidWebhook
	}
	seen := make(map[string]bool, len(events))
	out := make([]string, 0, len(events))
	for _, ev := range events {
		ev = strings.TrimSpace(ev)
		if ev != "*" {
			if _, ok := validEventTypes[ev]; !ok {
				return nil, ErrInvalidWebhook
			}
		}
		if !seen[ev] {
			seen[ev] = true
			out = append(out, ev)
		}
	}
	return out, nil
}

func toWebhookInfo(w *storedWebhook) WebhookInfo {
	return WebhookInfo{
		ID:        w.ID,
		Name:      w.Name,
		URL:       w.URL,
		Events:    append([]string(nil), w.Events...),
		Enabled:   w.Enabled,
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}
}

// CreateWebhook registers a new webhook subscription for the tenant.
func (m *Manager) CreateWebhook(apiKey string, in CreateWebhookInput) (WebhookWithSecret, error) {
	if strings.TrimSpace(apiKey) == "" {
		return WebhookWithSecret{}, ErrUnauthorized
	}
	if !validWebhookName.MatchString(in.Name) {
		return WebhookWithSecret{}, ErrInvalidWebhook
	}
	if err := m.validateWebhookURL(in.URL); err != nil {
		return WebhookWithSecret{}, err
	}
	evs, err := validateEventList(in.Events)
	if err != nil {
		return WebhookWithSecret{}, err
	}

	tenant := TenantHash(apiKey)
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, w := range m.webhooks {
		if w.TenantHash == tenant {
			count++
		}
	}
	if count >= m.maxWebhooks {
		return WebhookWithSecret{}, ErrLimitExceeded
	}

	now := m.nowFn()
	rec := &storedWebhook{
		TenantHash:   tenant,
		ID:           newWebhookID(),
		Name:         in.Name,
		URL:          strings.TrimSpace(in.URL),
		Events:       evs,
		Enabled:      enabled,
		SignatureKey: newSignatureKey(),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.webhooks[rec.ID] = rec
	if err := m.saveWebhooksLocked(); err != nil {
		delete(m.webhooks, rec.ID)
		return WebhookWithSecret{}, err
	}
	return WebhookWithSecret{
		WebhookInfo:  toWebhookInfo(rec),
		SignatureKey: rec.SignatureKey,
	}, nil
}

// ListWebhooks returns all webhooks owned by the tenant (never includes SignatureKey).
func (m *Manager) ListWebhooks(apiKey string) ([]WebhookInfo, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrUnauthorized
	}
	tenant := TenantHash(apiKey)

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]WebhookInfo, 0)
	for _, w := range m.webhooks {
		if w.TenantHash == tenant {
			out = append(out, toWebhookInfo(w))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetWebhook returns a single webhook owned by the tenant (never includes SignatureKey).
func (m *Manager) GetWebhook(apiKey, webhookID string) (WebhookInfo, error) {
	if strings.TrimSpace(apiKey) == "" {
		return WebhookInfo{}, ErrUnauthorized
	}
	tenant := TenantHash(apiKey)

	m.mu.RLock()
	defer m.mu.RUnlock()

	w, ok := m.webhooks[webhookID]
	if !ok || w.TenantHash != tenant {
		return WebhookInfo{}, ErrNotFound
	}
	return toWebhookInfo(w), nil
}

// UpdateWebhook modifies URL, subscribed events, enabled flag, or rotates the HMAC key.
func (m *Manager) UpdateWebhook(apiKey, webhookID string, in UpdateWebhookInput) (WebhookWithSecret, error) {
	if strings.TrimSpace(apiKey) == "" {
		return WebhookWithSecret{}, ErrUnauthorized
	}
	tenant := TenantHash(apiKey)

	if in.URL != nil {
		if err := m.validateWebhookURL(*in.URL); err != nil {
			return WebhookWithSecret{}, err
		}
	}
	var newEvents []string
	if in.Events != nil {
		var err error
		newEvents, err = validateEventList(in.Events)
		if err != nil {
			return WebhookWithSecret{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.webhooks[webhookID]
	if !ok || w.TenantHash != tenant {
		return WebhookWithSecret{}, ErrNotFound
	}

	prev := *w
	if in.URL != nil {
		w.URL = strings.TrimSpace(*in.URL)
	}
	if in.Events != nil {
		w.Events = newEvents
	}
	if in.Enabled != nil {
		w.Enabled = *in.Enabled
	}
	var rotatedKey string
	if in.RotateSecret {
		w.SignatureKey = newSignatureKey()
		rotatedKey = w.SignatureKey
	}
	w.UpdatedAt = m.nowFn()

	if err := m.saveWebhooksLocked(); err != nil {
		*w = prev
		return WebhookWithSecret{}, err
	}
	return WebhookWithSecret{
		WebhookInfo:  toWebhookInfo(w),
		SignatureKey: rotatedKey,
	}, nil
}

// DeleteWebhook deletes a webhook subscription owned by the tenant.
func (m *Manager) DeleteWebhook(apiKey, webhookID string) error {
	if strings.TrimSpace(apiKey) == "" {
		return ErrUnauthorized
	}
	tenant := TenantHash(apiKey)

	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.webhooks[webhookID]
	if !ok || w.TenantHash != tenant {
		return ErrNotFound
	}
	delete(m.webhooks, webhookID)
	if err := m.saveWebhooksLocked(); err != nil {
		m.webhooks[webhookID] = w
		return err
	}
	return nil
}

// Emit records a sandbox lifecycle event for tenantHash and enqueues async webhook deliveries.
func (m *Manager) Emit(tenantHash, eventType, sandboxID, templateID, buildID, executionID string, data *EventData) LifecycleEvent {
	label := validEventTypes[eventType]
	if label == "" {
		label = "lifecycle"
	}
	if buildID == "" {
		buildID = buildDeterministicUUID(templateID)
	}
	if executionID == "" {
		executionID = buildDeterministicUUID(sandboxID)
	}
	teamID := tenantHash
	if len(teamID) > 16 {
		teamID = "team_" + teamID[:16]
	}

	ev := LifecycleEvent{
		Version:            "v1",
		ID:                 newUUIDv4(),
		Type:               eventType,
		Timestamp:          m.nowFn(),
		EventCategory:      "lifecycle",
		EventLabel:         label,
		SandboxID:          sandboxID,
		SandboxTeamID:      teamID,
		SandboxTemplateID:  templateID,
		SandboxBuildID:     buildID,
		SandboxExecutionID: executionID,
		EventData:          data,
	}

	m.EventsRecordedTotal.Add(1)

	m.mu.Lock()
	m.events = append(m.events, storedEvent{TenantHash: tenantHash, Event: ev})
	// Enforce bounded retention per tenant
	if m.maxEvents > 0 {
		var tenantCount int
		for i := range m.events {
			if m.events[i].TenantHash == tenantHash {
				tenantCount++
			}
		}
		if tenantCount > m.maxEvents {
			toDrop := tenantCount - m.maxEvents
			filtered := m.events[:0]
			for _, item := range m.events {
				if item.TenantHash == tenantHash && toDrop > 0 {
					toDrop--
					continue
				}
				filtered = append(filtered, item)
			}
			m.events = filtered
		}
	}
	eventsSnapshot := append([]storedEvent(nil), m.events...)

	var targets []*storedWebhook
	for _, w := range m.webhooks {
		if w.TenantHash == tenantHash && w.Enabled && webhookMatchesEvent(w.Events, eventType) {
			cp := *w
			targets = append(targets, &cp)
		}
	}
	m.mu.Unlock()

	m.saveEventsSnapshot(eventsSnapshot)

	if len(targets) == 0 {
		return ev
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return ev
	}
	m.closeMu.RLock()
	defer m.closeMu.RUnlock()
	if m.closed.Load() {
		return ev
	}
	for _, w := range targets {
		select {
		case m.queue <- deliveryJob{
			webhookID:    w.ID,
			url:          w.URL,
			signatureKey: w.SignatureKey,
			event:        ev,
			payload:      payload,
		}:
		default:
			m.WebhookDeliveriesDrop.Add(1)
		}
	}
	return ev
}

// ListEvents returns paginated lifecycle events for the tenant, newest first.
func (m *Manager) ListEvents(apiKey, sandboxID, eventType string, limit int, nextToken string) ([]LifecycleEvent, string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, "", ErrUnauthorized
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

	tenant := TenantHash(apiKey)

	m.mu.RLock()
	matched := make([]LifecycleEvent, 0)
	for i := len(m.events) - 1; i >= 0; i-- {
		item := m.events[i]
		if item.TenantHash != tenant {
			continue
		}
		if sandboxID != "" && item.Event.SandboxID != sandboxID {
			continue
		}
		if eventType != "" && item.Event.Type != eventType {
			continue
		}
		matched = append(matched, item.Event)
	}
	m.mu.RUnlock()

	if offset >= len(matched) {
		return []LifecycleEvent{}, "", nil
	}
	end := offset + limit
	var outNext string
	if end < len(matched) {
		outNext = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	} else {
		end = len(matched)
	}
	return matched[offset:end], outNext, nil
}

func webhookMatchesEvent(subscribed []string, eventType string) bool {
	for _, s := range subscribed {
		if s == "*" || s == eventType {
			return true
		}
	}
	return false
}

func buildDeterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// ComputeSignature computes the HMAC-SHA256 hex digest and formatted signature header.
func ComputeSignature(signatureKey string, timestamp int64, payload []byte) (rawHex string, formatted string) {
	mac := hmac.New(sha256.New, []byte(signatureKey))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	rawHex = hex.EncodeToString(mac.Sum(nil))
	formatted = fmt.Sprintf("t=%d,v1=%s", timestamp, rawHex)
	return rawHex, formatted
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case job := <-m.queue:
			m.deliver(job)
		case <-m.stopCh:
			for {
				select {
				case job := <-m.queue:
					m.deliver(job)
				default:
					return
				}
			}
		}
	}
}

func (m *Manager) deliver(job deliveryJob) {
	for attempt := 0; attempt < m.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := m.retryBase << uint(attempt-1)
			select {
			case <-time.After(backoff):
			case <-m.stopCh:
				m.WebhookDeliveriesFailed.Add(1)
				return
			}
		}

		ts := m.nowFn().Unix()
		rawHex, formatted := ComputeSignature(job.signatureKey, ts, job.payload)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, job.url, bytes.NewReader(job.payload))
		if err != nil {
			m.WebhookDeliveriesFailed.Add(1)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("e2b-signature", rawHex)
		req.Header.Set("X-CubePod-Signature", formatted)
		req.Header.Set("X-CubePod-Event-ID", job.event.ID)

		resp, err := m.httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				m.WebhookDeliveriesOK.Add(1)
				return
			}
		}
	}
	m.WebhookDeliveriesFailed.Add(1)
}
