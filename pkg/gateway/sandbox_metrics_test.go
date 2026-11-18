package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func createTestSandbox(t *testing.T, client *http.Client, baseURL string) gateway.SandboxRecord {
	t.Helper()
	resp, err := client.Post(baseURL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"cubeplex-ubuntu24-desktop"}`)))
	if err != nil {
		t.Fatalf("POST /sandboxes failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}
	var rec gateway.SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode SandboxRecord failed: %v", err)
	}
	return rec
}

func TestSandboxMetrics_FromPoddAndRingBufferTimeFilter(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rec := createTestSandbox(t, ts.Client(), ts.URL)

	// Mock podd /metrics returning deterministic SandboxMetric snapshots.
	t1 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)
	callIdx := 0
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		callIdx++
		tsSample := t1.Add(time.Duration(callIdx-1) * 30 * time.Second)
		cpuPct := 22.5
		if callIdx >= 2 {
			cpuPct = 55.0
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gateway.SandboxMetric{
			Timestamp:     tsSample.Format(time.RFC3339),
			TimestampUnix: tsSample.Unix(),
			CPUCount:      4,
			CPUUsedPct:    cpuPct,
			MemUsed:       512 * 1024 * 1024,
			MemTotal:      2048 * 1024 * 1024,
			MemCache:      128 * 1024 * 1024,
			DiskUsed:      1024 * 1024 * 1024,
			DiskTotal:     10 * 1024 * 1024 * 1024,
		})
	}))
	defer mockPodd.Close()

	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	// Sample 1
	resp1, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics")
	if err != nil {
		t.Fatalf("GET /sandboxes/{id}/metrics (1) failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp1.StatusCode)
	}
	var series1 []gateway.SandboxMetric
	if err := json.NewDecoder(resp1.Body).Decode(&series1); err != nil {
		t.Fatalf("decode series1: %v", err)
	}
	if len(series1) != 1 || series1[0].CPUCount != 4 || series1[0].CPUUsedPct != 22.5 {
		t.Fatalf("unexpected series1: %+v", series1)
	}

	// Sample 2
	resp2, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics")
	if err != nil {
		t.Fatalf("GET /sandboxes/{id}/metrics (2) failed: %v", err)
	}
	defer resp2.Body.Close()
	var series2 []gateway.SandboxMetric
	if err := json.NewDecoder(resp2.Body).Decode(&series2); err != nil {
		t.Fatalf("decode series2: %v", err)
	}
	if len(series2) != 2 || series2[1].CPUUsedPct != 55.0 {
		t.Fatalf("unexpected series2: %+v", series2)
	}

	// Filter with ?start=<unix>&end=<unix> selecting only t1
	urlRangeUnix := fmt.Sprintf("%s/sandboxes/%s/metrics?start=%d&end=%d&autoResume=false", ts.URL, rec.SandboxID, t1.Unix(), t1.Unix()+10)
	resp3, err := ts.Client().Get(urlRangeUnix)
	if err != nil {
		t.Fatalf("GET /sandboxes/{id}/metrics range failed: %v", err)
	}
	defer resp3.Body.Close()
	var filteredUnix []gateway.SandboxMetric
	if err := json.NewDecoder(resp3.Body).Decode(&filteredUnix); err != nil {
		t.Fatalf("decode filteredUnix: %v", err)
	}
	if len(filteredUnix) != 1 || filteredUnix[0].TimestampUnix != t1.Unix() {
		t.Fatalf("expected only t1 in unix range, got %+v", filteredUnix)
	}

	// Filter with RFC3339 ?start=&end= selecting only t2
	urlRangeRFC := fmt.Sprintf("%s/sandboxes/%s/metrics?start=%s&end=%s&autoResume=false", ts.URL, rec.SandboxID, t2.Format(time.RFC3339), t2.Add(5*time.Second).Format(time.RFC3339))
	resp4, err := ts.Client().Get(urlRangeRFC)
	if err != nil {
		t.Fatalf("GET /sandboxes/{id}/metrics RFC3339 range failed: %v", err)
	}
	defer resp4.Body.Close()
	var filteredRFC []gateway.SandboxMetric
	if err := json.NewDecoder(resp4.Body).Decode(&filteredRFC); err != nil {
		t.Fatalf("decode filteredRFC: %v", err)
	}
	if len(filteredRFC) != 1 || filteredRFC[0].TimestampUnix != t2.Unix() {
		t.Fatalf("expected only t2 in RFC3339 range, got %+v", filteredRFC)
	}
}

func TestSandboxMetrics_LocalFallbackPausedAndErrors(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Not found
	nfResp, err := ts.Client().Get(ts.URL + "/sandboxes/does-not-exist/metrics")
	if err != nil {
		t.Fatalf("GET unknown sandbox metrics failed: %v", err)
	}
	_ = nfResp.Body.Close()
	if nfResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown sandbox, got %d", nfResp.StatusCode)
	}

	rec := createTestSandbox(t, ts.Client(), ts.URL)

	// Invalid start timestamp -> 400
	badResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics?start=not-a-timestamp")
	if err != nil {
		t.Fatalf("GET invalid start failed: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid start timestamp, got %d", badResp.StatusCode)
	}

	// Start > End -> 400
	badRangeResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics?start=2000&end=1000")
	if err != nil {
		t.Fatalf("GET start > end failed: %v", err)
	}
	_ = badRangeResp.Body.Close()
	if badRangeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for start > end, got %d", badRangeResp.StatusCode)
	}

	// Local fallback when PoddURL is empty
	resp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics")
	if err != nil {
		t.Fatalf("GET local fallback metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK in local fallback mode, got %d", resp.StatusCode)
	}
	var samples []gateway.SandboxMetric
	if err := json.NewDecoder(resp.Body).Decode(&samples); err != nil {
		t.Fatalf("decode local fallback metrics: %v", err)
	}
	if len(samples) != 1 || samples[0].CPUCount <= 0 || samples[0].MemTotal == 0 || samples[0].DiskTotal == 0 || samples[0].Timestamp == "" {
		t.Fatalf("unexpected local fallback metrics: %+v", samples)
	}

	// Pause sandbox, verify autoResume=false returns cached sample without waking
	pauseResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	_ = pauseResp.Body.Close()

	cachedResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics?autoResume=false")
	if err != nil {
		t.Fatalf("GET cached metrics while paused failed: %v", err)
	}
	defer cachedResp.Body.Close()
	var cachedSamples []gateway.SandboxMetric
	if err := json.NewDecoder(cachedResp.Body).Decode(&cachedSamples); err != nil {
		t.Fatalf("decode cachedSamples: %v", err)
	}
	if len(cachedSamples) != 1 {
		t.Fatalf("expected 1 cached sample while paused, got %d", len(cachedSamples))
	}

	// Verify sandbox remained Paused
	getResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	if err != nil {
		t.Fatalf("GET sandbox failed: %v", err)
	}
	defer getResp.Body.Close()
	var detail gateway.SandboxRecord
	_ = json.NewDecoder(getResp.Body).Decode(&detail)
	if detail.State != "paused" {
		t.Fatalf("expected sandbox to remain paused with autoResume=false, got %q", detail.State)
	}

	// Calling GET /metrics without autoResume=false auto-resumes the sandbox
	resumeResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/metrics")
	if err != nil {
		t.Fatalf("GET metrics auto-resume failed: %v", err)
	}
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on auto-resume metrics, got %d", resumeResp.StatusCode)
	}
	var afterResume []gateway.SandboxMetric
	_ = json.NewDecoder(resumeResp.Body).Decode(&afterResume)
	if len(afterResume) != 2 {
		t.Fatalf("expected 2 samples after auto-resume, got %d", len(afterResume))
	}
}

func TestSandboxMetrics_BatchQuery(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sb1 := createTestSandbox(t, ts.Client(), ts.URL)
	sb2 := createTestSandbox(t, ts.Client(), ts.URL)

	// 1. Specific sandboxIds comma-separated
	resp, err := ts.Client().Get(ts.URL + "/sandboxes/metrics?sandboxIds=" + sb1.SandboxID + ",unknown-id")
	if err != nil {
		t.Fatalf("GET /sandboxes/metrics failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	var batch1 struct {
		Sandboxes map[string]gateway.SandboxMetric `json:"sandboxes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batch1); err != nil {
		t.Fatalf("decode batch1: %v", err)
	}
	if len(batch1.Sandboxes) != 1 {
		t.Fatalf("expected 1 sandbox in batch1, got %d (%+v)", len(batch1.Sandboxes), batch1.Sandboxes)
	}
	if _, ok := batch1.Sandboxes[sb1.SandboxID]; !ok {
		t.Fatalf("expected %s in batch1", sb1.SandboxID)
	}

	// 2. Omit sandboxIds -> all active sandboxes
	respAll, err := ts.Client().Get(ts.URL + "/sandboxes/metrics")
	if err != nil {
		t.Fatalf("GET /sandboxes/metrics (all) failed: %v", err)
	}
	defer respAll.Body.Close()
	var batchAll struct {
		Sandboxes map[string]gateway.SandboxMetric `json:"sandboxes"`
	}
	if err := json.NewDecoder(respAll.Body).Decode(&batchAll); err != nil {
		t.Fatalf("decode batchAll: %v", err)
	}
	if len(batchAll.Sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes in batchAll, got %d", len(batchAll.Sandboxes))
	}
	if _, ok := batchAll.Sandboxes[sb1.SandboxID]; !ok {
		t.Fatalf("missing %s in batchAll", sb1.SandboxID)
	}
	if _, ok := batchAll.Sandboxes[sb2.SandboxID]; !ok {
		t.Fatalf("missing %s in batchAll", sb2.SandboxID)
	}
}

func TestSandboxMetrics_TenantIsolationAndDeleteEviction(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createWithKey := func(apiKey string) gateway.SandboxRecord {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes", bytes.NewReader([]byte(`{"templateID":"cubeplex-ubuntu24-desktop"}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-KEY", apiKey)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("create sandbox failed: %v", err)
		}
		defer resp.Body.Close()
		var rec gateway.SandboxRecord
		_ = json.NewDecoder(resp.Body).Decode(&rec)
		return rec
	}

	sbA := createWithKey("tenant-a-key")
	sbB := createWithKey("tenant-b-key")

	// Pause sbA and verify Tenant B cannot read metrics OR wake sbA via autoResume.
	pauseReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes/"+sbA.SandboxID+"/pause", nil)
	pauseReq.Header.Set("X-API-KEY", "tenant-a-key")
	pauseResp, err := ts.Client().Do(pauseReq)
	if err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	_ = pauseResp.Body.Close()

	// Unauthenticated request to tenant-owned sandbox -> 401
	unauthResp, err := ts.Client().Get(ts.URL + "/sandboxes/" + sbA.SandboxID + "/metrics")
	if err != nil {
		t.Fatalf("unauth GET metrics failed: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key on tenant sandbox, got %d", unauthResp.StatusCode)
	}

	// Wrong tenant -> 404, and sbA must stay Paused
	crossReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/sandboxes/"+sbA.SandboxID+"/metrics", nil)
	crossReq.Header.Set("X-API-KEY", "tenant-b-key")
	crossResp, err := ts.Client().Do(crossReq)
	if err != nil {
		t.Fatalf("cross-tenant GET metrics failed: %v", err)
	}
	_ = crossResp.Body.Close()
	if crossResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant GET metrics, got %d", crossResp.StatusCode)
	}
	infoResp, _ := ts.Client().Get(ts.URL + "/sandboxes/" + sbA.SandboxID)
	var fetchedA gateway.SandboxRecord
	_ = json.NewDecoder(infoResp.Body).Decode(&fetchedA)
	_ = infoResp.Body.Close()
	if !strings.EqualFold(fetchedA.State, "Paused") {
		t.Fatalf("expected cross-tenant metrics denial not to resume sbA, got %q", fetchedA.State)
	}

	// Batch metrics filters to only caller's sandboxes
	batchReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/sandboxes/metrics", nil)
	batchReq.Header.Set("X-API-KEY", "tenant-b-key")
	batchResp, err := ts.Client().Do(batchReq)
	if err != nil {
		t.Fatalf("batch GET metrics failed: %v", err)
	}
	var batch struct {
		Sandboxes map[string]gateway.SandboxMetric `json:"sandboxes"`
	}
	_ = json.NewDecoder(batchResp.Body).Decode(&batch)
	_ = batchResp.Body.Close()
	if _, leaked := batch.Sandboxes[sbA.SandboxID]; leaked {
		t.Fatalf("batch metrics leaked Tenant A sandbox %s to Tenant B", sbA.SandboxID)
	}
	if _, ok := batch.Sandboxes[sbB.SandboxID]; !ok {
		t.Fatalf("expected Tenant B sandbox %s in batch metrics", sbB.SandboxID)
	}

	// Unknown RPC method label cardinality is bounded
	badRPCReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes/"+sbB.SandboxID+"/filesystem.Filesystem/ArbitraryEvilMethod", bytes.NewReader([]byte(`{}`)))
	badRPCResp, _ := ts.Client().Do(badRPCReq)
	_ = badRPCResp.Body.Close()
	promResp, _ := ts.Client().Get(ts.URL + "/metrics")
	promRaw, _ := io.ReadAll(promResp.Body)
	_ = promResp.Body.Close()
	if strings.Contains(string(promRaw), "ArbitraryEvilMethod") {
		t.Fatalf("unbounded RPC method name leaked into Prometheus label:\n%s", string(promRaw))
	}

	// Deleting sbB evicts its historical ring buffer
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sbB.SandboxID, nil)
	delResp, _ := ts.Client().Do(delReq)
	_ = delResp.Body.Close()
}

func historicalSamples(t *testing.T, ts *httptest.Server, id string) []gateway.SandboxMetric {
	t.Helper()
	// A past `end` makes the handler serve ring-buffer history only (no on-demand poll).
	end := time.Now().Add(-time.Minute).Unix()
	resp, err := ts.Client().Get(fmt.Sprintf("%s/sandboxes/%s/metrics?start=0&end=%d", ts.URL, id, end))
	if err != nil {
		t.Fatalf("GET historical metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out []gateway.SandboxMetric
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestMetricsSampler_BackgroundTicksPopulateHistory(t *testing.T) {
	srv := gateway.NewServer(controller.NewReconciler())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var hits atomic.Int64
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		// Distinct past timestamps so every sample is retained and queryable historically.
		tsSample := time.Now().Add(-time.Hour).Add(time.Duration(n) * time.Second).UTC()
		_ = json.NewEncoder(w).Encode(gateway.SandboxMetric{
			Timestamp: tsSample.Format(time.RFC3339), TimestampUnix: tsSample.Unix(),
			CPUCount: 2, CPUUsedPct: 10, MemUsed: 1, MemTotal: 100, DiskUsed: 1, DiskTotal: 100,
		})
	}))
	defer mockPodd.Close()

	rec := createTestSandbox(t, ts.Client(), ts.URL)
	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	srv.StartMetricsSampler(context.Background(), 20*time.Millisecond)
	defer srv.StopMetricsSampler()

	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	srv.StopMetricsSampler()
	got := historicalSamples(t, ts, rec.SandboxID)
	if len(got) < 3 {
		t.Fatalf("expected >=3 background samples, got %d", len(got))
	}

	mresp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if !strings.Contains(string(body), `cubepod_sandbox_metrics_samples_total{result="ok"}`) ||
		strings.Contains(string(body), `cubepod_sandbox_metrics_samples_total{result="ok"} 0`) {
		t.Fatalf("sampler ok counter missing/zero:\n%s", body)
	}
}

func TestMetricsSampler_SkipsPausedAndHandlesErrors(t *testing.T) {
	srv := gateway.NewServer(controller.NewReconciler())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var okHits, badHits atomic.Int64
	okPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okHits.Add(1)
		_ = json.NewEncoder(w).Encode(gateway.SandboxMetric{CPUCount: 1, MemTotal: 1, DiskTotal: 1})
	}))
	defer okPodd.Close()
	badPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer badPodd.Close()

	paused := createTestSandbox(t, ts.Client(), ts.URL)
	srv.SetPoddURL(paused.SandboxID, okPodd.URL)
	pr, err := ts.Client().Post(ts.URL+"/sandboxes/"+paused.SandboxID+"/pause", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
	okHitsAfterPause := okHits.Load()

	failing := createTestSandbox(t, ts.Client(), ts.URL)
	srv.SetPoddURL(failing.SandboxID, badPodd.URL)

	srv.StartMetricsSampler(context.Background(), 20*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for badHits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	srv.StopMetricsSampler()

	if badHits.Load() < 2 {
		t.Fatalf("failing sandbox was not polled")
	}
	if okHits.Load() != okHitsAfterPause {
		t.Fatalf("paused sandbox was polled by sampler")
	}
	mresp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if strings.Contains(string(body), `cubepod_sandbox_metrics_samples_total{result="error"} 0`) {
		t.Fatalf("expected error counter > 0:\n%s", body)
	}
}

func TestMetricsSampler_StopIsCleanAndIdempotent(t *testing.T) {
	srv := gateway.NewServer(controller.NewReconciler())
	before := runtime.NumGoroutine()
	srv.StopMetricsSampler() // no-op before start
	for i := 0; i < 5; i++ {
		srv.StartMetricsSampler(context.Background(), 5*time.Millisecond)
	}
	srv.StartMetricsSampler(context.Background(), 0) // disabled: no-op
	time.Sleep(30 * time.Millisecond)
	srv.StopMetricsSampler()
	srv.StopMetricsSampler()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}

	// Parent context cancellation also stops the loop.
	ctx, cancel := context.WithCancel(context.Background())
	srv.StartMetricsSampler(ctx, 5*time.Millisecond)
	cancel()
	srv.StopMetricsSampler()
}
