package gateway_test

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
