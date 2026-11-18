// Package events implements tenant-isolated Sandbox Lifecycle Events and
// SSRF-safe, HMAC-SHA256 signed Webhook delivery (CEP-0028).
package events

import (
	"bytes"
	"context"
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
	// DefaultPersistQueueSize bounds async backend writes (delivery status, dead letters).
	DefaultPersistQueueSize = 1024

	// backendTimeout bounds synchronous Backend calls made on API paths.
	backendTimeout = 5 * time.Second
	// statusPersistInterval coalesces delivery-status writes for healthy webhooks.
	statusPersistInterval = 10 * time.Second
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
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Enabled             bool       `json:"enabled"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	LastDeliveryTime    *time.Time `json:"lastDeliveryTime,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty"`
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
	EncryptionKey           []byte
	MaxWebhooksPerTenant    int
	MaxEventsPerTenant      int
	MaxDeadLettersPerTenant int
	AllowPrivateIPs         bool
	DeliveryTimeout         time.Duration
	MaxRetries              int
	RetryBaseDelay          time.Duration
	QueueSize               int
}

type deliveryJob struct {
	webhookID    string
	url          string
	signatureKey string
	event        LifecycleEvent
	payload      []byte

	// Set when the job retries an existing dead letter.
	deadLetterID        string
	deadLetterCreatedAt time.Time
	retryCount          int
}

// Manager manages tenant-isolated lifecycle events and async signed webhook delivery.
type Manager struct {
	mu                 sync.RWMutex
	crudMu             sync.Mutex
	dlqMu              sync.Mutex
	saveMu             sync.Mutex
	closeMu            sync.RWMutex
	syncMu             sync.Mutex
	backend            Backend
	maxWebhooks        int
	maxEvents          int
	maxDeadLetters     int
	allowPrivate       bool
	maxRetries         int
	retryBase          time.Duration
	webhooks           map[string]*StoredWebhook
	events             []StoredEvent
	deadLetters        []StoredDeadLetter
	pendingDeadLetters map[string]struct{}
	inflightDLQDelete  map[string]int
	dlqGen             map[string]uint64
	httpClient         *http.Client
	queue              chan deliveryJob
	persistCh          chan func(context.Context)
	stopCh             chan struct{}
	persistStopCh      chan struct{}
	wg                 sync.WaitGroup
	persistWG          sync.WaitGroup
	closed             atomic.Bool
	persistClosed      atomic.Bool
	writeSeq           atomic.Uint64
	nowFn              func() time.Time

	EventsRecordedTotal     atomic.Uint64
	WebhookDeliveriesOK     atomic.Uint64
	WebhookDeliveriesFailed atomic.Uint64
	WebhookDeliveriesDrop   atomic.Uint64
	DeadLettersTotal        atomic.Uint64
	SyncErrorsTotal         atomic.Uint64
}

// TenantHash derives the opaque tenant identifier from an API key.
func TenantHash(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}

// Open opens or initializes a file-backed events and webhook manager under dir.
func Open(dir string, cfg Config) (*Manager, error) {
	backend, err := NewFileBackend(dir, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	return OpenWithBackend(backend, cfg)
}

// OpenWithBackend initializes a Manager backed by backend and performs the initial load.
func OpenWithBackend(backend Backend, cfg Config) (*Manager, error) {
	if backend == nil {
		return nil, errors.New("events backend is required")
	}
	maxWh := cfg.MaxWebhooksPerTenant
	if maxWh <= 0 {
		maxWh = DefaultMaxWebhooksPerTenant
	}
	maxEv := cfg.MaxEventsPerTenant
	if maxEv <= 0 {
		maxEv = DefaultMaxEventsPerTenant
	}
	maxDL := cfg.MaxDeadLettersPerTenant
	if maxDL <= 0 {
		maxDL = DefaultMaxDeadLettersPerTenant
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
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}

	m := &Manager{
		backend:            backend,
		maxWebhooks:        maxWh,
		maxEvents:          maxEv,
		maxDeadLetters:     maxDL,
		allowPrivate:       cfg.AllowPrivateIPs,
		maxRetries:         maxRetries,
		retryBase:          retryBase,
		webhooks:           make(map[string]*StoredWebhook),
		pendingDeadLetters: make(map[string]struct{}),
		inflightDLQDelete:  make(map[string]int),
		dlqGen:             make(map[string]uint64),
		httpClient:         buildSSRFClient(timeout, cfg.AllowPrivateIPs),
		queue:              make(chan deliveryJob, queueSize),
		persistCh:          make(chan func(context.Context), DefaultPersistQueueSize),
		stopCh:             make(chan struct{}),
		persistStopCh:      make(chan struct{}),
		nowFn:              func() time.Time { return time.Now().UTC() },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := backend.LoadState(ctx)
	if err != nil {
		return nil, fmt.Errorf("load events state (%s backend): %w", backend.Name(), err)
	}
	m.applyState(st, true)

	m.persistWG.Add(1)
	go m.persistWorker()
	m.wg.Add(1)
	go m.worker()
	return m, nil
}

// BackendName returns the active persistence backend ("file" or "k8s").
func (m *Manager) BackendName() string {
	return m.backend.Name()
}

// Close drains pending webhook deliveries and backend writes, then stops background workers.
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

	m.closeMu.Lock()
	m.persistClosed.Store(true)
	close(m.persistStopCh)
	m.closeMu.Unlock()
	m.persistWG.Wait()
}

// QueueDepth returns the current number of queued webhook delivery jobs.
func (m *Manager) QueueDepth() int {
	return len(m.queue)
}

// applyStateLocked replaces the in-memory webhook and dead-letter caches with st while
// holding m.mu. When includeEvents is set (initial load) the local event ring is replaced
// too. Local delivery status newer than the backend copy and dead letters still awaiting
// persistence are preserved so a Sync never regresses this replica's view.
func (m *Manager) applyStateLocked(st State, includeEvents bool) {
	next := make(map[string]*StoredWebhook, len(st.Webhooks))
	for i := range st.Webhooks {
		rec := st.Webhooks[i]
		if prev, ok := m.webhooks[rec.ID]; ok {
			rec.statusPersistedAt = prev.statusPersistedAt
			if prev.LastDeliveryTime != nil && (rec.LastDeliveryTime == nil || prev.LastDeliveryTime.After(*rec.LastDeliveryTime)) {
				rec.LastDeliveryTime = prev.LastDeliveryTime
				rec.ConsecutiveFailures = prev.ConsecutiveFailures
			}
		}
		rec.Events = append([]string(nil), rec.Events...)
		next[rec.ID] = &rec
	}
	m.webhooks = next

	dls := make([]StoredDeadLetter, 0, len(st.DeadLetters))
	seen := make(map[string]bool, len(st.DeadLetters))
	for _, d := range st.DeadLetters {
		if _, ok := next[d.WebhookID]; !ok || seen[d.ID] || m.inflightDLQDelete[d.ID] > 0 {
			continue
		}
		seen[d.ID] = true
		dls = append(dls, d)
	}
	for _, d := range m.deadLetters {
		if _, pending := m.pendingDeadLetters[d.ID]; pending && !seen[d.ID] {
			if _, ok := next[d.WebhookID]; ok {
				dls = append(dls, d)
				seen[d.ID] = true
			}
		}
	}
	sortDeadLetters(dls)
	m.deadLetters = dls
	tenants := make(map[string]bool)
	for _, d := range m.deadLetters {
		tenants[d.TenantHash] = true
	}
	for t := range tenants {
		m.trimDeadLettersLocked(t)
	}

	if includeEvents {
		m.events = append([]StoredEvent(nil), st.Events...)
	}
}

func (m *Manager) applyState(st State, includeEvents bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyStateLocked(st, includeEvents)
}

// Sync reloads shared webhook configuration, delivery status, and dead letters from the
// backend so multi-replica gateways converge without an external database.
func (m *Manager) Sync(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		seqBefore := m.writeSeq.Load()
		st, err := m.backend.LoadState(ctx)
		if err != nil {
			m.SyncErrorsTotal.Add(1)
			return err
		}
		m.mu.Lock()
		changed := m.writeSeq.Load() != seqBefore
		if !changed {
			m.applyStateLocked(st, false)
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()
	}
	return nil
}

// StartSync runs Sync every interval until ctx is cancelled (shared backends only).
func (m *Manager) StartSync(ctx context.Context, interval time.Duration) {
	if !m.backend.Shared() || interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			case <-ticker.C:
				syncCtx, cancel := context.WithTimeout(ctx, interval*2)
				if err := m.Sync(syncCtx); err != nil && ctx.Err() == nil {
					slog.Warn("events backend sync failed", "backend", m.backend.Name(), "err", err)
				}
				cancel()
			}
		}
	}()
}

// syncOnMiss reloads shared state after a cache miss. It reports whether a reload happened.
func (m *Manager) syncOnMiss() bool {
	if !m.backend.Shared() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	if err := m.Sync(ctx); err != nil {
		slog.Warn("events backend reload on cache miss failed", "backend", m.backend.Name(), "err", err)
		return false
	}
	return true
}

// lookupWebhook returns a copy of the tenant's webhook, reloading shared state once on a miss.
func (m *Manager) lookupWebhook(tenant, webhookID string) (StoredWebhook, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.RLock()
		w, ok := m.webhooks[webhookID]
		var cp StoredWebhook
		if ok {
			cp = *w
		}
		m.mu.RUnlock()
		if ok {
			if cp.TenantHash != tenant {
				return StoredWebhook{}, ErrNotFound
			}
			return cp, nil
		}
		if attempt == 0 && !m.syncOnMiss() {
			break
		}
	}
	return StoredWebhook{}, ErrNotFound
}

func (m *Manager) enqueuePersist(op func(context.Context)) {
	m.closeMu.RLock()
	defer m.closeMu.RUnlock()
	if m.persistClosed.Load() {
		return
	}
	select {
	case m.persistCh <- op:
	default:
		slog.Warn("events backend persistence queue full; dropping async write")
	}
}

func (m *Manager) persistWorker() {
	defer m.persistWG.Done()
	run := func(op func(context.Context)) {
		ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
		defer cancel()
		op(ctx)
	}
	for {
		select {
		case op := <-m.persistCh:
			run(op)
		case <-m.persistStopCh:
			for {
				select {
				case op := <-m.persistCh:
					run(op)
				default:
					return
				}
			}
		}
	}
}

func toWebhookInfo(w *StoredWebhook) WebhookInfo {
	info := WebhookInfo{
		ID:                  w.ID,
		Name:                w.Name,
		URL:                 w.URL,
		Events:              append([]string(nil), w.Events...),
		Enabled:             w.Enabled,
		CreatedAt:           w.CreatedAt,
		UpdatedAt:           w.UpdatedAt,
		ConsecutiveFailures: w.ConsecutiveFailures,
	}
	if w.LastDeliveryTime != nil {
		t := *w.LastDeliveryTime
		info.LastDeliveryTime = &t
	}
	return info
}

func (m *Manager) backendCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), backendTimeout)
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

	m.crudMu.Lock()
	defer m.crudMu.Unlock()

	if m.backend.Shared() {
		// Refresh so the per-tenant limit accounts for webhooks created on other replicas.
		m.syncOnMiss()
	}

	m.mu.RLock()
	count := 0
	for _, w := range m.webhooks {
		if w.TenantHash == tenant {
			count++
		}
	}
	m.mu.RUnlock()
	if count >= m.maxWebhooks {
		return WebhookWithSecret{}, ErrLimitExceeded
	}

	now := m.nowFn()
	rec := &StoredWebhook{
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
	ctx, cancel := m.backendCtx()
	err = m.backend.PutWebhook(ctx, *rec)
	cancel()
	if err != nil {
		return WebhookWithSecret{}, err
	}

	m.mu.Lock()
	m.webhooks[rec.ID] = rec
	m.writeSeq.Add(1)
	out := WebhookWithSecret{
		WebhookInfo:  toWebhookInfo(rec),
		SignatureKey: rec.SignatureKey,
	}
	m.mu.Unlock()
	return out, nil
}

// ListWebhooks returns all webhooks owned by the tenant (never includes SignatureKey).
func (m *Manager) ListWebhooks(apiKey string) ([]WebhookInfo, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrUnauthorized
	}
	tenant := TenantHash(apiKey)
	if m.backend.Shared() {
		m.syncOnMiss()
	}

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
	if m.backend.Shared() {
		m.syncOnMiss()
	}
	m.mu.RLock()
	w, ok := m.webhooks[webhookID]
	var info WebhookInfo
	if ok && w.TenantHash == tenant {
		info = toWebhookInfo(w)
	} else {
		ok = false
	}
	m.mu.RUnlock()
	if !ok {
		return WebhookInfo{}, ErrNotFound
	}
	return info, nil
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
	m.crudMu.Lock()
	defer m.crudMu.Unlock()

	if m.backend.Shared() {
		m.syncOnMiss()
	}
	curCopy, err := m.lookupWebhook(tenant, webhookID)
	if err != nil {
		return WebhookWithSecret{}, err
	}

	next := curCopy
	next.Events = append([]string(nil), curCopy.Events...)
	if in.URL != nil {
		next.URL = strings.TrimSpace(*in.URL)
	}
	if in.Events != nil {
		next.Events = newEvents
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	var rotatedKey string
	if in.RotateSecret {
		next.SignatureKey = newSignatureKey()
		rotatedKey = next.SignatureKey
	}
	next.UpdatedAt = m.nowFn()

	ctx, cancel := m.backendCtx()
	err = m.backend.PutWebhook(ctx, next)
	cancel()
	if err != nil {
		return WebhookWithSecret{}, err
	}

	m.mu.Lock()
	w, ok := m.webhooks[webhookID]
	if !ok {
		cp := next
		w = &cp
		m.webhooks[webhookID] = w
	} else {
		// Merge only updated config fields so concurrent delivery status updates survive.
		w.URL = next.URL
		w.Events = append([]string(nil), next.Events...)
		w.Enabled = next.Enabled
		w.SignatureKey = next.SignatureKey
		w.UpdatedAt = next.UpdatedAt
	}
	info := toWebhookInfo(w)
	m.writeSeq.Add(1)
	m.mu.Unlock()
	return WebhookWithSecret{
		WebhookInfo:  info,
		SignatureKey: rotatedKey,
	}, nil
}

// DeleteWebhook deletes a webhook subscription (and its dead letters) owned by the tenant.
func (m *Manager) DeleteWebhook(apiKey, webhookID string) error {
	if strings.TrimSpace(apiKey) == "" {
		return ErrUnauthorized
	}
	tenant := TenantHash(apiKey)

	m.crudMu.Lock()
	defer m.crudMu.Unlock()

	if _, err := m.lookupWebhook(tenant, webhookID); err != nil {
		return err
	}

	m.dlqMu.Lock()
	ctx, cancel := m.backendCtx()
	err := m.backend.DeleteWebhook(ctx, webhookID)
	cancel()
	m.dlqMu.Unlock()
	if err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.webhooks, webhookID)
	m.writeSeq.Add(1)
	filtered := make([]StoredDeadLetter, 0, len(m.deadLetters))
	for _, d := range m.deadLetters {
		if d.WebhookID != webhookID {
			filtered = append(filtered, d)
		} else {
			m.dlqGen[d.ID]++
			delete(m.pendingDeadLetters, d.ID)
		}
	}
	m.deadLetters = filtered
	m.mu.Unlock()
	return nil
}

// Emit records a sandbox lifecycle event for tenantHash and enqueues async webhook deliveries.
func (m *Manager) Emit(tenantHash, eventType, sandboxID, templateID, buildID, executionID string, data *EventData) LifecycleEvent {
	ev, _ := m.emit(tenantHash, eventType, sandboxID, templateID, buildID, executionID, newUUIDv4(), data)
	return ev
}

// EmitObserved records a lifecycle transition observed asynchronously (for example by the
// PodBox sync loop) using a deterministic event ID derived from dedupeKey. When several
// gateway replicas observe the same transition, only the first records it and delivers
// webhooks; the others detect the duplicate and return recorded=false.
func (m *Manager) EmitObserved(tenantHash, eventType, sandboxID, templateID, dedupeKey string, data *EventData) (LifecycleEvent, bool) {
	id := buildDeterministicUUID(strings.Join([]string{tenantHash, eventType, sandboxID, dedupeKey}, "|"))
	return m.emit(tenantHash, eventType, sandboxID, templateID, "", "", id, data)
}

func (m *Manager) emit(tenantHash, eventType, sandboxID, templateID, buildID, executionID, eventID string, data *EventData) (LifecycleEvent, bool) {
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
		ID:                 eventID,
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
	stored := StoredEvent{TenantHash: tenantHash, Event: ev}

	m.mu.RLock()
	for i := len(m.events) - 1; i >= 0; i-- {
		if m.events[i].Event.ID == eventID && m.events[i].TenantHash == tenantHash {
			m.mu.RUnlock()
			return m.events[i].Event, false
		}
	}
	m.mu.RUnlock()

	ctx, cancel := m.backendCtx()
	err := m.backend.RecordEvent(ctx, stored)
	cancel()
	if errors.Is(err, ErrDuplicateEvent) {
		return ev, false
	}
	if err != nil {
		slog.Warn("failed to record lifecycle event in backend", "backend", m.backend.Name(),
			"eventID", ev.ID, "sandboxID", sandboxID, "type", eventType, "err", err)
	}

	m.EventsRecordedTotal.Add(1)

	m.mu.Lock()
	m.events = append(m.events, stored)
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
	eventsSnapshot := append([]StoredEvent(nil), m.events...)

	var targets []StoredWebhook
	for _, w := range m.webhooks {
		if w.TenantHash == tenantHash && w.Enabled && webhookMatchesEvent(w.Events, eventType) {
			targets = append(targets, *w)
		}
	}
	m.mu.Unlock()

	if !m.backend.Shared() {
		m.saveEventsSnapshot(eventsSnapshot)
	}

	if len(targets) == 0 {
		return ev, true
	}
	payload, err := marshalEvent(ev)
	if err != nil {
		return ev, true
	}
	for _, w := range targets {
		job := deliveryJob{
			webhookID:    w.ID,
			url:          w.URL,
			signatureKey: w.SignatureKey,
			event:        ev,
			payload:      payload,
		}
		if !m.tryEnqueue(job) && !m.closed.Load() {
			m.WebhookDeliveriesDrop.Add(1)
			m.recordDeadLetter(job, DeadLetterReasonQueueFull, deliveryOutcome{errClass: "queue_full"})
		}
	}
	return ev, true
}

func marshalEvent(ev LifecycleEvent) ([]byte, error) {
	return json.Marshal(ev)
}

// tryEnqueue performs a non-blocking enqueue; it returns false when closed or the queue is full.
func (m *Manager) tryEnqueue(job deliveryJob) bool {
	m.closeMu.RLock()
	defer m.closeMu.RUnlock()
	if m.closed.Load() {
		return false
	}
	select {
	case m.queue <- job:
		return true
	default:
		return false
	}
}

func (m *Manager) saveEventsSnapshot(eventsCopy []StoredEvent) {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	ctx, cancel := m.backendCtx()
	defer cancel()
	if err := m.backend.SaveEvents(ctx, eventsCopy); err != nil {
		slog.Warn("failed to persist events state", "err", err)
	}
}

// ListEvents returns paginated lifecycle events for the tenant, newest first. With a shared
// backend, events recorded by other gateway replicas are merged in (deduplicated by ID).
func (m *Manager) ListEvents(apiKey, sandboxID, eventType string, limit int, nextToken string) ([]LifecycleEvent, string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, "", ErrUnauthorized
	}
	if limit <= 0 || limit > DefaultListLimit {
		limit = DefaultListLimit
	}
	offset, err := decodeOffsetCursor(nextToken)
	if err != nil {
		return nil, "", err
	}

	tenant := TenantHash(apiKey)
	keep := func(item StoredEvent) bool {
		if item.TenantHash != tenant {
			return false
		}
		if sandboxID != "" && item.Event.SandboxID != sandboxID {
			return false
		}
		if eventType != "" && item.Event.Type != eventType {
			return false
		}
		return true
	}

	m.mu.RLock()
	matched := make([]LifecycleEvent, 0)
	seen := make(map[string]bool)
	for i := len(m.events) - 1; i >= 0; i-- {
		item := m.events[i]
		if !keep(item) || seen[item.Event.ID] {
			continue
		}
		seen[item.Event.ID] = true
		matched = append(matched, item.Event)
	}
	m.mu.RUnlock()

	if m.backend.Shared() {
		ctx, cancel := m.backendCtx()
		shared, qerr := m.backend.QueryEvents(ctx, tenant, sandboxID)
		cancel()
		if qerr != nil {
			slog.Warn("failed to query shared lifecycle events; serving local cache", "backend", m.backend.Name(), "err", qerr)
		}
		for _, item := range shared {
			if !keep(item) || seen[item.Event.ID] {
				continue
			}
			seen[item.Event.ID] = true
			matched = append(matched, item.Event)
		}
		sort.SliceStable(matched, func(i, j int) bool {
			if !matched[i].Timestamp.Equal(matched[j].Timestamp) {
				return matched[i].Timestamp.After(matched[j].Timestamp)
			}
			return matched[i].ID > matched[j].ID
		})
	}

	page, next := paginate(matched, offset, limit)
	return page, next, nil
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case job := <-m.queue:
			m.handleJob(job)
		case <-m.stopCh:
			for {
				select {
				case job := <-m.queue:
					m.handleJob(job)
				default:
					return
				}
			}
		}
	}
}

func (m *Manager) handleJob(job deliveryJob) {
	out := m.deliver(job)
	if out.ok {
		m.WebhookDeliveriesOK.Add(1)
	} else {
		m.WebhookDeliveriesFailed.Add(1)
	}
	m.recordDeliveryResult(job, out)
}

// recordDeliveryResult updates webhook delivery status, persisting it to the backend when the
// failure streak changes or the last flush is older than statusPersistInterval, and
// dead-letters failed jobs.
func (m *Manager) recordDeliveryResult(job deliveryJob, out deliveryOutcome) {
	now := m.nowFn()
	m.mu.Lock()
	w, ok := m.webhooks[job.webhookID]
	if !ok {
		m.mu.Unlock()
		return
	}
	prevFailures := w.ConsecutiveFailures
	if out.ok {
		t := now
		w.LastDeliveryTime = &t
		w.ConsecutiveFailures = 0
	} else {
		w.ConsecutiveFailures++
	}
	persist := (prevFailures == 0) != (w.ConsecutiveFailures == 0) || now.Sub(w.statusPersistedAt) >= statusPersistInterval
	var status WebhookStatus
	if persist {
		w.statusPersistedAt = now
		status.ConsecutiveFailures = w.ConsecutiveFailures
		if w.LastDeliveryTime != nil {
			t := *w.LastDeliveryTime
			status.LastDeliveryTime = &t
		}
	}
	webhookID := w.ID
	m.mu.Unlock()

	if persist {
		m.enqueuePersist(func(ctx context.Context) {
			if err := m.backend.UpdateWebhookStatus(ctx, webhookID, status); err != nil {
				slog.Warn("failed to persist webhook delivery status", "webhookID", webhookID, "err", err)
			}
		})
	}
	if !out.ok {
		m.recordDeadLetter(job, DeadLetterReasonDeliveryFailed, out)
	}
}

func classifyDeliveryError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "blocked by SSRF policy") {
		return "ssrf_blocked"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "connection_error"
}

func (m *Manager) deliver(job deliveryJob) deliveryOutcome {
	var out deliveryOutcome
	for attempt := 0; attempt < m.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := m.retryBase << uint(attempt-1)
			select {
			case <-time.After(backoff):
			case <-m.stopCh:
				out.errClass = "shutdown"
				return out
			}
		}
		out.attempts = attempt + 1

		ts := m.nowFn().Unix()
		rawHex, formatted := ComputeSignature(job.signatureKey, ts, job.payload)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, job.url, bytes.NewReader(job.payload))
		if err != nil {
			out.errClass = "invalid_request"
			return out
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("e2b-signature", rawHex)
		req.Header.Set("X-CubePod-Signature", formatted)
		req.Header.Set("X-CubePod-Event-ID", job.event.ID)

		resp, err := m.httpClient.Do(req)
		if err != nil {
			out.errClass = classifyDeliveryError(err)
			out.statusCode = 0
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if job.deadLetterID != "" {
				m.forgetRetriedDeadLetter(job)
			}
			return deliveryOutcome{ok: true, attempts: attempt + 1, statusCode: resp.StatusCode}
		}
		out.statusCode = resp.StatusCode
		out.errClass = "http_status"
	}
	return out
}

// forgetRetriedDeadLetter makes sure a successfully retried dead letter is removed from the
// backend (idempotent; covers replicas that re-persisted it concurrently).
func (m *Manager) forgetRetriedDeadLetter(job deliveryJob) {
	m.mu.RLock()
	w, ok := m.webhooks[job.webhookID]
	tenant := ""
	if ok {
		tenant = w.TenantHash
	}
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec := StoredDeadLetter{TenantHash: tenant, DeadLetter: DeadLetter{ID: job.deadLetterID, WebhookID: job.webhookID}}
	m.enqueuePersist(func(ctx context.Context) {
		m.dlqMu.Lock()
		defer m.dlqMu.Unlock()
		if err := m.backend.DeleteDeadLetter(ctx, rec); err != nil {
			slog.Warn("failed to delete retried dead letter", "deadLetterID", rec.ID, "err", err)
		}
	})
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
