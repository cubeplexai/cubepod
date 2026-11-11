package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
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
