package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func parseMetricValue(metricsBody, metricPrefix string) (string, bool) {
	lines := strings.Split(metricsBody, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, metricPrefix) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[len(fields)-1], true
			}
		}
	}
	return "", false
}

func getMetricsText(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url + "/metrics")
	if err != nil {
		t.Fatalf("failed to GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 from /metrics, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Fatalf("expected Content-Type text/plain; version=0.0.4, got: %s", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics body: %v", err)
	}
	return string(body)
}

func TestMetricsInitialState(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := getMetricsText(t, ts.Client(), ts.URL)

	expectedMetrics := map[string]string{
		"cubepod_gateway_http_requests_total":               "1",
		"cubepod_gateway_sandboxes_created_total":           "0",
		"cubepod_gateway_autoresume_total":                  "0",
		"cubepod_gateway_commands_executed_total":           "0",
		`cubepod_gateway_sandboxes_active{state="Running"}`: "0",
		`cubepod_gateway_sandboxes_active{state="Paused"}`:  "0",
		"cubepod_gateway_warmpools_total":                   "0",
		"cubepod_gateway_templates_total":                   "3",
	}

	for metric, expectedVal := range expectedMetrics {
		val, ok := parseMetricValue(body, metric)
		if !ok {
			t.Fatalf("metric %q not found in /metrics output:\n%s", metric, body)
		}
		if val != expectedVal {
			t.Fatalf("metric %q: expected %s, got %s", metric, expectedVal, val)
		}
	}
}

func TestMetricsSandboxLifecycleAndAutoResume(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create a sandbox
	createReq := []byte(`{"templateID": "cubeplex-ubuntu24-desktop"}`)
	createResp, err := ts.Client().Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createReq))
	if err != nil {
		t.Fatalf("failed to create sandbox: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", createResp.StatusCode)
	}

	var rec gateway.SandboxRecord
	if err := json.NewDecoder(createResp.Body).Decode(&rec); err != nil {
		t.Fatalf("failed to decode sandbox record: %v", err)
	}

	// Verify metrics after sandbox creation
	body := getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_sandboxes_created_total"); val != "1" {
		t.Fatalf("expected sandboxes_created_total 1, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Running"}`); val != "1" {
		t.Fatalf("expected sandboxes_active Running 1, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Paused"}`); val != "0" {
		t.Fatalf("expected sandboxes_active Paused 0, got %s", val)
	}

	// 2. Run a command
	cmdReq := []byte(`{"cmd": "echo 'metrics-test-output'"}`)
	cmdResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader(cmdReq))
	if err != nil {
		t.Fatalf("failed to run command: %v", err)
	}
	defer cmdResp.Body.Close()
	if cmdResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from command run, got %d", cmdResp.StatusCode)
	}

	body = getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_commands_executed_total"); val != "1" {
		t.Fatalf("expected commands_executed_total 1, got %s", val)
	}

	// 3. Pause the sandbox
	pauseResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("failed to pause sandbox: %v", err)
	}
	defer pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from pause, got %d", pauseResp.StatusCode)
	}

	body = getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Running"}`); val != "0" {
		t.Fatalf("expected sandboxes_active Running 0 after pause, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Paused"}`); val != "1" {
		t.Fatalf("expected sandboxes_active Paused 1 after pause, got %s", val)
	}

	// 4. Trigger auto-resume by running a command on the Paused sandbox
	resumeCmdReq := []byte(`{"cmd": "echo 'auto-resume-via-command'"}`)
	resumeCmdResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader(resumeCmdReq))
	if err != nil {
		t.Fatalf("failed to run command on paused sandbox: %v", err)
	}
	defer resumeCmdResp.Body.Close()
	if resumeCmdResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after auto-resuming command, got %d", resumeCmdResp.StatusCode)
	}

	var cmdOut map[string]interface{}
	_ = json.NewDecoder(resumeCmdResp.Body).Decode(&cmdOut)
	if stdout, _ := cmdOut["stdout"].(string); stdout != "auto-resume-via-command\n" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}

	// Verify metrics after auto-resume via command
	body = getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_autoresume_total"); val != "1" {
		t.Fatalf("expected autoresume_total 1, got %s", val)
	}
	if val, _ := parseMetricValue(body, "cubepod_gateway_commands_executed_total"); val != "2" {
		t.Fatalf("expected commands_executed_total 2, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Running"}`); val != "1" {
		t.Fatalf("expected sandboxes_active Running 1 after auto-resume, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Paused"}`); val != "0" {
		t.Fatalf("expected sandboxes_active Paused 0 after auto-resume, got %s", val)
	}

	// 5. Pause again and auto-resume via /connect
	pauseResp2, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("failed to pause sandbox: %v", err)
	}
	pauseResp2.Body.Close()

	connResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/connect", "application/json", nil)
	if err != nil {
		t.Fatalf("failed to connect paused sandbox: %v", err)
	}
	connResp.Body.Close()

	body = getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_autoresume_total"); val != "2" {
		t.Fatalf("expected autoresume_total 2 after connect, got %s", val)
	}
	if val, _ := parseMetricValue(body, `cubepod_gateway_sandboxes_active{state="Running"}`); val != "1" {
		t.Fatalf("expected sandboxes_active Running 1 after connect, got %s", val)
	}
}

func TestMetricsWarmpoolsAndTemplates(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Initially 0 warmpools
	body := getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_warmpools_total"); val != "0" {
		t.Fatalf("expected warmpools_total 0 initially, got %s", val)
	}
	if val, _ := parseMetricValue(body, "cubepod_gateway_templates_total"); val != "3" {
		t.Fatalf("expected templates_total 3, got %s", val)
	}

	// Create a warmpool
	wpReq := []byte(`{"name": "test-wp", "templateRef": "cubeplex-ubuntu24-desktop", "replicas": 2}`)
	wpResp, err := ts.Client().Post(ts.URL+"/warmpools", "application/json", bytes.NewReader(wpReq))
	if err != nil {
		t.Fatalf("failed to create warmpool: %v", err)
	}
	defer wpResp.Body.Close()
	if wpResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from warmpool create, got %d", wpResp.StatusCode)
	}

	body = getMetricsText(t, ts.Client(), ts.URL)
	if val, _ := parseMetricValue(body, "cubepod_gateway_warmpools_total"); val != "1" {
		t.Fatalf("expected warmpools_total 1 after creation, got %s", val)
	}
}

func TestMetricsJSONAccept(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("failed to execute request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected Content-Type application/json, got %s", ct)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	expectedKeys := []string{
		"cubepod_gateway_http_requests_total",
		"cubepod_gateway_sandboxes_created_total",
		"cubepod_gateway_autoresume_total",
		"cubepod_gateway_commands_executed_total",
		"cubepod_gateway_sandboxes_active_running",
		"cubepod_gateway_sandboxes_active_paused",
		"cubepod_gateway_warmpools_total",
		"cubepod_gateway_templates_total",
	}

	for _, k := range expectedKeys {
		if _, ok := data[k]; !ok {
			t.Fatalf("expected key %q in JSON metrics output, got: %+v", k, data)
		}
	}
}

func TestMetricsConcurrentRequests(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	concurrency := 20
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			resp, err := ts.Client().Get(ts.URL + "/health")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	body := getMetricsText(t, ts.Client(), ts.URL)
	// concurrency requests to /health + 1 request to /metrics = concurrency + 1
	val, ok := parseMetricValue(body, "cubepod_gateway_http_requests_total")
	if !ok {
		t.Fatalf("cubepod_gateway_http_requests_total not found")
	}
	if val != "21" {
		t.Fatalf("expected 21 http requests total, got %s", val)
	}
}

func TestMetricsCEPAlignmentAndPanelGauge(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	if err := srv.ConfigureE2BSecrets(gateway.E2BSecretConfig{Dir: t.TempDir()}); err != nil {
		t.Fatalf("ConfigureE2BSecrets failed: %v", err)
	}
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{Dir: t.TempDir()}); err != nil {
		t.Fatalf("ConfigureE2BVolumes failed: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create a sandbox
	createResp, err := ts.Client().Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"cubeplex-ubuntu24-desktop"}`)))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)
	_ = createResp.Body.Close()

	// 2. Fork the sandbox
	forkResp, err := ts.Client().Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/fork", "application/json", nil)
	if err != nil {
		t.Fatalf("fork sandbox failed: %v", err)
	}
	_ = forkResp.Body.Close()
	if forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from fork, got %d", forkResp.StatusCode)
	}

	// 3. Create an E2B secret
	secReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/secrets", bytes.NewReader([]byte(`{"name":"API_KEY","value":"secret-123"}`)))
	secReq.Header.Set("X-API-KEY", "test-tenant-key")
	secReq.Header.Set("Content-Type", "application/json")
	secResp, err := ts.Client().Do(secReq)
	if err != nil {
		t.Fatalf("POST /secrets failed: %v", err)
	}
	_ = secResp.Body.Close()
	if secResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from /secrets, got %d", secResp.StatusCode)
	}

	// 4. Create an E2B volume and mount it on a new sandbox
	volReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/volumes", bytes.NewReader([]byte(`{"name":"data-vol"}`)))
	volReq.Header.Set("X-API-KEY", "test-tenant-key")
	volReq.Header.Set("Content-Type", "application/json")
	volResp, err := ts.Client().Do(volReq)
	if err != nil {
		t.Fatalf("POST /volumes failed: %v", err)
	}
	_ = volResp.Body.Close()
	if volResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from /volumes, got %d", volResp.StatusCode)
	}

	mntReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes", bytes.NewReader([]byte(`{"templateID":"cubeplex-ubuntu24-desktop","volumeMounts":[{"name":"data-vol","path":"/mnt/data"}]}`)))
	mntReq.Header.Set("X-API-KEY", "test-tenant-key")
	mntReq.Header.Set("Content-Type", "application/json")
	mntResp, err := ts.Client().Do(mntReq)
	if err != nil {
		t.Fatalf("POST /sandboxes with volumeMounts failed: %v", err)
	}
	_ = mntResp.Body.Close()
	if mntResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from mounted sandbox, got %d", mntResp.StatusCode)
	}

	// Trigger a volume mount error (missing volume)
	badMntReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes", bytes.NewReader([]byte(`{"templateID":"cubeplex-ubuntu24-desktop","volumeMounts":[{"name":"missing-vol","path":"/mnt/bad"}]}`)))
	badMntReq.Header.Set("X-API-KEY", "test-tenant-key")
	badMntReq.Header.Set("Content-Type", "application/json")
	badMntResp, err := ts.Client().Do(badMntReq)
	if err != nil {
		t.Fatalf("POST /sandboxes with bad mount failed: %v", err)
	}
	_ = badMntResp.Body.Close()
	if badMntResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing volume mount, got %d", badMntResp.StatusCode)
	}

	// Record a CRD resync
	srv.Metrics().RecordCRDResync()

	// 5. Panel active sessions gauge around a live proxied request
	inFlight := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mintReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes/"+rec.SandboxID+"/endpoints/8080", bytes.NewReader([]byte(`{"targetUrl":"`+upstream.URL+`"}`)))
	mintReq.Header.Set("Content-Type", "application/json")
	mintResp, err := ts.Client().Do(mintReq)
	if err != nil {
		t.Fatalf("mint endpoint failed: %v", err)
	}
	var minted struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(mintResp.Body).Decode(&minted)
	_ = mintResp.Body.Close()

	proxyDone := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Get(ts.URL + minted.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		proxyDone <- err
	}()

	<-inFlight
	midBody := getMetricsText(t, ts.Client(), ts.URL)
	if val, ok := parseMetricValue(midBody, "cubepod_panel_active_sessions"); !ok || val != "1" {
		close(release)
		<-proxyDone
		t.Fatalf("expected cubepod_panel_active_sessions=1 while proxy active, got %q (ok=%v)\n%s", val, ok, midBody)
	}
	close(release)
	if err := <-proxyDone; err != nil {
		t.Fatalf("panel proxy request failed: %v", err)
	}

	body := getMetricsText(t, ts.Client(), ts.URL)
	checks := map[string]string{
		"cubepod_panel_active_sessions":                                                        "0",
		"cubepod_secrets_store_count":                                                          "1",
		"cubepod_e2b_volume_mounts_total":                                                      "1",
		"cubepod_e2b_volume_mount_errors_total":                                                "1",
		"cubepod_gateway_crd_resyncs_total":                                                    "1",
		`cubepod_secrets_operations_total{op="create",status="ok"}`:                            "1",
		`cubepod_sandbox_forks_total{mode="loopback",result="ok"}`:                             "1",
		`cubepod_e2b_requests_total{endpoint="POST /sandboxes",status="201"}`:                  "2",
		`cubepod_e2b_requests_total{endpoint="POST /sandboxes",status="404"}`:                  "1",
		`cubepod_e2b_requests_total{endpoint="POST /sandboxes/{sandboxID}/fork",status="201"}`: "1",
	}
	for metric, want := range checks {
		got, ok := parseMetricValue(body, metric)
		if !ok || got != want {
			t.Fatalf("metric %s: expected %s, got %q (ok=%v)\n%s", metric, want, got, ok, body)
		}
	}

	requiredSubstrings := []string{
		"cubepod_gateway_requests_total ",
		"cubepod_gateway_auto_resumes_total ",
		"cubepod_e2b_request_duration_seconds_bucket{le=\"+Inf\"}",
		"cubepod_e2b_request_duration_seconds_count ",
		"cubepod_sandbox_fork_duration_seconds_bucket{le=\"+Inf\"}",
		"cubepod_sandbox_fork_duration_seconds_count 1",
	}
	for _, sub := range requiredSubstrings {
		if !strings.Contains(body, sub) {
			t.Fatalf("expected %q in /metrics output:\n%s", sub, body)
		}
	}
}
