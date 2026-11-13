package controller_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

type spyContextStore struct {
	controller.ContextStore
	mu         sync.Mutex
	sweepCalls int
	lastNow    time.Time
}

func (s *spyContextStore) SweepExpired(now time.Time, contextTTL, logTTL time.Duration) error {
	s.mu.Lock()
	s.sweepCalls++
	s.lastNow = now
	s.mu.Unlock()
	return s.ContextStore.SweepExpired(now, contextTTL, logTTL)
}

func makeTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader failed: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close failed: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close failed: %v", err)
	}
	return buf.Bytes()
}

func TestControllerManager_LivezAndHealthz(t *testing.T) {
	builder := controller.NewFakeImageBuilder()
	r := controller.NewReconciler(
		controller.WithTemplateBuildEnabled(false),
		controller.WithImageBuilder(builder),
	)
	mgr := controller.NewControllerManager(r, ":0", time.Second)
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	// 1. Even if PingErr is set, when TemplateBuildEnabled is false, /healthz is 200
	builder.PingErr = errors.New("buildkit unreachable")
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /healthz when build disabled, got %d", resp.StatusCode)
	}

	// 2. Enable template build: /livez stays 200, /healthz becomes 503
	r.SetTemplateBuildEnabled(true)

	resp, err = http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatalf("GET /livez failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /livez even when PingBuilder fails, got %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on /healthz when PingBuilder fails, got %d", resp.StatusCode)
	}

	// 3. Clear PingErr -> /healthz returns 200
	builder.PingErr = nil
	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /healthz when PingBuilder succeeds, got %d", resp.StatusCode)
	}
}

func TestControllerManager_Metrics(t *testing.T) {
	kc := controller.NewMemoryKubeClient()
	builder := controller.NewFakeImageBuilder()
	r := controller.NewReconciler(
		controller.WithKubeClient(kc),
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithImageBuilder(builder),
	)

	// Create one template and complete a fast-path build (ready) + one failing build (error)
	ctx := context.Background()
	created, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "alpha-ready"}, "")
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}
	if err := r.StartBuild(ctx, created.TemplateID, created.BuildID, controller.StartBuildRequest{
		FromImage: "ubuntu:24.04",
	}); err != nil {
		t.Fatalf("StartBuild fast-path failed: %v", err)
	}

	builder.BuildErr = errors.New("boom")
	createdErr, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "beta-err"}, "")
	if err != nil {
		t.Fatalf("CreateTemplate err failed: %v", err)
	}
	_ = r.StartBuild(ctx, createdErr.TemplateID, createdErr.BuildID, controller.StartBuildRequest{
		FromImage: "ubuntu:24.04",
		Steps:     []controller.BuildStep{{Type: "RUN", Args: []string{"false"}}},
	})

	// One waiting template
	_, err = r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "gamma-waiting"}, "")
	if err != nil {
		t.Fatalf("CreateTemplate waiting failed: %v", err)
	}

	mgr := controller.NewControllerManager(r, ":0", time.Second)
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /metrics, got %d", resp.StatusCode)
	}

	body := string(raw)
	expectedSubstrings := []string{
		`cubepod_active_sandboxes 0`,
		`cubepod_paused_sandboxes 0`,
		`cubepod_warmpool_ready 0`,
		`cubepod_templates{status="ready"} 4`, // 3 seeded + alpha-ready
		`cubepod_templates{status="building"} 0`,
		`cubepod_templates{status="waiting"} 1`,
		`cubepod_templates{status="error"} 1`,
		`cubepod_template_builds_total{result="ready"} 1`,
		`cubepod_template_builds_total{result="error"} 1`,
		`cubepod_template_build_duration_seconds_bucket{le="5"}`,
		`cubepod_template_build_duration_seconds_bucket{le="+Inf"} 2`,
		`cubepod_template_build_duration_seconds_count 2`,
		`cubepod_template_build_duration_seconds_sum `,
	}
	for _, sub := range expectedSubstrings {
		if !strings.Contains(body, sub) {
			t.Errorf("metrics output missing %q;\ngot:\n%s", sub, body)
		}
	}
}

func TestControllerManager_StartRecoverAndReconcileOnceSweep(t *testing.T) {
	kc := controller.NewMemoryKubeClient()
	spyStore := &spyContextStore{ContextStore: controller.NewMemoryContextStore()}
	builder := controller.NewFakeImageBuilder()

	// Seed an in-flight building PodTemplate in KubeClient before ControllerManager starts
	_ = kc.CreatePodTemplate(context.Background(), v1alpha1.PodTemplate{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodTemplate",
		Metadata:   v1alpha1.ObjectMeta{Name: "stuck-build", Namespace: "cubepod-workloads"},
		Spec:       v1alpha1.PodTemplateSpec{Image: "cubepod.io/unbuilt:waiting", Names: []string{"stuck-build"}},
		Status: v1alpha1.PodTemplateStatus{
			TemplateID:    "tpl_stuck123",
			LatestBuildID: "bld_stuck123",
			BuildStatus:   v1alpha1.BuildStatusBuilding,
		},
	})

	r := controller.NewReconciler(
		controller.WithKubeClient(kc),
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithImageBuilder(builder),
		controller.WithContextStore(spyStore),
	)

	mgr := controller.NewControllerManager(r, "127.0.0.1:0", 200*time.Millisecond)

	// Verify ReconcileOnce calls SweepTemplateContexts
	mgr.ReconcileOnce(context.Background())
	spyStore.mu.Lock()
	sweepsAfterOnce := spyStore.sweepCalls
	spyStore.mu.Unlock()
	if sweepsAfterOnce < 1 {
		t.Fatalf("expected ReconcileOnce to call SweepTemplateContexts, got %d", sweepsAfterOnce)
	}

	// Verify Start calls SyncFromKube + RecoverInFlightBuilds
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	var recovered *v1alpha1.PodTemplate
	for time.Now().Before(deadline) {
		tpl, ok := r.GetPodTemplate("tpl_stuck123")
		if ok && tpl.Status.BuildStatus == v1alpha1.BuildStatusError {
			recovered = tpl
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if recovered == nil {
		t.Fatal("expected stuck building template to be recovered to error on Start")
	}
	if recovered.Status.Reason != "builder disappeared" {
		t.Fatalf("expected reason 'builder disappeared', got %q", recovered.Status.Reason)
	}
}

func TestControllerManager_InternalTemplateBuildEndpoints(t *testing.T) {
	kc := controller.NewMemoryKubeClient()
	memStore := controller.NewMemoryContextStore()
	memStore.SetMaxBytes(4096)
	builder := controller.NewFakeImageBuilder()
	r := controller.NewReconciler(
		controller.WithKubeClient(kc),
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithImageBuilder(builder),
		controller.WithContextStore(memStore),
	)

	mgr := controller.NewControllerManager(r, ":0", time.Second)
	srv := httptest.NewServer(mgr.Handler())
	defer srv.Close()

	validHash := strings.Repeat("a", 64)
	tarGz := makeTestTarGz(t, map[string]string{"hello.txt": "world\n"})

	// 1. HEAD invalid hash -> 400
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/internal/v1/template-builds/context/badhash", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD bad hash failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid hash HEAD, got %d", resp.StatusCode)
	}

	// 2. HEAD absent -> 404
	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/internal/v1/template-builds/context/"+validHash, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD absent failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for absent context HEAD, got %d", resp.StatusCode)
	}

	// 3. PUT invalid tar -> 400
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/internal/v1/template-builds/context/"+validHash, strings.NewReader("not-a-tar"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT invalid tar failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid tar PUT, got %d", resp.StatusCode)
	}

	// 4. PUT valid gzip tar -> 201, then HEAD -> 200
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/internal/v1/template-builds/context/"+validHash, bytes.NewReader(tarGz))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT valid tar failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for valid tar PUT, got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodHead, srv.URL+"/internal/v1/template-builds/context/"+validHash, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD present failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for present context HEAD, got %d", resp.StatusCode)
	}

	// 5. PUT mismatched bytes for same hash -> 409
	otherTarGz := makeTestTarGz(t, map[string]string{"hello.txt": "different\n"})
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/internal/v1/template-builds/context/"+validHash, bytes.NewReader(otherTarGz))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT mismatched tar failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for mismatched tar PUT, got %d", resp.StatusCode)
	}

	// 6. PUT oversized payload -> 413
	bigHash := strings.Repeat("b", 64)
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/internal/v1/template-builds/context/"+bigHash, bytes.NewReader(bytes.Repeat([]byte("x"), 5000)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT oversized failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized PUT, got %d", resp.StatusCode)
	}

	// 7. POST start build for unknown template -> 404
	startBody, _ := json.Marshal(controller.StartBuildRequest{
		FromImage: "busybox:latest",
		Steps:     []controller.BuildStep{{Type: "RUN", Args: []string{"echo ok"}}},
	})
	resp, err = http.Post(srv.URL+"/internal/v1/template-builds/tpl_missing/bld_1/start", "application/json", bytes.NewReader(startBody))
	if err != nil {
		t.Fatalf("POST start unknown failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown template start, got %d", resp.StatusCode)
	}

	// 8. Create template, POST start build -> 202, wait for ready, then GET logs -> 200
	created, err := r.CreateTemplate(context.Background(), controller.CreateTemplateRequest{Name: "internal-build"}, "")
	if err != nil {
		t.Fatalf("CreateTemplate failed: %v", err)
	}
	validStartBody, _ := json.Marshal(controller.StartBuildRequest{
		FromImage: "busybox:latest",
		Steps: []controller.BuildStep{
			{Type: "COPY", Args: []string{"hello.txt", "/etc/hello.txt"}, FilesHash: validHash},
			{Type: "RUN", Args: []string{"cat /etc/hello.txt"}},
		},
	})
	resp, err = http.Post(srv.URL+"/internal/v1/template-builds/"+created.TemplateID+"/"+created.BuildID+"/start", "application/json", bytes.NewReader(validStartBody))
	if err != nil {
		t.Fatalf("POST start valid failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 for valid start, got %d", resp.StatusCode)
	}

	// Wait for build to reach ready
	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		tpl, ok := r.GetPodTemplate(created.TemplateID)
		if ok && tpl.Status.BuildStatus == v1alpha1.BuildStatusReady {
			ready = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !ready {
		t.Fatal("expected async build to reach ready")
	}

	// Append structured log lines and query GET /internal/v1/template-builds/{buildID}/logs
	_ = memStore.AppendLog(created.BuildID, []string{"INFO: step 1", "ERROR: sample error", "INFO: done"})
	resp, err = http.Get(srv.URL + "/internal/v1/template-builds/" + created.BuildID + "/logs?cursor=0&limit=10")
	if err != nil {
		t.Fatalf("GET logs failed: %v", err)
	}
	var logResp struct {
		Logs []controller.BuildLogEntry `json:"logs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&logResp); err != nil {
		t.Fatalf("decode logs failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for GET logs, got %d", resp.StatusCode)
	}
	if len(logResp.Logs) < 3 {
		t.Fatalf("expected at least 3 log entries, got %d", len(logResp.Logs))
	}

	// Backward direction + limit
	resp, err = http.Get(srv.URL + "/internal/v1/template-builds/" + created.BuildID + "/logs?direction=backward&limit=1")
	if err != nil {
		t.Fatalf("GET backward logs failed: %v", err)
	}
	var backResp struct {
		Logs []controller.BuildLogEntry `json:"logs"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&backResp)
	resp.Body.Close()
	if len(backResp.Logs) != 1 || !strings.Contains(backResp.Logs[0].Message, "INFO: done") {
		t.Fatalf("expected last log entry on backward limit=1, got %+v", backResp.Logs)
	}
}
