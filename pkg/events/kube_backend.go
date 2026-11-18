package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

// Kubernetes labels, annotations, and name prefixes used by KubeBackend.
const (
	ResourceLabel              = "cubepod.io/resource"
	TenantLabel                = "cubepod.io/tenant"
	SandboxLabel               = "cubepod.io/sandbox"
	SourceLabel                = "cubepod.io/source"
	WebhookResourceValue       = "sandbox-webhook"
	WebhookSecretResourceValue = "sandbox-webhook-secret"
	EventResourceValue         = "sandbox-event"
	GatewaySourceValue         = "gateway"

	WebhookIDAnnotation      = "cubepod.io/webhook-id"
	CreatedAtAnnotation      = "cubepod.io/created-at"
	UpdatedAtAnnotation      = "cubepod.io/updated-at"
	TenantHashAnnotation     = "cubepod.io/tenant-hash"
	LifecycleEventAnnotation = "cubepod.io/lifecycle-event"

	WebhookNamePrefix       = "cubepod-whk-"
	WebhookSecretNamePrefix = "cubepod-whk-sec-"
	// SignatureKeyField is the only data key stored in the companion webhook Secret.
	SignatureKeyField = "signatureKey"

	kubeStatusRetries = 3
)

var (
	labelValuePattern = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
	dnsUnsafeChars    = regexp.MustCompile(`[^a-z0-9-]+`)
)

// KubeBackend persists webhooks as SandboxWebhook CRs (configuration + delivery status +
// bounded dead letters), isolates each HMAC signatureKey in a companion Kubernetes Secret
// referenced via spec.secretName, and records lifecycle events as labeled core/v1 Events
// attached to the sandbox PodBox. Multiple gateway replicas share this state without an
// external database.
type KubeBackend struct {
	client                controller.KubeClient
	namespace             string
	maxDeadLettersPerHook int
}

// NewKubeBackend creates a Kubernetes backend targeting namespace (default "cubepod-workloads").
func NewKubeBackend(client controller.KubeClient, namespace string) (*KubeBackend, error) {
	if client == nil {
		return nil, errors.New("kube client is required")
	}
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = "cubepod-workloads"
	}
	return &KubeBackend{client: client, namespace: ns, maxDeadLettersPerHook: DefaultMaxDeadLettersPerWebhook}, nil
}

// Name implements Backend.
func (b *KubeBackend) Name() string { return "k8s" }

// Shared implements Backend; Kubernetes state is shared across gateway replicas.
func (b *KubeBackend) Shared() bool { return true }

func kubeIDSuffix(id string) string {
	clean := strings.ToLower(strings.TrimSpace(id))
	clean = strings.TrimPrefix(clean, "whk_")
	clean = strings.ReplaceAll(clean, "_", "-")
	return dnsUnsafeChars.ReplaceAllString(clean, "-")
}

// KubeWebhookName returns the SandboxWebhook CR name for a webhook ID (cubepod-whk-<id>).
func KubeWebhookName(id string) string {
	return WebhookNamePrefix + kubeIDSuffix(id)
}

// KubeWebhookSecretName returns the companion Secret name for a webhook ID (cubepod-whk-sec-<id>).
func KubeWebhookSecretName(id string) string {
	return WebhookSecretNamePrefix + kubeIDSuffix(id)
}

// TenantLabelValue returns the 16-character tenant hash prefix used in cubepod.io/tenant labels.
func TenantLabelValue(tenantHash string) string {
	h := strings.ToLower(strings.TrimSpace(tenantHash))
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// SandboxLabelValue returns sandboxID when it is a valid Kubernetes label value, else "".
func SandboxLabelValue(sandboxID string) string {
	if len(sandboxID) == 0 || len(sandboxID) > 63 || !labelValuePattern.MatchString(sandboxID) {
		return ""
	}
	return sandboxID
}

// KubeEventName returns the deterministic core/v1 Event name for a lifecycle event so
// replicas recording the same (deterministic) event ID collide with 409 AlreadyExists.
func KubeEventName(sandboxID, eventID string) string {
	sum := sha256.Sum256([]byte(eventID))
	prefix := strings.Trim(dnsUnsafeChars.ReplaceAllString(strings.ToLower(sandboxID), "-"), "-")
	if len(prefix) > 40 {
		prefix = strings.Trim(prefix[:40], "-")
	}
	if prefix == "" {
		prefix = "cubepod-sandbox"
	}
	return prefix + "." + hex.EncodeToString(sum[:8])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// LoadState lists SandboxWebhook CRs and their companion Secrets, returning webhooks and
// dead letters. Lifecycle events are not preloaded; they are queried on demand.
func (b *KubeBackend) LoadState(ctx context.Context) (State, error) {
	crs, err := b.client.ListSandboxWebhooks(ctx, b.namespace, ResourceLabel+"="+WebhookResourceValue)
	if err != nil {
		return State{}, fmt.Errorf("list SandboxWebhooks: %w", err)
	}
	secrets, err := b.client.ListSecrets(ctx, b.namespace, ResourceLabel+"="+WebhookSecretResourceValue)
	if err != nil {
		return State{}, fmt.Errorf("list webhook secrets: %w", err)
	}
	keys := make(map[string]string, len(secrets))
	for _, sec := range secrets {
		if raw, ok := sec.Data[SignatureKeyField]; ok && len(raw) > 0 {
			keys[sec.Metadata.Name] = string(raw)
		}
	}

	var st State
	for _, cr := range crs {
		id := cr.Metadata.Annotations[WebhookIDAnnotation]
		if id == "" || cr.Spec.TenantHash == "" {
			slog.Warn("skipping SandboxWebhook without webhook-id annotation or tenantHash", "name", cr.Metadata.Name)
			continue
		}
		key, ok := keys[cr.Spec.SecretName]
		if !ok {
			slog.Warn("skipping SandboxWebhook whose signatureKey Secret is missing", "name", cr.Metadata.Name, "secretName", cr.Spec.SecretName)
			continue
		}
		rec := StoredWebhook{
			TenantHash:          cr.Spec.TenantHash,
			ID:                  id,
			Name:                cr.Spec.Name,
			URL:                 cr.Spec.URL,
			Events:              append([]string(nil), cr.Spec.Events...),
			Enabled:             cr.Spec.Enabled,
			SignatureKey:        key,
			CreatedAt:           parseTime(cr.Metadata.Annotations[CreatedAtAnnotation]),
			UpdatedAt:           parseTime(cr.Metadata.Annotations[UpdatedAtAnnotation]),
			ConsecutiveFailures: cr.Status.ConsecutiveFailures,
		}
		if cr.Status.LastDeliveryTime != nil {
			t := cr.Status.LastDeliveryTime.UTC()
			rec.LastDeliveryTime = &t
		}
		st.Webhooks = append(st.Webhooks, rec)
		for _, dl := range cr.Status.DeadLetters {
			d, ok := fromKubeDeadLetter(rec.TenantHash, id, dl)
			if ok {
				st.DeadLetters = append(st.DeadLetters, d)
			}
		}
	}
	sortDeadLetters(st.DeadLetters)
	return st, nil
}

func fromKubeDeadLetter(tenant, webhookID string, dl v1alpha1.SandboxWebhookDeadLetter) (StoredDeadLetter, bool) {
	var ev LifecycleEvent
	if len(dl.Event) > 0 {
		if err := json.Unmarshal(dl.Event, &ev); err != nil {
			slog.Warn("skipping dead letter with malformed event payload", "deadLetterID", dl.ID, "err", err)
			return StoredDeadLetter{}, false
		}
	}
	return StoredDeadLetter{
		TenantHash: tenant,
		DeadLetter: DeadLetter{
			ID:             dl.ID,
			WebhookID:      webhookID,
			EventID:        dl.EventID,
			EventType:      dl.EventType,
			SandboxID:      dl.SandboxID,
			Reason:         dl.Reason,
			Attempts:       dl.Attempts,
			LastStatusCode: dl.LastStatusCode,
			LastError:      dl.LastError,
			RetryCount:     dl.RetryCount,
			CreatedAt:      dl.CreatedAt.UTC(),
			UpdatedAt:      dl.UpdatedAt.UTC(),
			Event:          ev,
		},
	}, true
}

func toKubeDeadLetter(d StoredDeadLetter) (v1alpha1.SandboxWebhookDeadLetter, error) {
	raw, err := json.Marshal(d.Event)
	if err != nil {
		return v1alpha1.SandboxWebhookDeadLetter{}, err
	}
	return v1alpha1.SandboxWebhookDeadLetter{
		ID:             d.ID,
		EventID:        d.EventID,
		EventType:      d.EventType,
		SandboxID:      d.SandboxID,
		Reason:         d.Reason,
		Attempts:       d.Attempts,
		LastStatusCode: d.LastStatusCode,
		LastError:      d.LastError,
		RetryCount:     d.RetryCount,
		CreatedAt:      d.CreatedAt,
		UpdatedAt:      d.UpdatedAt,
		Event:          raw,
	}, nil
}

// PutWebhook writes the signatureKey Secret first, then creates or updates metadata/spec
// of the SandboxWebhook CR without touching /status, so concurrent delivery status and
// dead-letter updates from other replicas are never clobbered.
func (b *KubeBackend) PutWebhook(ctx context.Context, rec StoredWebhook) error {
	secretName := KubeWebhookSecretName(rec.ID)
	crName := KubeWebhookName(rec.ID)
	tenantLabel := TenantLabelValue(rec.TenantHash)

	_, err := b.client.GetSandboxWebhook(ctx, b.namespace, crName)
	if err != nil && !errors.Is(err, controller.ErrKubeNotFound) {
		return fmt.Errorf("get SandboxWebhook %s: %w", crName, err)
	}
	isNew := errors.Is(err, controller.ErrKubeNotFound)

	sec := controller.KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata: v1alpha1.ObjectMeta{
			Name:      secretName,
			Namespace: b.namespace,
			Labels: map[string]string{
				ResourceLabel: WebhookSecretResourceValue,
				TenantLabel:   tenantLabel,
			},
			Annotations: map[string]string{WebhookIDAnnotation: rec.ID},
		},
		Data: map[string][]byte{SignatureKeyField: []byte(rec.SignatureKey)},
	}
	if err := b.client.EnsureSecret(ctx, sec); err != nil {
		return fmt.Errorf("ensure webhook secret %s: %w", secretName, err)
	}

	cr := v1alpha1.SandboxWebhook{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "SandboxWebhook",
		Metadata: v1alpha1.ObjectMeta{
			Name:      crName,
			Namespace: b.namespace,
			Labels: map[string]string{
				ResourceLabel: WebhookResourceValue,
				TenantLabel:   tenantLabel,
			},
			Annotations: map[string]string{
				WebhookIDAnnotation: rec.ID,
				CreatedAtAnnotation: formatTime(rec.CreatedAt),
				UpdatedAtAnnotation: formatTime(rec.UpdatedAt),
			},
		},
		Spec: v1alpha1.SandboxWebhookSpec{
			TenantHash: rec.TenantHash,
			Name:       rec.Name,
			URL:        rec.URL,
			Events:     append([]string(nil), rec.Events...),
			Enabled:    rec.Enabled,
			SecretName: secretName,
		},
	}
	if err := b.client.UpsertSandboxWebhook(ctx, cr); err != nil {
		if isNew {
			if delErr := b.client.DeleteSecret(ctx, b.namespace, secretName); delErr != nil {
				slog.Warn("failed to clean up webhook secret after CR failure", "secretName", secretName, "err", delErr)
			}
		}
		return fmt.Errorf("upsert SandboxWebhook %s: %w", crName, err)
	}
	return nil
}

// DeleteWebhook removes the SandboxWebhook CR (and with it the dead letters) and its Secret.
func (b *KubeBackend) DeleteWebhook(ctx context.Context, id string) error {
	if err := b.client.DeleteSandboxWebhook(ctx, b.namespace, KubeWebhookName(id)); err != nil {
		return fmt.Errorf("delete SandboxWebhook: %w", err)
	}
	if err := b.client.DeleteSecret(ctx, b.namespace, KubeWebhookSecretName(id)); err != nil {
		return fmt.Errorf("delete webhook secret: %w", err)
	}
	return nil
}

// mutateStatus applies fn to the latest SandboxWebhook status and writes it back with
// optimistic concurrency, retrying on resourceVersion conflicts. A missing CR is a no-op.
func (b *KubeBackend) mutateStatus(ctx context.Context, id string, fn func(*v1alpha1.SandboxWebhookStatus) bool) error {
	name := KubeWebhookName(id)
	var lastErr error
	for attempt := 0; attempt < kubeStatusRetries; attempt++ {
		cr, err := b.client.GetSandboxWebhook(ctx, b.namespace, name)
		if errors.Is(err, controller.ErrKubeNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fn(&cr.Status) {
			return nil
		}
		err = b.client.UpdateSandboxWebhookStatus(ctx, *cr)
		if err == nil || errors.Is(err, controller.ErrKubeNotFound) {
			return nil
		}
		if !errors.Is(err, controller.ErrCASConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// UpdateWebhookStatus writes LastDeliveryTime and ConsecutiveFailures to the CR /status.
func (b *KubeBackend) UpdateWebhookStatus(ctx context.Context, id string, status WebhookStatus) error {
	return b.mutateStatus(ctx, id, func(st *v1alpha1.SandboxWebhookStatus) bool {
		if status.LastDeliveryTime != nil && (st.LastDeliveryTime == nil || status.LastDeliveryTime.After(*st.LastDeliveryTime)) {
			t := status.LastDeliveryTime.UTC()
			st.LastDeliveryTime = &t
		}
		st.ConsecutiveFailures = status.ConsecutiveFailures
		return true
	})
}

// PutDeadLetter upserts a dead letter into the webhook's CR status, keeping at most
// maxDeadLettersPerHook (oldest evicted first).
func (b *KubeBackend) PutDeadLetter(ctx context.Context, rec StoredDeadLetter) error {
	item, err := toKubeDeadLetter(rec)
	if err != nil {
		return err
	}
	return b.mutateStatus(ctx, rec.WebhookID, func(st *v1alpha1.SandboxWebhookStatus) bool {
		out := make([]v1alpha1.SandboxWebhookDeadLetter, 0, len(st.DeadLetters)+1)
		for _, d := range st.DeadLetters {
			if d.ID != item.ID {
				out = append(out, d)
			}
		}
		out = append(out, item)
		if over := len(out) - b.maxDeadLettersPerHook; over > 0 {
			out = out[over:]
		}
		st.DeadLetters = out
		return true
	})
}

// DeleteDeadLetter removes a dead letter from the webhook's CR status.
func (b *KubeBackend) DeleteDeadLetter(ctx context.Context, rec StoredDeadLetter) error {
	return b.mutateStatus(ctx, rec.WebhookID, func(st *v1alpha1.SandboxWebhookStatus) bool {
		out := make([]v1alpha1.SandboxWebhookDeadLetter, 0, len(st.DeadLetters))
		for _, d := range st.DeadLetters {
			if d.ID != rec.ID {
				out = append(out, d)
			}
		}
		if len(out) == len(st.DeadLetters) {
			return false
		}
		st.DeadLetters = out
		return true
	})
}

func eventReason(eventType string) string {
	label := strings.TrimPrefix(eventType, "sandbox.lifecycle.")
	if label == "" {
		return "SandboxLifecycle"
	}
	return "Sandbox" + strings.ToUpper(label[:1]) + label[1:]
}

// RecordEvent creates a labeled core/v1 Event attached to the sandbox PodBox. A 409
// AlreadyExists (same deterministic name) is reported as ErrDuplicateEvent.
func (b *KubeBackend) RecordEvent(ctx context.Context, se StoredEvent) error {
	payload, err := json.Marshal(se.Event)
	if err != nil {
		return err
	}
	labels := map[string]string{
		ResourceLabel: EventResourceValue,
		TenantLabel:   TenantLabelValue(se.TenantHash),
		SourceLabel:   GatewaySourceValue,
	}
	if v := SandboxLabelValue(se.Event.SandboxID); v != "" {
		labels[SandboxLabel] = v
	}
	ts := se.Event.Timestamp.UTC()
	msg := fmt.Sprintf("Sandbox %s lifecycle event %s", se.Event.SandboxID, se.Event.Type)
	if se.Event.EventData != nil && se.Event.EventData.Reason != "" {
		msg += ": " + se.Event.EventData.Reason
	}
	ev := controller.KubeEvent{
		APIVersion: "v1",
		Kind:       "Event",
		Metadata: v1alpha1.ObjectMeta{
			Name:      KubeEventName(se.Event.SandboxID, se.Event.ID),
			Namespace: b.namespace,
			Labels:    labels,
			Annotations: map[string]string{
				TenantHashAnnotation:     se.TenantHash,
				LifecycleEventAnnotation: string(payload),
			},
		},
		InvolvedObject: controller.KubeObjectReference{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Name:       se.Event.SandboxID,
			Namespace:  b.namespace,
		},
		Reason:         eventReason(se.Event.Type),
		Message:        msg,
		Type:           controller.KubeEventTypeNormal,
		Source:         controller.KubeEventSource{Component: "cubepod-gateway"},
		FirstTimestamp: &ts,
		LastTimestamp:  &ts,
		Count:          1,
	}
	if err := b.client.CreateKubeEvent(ctx, ev); err != nil {
		if errors.Is(err, controller.ErrKubeAlreadyExists) {
			return ErrDuplicateEvent
		}
		return err
	}
	return nil
}

// SaveEvents is a no-op: Kubernetes Events are the shared durable record.
func (b *KubeBackend) SaveEvents(context.Context, []StoredEvent) error { return nil }

// QueryEvents lists the tenant's labeled lifecycle Events (optionally one sandbox),
// verifying the full tenant hash annotation to guard against label-prefix collisions.
func (b *KubeBackend) QueryEvents(ctx context.Context, tenantHash, sandboxID string) ([]StoredEvent, error) {
	selector := ResourceLabel + "=" + EventResourceValue + "," + TenantLabel + "=" + TenantLabelValue(tenantHash)
	if v := SandboxLabelValue(sandboxID); v != "" {
		selector += "," + SandboxLabel + "=" + v
	}
	items, err := b.client.ListKubeEvents(ctx, b.namespace, selector)
	if err != nil {
		return nil, fmt.Errorf("list lifecycle events: %w", err)
	}
	out := make([]StoredEvent, 0, len(items))
	for _, item := range items {
		if item.Metadata.Annotations[TenantHashAnnotation] != tenantHash {
			continue
		}
		raw := item.Metadata.Annotations[LifecycleEventAnnotation]
		if raw == "" {
			continue
		}
		var ev LifecycleEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			slog.Warn("skipping lifecycle Event with malformed payload", "name", item.Metadata.Name, "err", err)
			continue
		}
		if sandboxID != "" && ev.SandboxID != sandboxID {
			continue
		}
		out = append(out, StoredEvent{TenantHash: tenantHash, Event: ev})
	}
	return out, nil
}
