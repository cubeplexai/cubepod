package events

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

const testKubeNamespace = "cubepod-workloads"

func openKubeManager(t *testing.T, kc controller.KubeClient, cfg Config) *Manager {
	t.Helper()
	backend, err := NewKubeBackend(kc, testKubeNamespace)
	if err != nil {
		t.Fatalf("NewKubeBackend: %v", err)
	}
	cfg.AllowPrivateIPs = true
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = 5 * time.Millisecond
	}
	m, err := OpenWithBackend(backend, cfg)
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestKubeBackendWebhookCRDAndSecretIsolation(t *testing.T) {
	kc := controller.NewMemoryKubeClient()
	replicaA := openKubeManager(t, kc, Config{})
	replicaB := openKubeManager(t, kc, Config{})

	created, err := replicaA.CreateWebhook("tenant-key-1", CreateWebhookInput{
		Name:   "billing",
		URL:    "https://example.com/hook",
		Events: []string{EventCreated},
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	crName := KubeWebhookName(created.ID)
	if !strings.HasPrefix(crName, "cubepod-whk-") || strings.Contains(crName, "_") {
		t.Fatalf("unexpected CR name %q", crName)
	}
	cr, err := kc.GetSandboxWebhook(t.Context(), testKubeNamespace, crName)
	if err != nil {
		t.Fatalf("GetSandboxWebhook: %v", err)
	}
	if cr.Metadata.Labels[ResourceLabel] != WebhookResourceValue || cr.Metadata.Labels[TenantLabel] != TenantLabelValue(TenantHash("tenant-key-1")) {
		t.Fatalf("unexpected CR labels: %v", cr.Metadata.Labels)
	}
	if cr.Spec.SecretName != KubeWebhookSecretName(created.ID) || cr.Spec.TenantHash != TenantHash("tenant-key-1") {
		t.Fatalf("unexpected CR spec: %+v", cr.Spec)
	}
	rawCR, _ := json.Marshal(cr)
	if strings.Contains(string(rawCR), created.SignatureKey) {
		t.Fatal("SandboxWebhook CR must never contain the plaintext signatureKey")
	}
	sec, err := kc.GetSecret(t.Context(), testKubeNamespace, cr.Spec.SecretName)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(sec.Data[SignatureKeyField]) != created.SignatureKey || len(sec.Data) != 1 {
		t.Fatalf("companion Secret must hold only signatureKey, got keys %d", len(sec.Data))
	}

	// Replica B serves the webhook via cache-miss reload (no periodic sync yet).
	got, err := replicaB.GetWebhook("tenant-key-1", created.ID)
	if err != nil || got.URL != "https://example.com/hook" {
		t.Fatalf("replica B GetWebhook = %+v, %v", got, err)
	}
	if _, err := replicaB.GetWebhook("tenant-key-2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GetWebhook err = %v; want ErrNotFound", err)
	}
	rotated, err := replicaB.UpdateWebhook("tenant-key-1", created.ID, UpdateWebhookInput{RotateSecret: true})
	if err != nil || rotated.SignatureKey == "" || rotated.SignatureKey == created.SignatureKey {
		t.Fatalf("rotate on replica B failed: %+v, %v", rotated, err)
	}
	if err := replicaA.Sync(t.Context()); err != nil {
		t.Fatalf("replica A Sync: %v", err)
	}
	replicaA.mu.RLock()
	keyOnA := replicaA.webhooks[created.ID].SignatureKey
	replicaA.mu.RUnlock()
	if keyOnA != rotated.SignatureKey {
		t.Fatal("replica A did not converge to rotated signatureKey after Sync")
	}

	if list, err := replicaA.ListWebhooks("tenant-key-1"); err != nil || len(list) != 1 {
		t.Fatalf("ListWebhooks = %d, %v; want 1", len(list), err)
	}
	if err := replicaA.DeleteWebhook("tenant-key-1", created.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if _, err := kc.GetSandboxWebhook(t.Context(), testKubeNamespace, crName); !errors.Is(err, controller.ErrKubeNotFound) {
		t.Fatalf("CR not deleted: %v", err)
	}
	if _, err := kc.GetSecret(t.Context(), testKubeNamespace, cr.Spec.SecretName); err == nil {
		t.Fatal("companion Secret not deleted")
	}
	if err := replicaB.Sync(t.Context()); err != nil {
		t.Fatalf("replica B Sync: %v", err)
	}
	if _, err := replicaB.GetWebhook("tenant-key-1", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replica B still serves deleted webhook: %v", err)
	}
}

func TestKubeBackendEventsSharedAcrossReplicas(t *testing.T) {
	kc := controller.NewMemoryKubeClient()
	replicaA := openKubeManager(t, kc, Config{})
	replicaB := openKubeManager(t, kc, Config{})
	tenant := TenantHash("tenant-key-1")

	ev := replicaA.Emit(tenant, EventCreated, "cpod-ha-1", "base", "", "", nil)
	replicaA.Emit(TenantHash("tenant-key-2"), EventCreated, "cpod-other", "base", "", "", nil)

	items, _, err := replicaB.ListEvents("tenant-key-1", "", "", 10, "")
	if err != nil || len(items) != 1 || items[0].ID != ev.ID {
		t.Fatalf("replica B ListEvents = %+v, %v; want event %s", items, err, ev.ID)
	}
	if items, _, _ := replicaB.ListEvents("tenant-key-1", "cpod-ha-1", "", 10, ""); len(items) != 1 {
		t.Fatalf("sandbox-scoped ListEvents = %d; want 1", len(items))
	}
	if items, _, _ := replicaB.ListEvents("tenant-key-3", "", "", 10, ""); len(items) != 0 {
		t.Fatalf("tenant isolation broken: %d events", len(items))
	}

	kubeEvents, _ := kc.ListKubeEvents(t.Context(), testKubeNamespace, "cubepod.io/resource=sandbox-event,cubepod.io/sandbox=cpod-ha-1")
	if len(kubeEvents) != 1 {
		t.Fatalf("expected 1 labeled K8s Event, got %d", len(kubeEvents))
	}
	ke := kubeEvents[0]
	if ke.InvolvedObject.Kind != "PodBox" || ke.InvolvedObject.Name != "cpod-ha-1" || ke.InvolvedObject.Namespace != testKubeNamespace {
		t.Fatalf("unexpected involvedObject: %+v", ke.InvolvedObject)
	}
	if ke.Metadata.Labels[TenantLabel] != TenantLabelValue(tenant) || ke.Metadata.Labels[SourceLabel] != GatewaySourceValue || ke.Reason != "SandboxCreated" {
		t.Fatalf("unexpected K8s Event metadata: labels=%v reason=%s", ke.Metadata.Labels, ke.Reason)
	}
}

func TestKubeBackendObservedEventDeliveredOnce(t *testing.T) {
	var deliveries atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deliveries.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	kc := controller.NewMemoryKubeClient()
	replicaA := openKubeManager(t, kc, Config{})
	replicaB := openKubeManager(t, kc, Config{})
	if _, err := replicaA.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "hook", URL: receiver.URL, Events: []string{EventPaused}}); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if err := replicaB.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tenant := TenantHash("tenant-key-1")
	evA, recordedA := replicaA.EmitObserved(tenant, EventPaused, "cpod-1", "base", "Paused|IdlePaused|123", nil)
	evB, recordedB := replicaB.EmitObserved(tenant, EventPaused, "cpod-1", "base", "Paused|IdlePaused|123", nil)
	if !recordedA || recordedB || evA.ID != evB.ID {
		t.Fatalf("dedupe failed: recordedA=%v recordedB=%v idA=%s idB=%s", recordedA, recordedB, evA.ID, evB.ID)
	}
	waitFor(t, "delivery", func() bool { return replicaA.WebhookDeliveriesOK.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if deliveries.Load() != 1 || replicaB.WebhookDeliveriesOK.Load() != 0 {
		t.Fatalf("observed transition delivered %d times (B ok=%d); want exactly once", deliveries.Load(), replicaB.WebhookDeliveriesOK.Load())
	}
}

func TestKubeBackendDeliveryStatusAndDeadLetterRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var received atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	kc := controller.NewMemoryKubeClient()
	replicaA := openKubeManager(t, kc, Config{MaxRetries: 2})
	replicaB := openKubeManager(t, kc, Config{MaxRetries: 2})
	wh, err := replicaA.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "hook", URL: receiver.URL, Events: []string{"*"}})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	ev := replicaA.Emit(TenantHash("tenant-key-1"), EventKilled, "cpod-dlq", "base", "", "", nil)

	crName := KubeWebhookName(wh.ID)
	waitFor(t, "dead letter persisted to CR status", func() bool {
		cr, err := kc.GetSandboxWebhook(t.Context(), testKubeNamespace, crName)
		return err == nil && len(cr.Status.DeadLetters) == 1 && cr.Status.ConsecutiveFailures == 1
	})
	cr, _ := kc.GetSandboxWebhook(t.Context(), testKubeNamespace, crName)
	dl := cr.Status.DeadLetters[0]
	if dl.Reason != DeadLetterReasonDeliveryFailed || dl.Attempts != 2 || dl.LastStatusCode != 500 || dl.EventID != ev.ID || dl.LastError != "http_status" {
		t.Fatalf("unexpected dead letter: %+v", dl)
	}
	rawCR, _ := json.Marshal(cr)
	if strings.Contains(string(rawCR), wh.SignatureKey) {
		t.Fatal("dead letter status leaked signatureKey")
	}
	if replicaA.DeadLettersTotal.Load() != 1 || replicaA.WebhookDeliveriesFailed.Load() != 1 {
		t.Fatalf("metrics: deadLetters=%d failed=%d", replicaA.DeadLettersTotal.Load(), replicaA.WebhookDeliveriesFailed.Load())
	}

	// Replica B inspects via shared state and retries after the receiver recovers.
	if err := replicaB.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	items, _, err := replicaB.ListDeadLetters("tenant-key-1", DeadLetterFilter{WebhookID: wh.ID}, 10, "")
	if err != nil || len(items) != 1 || items[0].ID != dl.ID {
		t.Fatalf("replica B ListDeadLetters = %+v, %v", items, err)
	}
	raw, _ := json.Marshal(items[0])
	if strings.Contains(string(raw), "signatureKey") || strings.Contains(string(raw), receiver.URL) || strings.Contains(string(raw), "tenantHash") {
		t.Fatalf("dead letter API view leaks sensitive fields: %s", raw)
	}
	if _, err := replicaB.RetryDeadLetter("tenant-key-2", wh.ID, dl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant retry err = %v; want ErrNotFound", err)
	}
	fail.Store(false)
	retried, err := replicaB.RetryDeadLetter("tenant-key-1", wh.ID, dl.ID)
	if err != nil || retried.RetryCount != 1 {
		t.Fatalf("RetryDeadLetter = %+v, %v", retried, err)
	}
	waitFor(t, "retried delivery and status reset", func() bool {
		cr, err := kc.GetSandboxWebhook(t.Context(), testKubeNamespace, crName)
		return err == nil && received.Load() == 1 && len(cr.Status.DeadLetters) == 0 &&
			cr.Status.ConsecutiveFailures == 0 && cr.Status.LastDeliveryTime != nil
	})
	info, err := replicaB.GetWebhook("tenant-key-1", wh.ID)
	if err != nil || info.LastDeliveryTime == nil || info.ConsecutiveFailures != 0 {
		t.Fatalf("webhook info status not updated: %+v, %v", info, err)
	}
}

// NewKubeBackendForTest exposes a KubeBackend backed by MemoryKubeClient for manager_test.go.
func NewKubeBackendForTest(t *testing.T) *KubeBackend {
	t.Helper()
	kb, err := NewKubeBackend(controller.NewMemoryKubeClient(), testKubeNamespace)
	if err != nil {
		t.Fatalf("NewKubeBackend: %v", err)
	}
	return kb
}
