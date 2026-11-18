package events

import (
	"context"
	"errors"
	"time"
)

// ErrDuplicateEvent is returned by Backend.RecordEvent when an event with the same
// deterministic ID was already recorded (for example by another gateway replica).
// The Manager then skips webhook delivery so receivers are not notified twice.
var ErrDuplicateEvent = errors.New("lifecycle event already recorded")

// StoredWebhook is the durable webhook record persisted by a Backend. SignatureKey is
// only ever written to an encrypted local file or an isolated Kubernetes Secret.
type StoredWebhook struct {
	TenantHash          string     `json:"tenantHash"`
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Enabled             bool       `json:"enabled"`
	SignatureKey        string     `json:"signatureKey"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	LastDeliveryTime    *time.Time `json:"lastDeliveryTime,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty"`

	// statusPersistedAt tracks when delivery status was last flushed to the Backend so
	// high-volume deliveries do not issue one Backend write per event.
	statusPersistedAt time.Time
}

// WebhookStatus carries delivery health persisted via Backend.UpdateWebhookStatus.
type WebhookStatus struct {
	LastDeliveryTime    *time.Time
	ConsecutiveFailures int
}

// StoredEvent is a tenant-scoped lifecycle event.
type StoredEvent struct {
	TenantHash string         `json:"tenantHash"`
	Event      LifecycleEvent `json:"event"`
}

// State is the durable snapshot loaded from a Backend on startup and on every Sync.
type State struct {
	Webhooks    []StoredWebhook
	Events      []StoredEvent
	DeadLetters []StoredDeadLetter
}

// Backend abstracts durable persistence for webhooks, lifecycle events, and dead letters.
//
// FileBackend stores everything under a node-local directory (AES-256-GCM encrypted
// webhooks.enc.json, events.json, dead_letters.json). KubeBackend stores webhook
// configuration/status in SandboxWebhook CRs, isolates the HMAC signatureKey in a
// companion Kubernetes Secret, and records lifecycle events as labeled core/v1 Events on
// the PodBox so multiple gateway replicas share state without an external database.
type Backend interface {
	// Name returns the backend identifier ("file" or "k8s").
	Name() string
	// Shared reports whether state is shared across gateway replicas, enabling Sync on
	// cache misses and cross-replica event queries.
	Shared() bool
	// LoadState returns the full durable snapshot.
	LoadState(ctx context.Context) (State, error)

	PutWebhook(ctx context.Context, rec StoredWebhook) error
	DeleteWebhook(ctx context.Context, id string) error
	UpdateWebhookStatus(ctx context.Context, id string, status WebhookStatus) error

	// RecordEvent durably records one event. It returns ErrDuplicateEvent when the
	// event ID was already recorded.
	RecordEvent(ctx context.Context, ev StoredEvent) error
	// SaveEvents persists the gateway-local bounded event ring (no-op for shared backends).
	SaveEvents(ctx context.Context, events []StoredEvent) error
	// QueryEvents returns shared events for a tenant (optionally one sandbox). Backends
	// that are not Shared return nil.
	QueryEvents(ctx context.Context, tenantHash, sandboxID string) ([]StoredEvent, error)

	PutDeadLetter(ctx context.Context, rec StoredDeadLetter) error
	DeleteDeadLetter(ctx context.Context, rec StoredDeadLetter) error
}
