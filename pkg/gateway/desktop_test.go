package gateway_test

import (
	"bufio"
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
	"sync/atomic"
	"testing"
	"time"

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

func TestDesktopTransportWebRTCAndFallback(t *testing.T) {
	nekoUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "neko-webrtc-ok path=%s", r.URL.Path)
	}))
	defer nekoUpstream.Close()

	nekoURL, _ := url.Parse(nekoUpstream.URL)
	_, nekoPortStr, _ := net.SplitHostPort(nekoURL.Host)
	nekoPort, _ := strconv.Atoi(nekoPortStr)

	var mu sync.Mutex
	nekoInstalled := true
	var lastCmd string
	lastStatusJSON := `{"status":"stopped","display":":99","resolution":"1280x800x24","transportUsed":"novnc","ports":{"desktop":8080,"terminal":7681,"cdp":9222},"pids":{}}`

	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			var req struct {
				Command string `json:"command"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			lastCmd = req.Command
			hasNeko := nekoInstalled

			w.Header().Set("Content-Type", "application/json")
			var statusJSON string
			if strings.HasSuffix(req.Command, " status") {
				statusJSON = lastStatusJSON
			} else {
				if strings.Contains(req.Command, "'webrtc'") && hasNeko {
					statusJSON = fmt.Sprintf(
						`{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"webrtc","ports":{"desktop":8080,"terminal":7681,"cdp":9222,"webrtc":%d},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15,"neko":16}}`,
						nekoPort,
					)
				} else if strings.Contains(req.Command, "'webrtc'") && !hasNeko {
					statusJSON = `{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"novnc","fallbackReason":"neko_not_installed","ports":{"desktop":8080,"terminal":7681,"cdp":9222},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15}}`
				} else {
					statusJSON = `{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"novnc","ports":{"desktop":8080,"terminal":7681,"cdp":9222},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15}}`
				}
				lastStatusJSON = statusJSON
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "cmd-webrtc",
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

	createResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"cubeplex-ubuntu24-desktop"}`))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	var rec gateway.SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)
	_ = createResp.Body.Close()
	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	// Invalid transport must return 400
	badResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start", "application/json", strings.NewReader(`{"transport":"rdp"}`))
	if err != nil {
		t.Fatalf("bad transport request failed: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid transport, got %d", badResp.StatusCode)
	}

	// WebRTC with Neko installed
	webrtcResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start", "application/json", strings.NewReader(`{"transport":"webrtc"}`))
	if err != nil {
		t.Fatalf("desktop/start webrtc failed: %v", err)
	}
	var webrtcBody struct {
		Status         string `json:"status"`
		DesktopURL     string `json:"desktopUrl"`
		WebRTCURL      string `json:"webrtcUrl"`
		TransportUsed  string `json:"transportUsed"`
		FallbackReason string `json:"fallbackReason"`
	}
	if err := json.NewDecoder(webrtcResp.Body).Decode(&webrtcBody); err != nil {
		_ = webrtcResp.Body.Close()
		t.Fatalf("decode webrtc response: %v", err)
	}
	_ = webrtcResp.Body.Close()
	if webrtcBody.TransportUsed != "webrtc" || webrtcBody.FallbackReason != "" {
		t.Fatalf("expected transportUsed=webrtc with empty fallbackReason, got %+v", webrtcBody)
	}
	if !strings.HasPrefix(webrtcBody.WebRTCURL, "/panel/") {
		t.Fatalf("expected /panel/ webrtcUrl, got %q", webrtcBody.WebRTCURL)
	}
	mu.Lock()
	gotCmd := lastCmd
	mu.Unlock()
	if !strings.Contains(gotCmd, "--transport 'webrtc'") {
		t.Fatalf("expected --transport 'webrtc' in ctl command, got %q", gotCmd)
	}

	panelResp, err := http.Get(ts.URL + webrtcBody.WebRTCURL)
	if err != nil {
		t.Fatalf("GET webrtcUrl failed: %v", err)
	}
	panelBytes, _ := io.ReadAll(panelResp.Body)
	_ = panelResp.Body.Close()
	if panelResp.StatusCode != http.StatusOK || !strings.Contains(string(panelBytes), "neko-webrtc-ok") {
		t.Fatalf("unexpected webrtc panel proxy response: status=%d body=%q", panelResp.StatusCode, string(panelBytes))
	}

	// WebRTC requested when Neko is absent -> graceful fallback to noVNC
	mu.Lock()
	nekoInstalled = false
	mu.Unlock()
	fbResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start", "application/json", strings.NewReader(`{"transport":"webrtc"}`))
	if err != nil {
		t.Fatalf("desktop/start webrtc fallback failed: %v", err)
	}
	var fbBody struct {
		Status         string `json:"status"`
		DesktopURL     string `json:"desktopUrl"`
		WebRTCURL      string `json:"webrtcUrl"`
		TransportUsed  string `json:"transportUsed"`
		FallbackReason string `json:"fallbackReason"`
	}
	if err := json.NewDecoder(fbResp.Body).Decode(&fbBody); err != nil {
		_ = fbResp.Body.Close()
		t.Fatalf("decode fallback response: %v", err)
	}
	_ = fbResp.Body.Close()
	if fbBody.TransportUsed != "novnc" || fbBody.FallbackReason != "neko_not_installed" || fbBody.WebRTCURL != "" {
		t.Fatalf("expected graceful fallback to novnc, got %+v", fbBody)
	}
	if !strings.HasPrefix(fbBody.DesktopURL, "/panel/") {
		t.Fatalf("expected desktopUrl in fallback response, got %q", fbBody.DesktopURL)
	}

	// Case-insensitive "WebRTC" when Neko is installed + GET /desktop/status check
	mu.Lock()
	nekoInstalled = true
	mu.Unlock()
	mixedResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start", "application/json", strings.NewReader(`{"transport":"WebRTC"}`))
	if err != nil {
		t.Fatalf("desktop/start WebRTC failed: %v", err)
	}
	var mixedBody struct {
		TransportUsed string `json:"transportUsed"`
		WebRTCURL     string `json:"webrtcUrl"`
	}
	if err := json.NewDecoder(mixedResp.Body).Decode(&mixedBody); err != nil {
		_ = mixedResp.Body.Close()
		t.Fatalf("decode WebRTC response: %v", err)
	}
	_ = mixedResp.Body.Close()
	if mixedBody.TransportUsed != "webrtc" || !strings.HasPrefix(mixedBody.WebRTCURL, "/panel/") {
		t.Fatalf("expected case-insensitive WebRTC start to succeed, got %+v", mixedBody)
	}

	statusResp, err := http.Get(ts.URL + "/sandboxes/" + rec.SandboxID + "/desktop/status")
	if err != nil {
		t.Fatalf("GET /desktop/status failed: %v", err)
	}
	var statusBody struct {
		Status        string `json:"status"`
		TransportUsed string `json:"transportUsed"`
		Ports         struct {
			WebRTC int `json:"webrtc"`
		} `json:"ports"`
	}
	if err := json.NewDecoder(statusResp.Body).Decode(&statusBody); err != nil {
		_ = statusResp.Body.Close()
		t.Fatalf("decode desktop status response: %v", err)
	}
	_ = statusResp.Body.Close()
	if statusBody.Status != "running" || statusBody.TransportUsed != "webrtc" || statusBody.Ports.WebRTC <= 0 {
		t.Fatalf("expected GET /desktop/status to report running webrtc with port, got %+v", statusBody)
	}
}

func TestDesktopConcurrentSessions50Plus(t *testing.T) {
	const totalSessions = 55

	var activeUpstreamWS atomic.Int64
	var peakUpstreamWS atomic.Int64

	desktopUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			cur := activeUpstreamWS.Add(1)
			defer activeUpstreamWS.Add(-1)
			for {
				prev := peakUpstreamWS.Load()
				if cur <= prev || peakUpstreamWS.CompareAndSwap(prev, cur) {
					break
				}
			}

			accept := computeWebSocketAccept(r.Header.Get("Sec-WebSocket-Key"))
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
			_ = rw.Flush()

			for {
				line, err := rw.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(line) == "bye" {
					return
				}
				_, _ = rw.WriteString("echo:" + line)
				_ = rw.Flush()
			}
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "panel-ok path=%s", r.URL.Path)
	}))
	defer desktopUpstream.Close()

	upURL, _ := url.Parse(desktopUpstream.URL)
	_, upPortStr, _ := net.SplitHostPort(upURL.Host)
	upPort, _ := strconv.Atoi(upPortStr)

	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			var req struct {
				Command string `json:"command"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			var statusJSON string
			if strings.Contains(req.Command, "'webrtc'") {
				statusJSON = fmt.Sprintf(
					`{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"webrtc","ports":{"desktop":%d,"terminal":%d,"cdp":%d,"webrtc":%d},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15,"neko":16}}`,
					upPort, upPort, upPort, upPort,
				)
			} else {
				statusJSON = fmt.Sprintf(
					`{"status":"running","display":":99","resolution":"1280x800x24","transportUsed":"novnc","ports":{"desktop":%d,"terminal":%d,"cdp":%d},"pids":{"xvfb":10,"wm":11,"chromium":12,"vnc":13,"websockify":14,"ttyd":15}}`,
					upPort, upPort, upPort,
				)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "cmd-conc",
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

	tsParsed, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse gateway URL: %v", err)
	}

	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 128,
		},
	}
	defer httpClient.CloseIdleConnections()

	startGate := make(chan struct{})
	var readyWG sync.WaitGroup
	var holdWG sync.WaitGroup
	holdGate := make(chan struct{})
	var doneWG sync.WaitGroup
	errCh := make(chan error, totalSessions)

	readyWG.Add(totalSessions)
	holdWG.Add(totalSessions)
	doneWG.Add(totalSessions)

	for idx := 0; idx < totalSessions; idx++ {
		idx := idx
		go func() {
			defer doneWG.Done()
			holdDone := false
			releaseHold := func() {
				if !holdDone {
					holdDone = true
					holdWG.Done()
				}
			}
			defer releaseHold()

			createResp, err := httpClient.Post(
				ts.URL+"/sandboxes",
				"application/json",
				strings.NewReader(`{"templateID":"cubeplex-ubuntu24-desktop"}`),
			)
			if err != nil {
				readyWG.Done()
				errCh <- fmt.Errorf("session %d create sandbox: %w", idx, err)
				return
			}
			var rec gateway.SandboxRecord
			decodeErr := json.NewDecoder(createResp.Body).Decode(&rec)
			_ = createResp.Body.Close()
			if decodeErr != nil {
				readyWG.Done()
				errCh <- fmt.Errorf("session %d decode sandbox: %w", idx, decodeErr)
				return
			}
			srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

			readyWG.Done()
			<-startGate

			transport := "novnc"
			if idx%2 == 0 {
				transport = "webrtc"
			}
			startReqBody := fmt.Sprintf(`{"resolution":"1280x800x24","transport":%q,"expiresInSeconds":600}`, transport)
			startResp, err := httpClient.Post(
				ts.URL+"/sandboxes/"+rec.SandboxID+"/desktop/start",
				"application/json",
				strings.NewReader(startReqBody),
			)
			if err != nil {
				errCh <- fmt.Errorf("session %d desktop/start: %w", idx, err)
				return
			}
			var startData struct {
				Status        string `json:"status"`
				DesktopURL    string `json:"desktopUrl"`
				WebRTCURL     string `json:"webrtcUrl"`
				TransportUsed string `json:"transportUsed"`
			}
			err = json.NewDecoder(startResp.Body).Decode(&startData)
			_ = startResp.Body.Close()
			if err != nil {
				errCh <- fmt.Errorf("session %d decode desktop/start: %w", idx, err)
				return
			}
			if startResp.StatusCode != http.StatusOK || startData.TransportUsed != transport {
				errCh <- fmt.Errorf("session %d unexpected start response (%d): %+v", idx, startResp.StatusCode, startData)
				return
			}

			// 1. Concurrent HTTP panel request
			httpTarget := startData.DesktopURL
			wsPath := strings.Split(startData.DesktopURL, "/vnc.html")[0] + "/websockify"
			if transport == "webrtc" {
				httpTarget = startData.WebRTCURL + "index.html"
				wsPath = startData.WebRTCURL + "ws"
			}

			panelResp, err := httpClient.Get(ts.URL + httpTarget)
			if err != nil {
				errCh <- fmt.Errorf("session %d HTTP panel GET: %w", idx, err)
				return
			}
			panelBytes, _ := io.ReadAll(panelResp.Body)
			_ = panelResp.Body.Close()
			if panelResp.StatusCode != http.StatusOK || !strings.Contains(string(panelBytes), "panel-ok") {
				errCh <- fmt.Errorf("session %d unexpected HTTP panel response (%d): %q", idx, panelResp.StatusCode, string(panelBytes))
				return
			}

			// 2. Concurrent WebSocket panel tunnel held open simultaneously across all 55 sessions
			conn, err := net.DialTimeout("tcp", tsParsed.Host, 10*time.Second)
			if err != nil {
				errCh <- fmt.Errorf("session %d dial WS: %w", idx, err)
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

			wsKey := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cubepod-ws-key-%02d", idx)))
			_, err = fmt.Fprintf(
				conn,
				"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
				wsPath,
				tsParsed.Host,
				wsKey,
			)
			if err != nil {
				errCh <- fmt.Errorf("session %d write WS upgrade: %w", idx, err)
				return
			}

			br := bufio.NewReader(conn)
			statusLine, err := br.ReadString('\n')
			if err != nil || !strings.Contains(statusLine, "101") {
				errCh <- fmt.Errorf("session %d expected 101 upgrade, got %q (err=%v)", idx, statusLine, err)
				return
			}
			for {
				hdr, err := br.ReadString('\n')
				if err != nil {
					errCh <- fmt.Errorf("session %d read upgrade header: %w", idx, err)
					return
				}
				if hdr == "\r\n" {
					break
				}
			}

			// Signal that this session's WebSocket tunnel is live, then wait until all 55 are open simultaneously.
			releaseHold()
			<-holdGate

			for round := 1; round <= 2; round++ {
				msg := fmt.Sprintf("session-%02d-sbx-%s-round-%d\n", idx, rec.SandboxID, round)
				if _, err := conn.Write([]byte(msg)); err != nil {
					errCh <- fmt.Errorf("session %d write WS payload: %w", idx, err)
					return
				}
				reply, err := br.ReadString('\n')
				if err != nil {
					errCh <- fmt.Errorf("session %d read WS reply: %w", idx, err)
					return
				}
				if reply != "echo:"+msg {
					errCh <- fmt.Errorf("session %d stream mismatch: want %q, got %q", idx, "echo:"+msg, reply)
					return
				}
			}

			_, _ = conn.Write([]byte("bye\n"))
		}()
	}

	readyWG.Wait()
	close(startGate)

	holdWG.Wait()
	activeAtBarrier := srv.Metrics().PanelActiveSessions.Load()
	close(holdGate)

	doneWG.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent desktop session error: %v", err)
		}
	}

	if activeAtBarrier < totalSessions {
		t.Fatalf("expected >= %d simultaneous active panel sessions at barrier, got %d (peak upstream=%d)",
			totalSessions, activeAtBarrier, peakUpstreamWS.Load())
	}
	if gotOK := srv.Metrics().DesktopActivationsOK.Load(); gotOK != totalSessions {
		t.Fatalf("expected %d successful desktop activations, got %d", totalSessions, gotOK)
	}

	// Verify all 55 reverse-proxy handlers unwind cleanly with zero leaked PanelActiveSessions.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Metrics().PanelActiveSessions.Load() == 0 && activeUpstreamWS.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := srv.Metrics().PanelActiveSessions.Load(); leaked != 0 {
		t.Fatalf("panel session leak detected: expected 0 active sessions after close, got %d", leaked)
	}
}
