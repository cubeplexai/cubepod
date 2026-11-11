package gateway_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func TestSandboxLifecycleAndRootfsReset(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create Sandbox
	createBody := []byte(`{"templateID":"cubeplex-ubuntu24-desktop","extension":{"volumeKey":"ws-1-user-1"}}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	var rec gateway.SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode response failed: %v", err)
	}
	if rec.State != "Running" || rec.RootfsGeneration != 1 {
		t.Fatalf("unexpected initial record: %+v", rec)
	}

	// 2. Pause Sandbox (delete compute pod, retain block PVCs)
	pauseResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	if err != nil {
		t.Fatalf("pause failed: %v", err)
	}
	defer pauseResp.Body.Close()
	_ = json.NewDecoder(pauseResp.Body).Decode(&rec)
	if rec.State != "Paused" {
		t.Fatalf("expected state Paused, got %s", rec.State)
	}

	// 3. Connect Sandbox (auto-resume from Paused)
	connResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/connect", "application/json", nil)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer connResp.Body.Close()
	_ = json.NewDecoder(connResp.Body).Decode(&rec)
	if rec.State != "Running" {
		t.Fatalf("expected state Running after connect, got %s", rec.State)
	}

	// 4. Reset RootFS Upper while preserving /workspace
	resetResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/reset-rootfs", "application/json", nil)
	if err != nil {
		t.Fatalf("reset-rootfs failed: %v", err)
	}
	defer resetResp.Body.Close()
	_ = json.NewDecoder(resetResp.Body).Decode(&rec)
	if rec.RootfsGeneration != 2 {
		t.Fatalf("expected RootfsGeneration 2, got %d", rec.RootfsGeneration)
	}

	// 5. Fork Sandbox (CSI CoW clone)
	forkResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/fork", "application/json", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}
	defer forkResp.Body.Close()
	if forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created on fork, got %d", forkResp.StatusCode)
	}
	var childRec gateway.SandboxRecord
	_ = json.NewDecoder(forkResp.Body).Decode(&childRec)
	if childRec.SandboxID == rec.SandboxID || childRec.State != "Running" {
		t.Fatalf("unexpected forked record: %+v", childRec)
	}

	// 6. Delete sandbox
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+rec.SandboxID, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil || delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete failed: status=%d err=%v", delResp.StatusCode, err)
	}
	delResp.Body.Close()
}

func TestSandboxListTimeoutAndLease(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create sandbox 1
	resp1, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"tpl-1"}`)))
	if err != nil {
		t.Fatalf("failed to create sandbox 1: %v", err)
	}
	var rec1 gateway.SandboxRecord
	_ = json.NewDecoder(resp1.Body).Decode(&rec1)
	resp1.Body.Close()

	// Create sandbox 2 and pause it
	resp2, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"tpl-2"}`)))
	if err != nil {
		t.Fatalf("failed to create sandbox 2: %v", err)
	}
	var rec2 gateway.SandboxRecord
	_ = json.NewDecoder(resp2.Body).Decode(&rec2)
	resp2.Body.Close()

	pResp, _ := http.Post(ts.URL+"/sandboxes/"+rec2.SandboxID+"/pause", "application/json", nil)
	pResp.Body.Close()

	// Test GET /sandboxes (all)
	listResp, err := http.Get(ts.URL + "/sandboxes")
	if err != nil {
		t.Fatalf("failed to list sandboxes: %v", err)
	}
	var all []*gateway.SandboxRecord
	_ = json.NewDecoder(listResp.Body).Decode(&all)
	listResp.Body.Close()
	if len(all) < 2 {
		t.Fatalf("expected at least 2 sandboxes, got %d", len(all))
	}

	// Test GET /sandboxes?state=Running
	runResp, err := http.Get(ts.URL + "/sandboxes?state=Running")
	if err != nil {
		t.Fatalf("failed to filter sandboxes by running: %v", err)
	}
	var runningList []*gateway.SandboxRecord
	_ = json.NewDecoder(runResp.Body).Decode(&runningList)
	runResp.Body.Close()
	if len(runningList) != 1 || runningList[0].SandboxID != rec1.SandboxID {
		t.Fatalf("expected 1 running sandbox, got %d", len(runningList))
	}

	// Test POST /sandboxes/{id}/timeout
	timeoutBody := []byte(`{"timeoutSeconds": 3600}`)
	tResp, err := http.Post(ts.URL+"/sandboxes/"+rec1.SandboxID+"/timeout", "application/json", bytes.NewReader(timeoutBody))
	if err != nil {
		t.Fatalf("failed to update timeout: %v", err)
	}
	var updatedRec gateway.SandboxRecord
	_ = json.NewDecoder(tResp.Body).Decode(&updatedRec)
	tResp.Body.Close()
	if updatedRec.TimeoutSeconds != 3600 {
		t.Fatalf("expected TimeoutSeconds=3600, got %d", updatedRec.TimeoutSeconds)
	}

	// Test POST /sandboxes/{id}/lease
	leaseBody := []byte(`{"leaseSeconds": 300}`)
	lResp, err := http.Post(ts.URL+"/sandboxes/"+rec1.SandboxID+"/lease", "application/json", bytes.NewReader(leaseBody))
	if err != nil {
		t.Fatalf("failed to renew lease: %v", err)
	}
	_ = json.NewDecoder(lResp.Body).Decode(&updatedRec)
	lResp.Body.Close()
	if updatedRec.LeaseSeconds != 300 || updatedRec.LeaseExpiresAt == nil {
		t.Fatalf("expected LeaseSeconds=300 and valid expiry, got %+v", updatedRec)
	}

	// Test GET /health
	hResp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	var health map[string]interface{}
	_ = json.NewDecoder(hResp.Body).Decode(&health)
	hResp.Body.Close()
	if health["status"] != "ok" || health["service"] != "cubepod-gateway" {
		t.Fatalf("unexpected health response: %+v", health)
	}
}

func TestCommandExecutionAndAutoResume(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create sandbox
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"default"}`)))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()

	// 2. Foreground Command
	cmdBody := []byte(`{"cmd": "echo 'cubepod-hello-test'", "envs": {"TEST_VAR": "cubepod-val"}}`)
	runResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader(cmdBody))
	if err != nil {
		t.Fatalf("run foreground command failed: %v", err)
	}
	var runResult map[string]interface{}
	_ = json.NewDecoder(runResp.Body).Decode(&runResult)
	runResp.Body.Close()

	stdout, _ := runResult["stdout"].(string)
	if runResult["exitCode"].(float64) != 0 || stdout != "cubepod-hello-test\n" {
		t.Fatalf("unexpected foreground command result: %+v", runResult)
	}
	cmdID := runResult["cmdID"].(string)

	// Verify command logs retrieval with cursor
	logResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/" + cmdID + "/logs?cursor=0")
	if err != nil {
		t.Fatalf("get logs failed: %v", err)
	}
	var logResult map[string]interface{}
	_ = json.NewDecoder(logResp.Body).Decode(&logResult)
	logResp.Body.Close()
	if logResult["content"].(string) != "cubepod-hello-test\n" {
		t.Fatalf("expected logged content, got: %+v", logResult)
	}

	// 3. Background Command & Kill
	bgBody := []byte(`{"cmd": "for i in $(seq 1 10); do echo step $i; sleep 0.05; done", "background": true}`)
	bgResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader(bgBody))
	if err != nil {
		t.Fatalf("start bg command failed: %v", err)
	}
	var bgResult map[string]interface{}
	_ = json.NewDecoder(bgResp.Body).Decode(&bgResult)
	bgResp.Body.Close()

	bgCmdID := bgResult["cmdID"].(string)
	if bgResult["running"] != true {
		t.Fatalf("expected bg command running=true, got: %+v", bgResult)
	}

	time.Sleep(120 * time.Millisecond)

	// Fetch partial logs
	partialResp, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/" + bgCmdID + "/logs?cursor=0")
	var partialLog map[string]interface{}
	_ = json.NewDecoder(partialResp.Body).Decode(&partialLog)
	partialResp.Body.Close()
	content := partialLog["content"].(string)
	if len(content) == 0 {
		t.Fatalf("expected some output from background command, got empty")
	}

	// Kill background command
	killResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/"+bgCmdID+"/kill", "application/json", nil)
	if err != nil {
		t.Fatalf("kill command failed: %v", err)
	}
	var killResult map[string]interface{}
	_ = json.NewDecoder(killResp.Body).Decode(&killResult)
	killResp.Body.Close()
	if killResult["killed"] != true {
		t.Fatalf("expected killed=true, got: %+v", killResult)
	}

	// 4. Test Auto-Resume when executing command on Paused sandbox
	pResp, _ := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	pResp.Body.Close()

	// Verify it's Paused
	getResp, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	_ = json.NewDecoder(getResp.Body).Decode(&rec)
	getResp.Body.Close()
	if rec.State != "Paused" {
		t.Fatalf("expected state Paused before auto-resume test, got %s", rec.State)
	}

	// Trigger command on paused sandbox
	autoResumeCmd := []byte(`{"cmd": "echo 'resumed-successfully'"}`)
	arResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader(autoResumeCmd))
	if err != nil {
		t.Fatalf("command on paused sandbox failed: %v", err)
	}
	var arResult map[string]interface{}
	_ = json.NewDecoder(arResp.Body).Decode(&arResult)
	arResp.Body.Close()
	if arResult["stdout"].(string) != "resumed-successfully\n" {
		t.Fatalf("unexpected output after auto-resume: %+v", arResult)
	}

	// Verify sandbox is now Running and AutoResumeCount was incremented
	getResp2, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	_ = json.NewDecoder(getResp2.Body).Decode(&rec)
	getResp2.Body.Close()
	if rec.State != "Running" || rec.AutoResumeCount < 1 {
		t.Fatalf("expected state Running and AutoResumeCount >= 1, got state=%s count=%d", rec.State, rec.AutoResumeCount)
	}
}

func TestFileOperationsAndArchiveSyncWithAutoResume(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create sandbox
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"default"}`)))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()

	// 2. PUT /sandboxes/{id}/files
	fileContent := []byte("hello from cubepod file upload!")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=/workspace/subdir/notes.txt", bytes.NewReader(fileContent))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("put file failed: status=%d err=%v", putResp.StatusCode, err)
	}
	putResp.Body.Close()

	// 3. GET /sandboxes/{id}/files (raw bytes)
	getResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files?path=/workspace/subdir/notes.txt")
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("get file failed: status=%d err=%v", getResp.StatusCode, err)
	}
	bodyBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if string(bodyBytes) != string(fileContent) {
		t.Fatalf("expected %q, got %q", string(fileContent), string(bodyBytes))
	}

	// 4. GET /sandboxes/{id}/files?format=json
	jsonGetResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files?path=/workspace/subdir/notes.txt&format=json")
	if err != nil || jsonGetResp.StatusCode != http.StatusOK {
		t.Fatalf("get json file failed: status=%d err=%v", jsonGetResp.StatusCode, err)
	}
	var fileJSON map[string]interface{}
	_ = json.NewDecoder(jsonGetResp.Body).Decode(&fileJSON)
	jsonGetResp.Body.Close()
	if fileJSON["content"] != string(fileContent) {
		t.Fatalf("unexpected json content: %+v", fileJSON)
	}

	// 5. Build tar.gz archive in memory with multiple skill files
	var tarGzBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gzw)

	filesToAdd := map[string]string{
		"skills/agent.py":      "print('skill initialized')",
		"skills/manifest.json": `{"name":"calculator","version":"1.0"}`,
	}
	for name, content := range filesToAdd {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header failed: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar body failed: %v", err)
		}
	}
	tw.Close()
	gzw.Close()

	// 6. Pause sandbox before archive-sync to verify auto-resume!
	pResp, _ := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	pResp.Body.Close()

	syncResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/files/archive-sync", "application/gzip", bytes.NewReader(tarGzBuf.Bytes()))
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("archive-sync failed: status=%d err=%v", syncResp.StatusCode, err)
	}
	var syncResult map[string]interface{}
	_ = json.NewDecoder(syncResp.Body).Decode(&syncResult)
	syncResp.Body.Close()
	if syncResult["extracted"].(float64) != 2 {
		t.Fatalf("expected 2 extracted files, got %+v", syncResult)
	}

	// Verify sandbox automatically resumed
	chkResp, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	_ = json.NewDecoder(chkResp.Body).Decode(&rec)
	chkResp.Body.Close()
	if rec.State != "Running" || rec.AutoResumeCount < 1 {
		t.Fatalf("expected auto-resumed state Running, got %s (count=%d)", rec.State, rec.AutoResumeCount)
	}

	// 7. Verify extracted files can be retrieved via GET /files
	chkFileResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files?path=/workspace/skills/agent.py")
	if err != nil || chkFileResp.StatusCode != http.StatusOK {
		t.Fatalf("read extracted skill failed: status=%d err=%v", chkFileResp.StatusCode, err)
	}
	skillBody, _ := io.ReadAll(chkFileResp.Body)
	chkFileResp.Body.Close()
	if string(skillBody) != "print('skill initialized')" {
		t.Fatalf("unexpected extracted content: %s", string(skillBody))
	}
}

func TestSignedPanelTokenAndReverseProxy(t *testing.T) {
	// 1. Mock upstream server simulating sandbox's internal web panel / Neko browser
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mock upstream hello"))
		case "/api/data":
			query := r.URL.Query().Get("q")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "query-result:%s", query)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockUpstream.Close()

	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	srv.SetPanelSecret("test-secret-key-for-panel-proxy")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 2. Create sandbox
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"desktop"}`)))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()

	// 3. Register sandbox port 8080 target upstream
	srv.RegisterPort(rec.SandboxID, 8080, mockUpstream.URL)

	// 4. Mint signed panel token via POST /sandboxes/{id}/endpoints/8080
	mintBody := []byte(`{"expiresInSeconds": 3600}`)
	mintResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/endpoints/8080", "application/json", bytes.NewReader(mintBody))
	if err != nil || mintResp.StatusCode != http.StatusOK {
		t.Fatalf("mint token failed: status=%d err=%v", mintResp.StatusCode, err)
	}
	var mintResult map[string]string
	_ = json.NewDecoder(mintResp.Body).Decode(&mintResult)
	mintResp.Body.Close()

	token := mintResult["token"]
	if token == "" || mintResult["url"] == "" {
		t.Fatalf("invalid mint result: %+v", mintResult)
	}

	// 5. Test reverse proxy with valid token: GET /panel/{token}/hello
	proxyResp, err := http.Get(ts.URL + "/panel/" + token + "/hello")
	if err != nil || proxyResp.StatusCode != http.StatusOK {
		t.Fatalf("proxy request failed: status=%d err=%v", proxyResp.StatusCode, err)
	}
	proxyBody, _ := io.ReadAll(proxyResp.Body)
	proxyResp.Body.Close()
	if string(proxyBody) != "mock upstream hello" {
		t.Fatalf("unexpected proxy response: %s", string(proxyBody))
	}

	// Test query parameter forwarding: GET /panel/{token}/api/data?q=cubepod
	qResp, err := http.Get(ts.URL + "/panel/" + token + "/api/data?q=cubepod")
	if err != nil || qResp.StatusCode != http.StatusOK {
		t.Fatalf("proxy query request failed: status=%d err=%v", qResp.StatusCode, err)
	}
	qBody, _ := io.ReadAll(qResp.Body)
	qResp.Body.Close()
	if string(qBody) != "query-result:cubepod" {
		t.Fatalf("unexpected query proxy response: %s", string(qBody))
	}

	// 6. Test invalid / tampered token -> 401 Unauthorized
	badToken := token + "tampered"
	badResp, err := http.Get(ts.URL + "/panel/" + badToken + "/hello")
	if err != nil {
		t.Fatalf("tampered request error: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for tampered token, got %d", badResp.StatusCode)
	}

	// 7. Test expired token -> 401 Unauthorized
	expMintBody := []byte(`{"expiresInSeconds": -10}`)
	expResp, _ := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/endpoints/8080", "application/json", bytes.NewReader(expMintBody))
	var expResult map[string]string
	_ = json.NewDecoder(expResp.Body).Decode(&expResult)
	expResp.Body.Close()

	expiredToken := expResult["token"]
	expProxyResp, err := http.Get(ts.URL + "/panel/" + expiredToken + "/hello")
	if err != nil {
		t.Fatalf("expired proxy request error: %v", err)
	}
	expProxyResp.Body.Close()
	if expProxyResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for expired token, got %d", expProxyResp.StatusCode)
	}

	// 8. Test Auto-Resume through panel proxy when sandbox is Paused!
	pResp, _ := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	pResp.Body.Close()

	// Verify it's Paused
	getResp, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	_ = json.NewDecoder(getResp.Body).Decode(&rec)
	getResp.Body.Close()
	if rec.State != "Paused" {
		t.Fatalf("expected sandbox to be Paused, got %s", rec.State)
	}

	// Request /panel/{token}/hello on the paused sandbox
	resumeProxyResp, err := http.Get(ts.URL + "/panel/" + token + "/hello")
	if err != nil || resumeProxyResp.StatusCode != http.StatusOK {
		t.Fatalf("panel proxy auto-resume request failed: status=%d err=%v", resumeProxyResp.StatusCode, err)
	}
	resumeProxyBody, _ := io.ReadAll(resumeProxyResp.Body)
	resumeProxyResp.Body.Close()
	if string(resumeProxyBody) != "mock upstream hello" {
		t.Fatalf("unexpected response after auto-resume: %s", string(resumeProxyBody))
	}

	// Verify sandbox is now Running
	getResp2, _ := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID)
	_ = json.NewDecoder(getResp2.Body).Decode(&rec)
	getResp2.Body.Close()
	if rec.State != "Running" || rec.AutoResumeCount < 1 {
		t.Fatalf("expected sandbox state Running and AutoResumeCount >= 1, got state=%s count=%d", rec.State, rec.AutoResumeCount)
	}
}

func TestPoddDataPlaneForwardingAndPrepareStopOnPause(t *testing.T) {
	var (
		runCalled         bool
		logsCalled        bool
		killCalled        bool
		putFileCalled     bool
		getFileCalled     bool
		archiveSyncCalled bool
		prepareStopCalled bool
	)

	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			runCalled = true
			_, _ = w.Write([]byte(`{"id":"podd-cmd-1","cmdID":"podd-cmd-1","stdout":"forwarded-podd-out\n","exitCode":0}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/commands/") && strings.HasSuffix(r.URL.Path, "/logs"):
			logsCalled = true
			cursor := r.URL.Query().Get("cursor")
			_, _ = fmt.Fprintf(w, `{"id":"podd-cmd-1","content":"podd-log-cursor-%s","cursor":10,"running":false}`, cursor)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/commands/") && strings.HasSuffix(r.URL.Path, "/kill"):
			killCalled = true
			_, _ = w.Write([]byte(`{"id":"podd-cmd-1","killed":true}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/files":
			putFileCalled = true
			p := r.URL.Query().Get("path")
			_, _ = fmt.Fprintf(w, `{"status":"ok","path":"%s","writtenBy":"podd"}`, p)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			getFileCalled = true
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("podd file content"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files/archive-sync":
			archiveSyncCalled = true
			_, _ = w.Write([]byte(`{"status":"ok","extracted":3,"daemon":"podd"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/lifecycle/prepare-stop":
			prepareStopCalled = true
			_, _ = w.Write([]byte(`{"status":"synced"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockPodd.Close()

	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create sandbox
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader([]byte(`{"templateID":"kata-pod"}`)))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	resp.Body.Close()

	// 2. Register live podd daemon URL
	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	// 3. Test /commands/run forwarding
	runResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", bytes.NewReader([]byte(`{"cmd":"ls"}`)))
	if err != nil || runResp.StatusCode != http.StatusOK {
		t.Fatalf("run failed: status=%d err=%v", runResp.StatusCode, err)
	}
	runBody, _ := io.ReadAll(runResp.Body)
	runResp.Body.Close()
	if !strings.Contains(string(runBody), "forwarded-podd-out") || !runCalled {
		t.Fatalf("expected podd command response, got: %s", string(runBody))
	}

	// 4. Test /commands/{cmdID}/logs forwarding
	logResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/podd-cmd-1/logs?cursor=5")
	if err != nil || logResp.StatusCode != http.StatusOK {
		t.Fatalf("logs failed: status=%d err=%v", logResp.StatusCode, err)
	}
	logBody, _ := io.ReadAll(logResp.Body)
	logResp.Body.Close()
	if !strings.Contains(string(logBody), "podd-log-cursor-5") || !logsCalled {
		t.Fatalf("expected podd logs, got: %s", string(logBody))
	}

	// 5. Test /commands/{cmdID}/kill forwarding
	killResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/podd-cmd-1/kill", "application/json", nil)
	if err != nil || killResp.StatusCode != http.StatusOK {
		t.Fatalf("kill failed: status=%d err=%v", killResp.StatusCode, err)
	}
	killBody, _ := io.ReadAll(killResp.Body)
	killResp.Body.Close()
	if !strings.Contains(string(killBody), "killed") || !killCalled {
		t.Fatalf("expected podd kill, got: %s", string(killBody))
	}

	// 6. Test PUT /files forwarding
	reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=/workspace/test.py", bytes.NewReader([]byte("code")))
	putResp, err := http.DefaultClient.Do(reqPut)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("put file failed: status=%d err=%v", putResp.StatusCode, err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if !strings.Contains(string(putBody), "podd") || !putFileCalled {
		t.Fatalf("expected podd put file, got: %s", string(putBody))
	}

	// 7. Test GET /files forwarding
	getResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files?path=/workspace/test.py")
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("get file failed: status=%d err=%v", getResp.StatusCode, err)
	}
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if string(getBody) != "podd file content" || !getFileCalled {
		t.Fatalf("expected podd get file, got: %s", string(getBody))
	}

	// 8. Test POST /files/archive-sync forwarding
	syncResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/files/archive-sync", "application/gzip", bytes.NewReader([]byte("fake-tar")))
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("archive-sync failed: status=%d err=%v", syncResp.StatusCode, err)
	}
	syncBody, _ := io.ReadAll(syncResp.Body)
	syncResp.Body.Close()
	if !strings.Contains(string(syncBody), `"daemon":"podd"`) || !archiveSyncCalled {
		t.Fatalf("expected podd archive-sync, got: %s", string(syncBody))
	}

	// 9. Test POST /pause calls <PoddURL>/v1/lifecycle/prepare-stop
	pResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/pause", "application/json", nil)
	if err != nil || pResp.StatusCode != http.StatusOK {
		t.Fatalf("pause failed: status=%d err=%v", pResp.StatusCode, err)
	}
	pResp.Body.Close()
	if !prepareStopCalled {
		t.Fatalf("expected /v1/lifecycle/prepare-stop to be called during pause, but was not")
	}
}

func TestGatewayEgressExchangeEndpoint(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sandboxID := "sbx-gw-egress-01"
	rawSecret := "sk-live-gateway-secret-42"
	placeholder, err := srv.Vault().BindSecret(
		sandboxID,
		"OPENAI_KEY",
		rawSecret,
		[]string{"api.openai.com"},
		[]string{"Authorization"},
	)
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	// 1. Authorized exchange
	body := fmt.Sprintf(`{"sandboxID":"%s","placeholder":"%s","sniHost":"api.openai.com","headerName":"Authorization"}`, sandboxID, placeholder)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/v1/egress/exchange", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CubePod-Sandbox-ID", sandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange failed: status=%d err=%v", resp.StatusCode, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var exResp map[string]string
	_ = json.Unmarshal(respBody, &exResp)
	if exResp["secret"] != rawSecret {
		t.Fatalf("expected secret %q, got %+v", rawSecret, exResp)
	}

	// 2. Forbidden host
	badHostReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/v1/egress/exchange", strings.NewReader(
		fmt.Sprintf(`{"sandboxID":"%s","placeholder":"%s","sniHost":"evil.com","headerName":"Authorization"}`, sandboxID, placeholder),
	))
	badHostReq.Header.Set("Content-Type", "application/json")
	badHostReq.Header.Set("X-CubePod-Sandbox-ID", sandboxID)
	badHostResp, err := http.DefaultClient.Do(badHostReq)
	if err != nil {
		t.Fatalf("bad host request failed: %v", err)
	}
	defer badHostResp.Body.Close()
	if badHostResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unallowed host, got %d", badHostResp.StatusCode)
	}
}

func TestFileDeleteAndList(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create Sandbox
	createBody := []byte(`{"templateID":"cubeplex-ubuntu24-desktop"}`)
	resp, err := http.Post(ts.URL+"/sandboxes", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	defer resp.Body.Close()
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(resp.Body).Decode(&rec)

	// 2. PUT two files
	putReq1, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=file1.txt", strings.NewReader("hello file 1"))
	putResp1, err := http.DefaultClient.Do(putReq1)
	if err != nil || putResp1.StatusCode != http.StatusOK {
		t.Fatalf("put file 1 failed: status=%d err=%v", putResp1.StatusCode, err)
	}
	putResp1.Body.Close()

	putReq2, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=subdir/file2.txt", strings.NewReader("hello file 2 in sub"))
	putResp2, err := http.DefaultClient.Do(putReq2)
	if err != nil || putResp2.StatusCode != http.StatusOK {
		t.Fatalf("put file 2 failed: status=%d err=%v", putResp2.StatusCode, err)
	}
	putResp2.Body.Close()

	// 3. GET files/list on /workspace
	listResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files/list?path=/workspace")
	if err != nil || listResp.StatusCode != http.StatusOK {
		t.Fatalf("list files failed: status=%d err=%v", listResp.StatusCode, err)
	}
	defer listResp.Body.Close()

	var listData struct {
		Path    string `json:"path"`
		Entries []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
			Mode  string `json:"mode"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listData); err != nil {
		t.Fatalf("decode list response failed: %v", err)
	}
	if len(listData.Entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(listData.Entries))
	}

	foundFile1 := false
	foundSubdir := false
	for _, entry := range listData.Entries {
		if entry.Name == "file1.txt" {
			foundFile1 = true
			if entry.IsDir || entry.Size != int64(len("hello file 1")) {
				t.Errorf("unexpected file1 info: %+v", entry)
			}
		}
		if entry.Name == "subdir" {
			foundSubdir = true
			if !entry.IsDir {
				t.Errorf("expected subdir to be a directory: %+v", entry)
			}
		}
	}
	if !foundFile1 || !foundSubdir {
		t.Fatalf("expected file1.txt and subdir in list, got %+v", listData.Entries)
	}

	// 4. DELETE /sandboxes/{id}/files?path=file1.txt
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=file1.txt", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete file failed: status=%d err=%v", delResp.StatusCode, err)
	}
	defer delResp.Body.Close()
	var delData map[string]interface{}
	_ = json.NewDecoder(delResp.Body).Decode(&delData)
	if delData["deleted"] != "file1.txt" {
		t.Fatalf("expected deleted file1.txt, got %+v", delData)
	}

	// 5. GET deleted file -> 404
	getResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files?path=file1.txt")
	if err != nil || getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for deleted file, got %d", getResp.StatusCode)
	}
	getResp.Body.Close()

	// 6. DELETE non-existent file -> 404
	delNotFoundReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+rec.SandboxID+"/files?path=file1.txt", nil)
	delNotFoundResp, err := http.DefaultClient.Do(delNotFoundReq)
	if err != nil || delNotFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on deleting non-existent file, got %d", delNotFoundResp.StatusCode)
	}
	delNotFoundResp.Body.Close()

	// 7. GET list on non-existent directory -> 404
	badListResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/files/list?path=/workspace/nonexistent")
	if err != nil || badListResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent dir, got %d", badListResp.StatusCode)
	}
	badListResp.Body.Close()
}

func TestCommandStatusAndSSEStream(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Create sandbox
	createResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	defer createResp.Body.Close()
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)

	// 2. Start background command
	cmdReq := `{"command":"echo step1 && sleep 0.05 && echo step2","background":true}`
	runResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/commands/run", "application/json", strings.NewReader(cmdReq))
	if err != nil || runResp.StatusCode != http.StatusOK {
		t.Fatalf("run command failed: status=%d err=%v", runResp.StatusCode, err)
	}
	defer runResp.Body.Close()

	var runData struct {
		CmdID   string `json:"cmdID"`
		Running bool   `json:"running"`
		PID     int    `json:"pid"`
	}
	_ = json.NewDecoder(runResp.Body).Decode(&runData)
	if runData.CmdID == "" {
		t.Fatalf("expected cmdID from run command, got %+v", runData)
	}

	// 3. GET /commands/{id}/status
	statusResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/" + runData.CmdID + "/status")
	if err != nil || statusResp.StatusCode != http.StatusOK {
		t.Fatalf("get status failed: status=%d err=%v", statusResp.StatusCode, err)
	}
	defer statusResp.Body.Close()

	var stData struct {
		ID       string `json:"id"`
		CmdID    string `json:"cmdID"`
		PID      int    `json:"pid"`
		Running  bool   `json:"running"`
		ExitCode int    `json:"exit_code"`
		Cursor   int    `json:"cursor"`
	}
	_ = json.NewDecoder(statusResp.Body).Decode(&stData)
	if stData.CmdID != runData.CmdID || stData.PID <= 0 {
		t.Fatalf("unexpected status data: %+v", stData)
	}

	// 4. GET /commands/{id}/stream (SSE)
	streamResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/" + runData.CmdID + "/stream")
	if err != nil || streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream logs failed: status=%d err=%v", streamResp.StatusCode, err)
	}
	defer streamResp.Body.Close()

	contentType := streamResp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("expected text/event-stream content-type, got %s", contentType)
	}

	var frames []string
	buf := make([]byte, 4096)
	var finalRunning bool
	timeout := time.After(5 * time.Second)

loop:
	for {
		select {
		case <-timeout:
			t.Fatalf("stream timed out without completing")
		default:
		}

		n, err := streamResp.Body.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			lines := strings.Split(chunk, "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "data: ") {
					jsonPart := strings.TrimPrefix(line, "data: ")
					frames = append(frames, jsonPart)
					var frameMap map[string]interface{}
					if err := json.Unmarshal([]byte(jsonPart), &frameMap); err == nil {
						if running, ok := frameMap["running"].(bool); ok && !running {
							finalRunning = false
							break loop
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read stream error: %v", err)
		}
	}

	if len(frames) == 0 {
		t.Fatalf("expected at least 1 SSE frame")
	}
	if finalRunning {
		t.Errorf("expected final command state to not be running")
	}

	// 5. GET non-existent command status -> 404
	nfStatus, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/commands/nonexistent/status")
	if err != nil || nfStatus.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent command status, got %d", nfStatus.StatusCode)
	}
	nfStatus.Body.Close()
}

func TestWarmPoolRoutes(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. POST /warmpools
	poolReq := `{"name":"pool-cep12","templateRef":"cubeplex-ubuntu24-desktop","replicas":2}`
	postResp, err := http.Post(ts.URL+"/warmpools", "application/json", strings.NewReader(poolReq))
	if err != nil || postResp.StatusCode != http.StatusCreated {
		t.Fatalf("create warm pool failed: status=%d err=%v", postResp.StatusCode, err)
	}
	defer postResp.Body.Close()

	var createdPool struct {
		Name          string `json:"name"`
		TemplateRef   string `json:"templateRef"`
		Replicas      int32  `json:"replicas"`
		ReadyReplicas int32  `json:"readyReplicas"`
	}
	_ = json.NewDecoder(postResp.Body).Decode(&createdPool)
	if createdPool.Name != "pool-cep12" || createdPool.ReadyReplicas != 2 {
		t.Fatalf("unexpected created pool: %+v", createdPool)
	}

	// 2. GET /warmpools
	getResp, err := http.Get(ts.URL + "/warmpools")
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("list warm pools failed: status=%d err=%v", getResp.StatusCode, err)
	}
	defer getResp.Body.Close()

	var listData struct {
		WarmPools []struct {
			Name          string `json:"name"`
			TemplateRef   string `json:"templateRef"`
			Replicas      int32  `json:"replicas"`
			ReadyReplicas int32  `json:"readyReplicas"`
		} `json:"warmPools"`
		WarmPoolReadyCount int `json:"warmPoolReadyCount"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&listData)
	if len(listData.WarmPools) == 0 || listData.WarmPoolReadyCount < 2 {
		t.Fatalf("unexpected warm pools list: %+v", listData)
	}
	found := false
	for _, p := range listData.WarmPools {
		if p.Name == "pool-cep12" && p.ReadyReplicas == 2 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pool-cep12 not found in list: %+v", listData.WarmPools)
	}
}

func TestTemplateRoutesAndOpenAPI(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. GET /templates
	tplResp, err := http.Get(ts.URL + "/templates")
	if err != nil || tplResp.StatusCode != http.StatusOK {
		t.Fatalf("get templates failed: status=%d err=%v", tplResp.StatusCode, err)
	}
	defer tplResp.Body.Close()

	var tplData struct {
		Templates []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Image       string `json:"image"`
			Description string `json:"description"`
		} `json:"templates"`
	}
	_ = json.NewDecoder(tplResp.Body).Decode(&tplData)
	if len(tplData.Templates) == 0 {
		t.Fatalf("expected non-empty templates list")
	}

	// 2. GET /openapi.yaml
	apiResp, err := http.Get(ts.URL + "/openapi.yaml")
	if err != nil || apiResp.StatusCode != http.StatusOK {
		t.Fatalf("get openapi.yaml failed: status=%d err=%v", apiResp.StatusCode, err)
	}
	defer apiResp.Body.Close()
	data, _ := io.ReadAll(apiResp.Body)
	if !strings.Contains(string(data), "openapi: 3.1.0") || !strings.Contains(string(data), "/warmpools") {
		t.Fatalf("expected openapi spec containing openapi: 3.1.0 and /warmpools")
	}
}

func TestLivePodRoutingAndEndpointDefault(t *testing.T) {
	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	srv.SetLivePodRouting(true)
	if !srv.LivePodRouting() {
		t.Fatalf("expected livePodRouting to be true")
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create a sandbox
	createResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	defer createResp.Body.Close()
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)

	// Set a custom podd URL
	mockPoddURL := "http://10.244.5.10:49983"
	srv.SetPoddURL(rec.SandboxID, mockPoddURL)

	// Mint endpoint with no targetURL override: should default targetURL to host of podd
	mintReq := `{"expiresInSeconds":3600}`
	mintResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/endpoints/8080", "application/json", strings.NewReader(mintReq))
	if err != nil || mintResp.StatusCode != http.StatusOK {
		t.Fatalf("mint endpoint failed: status=%d err=%v", mintResp.StatusCode, err)
	}
	defer mintResp.Body.Close()

	registered := srv.GetRegisteredPort(rec.SandboxID, 8080)
	if !strings.Contains(registered, ":8080") {
		t.Fatalf("expected registered target URL for port 8080, got %q", registered)
	}
}
