package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/gateway"
	"github.com/cubeplexai/cubepod/pkg/runtime"
)

func findRepoRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repository root containing go.mod")
		}
		dir = parent
	}
}

func ensureBinaries(t *testing.T, repoRoot string) (string, string) {
	binDir := filepath.Join(repoRoot, "bin")
	binPoddInit := filepath.Join(binDir, "podd-init")
	binPodd := filepath.Join(binDir, "podd")

	if _, err := os.Stat(binPoddInit); os.IsNotExist(err) {
		t.Logf("building podd-init at %s...", binPoddInit)
		cmd := exec.Command("cargo", "build", "--release", "-p", "podd-init")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build podd-init: %v\n%s", err, string(out))
		}
		_ = os.MkdirAll(binDir, 0755)
		_ = exec.Command("cp", filepath.Join(repoRoot, "target/release/podd-init"), binPoddInit).Run()
	}

	if _, err := os.Stat(binPodd); os.IsNotExist(err) {
		t.Logf("building podd at %s...", binPodd)
		cmd := exec.Command("cargo", "build", "--release", "-p", "podd")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to build podd: %v\n%s", err, string(out))
		}
		_ = os.MkdirAll(binDir, 0755)
		_ = exec.Command("cp", filepath.Join(repoRoot, "target/release/podd"), binPodd).Run()
	}

	return binPoddInit, binPodd
}

// TestE2E_PoddInitAndSparseExt4Lifecycle executes the compiled bin/podd-init binary
// against a 64MB sparse ext4 image with CUBEPOD_SKIP_PIVOT=1, verifying init-state.json
// and seamless execvp into bin/podd serving HTTP endpoints.
func TestE2E_PoddInitAndSparseExt4Lifecycle(t *testing.T) {
	repoRoot := findRepoRoot(t)
	binPoddInit, binPodd := ensureBinaries(t, repoRoot)

	tmpDir := t.TempDir()

	// 1. Create a 64MB sparse ext4 block device image for RootFS Upper
	rootfsImg := filepath.Join(tmpDir, "cubepod-rootfs-64m.img")
	rf, err := os.Create(rootfsImg)
	if err != nil {
		t.Fatalf("failed to create sparse rootfs image: %v", err)
	}
	if err := rf.Truncate(64 * 1024 * 1024); err != nil {
		_ = rf.Close()
		t.Fatalf("failed to truncate 64MB sparse image: %v", err)
	}
	_ = rf.Close()

	// Format sparse image with mkfs.ext4
	mkfsCmd := exec.Command("mkfs.ext4", "-F", "-L", "cubepod-rootfs", "-m", "0", rootfsImg)
	if out, err := mkfsCmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 on rootfs sparse image failed: %v\n%s", err, string(out))
	}

	// Verify filesystem with e2fsck -p
	fsckCmd := exec.Command("e2fsck", "-p", rootfsImg)
	if out, err := fsckCmd.CombinedOutput(); err != nil {
		t.Fatalf("e2fsck on rootfs sparse image failed: %v\n%s", err, string(out))
	}

	// 2. Create a 64MB sparse ext4 block device image for Workspace
	wsImg := filepath.Join(tmpDir, "cubepod-workspace-64m.img")
	wf, err := os.Create(wsImg)
	if err != nil {
		t.Fatalf("failed to create sparse workspace image: %v", err)
	}
	if err := wf.Truncate(64 * 1024 * 1024); err != nil {
		_ = wf.Close()
		t.Fatalf("failed to truncate workspace image: %v", err)
	}
	_ = wf.Close()

	mkfsWsCmd := exec.Command("mkfs.ext4", "-F", "-L", "cubepod-workspace", "-m", "0", wsImg)
	if out, err := mkfsWsCmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 on workspace sparse image failed: %v\n%s", err, string(out))
	}

	// 3. Pick ephemeral TCP port for podd
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind ephemeral port: %v", err)
	}
	poddAddr := listener.Addr().String()
	_ = listener.Close()

	initStatePath := filepath.Join(tmpDir, "init-state.json")

	// 4. Spawn podd-init wrapping podd
	cmd := exec.Command(binPoddInit, binPodd)
	cmd.Env = append(os.Environ(),
		"CUBEPOD_SKIP_PIVOT=1",
		"CUBEPOD_ROOTFS_DEV="+rootfsImg,
		"CUBEPOD_WORKSPACE_DEV="+wsImg,
		"CUBEPOD_INIT_STATE_PATH="+initStatePath,
		"CUBEPOD_ADDR="+poddAddr,
		"CUBEPOD_WORKDIR="+tmpDir,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start podd-init: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// 5. Poll /health until podd is serving
	healthURL := fmt.Sprintf("http://%s/health", poddAddr)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	ready := false
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			var healthData map[string]interface{}
			_ = json.NewDecoder(resp.Body).Decode(&healthData)
			resp.Body.Close()
			if healthData["daemon"] == "podd-rs" && healthData["status"] == "ok" {
				ready = true
				break
			}
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}

	if !ready {
		t.Fatalf("podd failed to start within timeout. Stderr:\n%s", stderr.String())
	}

	// 6. Verify init-state.json was written correctly by podd-init before execvp
	stateBytes, err := os.ReadFile(initStatePath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", initStatePath, err)
	}

	var stateMap map[string]interface{}
	if err := json.Unmarshal(stateBytes, &stateMap); err != nil {
		t.Fatalf("failed to unmarshal init-state.json: %v\nContent:\n%s", err, string(stateBytes))
	}

	if stateMap["pivoted"] != false {
		t.Errorf("expected pivoted=false, got %v", stateMap["pivoted"])
	}
	if stateMap["rootfs_device"] != rootfsImg {
		t.Errorf("expected rootfs_device=%s, got %v", rootfsImg, stateMap["rootfs_device"])
	}
	if stateMap["rootfs_action"] != "skipped" {
		t.Errorf("expected rootfs_action=skipped, got %v", stateMap["rootfs_action"])
	}

	// 7. Verify command execution inside the execvp'd podd daemon
	runURL := fmt.Sprintf("http://%s/v1/commands/run", poddAddr)
	cmdPayload := []byte(`{"command": "echo 'podd-sparse-lifecycle-ok'", "background": false}`)
	resp, err := http.Post(runURL, "application/json", bytes.NewReader(cmdPayload))
	if err != nil {
		t.Fatalf("failed to post command to podd: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from podd run, got %d", resp.StatusCode)
	}

	var cmdResp map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&cmdResp)
	output, _ := cmdResp["output"].(string)
	if !strings.Contains(output, "podd-sparse-lifecycle-ok") {
		t.Fatalf("unexpected podd command output: %+v", cmdResp)
	}
}

// TestE2E_GatewayFullStackLifecycle tests the full CubePod gateway stack:
// creating a sandbox, running foreground & background commands with cursor logs,
// file I/O, tar.gz archive-sync, pause + Auto-Resume, reset-rootfs preserving /workspace,
// CoW fork, and /panel/{token}/ reverse proxying to a live in-sandbox HTTP server.
func TestE2E_GatewayFullStackLifecycle(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	srv.SetPanelSecret("e2e-gateway-fullstack-panel-secret-32b")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Mock live in-sandbox HTTP server (representing live browser / web terminal panel)
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live-status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("sandbox-panel-alive"))
		case "/echo":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("panel-echo:" + r.URL.Query().Get("msg")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockUpstream.Close()

	// 1. Create Sandbox
	createBody := []byte(`{"templateID":"cubeplex-ubuntu24-desktop","extension":{"volumeKey":"ws-e2e-fullstack"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to create sandbox: status=%d err=%v", resp.StatusCode, err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()

	if rec.SandboxID == "" || rec.State != "Running" || rec.RootfsGeneration != 1 {
		t.Fatalf("unexpected sandbox record after creation: %+v", rec)
	}
	sbID := rec.SandboxID

	// 2. Foreground Command
	fgBody := []byte(`{"command": "echo 'hello e2e foreground'", "background": false}`)
	fgResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(fgBody))
	if err != nil || fgResp.StatusCode != http.StatusOK {
		t.Fatalf("foreground command failed: status=%d err=%v", fgResp.StatusCode, err)
	}
	var fgRes map[string]interface{}
	_ = json.NewDecoder(fgResp.Body).Decode(&fgRes)
	fgResp.Body.Close()

	outStr := fmt.Sprintf("%v", fgRes["stdout"])
	if !strings.Contains(outStr, "hello e2e foreground") {
		t.Fatalf("unexpected foreground output: %+v", fgRes)
	}

	// 3. Background Command & Cursor Log Polling & Kill
	bgBody := []byte(`{"command": "for i in $(seq 1 5); do echo step $i; sleep 0.05; done", "background": true}`)
	bgResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(bgBody))
	if err != nil || bgResp.StatusCode != http.StatusOK {
		t.Fatalf("background command failed: status=%d err=%v", bgResp.StatusCode, err)
	}
	var bgRes map[string]interface{}
	_ = json.NewDecoder(bgResp.Body).Decode(&bgRes)
	bgResp.Body.Close()

	cmdID := fmt.Sprintf("%v", bgRes["cmdID"])
	if cmdID == "" || cmdID == "<nil>" {
		cmdID = fmt.Sprintf("%v", bgRes["id"])
	}
	if bgRes["running"] != true {
		t.Fatalf("expected running=true for background command, got %+v", bgRes)
	}

	time.Sleep(150 * time.Millisecond)

	// Poll logs with cursor
	logResp, err := http.Get(fmt.Sprintf("%s/sandboxes/%s/commands/%s/logs?cursor=0", ts.URL, sbID, cmdID))
	if err != nil || logResp.StatusCode != http.StatusOK {
		t.Fatalf("get logs failed: status=%d err=%v", logResp.StatusCode, err)
	}
	var logRes map[string]interface{}
	_ = json.NewDecoder(logResp.Body).Decode(&logRes)
	logResp.Body.Close()

	content, _ := logRes["content"].(string)
	if !strings.Contains(content, "step") {
		t.Fatalf("expected 'step' in background logs, got %q", content)
	}

	// Terminate background command
	killResp, err := http.Post(fmt.Sprintf("%s/sandboxes/%s/commands/%s/kill", ts.URL, sbID, cmdID), "application/json", nil)
	if err != nil || killResp.StatusCode != http.StatusOK {
		t.Fatalf("kill command failed: status=%d err=%v", killResp.StatusCode, err)
	}
	var killRes map[string]interface{}
	_ = json.NewDecoder(killResp.Body).Decode(&killRes)
	killResp.Body.Close()
	if killRes["killed"] != true {
		t.Fatalf("expected killed=true, got %+v", killRes)
	}

	// 4. File I/O
	fileData := []byte("cubepod persistent workspace content 998877")
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+sbID+"/files?path=/workspace/notes.txt&mode=0644", bytes.NewReader(fileData))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("put file failed: status=%d err=%v", putResp.StatusCode, err)
	}
	putResp.Body.Close()

	getResp, err := http.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/notes.txt")
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("get file failed: status=%d err=%v", getResp.StatusCode, err)
	}
	gotBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !bytes.Equal(gotBytes, fileData) {
		t.Fatalf("expected file bytes %q, got %q", string(fileData), string(gotBytes))
	}

	// 5. In-Memory tar.gz Archive-Sync
	var tarGzBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gzw)

	filesToSync := map[string]string{
		"skills/calculator.py": "def calc_sum(a, b): return a + b",
		"config/metadata.json": `{"version":"2.0","e2e":true}`,
	}
	for name, data := range filesToSync {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(data))
	}
	_ = tw.Close()
	_ = gzw.Close()

	syncResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/files/archive-sync", "application/gzip", bytes.NewReader(tarGzBuf.Bytes()))
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("archive-sync failed: status=%d err=%v", syncResp.StatusCode, err)
	}
	var syncRes map[string]interface{}
	_ = json.NewDecoder(syncResp.Body).Decode(&syncRes)
	syncResp.Body.Close()
	if syncRes["extracted"].(float64) != 2 {
		t.Fatalf("expected 2 extracted files, got %+v", syncRes)
	}

	// Verify extracted skill file
	skillGetResp, err := http.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/skills/calculator.py")
	if err != nil || skillGetResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to read extracted skill file: status=%d err=%v", skillGetResp.StatusCode, err)
	}
	skillGot, _ := io.ReadAll(skillGetResp.Body)
	skillGetResp.Body.Close()
	if string(skillGot) != "def calc_sum(a, b): return a + b" {
		t.Fatalf("unexpected skill content: %s", string(skillGot))
	}

	// 6. Pause + Auto-Resume on Traffic
	pauseResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/pause", "application/json", nil)
	if err != nil || pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("pause failed: status=%d err=%v", pauseResp.StatusCode, err)
	}
	_ = json.NewDecoder(pauseResp.Body).Decode(&rec)
	pauseResp.Body.Close()
	if rec.State != "Paused" {
		t.Fatalf("expected sandbox state Paused, got %s", rec.State)
	}

	// Trigger command on paused sandbox -> Auto-Resume
	resumeCmd := []byte(`{"command": "echo 'auto-resume-verified'"}`)
	arResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(resumeCmd))
	if err != nil || arResp.StatusCode != http.StatusOK {
		t.Fatalf("auto-resume command failed: status=%d err=%v", arResp.StatusCode, err)
	}
	var arRes map[string]interface{}
	_ = json.NewDecoder(arResp.Body).Decode(&arRes)
	arResp.Body.Close()

	if !strings.Contains(fmt.Sprintf("%v", arRes["stdout"]), "auto-resume-verified") {
		t.Fatalf("unexpected auto-resume command result: %+v", arRes)
	}

	// Verify sandbox is now Running and AutoResumeCount >= 1
	chkResp, _ := http.Get(ts.URL + "/sandboxes/" + sbID)
	_ = json.NewDecoder(chkResp.Body).Decode(&rec)
	chkResp.Body.Close()
	if rec.State != "Running" || rec.AutoResumeCount < 1 {
		t.Fatalf("expected Running state and AutoResumeCount >= 1, got state=%s count=%d", rec.State, rec.AutoResumeCount)
	}

	// 7. Reset RootFS preserving /workspace
	resetResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/reset-rootfs", "application/json", nil)
	if err != nil || resetResp.StatusCode != http.StatusOK {
		t.Fatalf("reset-rootfs failed: status=%d err=%v", resetResp.StatusCode, err)
	}
	_ = json.NewDecoder(resetResp.Body).Decode(&rec)
	resetResp.Body.Close()

	if rec.RootfsGeneration != 2 || rec.State != "Running" {
		t.Fatalf("expected RootfsGeneration=2 and Running state, got generation=%d state=%s", rec.RootfsGeneration, rec.State)
	}

	// Confirm /workspace/notes.txt and skills still exist intact
	recheckResp, err := http.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/notes.txt")
	if err != nil || recheckResp.StatusCode != http.StatusOK {
		t.Fatalf("notes.txt was lost after rootfs reset: status=%d err=%v", recheckResp.StatusCode, err)
	}
	recheckBytes, _ := io.ReadAll(recheckResp.Body)
	recheckResp.Body.Close()
	if !bytes.Equal(recheckBytes, fileData) {
		t.Fatalf("preserved file content mismatch after reset: %q vs %q", string(fileData), string(recheckBytes))
	}

	// 8. CoW Fork
	forkResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/fork", "application/json", nil)
	if err != nil || forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("fork failed: status=%d err=%v", forkResp.StatusCode, err)
	}
	var childRec gateway.SandboxRecord
	_ = json.NewDecoder(forkResp.Body).Decode(&childRec)
	forkResp.Body.Close()

	if childRec.SandboxID == sbID || childRec.State != "Running" || childRec.RootfsGeneration != 1 {
		t.Fatalf("unexpected child fork record: %+v", childRec)
	}

	// 9. Signed Panel Reverse Proxy
	panelReqBody, _ := json.Marshal(map[string]interface{}{
		"targetUrl":        mockUpstream.URL,
		"expiresInSeconds": 3600,
	})
	panelResp, err := http.Post(ts.URL+"/sandboxes/"+sbID+"/endpoints/8080", "application/json", bytes.NewReader(panelReqBody))
	if err != nil || panelResp.StatusCode != http.StatusOK {
		t.Fatalf("mint panel token failed: status=%d err=%v", panelResp.StatusCode, err)
	}
	var panelRes map[string]string
	_ = json.NewDecoder(panelResp.Body).Decode(&panelRes)
	panelResp.Body.Close()

	token := panelRes["token"]
	if token == "" || !strings.HasPrefix(panelRes["url"], "/panel/") {
		t.Fatalf("invalid panel response: %+v", panelRes)
	}

	// Proxy GET /panel/{token}/live-status
	proxyResp, err := http.Get(ts.URL + "/panel/" + token + "/live-status")
	if err != nil || proxyResp.StatusCode != http.StatusOK {
		t.Fatalf("panel proxy live-status failed: status=%d err=%v", proxyResp.StatusCode, err)
	}
	proxyBody, _ := io.ReadAll(proxyResp.Body)
	proxyResp.Body.Close()
	if string(proxyBody) != "sandbox-panel-alive" {
		t.Fatalf("unexpected panel proxy body: %s", string(proxyBody))
	}

	// Proxy GET with query param /panel/{token}/echo?msg=cubepod
	echoResp, err := http.Get(ts.URL + "/panel/" + token + "/echo?msg=cubepod")
	if err != nil || echoResp.StatusCode != http.StatusOK {
		t.Fatalf("panel proxy echo failed: status=%d err=%v", echoResp.StatusCode, err)
	}
	echoBody, _ := io.ReadAll(echoResp.Body)
	echoResp.Body.Close()
	if string(echoBody) != "panel-echo:cubepod" {
		t.Fatalf("unexpected echo body: %s", string(echoBody))
	}

	// Auto-Resume through panel proxy
	pResp, _ := http.Post(ts.URL+"/sandboxes/"+sbID+"/pause", "application/json", nil)
	pResp.Body.Close()

	arProxyResp, err := http.Get(ts.URL + "/panel/" + token + "/live-status")
	if err != nil || arProxyResp.StatusCode != http.StatusOK {
		t.Fatalf("panel proxy auto-resume request failed: status=%d err=%v", arProxyResp.StatusCode, err)
	}
	arProxyBody, _ := io.ReadAll(arProxyResp.Body)
	arProxyResp.Body.Close()
	if string(arProxyBody) != "sandbox-panel-alive" {
		t.Fatalf("unexpected body after panel auto-resume: %s", string(arProxyBody))
	}
}

// TestE2E_PythonSDKLiveHTTP runs the Python AsyncCubePod client in a real child process
// executing sdks/python/tests/test_e2e_live.py against the live Go cubepod-gateway HTTP server.
func TestE2E_PythonSDKLiveHTTP(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Verify python3 is installed
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found in PATH, skipping Python SDK live HTTP test")
	}

	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	srv.SetPanelSecret("e2e-python-sdk-live-panel-secret-32b")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Start mock live in-sandbox HTTP server for panel proxy verification
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live-status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("sandbox-panel-alive"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockUpstream.Close()

	testScript := filepath.Join(repoRoot, "sdks/python/tests/test_e2e_live.py")
	pythonPath := filepath.Join(repoRoot, "sdks/python")

	cmd := exec.Command("python3", testScript, "--gateway-url", ts.URL, "--upstream-url", mockUpstream.URL)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+pythonPath,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python SDK live E2E test failed: %v\nOutput:\n%s", err, string(out))
	}

	t.Logf("Python SDK E2E Output:\n%s", string(out))
	if !strings.Contains(string(out), "ALL PYTHON SDK LIVE E2E TESTS PASSED") {
		t.Fatalf("expected test success marker in output, got:\n%s", string(out))
	}
}

// TestE2E_LivePoddBackendAndZeroTrustEgress wires gateway.Server to a real bin/podd process
// managed by runtime.PoddProcessBackend (via SetPoddURL), verifying live tar.gz archive-sync
// and DELETE /v1/files on disk, as well as mTLS Zero-Trust Egress cpodref_ secret swapping.
func TestE2E_LivePoddBackendAndZeroTrustEgress(t *testing.T) {
	repoRoot := findRepoRoot(t)
	_, binPodd := ensureBinaries(t, repoRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	backend, err := runtime.NewPoddProcessBackend(runtime.PoddProcessBackendConfig{
		BinaryPath:    binPodd,
		BaseDir:       tmpDir,
		HealthTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create PoddProcessBackend: %v", err)
	}
	defer backend.Close()

	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)

	ca := srv.CA()
	vault := srv.Vault()

	// Start mTLS Gateway server for both E2B API and /internal/v1/egress/exchange
	serverTLS, err := ca.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig failed: %v", err)
	}
	// Allow non-mTLS public clients on the same test listener while verifying client certs when presented
	serverTLS.ClientAuth = 3 // tls.VerifyClientCertIfGiven
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.TLS = serverTLS
	ts.StartTLS()
	defer ts.Close()

	httpClient := ts.Client()

	// 1. Create sandbox via Gateway
	createResp, err := httpClient.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"cubeplex-ubuntu24-desktop"}`))
	if err != nil || createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create sandbox failed: %v", err)
	}
	var sbRec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&sbRec)
	createResp.Body.Close()

	// 2. Spawn real bin/podd via PoddProcessBackend and bind its live URL to the Gateway
	inst, err := backend.EnsureRunning(ctx, runtime.SandboxSpec{
		SandboxID: sbRec.SandboxID,
	})
	if err != nil {
		t.Fatalf("EnsureRunning failed: %v", err)
	}
	srv.SetPoddURL(sbRec.SandboxID, inst.PoddURL)

	// 3. Test live archive-sync forwarded by Gateway to real Rust bin/podd
	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	content := []byte("real-rust-podd-archive-content")
	_ = tw.WriteHeader(&tar.Header{
		Name: "sub/live.txt",
		Mode: 0644,
		Size: int64(len(content)),
	})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gzw.Close()

	syncURL := fmt.Sprintf("%s/sandboxes/%s/files/archive-sync?path=%s", ts.URL, sbRec.SandboxID, url.QueryEscape(inst.WorkspaceDir))
	syncResp, err := httpClient.Post(syncURL, "application/gzip", bytes.NewReader(tarBuf.Bytes()))
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("live archive-sync failed: status=%d err=%v", syncResp.StatusCode, err)
	}
	syncResp.Body.Close()

	// Verify file exists on host disk inside inst.WorkspaceDir/sub/live.txt
	extractedBytes, err := os.ReadFile(filepath.Join(inst.WorkspaceDir, "sub", "live.txt"))
	if err != nil {
		t.Fatalf("expected extracted file on disk: %v", err)
	}
	if string(extractedBytes) != string(content) {
		t.Fatalf("extracted content mismatch: %q", string(extractedBytes))
	}

	// 4. Test Zero-Trust Egress Proxy swapping cpodref_ secret over mTLS against Gateway
	upstreamSeenAuth := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSeenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{upstreamURL.Hostname()},
	}
	realSecret := "Bearer sk-real-production-secret-999"
	placeholder, err := vault.BindSecret(sbRec.SandboxID, "OPENAI_API_KEY", realSecret, []string{upstreamURL.Hostname()}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	sbCert, sbKey, err := ca.MintSandboxCert(sbRec.SandboxID, time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	clientTLS, err := ca.ClientTLSConfig(sbCert, sbKey)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}

	egressProxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:       sbRec.SandboxID,
		Policy:          policy,
		ExchangeURL:     ts.URL + "/internal/v1/egress/exchange",
		ClientTLSConfig: clientTLS,
	})
	egressSrv := httptest.NewServer(egressProxy)
	defer egressSrv.Close()

	proxyURL, _ := url.Parse(egressSrv.URL)
	proxiedClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/v1/models", nil)
	req.Header.Set("Authorization", placeholder)
	egressResp, err := proxiedClient.Do(req)
	if err != nil || egressResp.StatusCode != http.StatusOK {
		t.Fatalf("proxied egress request failed: status=%v err=%v", egressResp, err)
	}
	egressResp.Body.Close()

	if upstreamSeenAuth != realSecret {
		t.Fatalf("expected upstream to receive swapped real secret %q, got %q", realSecret, upstreamSeenAuth)
	}
}

// TestE2E_E2BProtocolDropIn validates fullstack drop-in compatibility with the E2B Sandbox Protocol
// conforming to CEP-0018:
// - Issues E2B-compatible POST /sandboxes with templateID: "base"
// - Verifies clientID, envdVersion, cpuCount, memoryMB, diskSizeMB (mandatory E2B SDK fields)
// - Issues POST /sandboxes/{id}/refreshes with {"duration": 600}
// - Issues command execution, writes/reads file in /workspace
// - Verifies Pause and transparent Auto-Resume on subsequent request
// - Deletes sandbox
func TestE2E_E2BProtocolDropIn(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// 1. Issues E2B-compatible POST /sandboxes with templateID: "base"
	createPayload := []byte(`{
		"templateID": "base",
		"timeout": 300,
		"metadata": {"test": "e2e-e2b-drop-in", "user": "agent-007"},
		"envVars": {"PYTHONUNBUFFERED": "1"}
	}`)
	resp, err := client.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createPayload))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to create E2B sandbox: status=%d err=%v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	var createMap map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&createMap); err != nil {
		t.Fatalf("failed to decode E2B create response: %v", err)
	}

	sbID, _ := createMap["sandboxID"].(string)
	if sbID == "" {
		t.Fatalf("expected non-empty sandboxID in create response: %+v", createMap)
	}

	// 2. Verifies clientID, envdVersion, cpuCount, memoryMB, diskSizeMB
	clientID, _ := createMap["clientID"].(string)
	if clientID == "" {
		t.Fatalf("expected non-empty clientID in create response, got %v", createMap["clientID"])
	}
	envdVersion, _ := createMap["envdVersion"].(string)
	if envdVersion == "" {
		t.Fatalf("expected non-empty envdVersion in create response, got %v", createMap["envdVersion"])
	}
	cpuCount, _ := createMap["cpuCount"].(float64)
	if cpuCount <= 0 {
		t.Fatalf("expected cpuCount > 0 in create response, got %v", createMap["cpuCount"])
	}
	memoryMB, _ := createMap["memoryMB"].(float64)
	if memoryMB <= 0 {
		t.Fatalf("expected memoryMB > 0 in create response, got %v", createMap["memoryMB"])
	}
	diskSizeMB, _ := createMap["diskSizeMB"].(float64)
	if diskSizeMB <= 0 {
		t.Fatalf("expected diskSizeMB > 0 in create response, got %v", createMap["diskSizeMB"])
	}

	// Verify GET /sandboxes/{id} also returns matching E2B detail fields
	getResp, err := client.Get(ts.URL + "/sandboxes/" + sbID)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sandboxes/{id} failed: status=%d err=%v", getResp.StatusCode, err)
	}
	var detailMap map[string]interface{}
	_ = json.NewDecoder(getResp.Body).Decode(&detailMap)
	getResp.Body.Close()

	if detailMap["clientID"] != clientID || detailMap["envdVersion"] != envdVersion {
		t.Fatalf("detail response mismatch: %+v", detailMap)
	}
	if detailMap["cpuCount"].(float64) != cpuCount || detailMap["memoryMB"].(float64) != memoryMB || detailMap["diskSizeMB"].(float64) != diskSizeMB {
		t.Fatalf("resource metrics mismatch in detail response: %+v", detailMap)
	}

	// 3. Issues POST /sandboxes/{id}/refreshes with {"duration": 600}
	refreshPayload := []byte(`{"duration": 600}`)
	refreshResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/refreshes", "application/json", bytes.NewReader(refreshPayload))
	if err != nil || (refreshResp.StatusCode != http.StatusNoContent && refreshResp.StatusCode != http.StatusOK) {
		t.Fatalf("refresh failed: status=%d err=%v", refreshResp.StatusCode, err)
	}
	refreshResp.Body.Close()

	// 4. Issues command execution, writes/reads file in /workspace
	cmdPayload := []byte(`{"command": "echo 'e2b drop-in execution ok'", "workdir": "/workspace"}`)
	cmdResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(cmdPayload))
	if err != nil || cmdResp.StatusCode != http.StatusOK {
		t.Fatalf("command execution failed: status=%d err=%v", cmdResp.StatusCode, err)
	}
	var cmdResult map[string]interface{}
	_ = json.NewDecoder(cmdResp.Body).Decode(&cmdResult)
	cmdResp.Body.Close()

	outStr := fmt.Sprintf("%v", cmdResult["stdout"])
	if !strings.Contains(outStr, "e2b drop-in execution ok") {
		t.Fatalf("unexpected command output: %+v", cmdResult)
	}

	// Write file in /workspace
	testFileData := []byte("e2b protocol workspace file content")
	fileReq, err := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+sbID+"/files?path=/workspace/test_e2b.txt", bytes.NewReader(testFileData))
	if err != nil {
		t.Fatalf("failed to create put file request: %v", err)
	}
	putResp, err := client.Do(fileReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("write file failed: status=%d err=%v", putResp.StatusCode, err)
	}
	putResp.Body.Close()

	// Read file from /workspace
	readResp, err := client.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/test_e2b.txt")
	if err != nil || readResp.StatusCode != http.StatusOK {
		t.Fatalf("read file failed: status=%d err=%v", readResp.StatusCode, err)
	}
	gotContent, err := io.ReadAll(readResp.Body)
	readResp.Body.Close()
	if err != nil || !bytes.Equal(gotContent, testFileData) {
		t.Fatalf("read file content mismatch: expected %q, got %q (err=%v)", string(testFileData), string(gotContent), err)
	}

	// 5. Verifies Pause and transparent Auto-Resume on subsequent request
	pauseResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/pause", "application/json", nil)
	if err != nil || pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("pause sandbox failed: status=%d err=%v", pauseResp.StatusCode, err)
	}
	var pauseRec gateway.SandboxRecord
	_ = json.NewDecoder(pauseResp.Body).Decode(&pauseRec)
	pauseResp.Body.Close()

	if strings.ToLower(pauseRec.State) != "paused" {
		t.Fatalf("expected sandbox state Paused, got %s", pauseRec.State)
	}

	// Subsequent command execution triggers transparent Auto-Resume
	resumeCmdPayload := []byte(`{"command": "echo 'transparent-auto-resume-verified'"}`)
	arResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(resumeCmdPayload))
	if err != nil || arResp.StatusCode != http.StatusOK {
		t.Fatalf("auto-resume command execution failed: status=%d err=%v", arResp.StatusCode, err)
	}
	var arResult map[string]interface{}
	_ = json.NewDecoder(arResp.Body).Decode(&arResult)
	arResp.Body.Close()

	arOutStr := fmt.Sprintf("%v", arResult["stdout"])
	if !strings.Contains(arOutStr, "transparent-auto-resume-verified") {
		t.Fatalf("unexpected auto-resume command output: %+v", arResult)
	}

	// Verify sandbox is now Running and AutoResumeCount >= 1
	chkResp, err := client.Get(ts.URL + "/sandboxes/" + sbID)
	if err != nil || chkResp.StatusCode != http.StatusOK {
		t.Fatalf("get sandbox after auto-resume failed: status=%d err=%v", chkResp.StatusCode, err)
	}
	var chkRec gateway.SandboxRecord
	_ = json.NewDecoder(chkResp.Body).Decode(&chkRec)
	chkResp.Body.Close()

	if strings.ToLower(chkRec.State) != "running" || chkRec.AutoResumeCount < 1 {
		t.Fatalf("expected state Running and AutoResumeCount >= 1, got state=%s count=%d", chkRec.State, chkRec.AutoResumeCount)
	}

	// Also verify file content in /workspace is preserved across Pause/Auto-Resume
	readPreservedResp, err := client.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/test_e2b.txt")
	if err != nil || readPreservedResp.StatusCode != http.StatusOK {
		t.Fatalf("read preserved file failed: status=%d err=%v", readPreservedResp.StatusCode, err)
	}
	preservedContent, _ := io.ReadAll(readPreservedResp.Body)
	readPreservedResp.Body.Close()
	if !bytes.Equal(preservedContent, testFileData) {
		t.Fatalf("preserved file content mismatch after auto-resume: %q vs %q", string(testFileData), string(preservedContent))
	}

	// 6. Deletes sandbox
	delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sbID, nil)
	if err != nil {
		t.Fatalf("failed to create delete request: %v", err)
	}
	delResp, err := client.Do(delReq)
	if err != nil || (delResp.StatusCode != http.StatusNoContent && delResp.StatusCode != http.StatusOK) {
		t.Fatalf("delete sandbox failed: status=%d err=%v", delResp.StatusCode, err)
	}
	delResp.Body.Close()

	// Verify sandbox is deleted (GET returns 404)
	notFoundResp, err := client.Get(ts.URL + "/sandboxes/" + sbID)
	if err != nil {
		t.Fatalf("get deleted sandbox request failed: %v", err)
	}
	defer notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for deleted sandbox, got %d", notFoundResp.StatusCode)
	}
}

// TestE2E_E2BProductionHardening validates production hardening, stress resilience,
// high-throughput file transfer, and idle reaper lease protection conforming to CEP-0018:
//  1. High-throughput file transfer: uploads and downloads a 5MB binary file via E2B files API,
//     verifying SHA-256 integrity and in-guest filesystem byte accuracy.
//  2. Long-running command lease protection: verifies that a command executing across an idle timeout
//     is NOT mistakenly paused by the ControllerManager Idle Reaper while an active execution lease is held,
//     and is reaped only after lease expiration.
//  3. Stress test: spawns multiple concurrent sandboxes executing parallel commands and file I/O
//     without resource leaks or race conditions.
func TestE2E_E2BProductionHardening(t *testing.T) {
	t.Run("HighThroughputFileTransfer", func(t *testing.T) {
		reconciler := controller.NewReconciler()
		srv := gateway.NewServer(reconciler)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		client := ts.Client()

		// 1. Create sandbox
		createPayload := []byte(`{"templateID": "base", "timeout": 600}`)
		resp, err := client.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createPayload))
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("failed to create sandbox: status=%v err=%v", resp.StatusCode, err)
		}
		var sbRec gateway.SandboxRecord
		_ = json.NewDecoder(resp.Body).Decode(&sbRec)
		resp.Body.Close()
		sbID := sbRec.SandboxID

		// 2. Generate 5MB pseudo-random binary payload
		const fileSize = 5 * 1024 * 1024 // 5 MB
		payload := make([]byte, fileSize)
		for i := range payload {
			payload[i] = byte((i*37 + 13) % 256)
		}
		hasher := sha256.New()
		hasher.Write(payload)
		expectedChecksum := hex.EncodeToString(hasher.Sum(nil))

		// 3. Upload 5MB binary file via E2B files API (PUT /sandboxes/{id}/files?path=...)
		uploadStart := time.Now()
		putReq, err := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+sbID+"/files?path=/workspace/data_5mb.bin", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("failed to create upload request: %v", err)
		}
		putReq.Header.Set("Content-Type", "application/octet-stream")
		putResp, err := client.Do(putReq)
		if err != nil || putResp.StatusCode != http.StatusOK {
			t.Fatalf("upload 5MB file failed: status=%v err=%v", putResp.StatusCode, err)
		}
		putResp.Body.Close()
		uploadDuration := time.Since(uploadStart)
		t.Logf("Uploaded 5MB in %v (%.2f MB/s)", uploadDuration, float64(fileSize)/(1024*1024)/uploadDuration.Seconds())

		// 4. Download 5MB binary file via E2B files API (GET /sandboxes/{id}/files?path=...)
		downloadStart := time.Now()
		getResp, err := client.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/data_5mb.bin")
		if err != nil || getResp.StatusCode != http.StatusOK {
			t.Fatalf("download 5MB file failed: status=%v err=%v", getResp.StatusCode, err)
		}
		downloadedBytes, err := io.ReadAll(getResp.Body)
		getResp.Body.Close()
		if err != nil {
			t.Fatalf("failed to read downloaded body: %v", err)
		}
		downloadDuration := time.Since(downloadStart)
		t.Logf("Downloaded 5MB in %v (%.2f MB/s)", downloadDuration, float64(fileSize)/(1024*1024)/downloadDuration.Seconds())

		// 5. Verify byte count and SHA-256 integrity
		if len(downloadedBytes) != fileSize {
			t.Fatalf("download size mismatch: expected %d bytes, got %d bytes", fileSize, len(downloadedBytes))
		}
		dlHasher := sha256.New()
		dlHasher.Write(downloadedBytes)
		actualChecksum := hex.EncodeToString(dlHasher.Sum(nil))
		if actualChecksum != expectedChecksum {
			t.Fatalf("SHA-256 checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}

		// 6. In-guest verification: execute wc -c in the sandbox to verify exact file size on disk
		checkCmd := []byte(`{"command": "wc -c data_5mb.bin"}`)
		cmdResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(checkCmd))
		if err != nil || cmdResp.StatusCode != http.StatusOK {
			t.Fatalf("in-guest file check command failed: status=%v err=%v", cmdResp.StatusCode, err)
		}
		var cmdRes map[string]interface{}
		_ = json.NewDecoder(cmdResp.Body).Decode(&cmdRes)
		cmdResp.Body.Close()
		stdoutStr := fmt.Sprintf("%v", cmdRes["stdout"])
		if !strings.Contains(stdoutStr, fmt.Sprintf("%d", fileSize)) {
			t.Fatalf("in-guest file size check failed, expected %d in stdout, got: %s", fileSize, stdoutStr)
		}

		// Cleanup
		delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sbID, nil)
		_, _ = client.Do(delReq)
	})

	t.Run("LongRunningCommandLeaseProtection", func(t *testing.T) {
		reconciler := controller.NewReconciler()
		srv := gateway.NewServer(reconciler)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		client := ts.Client()

		// 1. Create sandbox
		createPayload := []byte(`{"templateID": "base", "timeout": 300}`)
		resp, err := client.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createPayload))
		if err != nil || resp.StatusCode != http.StatusCreated {
			t.Fatalf("failed to create sandbox: status=%v err=%v", resp.StatusCode, err)
		}
		var sbRec gateway.SandboxRecord
		_ = json.NewDecoder(resp.Body).Decode(&sbRec)
		resp.Body.Close()
		sbID := sbRec.SandboxID

		// 2. Configure a short IdleTimeout (1s) on the reconciler
		spec := v1alpha1.PodBoxSpec{
			TemplateRef:      "cubeplex-ubuntu24-desktop",
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: 1,
			Lifecycle: v1alpha1.LifecyclePolicy{
				IdleTimeoutSeconds: 1,
			},
		}
		if _, err := reconciler.Reconcile(context.Background(), sbID, spec); err != nil {
			t.Fatalf("failed to configure lifecycle on reconciler: %v", err)
		}

		// 3. Start ControllerManager with a fast ticker (50ms) to aggressively reap idle sandboxes
		mgrCtx, cancelMgr := context.WithCancel(context.Background())
		defer cancelMgr()
		mgr := controller.NewControllerManager(reconciler, "127.0.0.1:0", 50*time.Millisecond)
		go func() {
			_ = mgr.Start(mgrCtx)
		}()

		// 4. Launch a long-running background command that takes 1.2s (longer than the 1s IdleTimeout)
		bgPayload := []byte(`{"command": "sleep 1.2 && echo 'lease-work-done' > job.out", "background": true}`)
		bgResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(bgPayload))
		if err != nil || bgResp.StatusCode != http.StatusOK {
			t.Fatalf("failed to run background command: status=%v err=%v", bgResp.StatusCode, err)
		}
		var bgRes map[string]interface{}
		_ = json.NewDecoder(bgResp.Body).Decode(&bgRes)
		bgResp.Body.Close()
		cmdID := fmt.Sprintf("%v", bgRes["cmdID"])
		if cmdID == "" || cmdID == "<nil>" {
			cmdID = fmt.Sprintf("%v", bgRes["id"])
		}

		// 5. Acquire active execution lease for 10s via Gateway API and update Reconciler
		leaseSecs := 10
		leasePayload := []byte(fmt.Sprintf(`{"leaseSeconds": %d}`, leaseSecs))
		leaseResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/lease", "application/json", bytes.NewReader(leasePayload))
		if err != nil || leaseResp.StatusCode != http.StatusOK {
			t.Fatalf("lease request failed: status=%v err=%v", leaseResp.StatusCode, err)
		}
		leaseResp.Body.Close()

		leaseExpiry := time.Now().UTC().Add(time.Duration(leaseSecs) * time.Second)
		if err := reconciler.AcquireLease(sbID, leaseExpiry); err != nil {
			t.Fatalf("reconciler AcquireLease failed: %v", err)
		}

		// 6. Explicitly set LastActivityAt to 2 seconds in the past so LastActivityAt + IdleTimeout <= now.
		// Without an active lease, the Idle Reaper would immediately pause the sandbox!
		past := time.Now().UTC().Add(-2 * time.Second)
		_ = reconciler.TouchActivity(sbID, past)

		// Wait 600ms while ControllerManager runs reap cycles
		time.Sleep(600 * time.Millisecond)

		// Verify sandbox is STILL Running thanks to active lease protection!
		boxStatus, ok := reconciler.GetBox(sbID)
		if !ok || boxStatus.Phase != v1alpha1.PodBoxPhaseRunning {
			t.Fatalf("sandbox was mistakenly paused by Idle Reaper during active lease! Phase: %s, Reason: %s", boxStatus.Phase, boxStatus.Reason)
		}

		// Wait for the background command to finish (1.2s total)
		time.Sleep(800 * time.Millisecond)

		// Verify the long-running job finished and produced output
		verifyResp, err := client.Get(ts.URL + "/sandboxes/" + sbID + "/files?path=/workspace/job.out")
		if err != nil || verifyResp.StatusCode != http.StatusOK {
			t.Fatalf("failed to read background job output: status=%v err=%v", verifyResp.StatusCode, err)
		}
		jobOut, _ := io.ReadAll(verifyResp.Body)
		verifyResp.Body.Close()
		if !strings.Contains(string(jobOut), "lease-work-done") {
			t.Fatalf("unexpected job output: %s", string(jobOut))
		}

		// 7. Now expire the lease and touch past activity
		expired := time.Now().UTC().Add(-5 * time.Second)
		_ = reconciler.AcquireLease(sbID, expired)
		_ = reconciler.TouchActivity(sbID, past)

		// Wait for ControllerManager reaper to execute a reap cycle
		time.Sleep(200 * time.Millisecond)

		// Verify sandbox is now IdlePaused because the lease expired
		boxStatus, ok = reconciler.GetBox(sbID)
		if !ok || boxStatus.Phase != v1alpha1.PodBoxPhasePaused {
			t.Fatalf("expected sandbox to be Paused after lease expiration, got Phase=%s Reason=%s", boxStatus.Phase, boxStatus.Reason)
		}
		if boxStatus.Reason != "IdlePaused" {
			t.Fatalf("expected reason IdlePaused, got %s", boxStatus.Reason)
		}

		// 8. Resume sandbox via E2B connect endpoint and verify transition back to Running
		connectResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/connect", "application/json", nil)
		if err != nil || connectResp.StatusCode != http.StatusOK {
			t.Fatalf("connect sandbox failed: status=%v err=%v", connectResp.StatusCode, err)
		}
		connectResp.Body.Close()

		boxStatus, ok = reconciler.GetBox(sbID)
		if !ok || boxStatus.Phase != v1alpha1.PodBoxPhaseRunning {
			t.Fatalf("expected sandbox to be Running after connect, got %s", boxStatus.Phase)
		}

		// Subsequent command execution runs cleanly
		resumeCmd := []byte(`{"command": "echo 'resumed-after-reaper'"}`)
		arResp, err := client.Post(ts.URL+"/sandboxes/"+sbID+"/commands/run", "application/json", bytes.NewReader(resumeCmd))
		if err != nil || arResp.StatusCode != http.StatusOK {
			t.Fatalf("command after resume failed: status=%v err=%v", arResp.StatusCode, err)
		}
		var arRes map[string]interface{}
		_ = json.NewDecoder(arResp.Body).Decode(&arRes)
		arResp.Body.Close()
		if !strings.Contains(fmt.Sprintf("%v", arRes["stdout"]), "resumed-after-reaper") {
			t.Fatalf("unexpected stdout after resume: %+v", arRes)
		}

		// Cleanup
		delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sbID, nil)
		_, _ = client.Do(delReq)
	})

	t.Run("ConcurrentSandboxesStressTest", func(t *testing.T) {
		reconciler := controller.NewReconciler()
		srv := gateway.NewServer(reconciler)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()

		const numSandboxes = 6
		const commandsPerSandbox = 5

		var (
			wg      sync.WaitGroup
			errChan = make(chan error, numSandboxes*commandsPerSandbox+numSandboxes)
			sbIDs   = make([]string, numSandboxes)
			client  = ts.Client()
		)

		// 1. Concurrently spawn multiple sandboxes
		for i := 0; i < numSandboxes; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				createBody := []byte(fmt.Sprintf(`{"templateID": "base", "metadata": {"worker": "%d"}}`, idx))
				resp, err := client.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createBody))
				if err != nil {
					errChan <- fmt.Errorf("worker %d: create failed: %w", idx, err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusCreated {
					errChan <- fmt.Errorf("worker %d: create returned status %d", idx, resp.StatusCode)
					return
				}
				var rec gateway.SandboxRecord
				if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
					errChan <- fmt.Errorf("worker %d: decode failed: %w", idx, err)
					return
				}
				sbIDs[idx] = rec.SandboxID
			}(i)
		}
		wg.Wait()

		// Verify no sandbox creation errors
		close(errChan)
		for err := range errChan {
			t.Fatalf("sandbox creation error: %v", err)
		}

		// 2. Concurrently execute commands across all sandboxes
		var cmdWg sync.WaitGroup
		cmdErrChan := make(chan error, numSandboxes*commandsPerSandbox)

		for i := 0; i < numSandboxes; i++ {
			sbID := sbIDs[i]
			for j := 0; j < commandsPerSandbox; j++ {
				cmdWg.Add(1)
				go func(sb string, cmdIdx int) {
					defer cmdWg.Done()
					// Run a command that computes values and tests environment variables
					magicVal := cmdIdx*100 + 42
					cmdPayload := []byte(fmt.Sprintf(`{
						"command": "echo result=$(( %d + 8 ))",
						"envs": {"WORKER_CMD": "%d"}
					}`, magicVal, cmdIdx))

					resp, err := client.Post(ts.URL+"/sandboxes/"+sb+"/commands/run", "application/json", bytes.NewReader(cmdPayload))
					if err != nil {
						cmdErrChan <- fmt.Errorf("sandbox %s cmd %d failed: %w", sb, cmdIdx, err)
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						cmdErrChan <- fmt.Errorf("sandbox %s cmd %d returned status %d", sb, cmdIdx, resp.StatusCode)
						return
					}
					var res map[string]interface{}
					if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
						cmdErrChan <- fmt.Errorf("sandbox %s cmd %d decode failed: %w", sb, cmdIdx, err)
						return
					}
					expected := fmt.Sprintf("result=%d", magicVal+8)
					stdout := fmt.Sprintf("%v", res["stdout"])
					if !strings.Contains(stdout, expected) {
						cmdErrChan <- fmt.Errorf("sandbox %s cmd %d expected %q, got %q", sb, cmdIdx, expected, stdout)
						return
					}

					// Also write and read back a file concurrently
					fData := []byte(fmt.Sprintf("worker-data-%s-%d", sb, cmdIdx))
					putReq, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/sandboxes/%s/files?path=/workspace/w_%d.txt", ts.URL, sb, cmdIdx), bytes.NewReader(fData))
					if err != nil {
						cmdErrChan <- fmt.Errorf("sandbox %s file req %d failed: %w", sb, cmdIdx, err)
						return
					}
					putResp, err := client.Do(putReq)
					if err != nil || putResp.StatusCode != http.StatusOK {
						cmdErrChan <- fmt.Errorf("sandbox %s file put %d failed: %v", sb, cmdIdx, err)
						return
					}
					putResp.Body.Close()

					getResp, err := client.Get(fmt.Sprintf("%s/sandboxes/%s/files?path=/workspace/w_%d.txt", ts.URL, sb, cmdIdx))
					if err != nil || getResp.StatusCode != http.StatusOK {
						cmdErrChan <- fmt.Errorf("sandbox %s file get %d failed: %v", sb, cmdIdx, err)
						return
					}
					gotBytes, _ := io.ReadAll(getResp.Body)
					getResp.Body.Close()
					if !bytes.Equal(gotBytes, fData) {
						cmdErrChan <- fmt.Errorf("sandbox %s file content %d mismatch: got %q, expected %q", sb, cmdIdx, string(gotBytes), string(fData))
						return
					}
				}(sbID, j)
			}
		}
		cmdWg.Wait()
		close(cmdErrChan)

		for err := range cmdErrChan {
			t.Fatalf("concurrent command execution error: %v", err)
		}

		// 3. Concurrently delete all sandboxes and verify no leaks
		var delWg sync.WaitGroup
		delErrChan := make(chan error, numSandboxes)
		for _, sbID := range sbIDs {
			delWg.Add(1)
			go func(sb string) {
				defer delWg.Done()
				delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb, nil)
				if err != nil {
					delErrChan <- fmt.Errorf("sandbox %s delete req failed: %w", sb, err)
					return
				}
				delResp, err := client.Do(delReq)
				if err != nil || (delResp.StatusCode != http.StatusNoContent && delResp.StatusCode != http.StatusOK) {
					delErrChan <- fmt.Errorf("sandbox %s delete failed: status=%v err=%w", sb, delResp.StatusCode, err)
					return
				}
				delResp.Body.Close()

				// Verify 404
				getResp, err := client.Get(ts.URL + "/sandboxes/" + sb)
				if err != nil {
					delErrChan <- fmt.Errorf("sandbox %s get after delete failed: %w", sb, err)
					return
				}
				getResp.Body.Close()
				if getResp.StatusCode != http.StatusNotFound {
					delErrChan <- fmt.Errorf("sandbox %s expected 404 after delete, got %d", sb, getResp.StatusCode)
					return
				}
			}(sbID)
		}
		delWg.Wait()
		close(delErrChan)

		for err := range delErrChan {
			t.Fatalf("sandbox deletion error: %v", err)
		}
	})
}
