package events

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMaxDeadLettersPerTenant bounds retained dead letters per tenant.
	DefaultMaxDeadLettersPerTenant = 500
	// DefaultMaxDeadLettersPerWebhook bounds dead letters stored in one SandboxWebhook status.
	DefaultMaxDeadLettersPerWebhook = 50

	// DeadLetterReasonDeliveryFailed marks deliveries that failed after max retries.
	DeadLetterReasonDeliveryFailed = "delivery_failed"
	// DeadLetterReasonQueueFull marks deliveries dropped because the delivery queue was full.
	DeadLetterReasonQueueFull = "queue_full"
)

var (
	// ErrDeadLetterNotFound is returned when the dead letter does not exist or belongs to another tenant.
	ErrDeadLetterNotFound = errors.New("dead letter not found")
	// ErrWebhookDisabled is returned when retrying a dead letter for a disabled webhook.
	ErrWebhookDisabled = errors.New("webhook is disabled")
	// ErrQueueFull is returned when a retry cannot be enqueued because the delivery queue is full.
	ErrQueueFull = errors.New("webhook delivery queue is full")
)

// DeadLetter is the public, tenant-scoped view of an undeliverable webhook event. It never
// includes the webhook URL or the HMAC signatureKey.
type DeadLetter struct {
	ID             string         `json:"id"`
	WebhookID      string         `json:"webhookId"`
	EventID        string         `json:"eventId"`
	EventType      string         `json:"eventType"`
	SandboxID      string         `json:"sandboxId"`
	Reason         string         `json:"reason"`
	Attempts       int            `json:"attempts"`
	LastStatusCode int            `json:"lastStatusCode,omitempty"`
	LastError      string         `json:"lastError,omitempty"`
	RetryCount     int            `json:"retryCount,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	Event          LifecycleEvent `json:"event"`
}

// StoredDeadLetter is a DeadLetter plus its owning tenant, as persisted by a Backend.
type StoredDeadLetter struct {
	TenantHash string `json:"tenantHash"`
	DeadLetter
}

// DeadLetterFilter narrows ListDeadLetters results.
type DeadLetterFilter struct {
	WebhookID string
	SandboxID string
	Reason    string
}

// deliveryOutcome summarizes the final result of one delivery job.
type deliveryOutcome struct {
	ok         bool
	attempts   int
	statusCode int
	errClass   string
}

func newDeadLetterID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "dlq_" + hex.EncodeToString(b[:])
}

func sortDeadLetters(items []StoredDeadLetter) {
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

// recordDeadLetter stores (or re-stores, for failed retries) a dead letter for job in memory,
// enforces the per-tenant bound, and schedules async persistence.
func (m *Manager) recordDeadLetter(job deliveryJob, reason string, out deliveryOutcome) {
	now := m.nowFn()
	m.mu.Lock()
	w, ok := m.webhooks[job.webhookID]
	if !ok {
		m.mu.Unlock()
		return
	}
	rec := StoredDeadLetter{
		TenantHash: w.TenantHash,
		DeadLetter: DeadLetter{
			ID:             job.deadLetterID,
			WebhookID:      job.webhookID,
			EventID:        job.event.ID,
			EventType:      job.event.Type,
			SandboxID:      job.event.SandboxID,
			Reason:         reason,
			Attempts:       out.attempts,
			LastStatusCode: out.statusCode,
			LastError:      out.errClass,
			RetryCount:     job.retryCount,
			CreatedAt:      job.deadLetterCreatedAt,
			UpdatedAt:      now,
			Event:          job.event,
		},
	}
	if rec.ID == "" {
		rec.ID = newDeadLetterID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	filtered := m.deadLetters[:0]
	for _, d := range m.deadLetters {
		if d.ID != rec.ID {
			filtered = append(filtered, d)
		}
	}
	m.deadLetters = append(filtered, rec)
	evicted := m.trimDeadLettersLocked(rec.TenantHash)
	m.dlqGen[rec.ID]++
	gen := m.dlqGen[rec.ID]
	m.pendingDeadLetters[rec.ID] = struct{}{}
	m.mu.Unlock()

	m.DeadLettersTotal.Add(1)
	slog.Warn("webhook delivery dead-lettered",
		"webhookID", rec.WebhookID, "eventID", rec.EventID, "deadLetterID", rec.ID,
		"reason", reason, "attempts", out.attempts, "statusCode", out.statusCode, "error", out.errClass)

	m.enqueuePersist(func(ctx context.Context) {
		m.dlqMu.Lock()
		defer m.dlqMu.Unlock()
		for _, old := range evicted {
			if err := m.backend.DeleteDeadLetter(ctx, old); err != nil {
				slog.Warn("failed to delete evicted dead letter", "deadLetterID", old.ID, "err", err)
			}
		}
		m.mu.RLock()
		curGen := m.dlqGen[rec.ID]
		_, whExists := m.webhooks[rec.WebhookID]
		m.mu.RUnlock()
		if curGen != gen || !whExists {
			m.mu.Lock()
			if m.dlqGen[rec.ID] == curGen {
				delete(m.pendingDeadLetters, rec.ID)
			}
			m.mu.Unlock()
			return
		}
		if err := m.backend.PutDeadLetter(ctx, rec); err != nil {
			slog.Warn("failed to persist dead letter", "deadLetterID", rec.ID, "webhookID", rec.WebhookID, "err", err)
		}
		m.mu.Lock()
		if m.dlqGen[rec.ID] == gen {
			delete(m.pendingDeadLetters, rec.ID)
		}
		m.mu.Unlock()
	})
}

// trimDeadLettersLocked drops the oldest dead letters of tenant beyond maxDeadLetters and
// returns the evicted records.
func (m *Manager) trimDeadLettersLocked(tenant string) []StoredDeadLetter {
	count := 0
	for _, d := range m.deadLetters {
		if d.TenantHash == tenant {
			count++
		}
	}
	if count <= m.maxDeadLetters {
		return nil
	}
	toDrop := count - m.maxDeadLetters
	var evicted []StoredDeadLetter
	filtered := m.deadLetters[:0]
	for _, d := range m.deadLetters {
		if d.TenantHash == tenant && toDrop > 0 {
			toDrop--
			evicted = append(evicted, d)
			continue
		}
		filtered = append(filtered, d)
	}
	m.deadLetters = filtered
	return evicted
}

// DeadLettersPending returns the number of dead letters currently retained across tenants.
func (m *Manager) DeadLettersPending() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.deadLetters)
}

// ListDeadLetters returns the tenant's dead letters, newest first, with offset cursor pagination.
// When filter.WebhookID is set, the webhook must exist and belong to the tenant.
func (m *Manager) ListDeadLetters(apiKey string, filter DeadLetterFilter, limit int, nextToken string) ([]DeadLetter, string, error) {
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
	if m.backend.Shared() {
		m.syncOnMiss()
	}
	if filter.WebhookID != "" {
		m.mu.RLock()
		w, ok := m.webhooks[filter.WebhookID]
		owned := ok && w.TenantHash == tenant
		m.mu.RUnlock()
		if !owned {
			return nil, "", ErrNotFound
		}
	}

	m.mu.RLock()
	matched := make([]DeadLetter, 0)
	for i := len(m.deadLetters) - 1; i >= 0; i-- {
		d := m.deadLetters[i]
		if d.TenantHash != tenant {
			continue
		}
		if filter.WebhookID != "" && d.WebhookID != filter.WebhookID {
			continue
		}
		if filter.SandboxID != "" && d.SandboxID != filter.SandboxID {
			continue
		}
		if filter.Reason != "" && d.Reason != filter.Reason {
			continue
		}
		matched = append(matched, d.DeadLetter)
	}
	m.mu.RUnlock()

	page, next := paginate(matched, offset, limit)
	return page, next, nil
}

// RetryDeadLetter re-signs the dead-lettered event with the webhook's current key and
// re-enqueues it. The dead letter is removed; a failed retry re-records it under the same
// ID with RetryCount incremented.
func (m *Manager) RetryDeadLetter(apiKey, webhookID, deadLetterID string) (DeadLetter, error) {
	if strings.TrimSpace(apiKey) == "" {
		return DeadLetter{}, ErrUnauthorized
	}
	tenant := TenantHash(apiKey)
	if m.backend.Shared() {
		m.syncOnMiss()
	}
	w, err := m.lookupWebhook(tenant, webhookID)
	if err != nil {
		return DeadLetter{}, err
	}
	if !w.Enabled {
		return DeadLetter{}, ErrWebhookDisabled
	}
	rec, err := m.takeDeadLetter(tenant, webhookID, deadLetterID)
	if err != nil {
		return DeadLetter{}, err
	}
	payload, err := marshalEvent(rec.Event)
	if err != nil {
		m.clearInflightDelete(rec.ID)
		m.restoreDeadLetter(rec, false)
		return DeadLetter{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	if err := m.deleteDeadLetterBackend(ctx, rec); err != nil {
		m.restoreDeadLetter(rec, false)
		return DeadLetter{}, err
	}
	job := deliveryJob{
		webhookID:           w.ID,
		url:                 w.URL,
		signatureKey:        w.SignatureKey,
		event:               rec.Event,
		payload:             payload,
		deadLetterID:        rec.ID,
		deadLetterCreatedAt: rec.CreatedAt,
		retryCount:          rec.RetryCount + 1,
	}
	if !m.tryEnqueue(job) {
		gen := m.restoreDeadLetter(rec, true)
		m.enqueuePersist(func(ctx context.Context) {
			m.dlqMu.Lock()
			defer m.dlqMu.Unlock()
			m.mu.RLock()
			curGen := m.dlqGen[rec.ID]
			_, whExists := m.webhooks[rec.WebhookID]
			m.mu.RUnlock()
			if curGen != gen || !whExists {
				return
			}
			if err := m.backend.PutDeadLetter(ctx, rec); err != nil {
				slog.Warn("failed to re-persist dead letter after full queue", "deadLetterID", rec.ID, "err", err)
			}
			m.mu.Lock()
			if m.dlqGen[rec.ID] == gen {
				delete(m.pendingDeadLetters, rec.ID)
			}
			m.mu.Unlock()
		})
		return DeadLetter{}, ErrQueueFull
	}
	out := rec.DeadLetter
	out.RetryCount = job.retryCount
	return out, nil
}

// DeleteDeadLetter discards a dead letter owned by the tenant.
func (m *Manager) DeleteDeadLetter(apiKey, webhookID, deadLetterID string) error {
	if strings.TrimSpace(apiKey) == "" {
		return ErrUnauthorized
	}
	tenant := TenantHash(apiKey)
	if _, err := m.lookupWebhook(tenant, webhookID); err != nil {
		return err
	}
	rec, err := m.takeDeadLetter(tenant, webhookID, deadLetterID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), backendTimeout)
	defer cancel()
	if err := m.deleteDeadLetterBackend(ctx, rec); err != nil {
		m.restoreDeadLetter(rec, false)
		return err
	}
	return nil
}

func (m *Manager) clearInflightDelete(id string) {
	m.mu.Lock()
	if m.inflightDLQDelete[id] <= 1 {
		delete(m.inflightDLQDelete, id)
	} else {
		m.inflightDLQDelete[id]--
	}
	m.writeSeq.Add(1)
	m.mu.Unlock()
}

func (m *Manager) deleteDeadLetterBackend(ctx context.Context, rec StoredDeadLetter) error {
	m.dlqMu.Lock()
	err := m.backend.DeleteDeadLetter(ctx, rec)
	m.dlqMu.Unlock()
	m.clearInflightDelete(rec.ID)
	return err
}

// takeDeadLetter removes and returns the tenant's dead letter, reloading shared state once on a cache miss.
func (m *Manager) takeDeadLetter(tenant, webhookID, deadLetterID string) (StoredDeadLetter, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		for i, d := range m.deadLetters {
			if d.ID == deadLetterID && d.WebhookID == webhookID && d.TenantHash == tenant {
				m.deadLetters = append(m.deadLetters[:i:i], m.deadLetters[i+1:]...)
				m.dlqGen[d.ID]++
				delete(m.pendingDeadLetters, d.ID)
				m.inflightDLQDelete[d.ID]++
				m.writeSeq.Add(1)
				m.mu.Unlock()
				return d, nil
			}
		}
		m.mu.Unlock()
		if attempt == 0 && !m.syncOnMiss() {
			break
		}
	}
	return StoredDeadLetter{}, ErrDeadLetterNotFound
}

func (m *Manager) restoreDeadLetter(rec StoredDeadLetter, pending bool) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.webhooks[rec.WebhookID]; !ok {
		return 0
	}
	m.deadLetters = append(m.deadLetters, rec)
	sortDeadLetters(m.deadLetters)
	m.dlqGen[rec.ID]++
	gen := m.dlqGen[rec.ID]
	if pending {
		m.pendingDeadLetters[rec.ID] = struct{}{}
	}
	m.writeSeq.Add(1)
	return gen
}

func decodeOffsetCursor(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, ErrInvalidCursor
	}
	parsed, err := strconv.Atoi(string(raw))
	if err != nil || parsed < 0 {
		return 0, ErrInvalidCursor
	}
	return parsed, nil
}

func paginate[T any](items []T, offset, limit int) ([]T, string) {
	if offset >= len(items) {
		return []T{}, ""
	}
	end := offset + limit
	if end < len(items) {
		return items[offset:end], base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return items[offset:], ""
}
