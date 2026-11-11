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
