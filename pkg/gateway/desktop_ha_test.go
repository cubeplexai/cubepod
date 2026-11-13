package gateway_test

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func computeWebSocketAccept(key string) string {
	const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.Sum([]byte(strings.TrimSpace(key) + wsMagic))
	return base64.StdEncoding.EncodeToString(h[:])
}

func TestDesktopLifecycleAndWebSocketPanel(t *testing.T) {
	// 1. Mock WebSocket/HTTP upstream for desktop :8080
	wsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported", http.StatusInternalServerError)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()

			accept := computeWebSocketAccept(r.Header.Get("Sec-WebSocket-Key"))
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
			_ = rw.Flush()

			// Echo raw bytes back to verify bidirectional WebSocket tunnel
			buf := make([]byte, 256)
			n, err := rw.Read(buf)
			if err == nil && n > 0 {
				_, _ = rw.Write(buf[:n])
				_ = rw.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "novnc-ok path=%s query=%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer wsUpstream.Close()

	wsURL, _ := url.Parse(wsUpstream.URL)
	_, wsPortStr, _ := net.SplitHostPort(wsURL.Host)
	wsPort, _ := strconv.Atoi(wsPortStr)

	// 2. Mock podd server
	var mu sync.Mutex
	var lastRunCmd string
	var lastUID, lastGID int
	installed := true

	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			var req struct {
				Command string `json:"command"`
				UID     int    `json:"uid"`
				GID     int    `json:"gid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			lastRunCmd = req.Command
			lastUID = req.UID
			lastGID = req.GID
			isInstalled := installed
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if !isInstalled {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":        "cmd-1",
					"exit_code": 127,
					"output":    "bash: /opt/cubepod/desktop/cubepod-desktop-ctl: No such file or directory",
				})
				return
			}

			statusJSON := fmt.Sprintf(
				`{"status":"running","display":":99","resolution":"1920x1080x24","ports":{"desktop":%d,"terminal":7681,"cdp":9222},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15}}`,
				wsPort,
			)
			if strings.Contains(req.Command, " stop") {
				statusJSON = fmt.Sprintf(
					`{"status":"stopped","display":":99","resolution":"1920x1080x24","ports":{"desktop":%d,"terminal":7681,"cdp":9222},"pids":{}}`,
					wsPort,
				)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "cmd-1",
				"exit_code": 0,
				"output":    statusJSON,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockPodd.Close()

	reconciler := controller.NewReconciler()
	srv := gateway.NewServer(reconciler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create sandbox & attach mock podd
	createResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"cubeplex-ubuntu24-desktop"}`))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)
	_ = createResp.Body.Close()
	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	// 3. Invalid resolution must be rejected with 400
	badResp, err := http.Post(
		ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start",
		"application/json",
		strings.NewReader(`{"resolution":"1280x800; rm -rf /"}`),
	)
	if err != nil {
		t.Fatalf("bad resolution request failed: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid resolution, got %d", badResp.StatusCode)
	}

	// 4. POST /desktop/start succeeds and mints /panel/{token}/ URLs
	startResp, err := http.Post(
		ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start",
		"application/json",
		strings.NewReader(`{"resolution":"1920x1080x24","initialUrl":"https://example.com/?a=1&b=2","expiresInSeconds":600}`),
	)
	if err != nil {
		t.Fatalf("desktop start failed: %v", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResp.Body)
		t.Fatalf("expected 200 from desktop/start, got %d: %s", startResp.StatusCode, string(body))
	}

	var startBody struct {
		Status      string `json:"status"`
		DesktopURL  string `json:"desktopUrl"`
		TerminalURL string `json:"terminalUrl"`
		CDPURL      string `json:"cdpUrl"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if err := json.NewDecoder(startResp.Body).Decode(&startBody); err != nil {
		t.Fatalf("decode desktop/start response: %v", err)
	}
	if startBody.Status != "running" {
		t.Fatalf("expected status=running, got %q", startBody.Status)
	}
	if !strings.HasPrefix(startBody.DesktopURL, "/panel/") || !strings.Contains(startBody.DesktopURL, "vnc.html?autoconnect=true&resize=scale") {
		t.Fatalf("unexpected desktopUrl: %q", startBody.DesktopURL)
	}
	if !strings.HasPrefix(startBody.TerminalURL, "/panel/") || !strings.HasPrefix(startBody.CDPURL, "/panel/") {
		t.Fatalf("unexpected terminalUrl=%q or cdpUrl=%q", startBody.TerminalURL, startBody.CDPURL)
	}

	mu.Lock()
	gotCmd, gotUID, gotGID := lastRunCmd, lastUID, lastGID
	mu.Unlock()
	if gotUID != 1000 || gotGID != 1000 {
		t.Fatalf("expected uid/gid 1000:1000, got %d:%d", gotUID, gotGID)
	}
	if !strings.Contains(gotCmd, "/opt/cubepod/desktop/cubepod-desktop-ctl start") || !strings.Contains(gotCmd, "'1920x1080x24'") || !strings.Contains(gotCmd, "'https://example.com/?a=1&b=2'") {
		t.Fatalf("unexpected quoted command: %q", gotCmd)
	}

	// 5. HTTP GET through minted desktopUrl
	panelHTTPResp, err := http.Get(ts.URL + startBody.DesktopURL)
	if err != nil {
		t.Fatalf("panel HTTP GET failed: %v", err)
	}
	panelHTTPBody, _ := io.ReadAll(panelHTTPResp.Body)
	_ = panelHTTPResp.Body.Close()
	if panelHTTPResp.StatusCode != http.StatusOK || !strings.Contains(string(panelHTTPBody), "novnc-ok path=/vnc.html") {
		t.Fatalf("unexpected panel HTTP response: status=%d body=%q", panelHTTPResp.StatusCode, string(panelHTTPBody))
	}

	// 6. WebSocket upgrade through /panel/{token}/websockify
	wsTokenPath := strings.Split(startBody.DesktopURL, "/vnc.html")[0] + "/websockify"
	tsParsed, _ := url.Parse(ts.URL)
	conn, err := net.Dial("tcp", tsParsed.Host)
	if err != nil {
		t.Fatalf("dial gateway for WebSocket failed: %v", err)
	}
	defer conn.Close()

	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", wsTokenPath, tsParsed.Host)
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 Switching Protocols, got %q (err=%v)", statusLine, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte("ws-ping-payload"))
	echoBuf := make([]byte, len("ws-ping-payload"))
	if _, err := io.ReadFull(br, echoBuf); err != nil {
		t.Fatalf("reading echoed WebSocket bytes: %v", err)
	}
	if string(echoBuf) != "ws-ping-payload" {
		t.Fatalf("expected echoed payload, got %q", string(echoBuf))
	}

	// 7. GET /desktop/status & POST /desktop/stop
	statusResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/desktop/status")
	if err != nil || statusResp.StatusCode != http.StatusOK {
		t.Fatalf("desktop status failed: status=%v err=%v", statusResp, err)
	}
	_ = statusResp.Body.Close()

	stopResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/stop", "application/json", nil)
	if err != nil || stopResp.StatusCode != http.StatusOK {
		t.Fatalf("desktop stop failed: status=%v err=%v", stopResp, err)
	}
	_ = stopResp.Body.Close()

	// 8. Missing desktop stack -> 422 desktop_not_installed + metrics counter
	mu.Lock()
	installed = false
	mu.Unlock()

	unavailResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start", "application/json", nil)
	if err != nil {
		t.Fatalf("desktop start (uninstalled) failed: %v", err)
	}
	defer unavailResp.Body.Close()
	if unavailResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity, got %d", unavailResp.StatusCode)
	}
	var errPayload map[string]string
	_ = json.NewDecoder(unavailResp.Body).Decode(&errPayload)
	if errPayload["error"] != "desktop_not_installed" {
		t.Fatalf("expected error=desktop_not_installed, got %+v", errPayload)
	}

	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	metricsBytes, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	metricsStr := string(metricsBytes)
	if !strings.Contains(metricsStr, `cubepod_desktop_activations_total{result="ok"} 1`) ||
		!strings.Contains(metricsStr, `cubepod_desktop_activations_total{result="unsupported"} 1`) {
		t.Fatalf("missing expected desktop activation metrics in:\n%s", metricsStr)
	}
}

func TestStatelessGatewayMultiReplicaFailover(t *testing.T) {
	// Shared mock Kubernetes APIServer backing two independent Gateway + Reconciler replicas
	sharedMemKube := controller.NewMemoryKubeClient()

	// Mock podd server simulating live sandbox pod
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","daemon":"podd-rs"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmd-ha-1","cmdID":"cmd-ha-1","pid":4242,"running":true,"exit_code":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/commands/cmd-ha-1/logs":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmd-ha-1","content":"hello-from-podd\n","cursor":16,"running":true,"exit_code":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/commands/cmd-ha-1/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmd-ha-1","pid":4242,"running":true,"exit_code":null,"cursor":16}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/lifecycle/prepare-stop":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"prepared"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockPodd.Close()

	poddURLParsed, _ := url.Parse(mockPodd.URL)
	poddHost, poddPortStr, _ := net.SplitHostPort(poddURLParsed.Host)
	poddPort, _ := strconv.Atoi(poddPortStr)

	// Replica 1 (gw1)
	rec1 := controller.NewReconciler(controller.WithKubeClient(sharedMemKube))
	gw1 := gateway.NewServer(rec1)
	gw1.SetLivePodRouting(true)
	ts1 := httptest.NewServer(gw1.Handler())
	defer ts1.Close()

	// Replica 2 (gw2) - completely separate in-memory state, sharing only sharedMemKube
	rec2 := controller.NewReconciler(controller.WithKubeClient(sharedMemKube))
	gw2 := gateway.NewServer(rec2)
	gw2.SetLivePodRouting(true)
	ts2 := httptest.NewServer(gw2.Handler())
	defer ts2.Close()

	// 1. Create sandbox on gw1
	createResp, err := http.Post(ts1.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"cubeplex-ubuntu24-desktop"}`))
	if err != nil {
		t.Fatalf("gw1 create sandbox failed: %v", err)
	}
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from gw1, got %d", createResp.StatusCode)
	}
	var sbx gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&sbx)
	_ = createResp.Body.Close()

	// Update PodBox.Status.PodIP in sharedMemKube to point to mockPodd (simulating live Pod IP)
	boxes, err := sharedMemKube.ListPodBoxes(t.Context(), "")
	if err != nil || len(boxes) == 0 {
		t.Fatalf("ListPodBoxes from sharedMemKube failed: len=%d err=%v", len(boxes), err)
	}
	box := boxes[0]
	box.Status.PodIP = net.JoinHostPort(poddHost, poddPortStr)
	if err := sharedMemKube.UpsertPodBox(t.Context(), box); err != nil {
		t.Fatalf("UpsertPodBox failed: %v", err)
	}
	gw1.SetPoddURL(sbx.SandboxID, mockPodd.URL)

	// 2. Start background command on gw1
	cmdResp, err := http.Post(ts1.URL+"/sandboxes/"+sbx.SandboxID+"/commands/run", "application/json", strings.NewReader(`{"cmd":"sleep 30","background":true}`))
	if err != nil || cmdResp.StatusCode != http.StatusOK {
		t.Fatalf("gw1 commands/run failed: resp=%v err=%v", cmdResp, err)
	}
	_ = cmdResp.Body.Close()

	// 3. Mint /panel/{token}/ on gw1
	mintResp, err := http.Post(
		ts1.URL+"/sandboxes/"+sbx.SandboxID+"/endpoints/"+strconv.Itoa(poddPort),
		"application/json",
		strings.NewReader(`{"expiresInSeconds":3600}`),
	)
	if err != nil || mintResp.StatusCode != http.StatusOK {
		t.Fatalf("gw1 mint endpoint failed: resp=%v err=%v", mintResp, err)
	}
	var mintBody struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(mintResp.Body).Decode(&mintBody)
	_ = mintResp.Body.Close()

	// 4. Failover to gw2 (zero shared process memory!):
	// 4a. GET /sandboxes/{id} on gw2
	getResp, err := http.Get(ts2.URL + "/sandboxes/" + sbx.SandboxID)
	if err != nil {
		t.Fatalf("gw2 GET sandbox failed: %v", err)
	}
	_ = getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from gw2 GET /sandboxes/%s, got %d", sbx.SandboxID, getResp.StatusCode)
	}

	// 4b. GET /commands/cmd-ha-1/logs?cursor=0 on gw2 (proxies to podd, not local 404)
	logsResp, err := http.Get(ts2.URL + "/sandboxes/" + sbx.SandboxID + "/commands/cmd-ha-1/logs?cursor=0")
	if err != nil {
		t.Fatalf("gw2 GET command logs failed: %v", err)
	}
	logsBody, _ := io.ReadAll(logsResp.Body)
	_ = logsResp.Body.Close()
	if logsResp.StatusCode != http.StatusOK || !strings.Contains(string(logsBody), "hello-from-podd") {
		t.Fatalf("expected 200 with podd logs from gw2, got %d: %s", logsResp.StatusCode, string(logsBody))
	}

	// 4c. GET /commands/cmd-ha-1/status on gw2
	cmdStatusResp, err := http.Get(ts2.URL + "/sandboxes/" + sbx.SandboxID + "/commands/cmd-ha-1/status")
	if err != nil {
		t.Fatalf("gw2 GET command status failed: %v", err)
	}
	_ = cmdStatusResp.Body.Close()
	if cmdStatusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for command status on gw2, got %d", cmdStatusResp.StatusCode)
	}

	// 4d. Stateless /panel/{token}/health on gw2 (no s.endpoints entry on gw2)
	panelResp, err := http.Get(ts2.URL + mintBody.URL + "health")
	if err != nil {
		t.Fatalf("gw2 panel proxy failed: %v", err)
	}
	panelBody, _ := io.ReadAll(panelResp.Body)
	_ = panelResp.Body.Close()
	if panelResp.StatusCode != http.StatusOK || !strings.Contains(string(panelBody), "podd-rs") {
		t.Fatalf("expected 200 from gw2 stateless panel proxy, got %d: %s", panelResp.StatusCode, string(panelBody))
	}

	// 4e. Pause on gw2, then auto-resume via /panel/{token}/health on gw2
	pauseResp, err := http.Post(ts2.URL+"/sandboxes/"+sbx.SandboxID+"/pause", "application/json", bytes.NewReader(nil))
	if err != nil || pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("gw2 pause failed: resp=%v err=%v", pauseResp, err)
	}
	var pausedRec gateway.SandboxRecord
	_ = json.NewDecoder(pauseResp.Body).Decode(&pausedRec)
	_ = pauseResp.Body.Close()
	if pausedRec.State != string(v1alpha1.PodBoxPhasePaused) {
		t.Fatalf("expected Paused state after gw2 pause, got %q", pausedRec.State)
	}

	// Simulate pod IP restoration on resume in sharedMemKube when Reconcile(Running) runs:
	// First trigger connect on gw2 (or panel proxy after updating PodIP), verify auto-resume!
	if boxesAfterPause, err := sharedMemKube.ListPodBoxes(t.Context(), ""); err == nil && len(boxesAfterPause) > 0 {
		b := boxesAfterPause[0]
		b.Status.PodIP = net.JoinHostPort(poddHost, poddPortStr)
		_ = sharedMemKube.UpsertPodBox(t.Context(), b)
	}

	connectResp, err := http.Post(ts2.URL+"/sandboxes/"+sbx.SandboxID+"/connect", "application/json", nil)
	if err != nil || connectResp.StatusCode != http.StatusOK {
		t.Fatalf("gw2 connect auto-resume failed: resp=%v err=%v", connectResp, err)
	}
	var resumedRec gateway.SandboxRecord
	_ = json.NewDecoder(connectResp.Body).Decode(&resumedRec)
	_ = connectResp.Body.Close()
	if resumedRec.State != string(v1alpha1.PodBoxPhaseRunning) {
		t.Fatalf("expected Running state after gw2 auto-resume, got %q", resumedRec.State)
	}

	// 5. Stale cache eviction: delete on gw1 -> gw2 must return 404
	delReq, _ := http.NewRequest(http.MethodDelete, ts1.URL+"/sandboxes/"+sbx.SandboxID, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("gw1 delete failed: resp=%v err=%v", delResp, err)
	}
	_ = delResp.Body.Close()

	staleResp, err := http.Get(ts2.URL + "/sandboxes/" + sbx.SandboxID)
	if err != nil {
		t.Fatalf("gw2 GET after delete failed: %v", err)
	}
	_ = staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on gw2 after gw1 deleted sandbox, got %d", staleResp.StatusCode)
	}
}
