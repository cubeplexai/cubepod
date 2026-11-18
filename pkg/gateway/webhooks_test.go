package gateway_test

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/events"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func newEventsTestServer(t *testing.T, allowPrivate bool) *gateway.Server {
	t.Helper()
	kube := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(
		controller.WithKubeClient(kube),
		controller.WithNamespace("default"),
	)
	srv := gateway.NewServer(rec)
	srv.SetWorkspaceBaseDir(t.TempDir())
	if err := srv.ConfigureEvents(gateway.E2BEventsConfig{
		Dir:             t.TempDir(),
		AllowPrivateIPs: allowPrivate,
	}); err != nil {
		t.Fatalf("ConfigureEvents: %v", err)
	}
	t.Cleanup(srv.CloseEvents)
	return srv
}

func TestWebhooksCRUDAndEventsEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var delivered []events.LifecycleEvent
	var sigHeaders []string

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev events.LifecycleEvent
		if err := json.Unmarshal(body, &ev); err == nil {
			mu.Lock()
			delivered = append(delivered, ev)
			sigHeaders = append(sigHeaders, r.Header.Get("e2b-signature"))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	srv := newEventsTestServer(t, true)
	h := srv.Handler()

	// 1. Register webhook
	rrWh := doJSONReq(t, h, http.MethodPost, "/webhooks", "tenant-a", map[string]any{
		"name":   "lifecycle-hook",
		"url":    receiver.URL,
		"events": []string{"*"},
	})
	if rrWh.Code != http.StatusCreated {
		t.Fatalf("POST /webhooks = %d (%s); want 201", rrWh.Code, rrWh.Body.String())
	}
	var whCreated events.WebhookWithSecret
	if err := json.Unmarshal(rrWh.Body.Bytes(), &whCreated); err != nil {
		t.Fatalf("unmarshal webhook: %v", err)
	}
	if whCreated.SignatureKey == "" {
		t.Fatal("expected signatureKey on creation")
	}

	// 2. GET /webhooks and GET /webhooks/{id} must NOT expose signatureKey
	rrList := doJSONReq(t, h, http.MethodGet, "/webhooks", "tenant-a", nil)
	if rrList.Code != http.StatusOK || len(rrList.Body.Bytes()) == 0 {
		t.Fatalf("GET /webhooks = %d", rrList.Code)
	}
	if bytesContains(rrList.Body.Bytes(), whCreated.SignatureKey) {
		t.Fatal("GET /webhooks leaked signatureKey")
	}
	if rrCross := doJSONReq(t, h, http.MethodGet, "/webhooks/"+whCreated.ID, "tenant-b", nil); rrCross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET /webhooks/{id} = %d; want 404", rrCross.Code)
	}

	// 3. Run full sandbox lifecycle: create -> pause -> connect -> timeout -> delete
	rrCreate := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-a", map[string]any{"templateID": "base"})
	if rrCreate.Code != http.StatusCreated {
		t.Fatalf("POST /sandboxes = %d (%s)", rrCreate.Code, rrCreate.Body.String())
	}
	var sbx struct {
		SandboxID string `json:"sandboxID"`
	}
	_ = json.Unmarshal(rrCreate.Body.Bytes(), &sbx)

	if rr := doJSONReq(t, h, http.MethodPost, "/sandboxes/"+sbx.SandboxID+"/pause", "tenant-a", nil); rr.Code != http.StatusOK {
		t.Fatalf("pause = %d", rr.Code)
	}
	if rr := doJSONReq(t, h, http.MethodPost, "/sandboxes/"+sbx.SandboxID+"/connect", "tenant-a", nil); rr.Code != http.StatusOK {
		t.Fatalf("connect = %d", rr.Code)
	}
	if rr := doJSONReq(t, h, http.MethodPost, "/sandboxes/"+sbx.SandboxID+"/timeout", "tenant-a", map[string]int{"timeoutSeconds": 600}); rr.Code != http.StatusOK {
		t.Fatalf("timeout = %d", rr.Code)
	}
	if rr := doJSONReq(t, h, http.MethodDelete, "/sandboxes/"+sbx.SandboxID, "tenant-a", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rr.Code)
	}

	// 4. Verify GET /sandboxes/{id}/events & GET /events pagination
	rrSbxEv := doJSONReq(t, h, http.MethodGet, "/sandboxes/"+sbx.SandboxID+"/events?limit=3", "tenant-a", nil)
	if rrSbxEv.Code != http.StatusOK || rrSbxEv.Header().Get("X-Next-Token") == "" {
		t.Fatalf("GET /sandboxes/{id}/events = %d next=%q", rrSbxEv.Code, rrSbxEv.Header().Get("X-Next-Token"))
	}
	var evPage1 []events.LifecycleEvent
	_ = json.Unmarshal(rrSbxEv.Body.Bytes(), &evPage1)
	if len(evPage1) != 3 || evPage1[0].Version != "v1" || evPage1[0].Type != events.EventKilled {
		t.Fatalf("unexpected page 1 events: %+v", evPage1)
	}

	// 5. Wait for all 5 async webhook deliveries
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n >= 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 5 {
		t.Fatalf("expected 5 delivered webhook events, got %d", len(delivered))
	}
	if sigHeaders[0] == "" || hmac.Equal([]byte(sigHeaders[0]), []byte(whCreated.SignatureKey)) {
		t.Fatalf("unexpected e2b-signature header: %q", sigHeaders[0])
	}

	// 6. PATCH rotateSecret & DELETE + verify Prometheus metrics
	rrMetrics := doJSONReq(t, h, http.MethodGet, "/metrics", "", nil)
	if rrMetrics.Code != http.StatusOK || !bytesContains(rrMetrics.Body.Bytes(), "cubepod_events_recorded_total 5") || !bytesContains(rrMetrics.Body.Bytes(), "cubepod_webhook_deliveries_total{result=\"ok\"} 5") {
		t.Fatalf("unexpected /metrics output:\n%s", rrMetrics.Body.String())
	}
	rrPatch := doJSONReq(t, h, http.MethodPatch, "/webhooks/"+whCreated.ID, "tenant-a", map[string]any{"rotateSecret": true})
	if rrPatch.Code != http.StatusOK {
		t.Fatalf("PATCH /webhooks/{id} = %d", rrPatch.Code)
	}
	if rrDel := doJSONReq(t, h, http.MethodDelete, "/webhooks/"+whCreated.ID, "tenant-a", nil); rrDel.Code != http.StatusNoContent {
		t.Fatalf("DELETE /webhooks/{id} = %d", rrDel.Code)
	}
}

func bytesContains(b []byte, sub string) bool {
	return len(sub) > 0 && len(b) >= len(sub) && stringContains(string(b), sub)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestAsyncK8sIdleReaperEventsResolveAndSyncNoDuplicates(t *testing.T) {
	kube := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(
		controller.WithKubeClient(kube),
		controller.WithNamespace("default"),
	)
	srv := gateway.NewServer(rec)
	srv.SetWorkspaceBaseDir(t.TempDir())
	if err := srv.ConfigureEvents(gateway.E2BEventsConfig{
		Dir:             t.TempDir(),
		AllowPrivateIPs: true,
	}); err != nil {
		t.Fatalf("ConfigureEvents: %v", err)
	}
	srv.SetPodBoxSyncInterval(20 * time.Millisecond)
	srv.SetLivePodRouting(true)
	t.Cleanup(srv.Close)

	h := srv.Handler()

	createSbx := func() string {
		t.Helper()
		rr := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-a", map[string]any{"templateID": "base"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("POST /sandboxes = %d (%s)", rr.Code, rr.Body.String())
		}
		var out struct {
			SandboxID string `json:"sandboxID"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return out.SandboxID
	}

	sbxA := createSbx()
	sbxB := createSbx()

	// 1. Simulate cubepod-controller ReapIdle asynchronously pausing sbxA in Kubernetes.
	boxA, err := kube.GetPodBox(context.Background(), "default", sbxA)
	if err != nil {
		t.Fatalf("GetPodBox(A): %v", err)
	}
	now := time.Now().UTC()
	boxA.Spec.DesiredState = v1alpha1.DesiredStatePaused
	boxA.Status.Phase = v1alpha1.PodBoxPhasePaused
	boxA.Status.PodIP = ""
	boxA.Status.PodName = ""
	boxA.Status.PausedAt = &now
	boxA.Status.Reason = "IdlePaused"
	if err := kube.UpsertPodBox(context.Background(), *boxA); err != nil {
		t.Fatalf("UpsertPodBox(A): %v", err)
	}

	// Trigger resolveSandbox via GET /sandboxes/{id}.
	rrGet := doJSONReq(t, h, http.MethodGet, "/sandboxes/"+sbxA, "tenant-a", nil)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /sandboxes/A = %d", rrGet.Code)
	}
	// Repeat GET, run SyncPodBoxesOnce, and explicit /pause -> none may emit duplicate paused event.
	_ = doJSONReq(t, h, http.MethodGet, "/sandboxes/"+sbxA, "tenant-a", nil)
	srv.SyncPodBoxesOnce(context.Background())
	if rrPause := doJSONReq(t, h, http.MethodPost, "/sandboxes/"+sbxA+"/pause", "tenant-a", nil); rrPause.Code != http.StatusOK {
		t.Fatalf("POST /pause A = %d", rrPause.Code)
	}

	listEvents := func(sbxID string) []events.LifecycleEvent {
		t.Helper()
		rr := doJSONReq(t, h, http.MethodGet, "/sandboxes/"+sbxID+"/events", "tenant-a", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /sandboxes/%s/events = %d (%s)", sbxID, rr.Code, rr.Body.String())
		}
		var out []events.LifecycleEvent
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return out
	}

	evsA := listEvents(sbxA)
	pausedCount := 0
	for _, ev := range evsA {
		if ev.Type == events.EventPaused {
			pausedCount++
			if ev.EventData == nil || ev.EventData.PauseMode != "filesystem" {
				t.Fatalf("expected pause_mode=filesystem on async paused event, got %+v", ev.EventData)
			}
		}
	}
	if pausedCount != 1 {
		t.Fatalf("expected exactly 1 paused event for sbxA, got %d (%+v)", pausedCount, evsA)
	}

	// 2. Connect sbxA (resumed), then delete PodBox A in K8s asynchronously and wait for periodic sync worker.
	if rrConn := doJSONReq(t, h, http.MethodPost, "/sandboxes/"+sbxA+"/connect", "tenant-a", nil); rrConn.Code != http.StatusOK {
		t.Fatalf("POST /connect A = %d", rrConn.Code)
	}
	if err := kube.DeletePodBox(context.Background(), "default", sbxA); err != nil {
		t.Fatalf("DeletePodBox(A): %v", err)
	}

	// Also pause sbxB asynchronously in K8s and let periodic sync detect both.
	boxB, err := kube.GetPodBox(context.Background(), "default", sbxB)
	if err != nil {
		t.Fatalf("GetPodBox(B): %v", err)
	}
	boxB.Spec.DesiredState = v1alpha1.DesiredStatePaused
	boxB.Status.Phase = v1alpha1.PodBoxPhasePaused
	boxB.Status.PodIP = ""
	boxB.Status.Reason = "IdlePaused"
	if err := kube.UpsertPodBox(context.Background(), *boxB); err != nil {
		t.Fatalf("UpsertPodBox(B): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evsA = listEvents(sbxA)
		evsB := listEvents(sbxB)
		var sawKilledA, sawPausedB bool
		for _, ev := range evsA {
			if ev.Type == events.EventKilled {
				sawKilledA = true
			}
		}
		for _, ev := range evsB {
			if ev.Type == events.EventPaused {
				sawPausedB = true
			}
		}
		if sawKilledA && sawPausedB {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Explicit delete of already-reaped sbxA must not emit a second killed event.
	_ = doJSONReq(t, h, http.MethodDelete, "/sandboxes/"+sbxA, "tenant-a", nil)
	srv.SyncPodBoxesOnce(context.Background())

	evsA = listEvents(sbxA)
	killedCountA := 0
	for _, ev := range evsA {
		if ev.Type == events.EventKilled {
			killedCountA++
		}
	}
	if killedCountA != 1 {
		t.Fatalf("expected exactly 1 killed event for sbxA, got %d (%+v)", killedCountA, evsA)
	}

	evsB := listEvents(sbxB)
	pausedCountB := 0
	for _, ev := range evsB {
		if ev.Type == events.EventPaused {
			pausedCountB++
		}
	}
	if pausedCountB != 1 {
		t.Fatalf("expected exactly 1 paused event for sbxB, got %d (%+v)", pausedCountB, evsB)
	}

	// Explicit delete on sbxB emits killed once, even if SyncPodBoxesOnce runs afterwards.
	if rrDelB := doJSONReq(t, h, http.MethodDelete, "/sandboxes/"+sbxB, "tenant-a", nil); rrDelB.Code != http.StatusNoContent {
		t.Fatalf("DELETE /sandboxes/B = %d", rrDelB.Code)
	}
	srv.SyncPodBoxesOnce(context.Background())
	evsB = listEvents(sbxB)
	killedCountB := 0
	for _, ev := range evsB {
		if ev.Type == events.EventKilled {
			killedCountB++
		}
	}
	if killedCountB != 1 {
		t.Fatalf("expected exactly 1 killed event for sbxB, got %d (%+v)", killedCountB, evsB)
	}

	if srv.CRDResyncsTotal() == 0 {
		t.Fatal("expected CRDResyncsTotal > 0 after periodic PodBox sync")
	}
}

func TestFeatureGateSandboxEventsAndWebhooksDisabled(t *testing.T) {
	srv := newEventsTestServer(t, true)
	t.Cleanup(srv.Close)
	srv.SetFeatureGate(gateway.FeatureGateSandboxEventsAndWebhooks, false)
	h := srv.Handler()

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/webhooks", map[string]any{"name": "h1", "url": "https://example.com/hook", "events": []string{"*"}}},
		{http.MethodGet, "/webhooks", nil},
		{http.MethodGet, "/webhooks/whk_123", nil},
		{http.MethodPatch, "/webhooks/whk_123", map[string]any{"enabled": false}},
		{http.MethodDelete, "/webhooks/whk_123", nil},
		{http.MethodGet, "/events", nil},
		{http.MethodGet, "/sandboxes/sbx-1/events", nil},
		{http.MethodGet, "/webhooks/dead-letters", nil},
		{http.MethodGet, "/webhooks/whk_123/dead-letters", nil},
		{http.MethodPost, "/webhooks/whk_123/dead-letters/dlq_123/retry", nil},
		{http.MethodDelete, "/webhooks/whk_123/dead-letters/dlq_123", nil},
	} {
		rr := doJSONReq(t, h, tc.method, tc.path, "tenant-a", tc.body)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s = %d (%s); want 501 Not Implemented", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}

	// Lifecycle ops while gate is disabled must not record events.
	rrCreate := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-a", map[string]any{"templateID": "base"})
	if rrCreate.Code != http.StatusCreated {
		t.Fatalf("POST /sandboxes = %d", rrCreate.Code)
	}
	var sbx struct {
		SandboxID string `json:"sandboxID"`
	}
	_ = json.Unmarshal(rrCreate.Body.Bytes(), &sbx)
	_ = doJSONReq(t, h, http.MethodDelete, "/sandboxes/"+sbx.SandboxID, "tenant-a", nil)

	srv.SetFeatureGate(gateway.FeatureGateSandboxEventsAndWebhooks, true)
	rrEv := doJSONReq(t, h, http.MethodGet, "/events", "tenant-a", nil)
	if rrEv.Code != http.StatusOK {
		t.Fatalf("GET /events after re-enable = %d", rrEv.Code)
	}
	var items []events.LifecycleEvent
	_ = json.Unmarshal(rrEv.Body.Bytes(), &items)
	if len(items) != 0 {
		t.Fatalf("expected 0 events recorded while SandboxEventsAndWebhooks was disabled, got %d", len(items))
	}
}

func newK8sEventsTestServer(t *testing.T, kube controller.KubeClient) *gateway.Server {
	t.Helper()
	rec := controller.NewReconciler(
		controller.WithKubeClient(kube),
		controller.WithNamespace("default"),
	)
	srv := gateway.NewServer(rec)
	srv.SetWorkspaceBaseDir(t.TempDir())
	srv.SetLivePodRouting(true)
	if err := srv.ConfigureEvents(gateway.E2BEventsConfig{
		Backend:         "k8s",
		KubeClient:      kube,
		Namespace:       "default",
		AllowPrivateIPs: true,
	}); err != nil {
		t.Fatalf("ConfigureEvents(k8s): %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestK8sHAMultiReplicaWebhooksEventsAndDeadLetters(t *testing.T) {
	var failMode atomic.Bool
	failMode.Store(true)
	var okDeliveries atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failMode.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		okDeliveries.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	kube := controller.NewMemoryKubeClient()
	replicaA := newK8sEventsTestServer(t, kube)
	replicaB := newK8sEventsTestServer(t, kube)
	hA := replicaA.Handler()
	hB := replicaB.Handler()

	// 1. Register webhook on replica A; verify SandboxWebhook CR + isolated Secret in kube.
	rrCreateWh := doJSONReq(t, hA, http.MethodPost, "/webhooks", "tenant-a", map[string]any{
		"name":   "ha-dlq-hook",
		"url":    receiver.URL,
		"events": []string{events.EventCreated, events.EventPaused},
	})
	if rrCreateWh.Code != http.StatusCreated {
		t.Fatalf("POST /webhooks = %d (%s)", rrCreateWh.Code, rrCreateWh.Body.String())
	}
	var wh events.WebhookWithSecret
	if err := json.Unmarshal(rrCreateWh.Body.Bytes(), &wh); err != nil {
		t.Fatalf("unmarshal webhook: %v", err)
	}
	cr, err := kube.GetSandboxWebhook(context.Background(), "default", events.KubeWebhookName(wh.ID))
	if err != nil {
		t.Fatalf("GetSandboxWebhook: %v", err)
	}
	if cr.Spec.SecretName == "" {
		t.Fatal("expected spec.secretName to reference companion Secret")
	}
	rawCR, _ := json.Marshal(cr)
	if bytesContains(rawCR, wh.SignatureKey) {
		t.Fatal("SandboxWebhook CR leaked signatureKey")
	}

	// Replica B sees the webhook via cache-miss reload without signatureKey.
	rrGetB := doJSONReq(t, hB, http.MethodGet, "/webhooks/"+wh.ID, "tenant-a", nil)
	if rrGetB.Code != http.StatusOK {
		t.Fatalf("replica B GET /webhooks/{id} = %d (%s)", rrGetB.Code, rrGetB.Body.String())
	}
	if bytesContains(rrGetB.Body.Bytes(), wh.SignatureKey) {
		t.Fatal("GET /webhooks/{id} leaked signatureKey")
	}

	// 2. Create sandbox on replica A; replica B lists the lifecycle event via K8s Events.
	rrSbx := doJSONReq(t, hA, http.MethodPost, "/sandboxes", "tenant-a", map[string]any{"templateID": "base"})
	if rrSbx.Code != http.StatusCreated {
		t.Fatalf("POST /sandboxes = %d (%s)", rrSbx.Code, rrSbx.Body.String())
	}
	var sbx struct {
		SandboxID string `json:"sandboxID"`
	}
	_ = json.Unmarshal(rrSbx.Body.Bytes(), &sbx)

	rrEvB := doJSONReq(t, hB, http.MethodGet, "/sandboxes/"+sbx.SandboxID+"/events", "tenant-a", nil)
	if rrEvB.Code != http.StatusOK {
		t.Fatalf("replica B GET /sandboxes/{id}/events = %d (%s)", rrEvB.Code, rrEvB.Body.String())
	}
	var sbxEvents []events.LifecycleEvent
	_ = json.Unmarshal(rrEvB.Body.Bytes(), &sbxEvents)
	if len(sbxEvents) != 1 || sbxEvents[0].Type != events.EventCreated {
		t.Fatalf("replica B expected 1 created event from K8s Events, got %+v", sbxEvents)
	}

	// 3. Wait for failing delivery on replica A to record a dead letter; inspect via replica B.
	var deadLetters []events.DeadLetter
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rrDL := doJSONReq(t, hB, http.MethodGet, "/webhooks/dead-letters?webhookID="+wh.ID, "tenant-a", nil)
		if rrDL.Code == http.StatusOK {
			var list []events.DeadLetter
			if json.Unmarshal(rrDL.Body.Bytes(), &list) == nil && len(list) == 1 {
				deadLetters = list
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(deadLetters) != 1 {
		t.Fatalf("expected 1 dead letter visible on replica B, got %d", len(deadLetters))
	}
	dl := deadLetters[0]
	if dl.Reason != events.DeadLetterReasonDeliveryFailed || dl.LastStatusCode != http.StatusBadGateway || dl.LastError != "http_status" {
		t.Fatalf("unexpected dead letter: %+v", dl)
	}
	rawDL, _ := json.Marshal(dl)
	if bytesContains(rawDL, wh.SignatureKey) || bytesContains(rawDL, receiver.URL) {
		t.Fatalf("dead letter response exposed secret or URL: %s", rawDL)
	}

	// Scoped list, validation errors, and cross-tenant isolation.
	if rrScoped := doJSONReq(t, hB, http.MethodGet, "/webhooks/"+wh.ID+"/dead-letters?sandboxID="+sbx.SandboxID, "tenant-a", nil); rrScoped.Code != http.StatusOK || !bytesContains(rrScoped.Body.Bytes(), dl.ID) {
		t.Fatalf("GET /webhooks/{id}/dead-letters = %d (%s)", rrScoped.Code, rrScoped.Body.String())
	}
	if rrBad := doJSONReq(t, hB, http.MethodGet, "/webhooks/dead-letters?reason=bogus", "tenant-a", nil); rrBad.Code != http.StatusBadRequest {
		t.Fatalf("bad reason = %d; want 400", rrBad.Code)
	}
	if rrCross := doJSONReq(t, hB, http.MethodGet, "/webhooks/"+wh.ID+"/dead-letters", "tenant-b", nil); rrCross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant dead letters = %d; want 404", rrCross.Code)
	}
	if rrCrossRetry := doJSONReq(t, hB, http.MethodPost, "/webhooks/"+wh.ID+"/dead-letters/"+dl.ID+"/retry", "tenant-b", nil); rrCrossRetry.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant retry = %d; want 404", rrCrossRetry.Code)
	}

	// Metrics on replica A expose dead-letter counters and gauges.
	rrMetrics := doJSONReq(t, hA, http.MethodGet, "/metrics", "", nil)
	if rrMetrics.Code != http.StatusOK ||
		!bytesContains(rrMetrics.Body.Bytes(), "cubepod_webhook_dead_letters_total 1") ||
		!bytesContains(rrMetrics.Body.Bytes(), "cubepod_webhook_dead_letters_pending 1") ||
		!bytesContains(rrMetrics.Body.Bytes(), "cubepod_webhook_deliveries_total{result=\"failed\"} 1") {
		t.Fatalf("unexpected dead-letter metrics:\n%s", rrMetrics.Body.String())
	}

	// 4. Disabled webhook rejects retry with 409; re-enabling + recovering receiver retries with 202.
	if rrDis := doJSONReq(t, hA, http.MethodPatch, "/webhooks/"+wh.ID, "tenant-a", map[string]any{"enabled": false}); rrDis.Code != http.StatusOK {
		t.Fatalf("disable webhook = %d", rrDis.Code)
	}
	if rrConflict := doJSONReq(t, hB, http.MethodPost, "/webhooks/"+wh.ID+"/dead-letters/"+dl.ID+"/retry", "tenant-a", nil); rrConflict.Code != http.StatusConflict {
		t.Fatalf("retry while disabled = %d (%s); want 409", rrConflict.Code, rrConflict.Body.String())
	}
	if rrEn := doJSONReq(t, hA, http.MethodPatch, "/webhooks/"+wh.ID, "tenant-a", map[string]any{"enabled": true}); rrEn.Code != http.StatusOK {
		t.Fatalf("re-enable webhook = %d", rrEn.Code)
	}
	failMode.Store(false)
	rrRetry := doJSONReq(t, hB, http.MethodPost, "/webhooks/"+wh.ID+"/dead-letters/"+dl.ID+"/retry", "tenant-a", nil)
	if rrRetry.Code != http.StatusAccepted {
		t.Fatalf("POST retry = %d (%s); want 202", rrRetry.Code, rrRetry.Body.String())
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if okDeliveries.Load() == 1 {
			rrCheck := doJSONReq(t, hA, http.MethodGet, "/webhooks/"+wh.ID, "tenant-a", nil)
			var info events.WebhookInfo
			if json.Unmarshal(rrCheck.Body.Bytes(), &info) == nil && info.LastDeliveryTime != nil && info.ConsecutiveFailures == 0 {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if okDeliveries.Load() != 1 {
		t.Fatalf("expected 1 retried OK delivery, got %d", okDeliveries.Load())
	}
	rrEmptyDL := doJSONReq(t, hA, http.MethodGet, "/webhooks/dead-letters", "tenant-a", nil)
	if rrEmptyDL.Code != http.StatusOK || stringContains(rrEmptyDL.Body.String(), dl.ID) {
		t.Fatalf("dead letter not cleared after successful retry: %s", rrEmptyDL.Body.String())
	}

	// 5. Multi-replica PodBox idle-pause observed by BOTH replica A and replica B triggers exactly 1 webhook delivery.
	if rrKill := doJSONReq(t, hA, http.MethodDelete, "/sandboxes/"+sbx.SandboxID, "tenant-a", nil); rrKill.Code != http.StatusNoContent {
		t.Fatalf("DELETE sandbox = %d", rrKill.Code)
	}
	okDeliveries.Store(0)
	rrSbx2 := doJSONReq(t, hA, http.MethodPost, "/sandboxes", "tenant-a", map[string]any{"templateID": "base"})
	if rrSbx2.Code != http.StatusCreated {
		t.Fatalf("POST sbx2 = %d", rrSbx2.Code)
	}
	var sbx2 struct {
		SandboxID string `json:"sandboxID"`
	}
	_ = json.Unmarshal(rrSbx2.Body.Bytes(), &sbx2)
	// Warm replica B cache with sbx2 while Running (restores OwnerTenantHash from PodBox annotation),
	// then idle-pause in K8s and sync both replicas.
	if rrWarm := doJSONReq(t, hB, http.MethodGet, "/sandboxes/"+sbx2.SandboxID, "tenant-a", nil); rrWarm.Code != http.StatusOK {
		t.Fatalf("GET sbx2 on B = %d", rrWarm.Code)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && okDeliveries.Load() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	box2, _ := kube.GetPodBox(context.Background(), "default", sbx2.SandboxID)
	pausedAt2 := time.Now().UTC()
	box2.Spec.DesiredState = v1alpha1.DesiredStatePaused
	box2.Status.Phase = v1alpha1.PodBoxPhasePaused
	box2.Status.PodIP = ""
	box2.Status.PodName = ""
	box2.Status.PausedAt = &pausedAt2
	box2.Status.Reason = "IdlePaused"
	_ = kube.UpsertPodBox(context.Background(), *box2)
	replicaA.SyncPodBoxesOnce(context.Background())
	replicaB.SyncPodBoxesOnce(context.Background())

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && okDeliveries.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := okDeliveries.Load(); got != 2 {
		t.Fatalf("expected 2 total OK deliveries (1 create + 1 deduplicated sync pause), got %d", got)
	}

	// Verify DELETE dead letter 204 and 404.
	failMode.Store(true)
	_ = doJSONReq(t, hA, http.MethodPost, "/sandboxes/"+sbx2.SandboxID+"/connect", "tenant-a", nil)
	_ = doJSONReq(t, hA, http.MethodPost, "/sandboxes/"+sbx2.SandboxID+"/pause", "tenant-a", nil)
	var dl2ID string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rrDL := doJSONReq(t, hB, http.MethodGet, "/webhooks/"+wh.ID+"/dead-letters", "tenant-a", nil)
		var list []events.DeadLetter
		if json.Unmarshal(rrDL.Body.Bytes(), &list) == nil && len(list) == 1 {
			dl2ID = list[0].ID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if dl2ID == "" {
		t.Fatal("expected dead letter for failed pause webhook")
	}
	if rrDel := doJSONReq(t, hB, http.MethodDelete, "/webhooks/"+wh.ID+"/dead-letters/"+dl2ID, "tenant-a", nil); rrDel.Code != http.StatusNoContent {
		t.Fatalf("DELETE dead letter = %d (%s); want 204", rrDel.Code, rrDel.Body.String())
	}
	if rrDel2 := doJSONReq(t, hB, http.MethodDelete, "/webhooks/"+wh.ID+"/dead-letters/"+dl2ID, "tenant-a", nil); rrDel2.Code != http.StatusNotFound {
		t.Fatalf("second DELETE dead letter = %d; want 404", rrDel2.Code)
	}
}
