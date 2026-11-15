package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

// parseConnectFrames parses a stream of Connect-RPC frames.
type parsedConnectFrame struct {
	Flag    byte
	Length  uint32
	Payload []byte
}

func parseConnectFrames(data []byte) ([]parsedConnectFrame, error) {
	var frames []parsedConnectFrame
	r := bytes.NewReader(data)
	for {
		header := make([]byte, 5)
		n, err := io.ReadFull(r, header)
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			return frames, fmt.Errorf("read header: %w", err)
		}
		flag := header[0]
		length := binary.BigEndian.Uint32(header[1:5])
		payload := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(r, payload); err != nil {
				return frames, fmt.Errorf("read payload: %w", err)
			}
		}
		frames = append(frames, parsedConnectFrame{
			Flag:    flag,
			Length:  length,
			Payload: payload,
		})
	}
	return frames, nil
}

func setupTestServerWithSandbox(t *testing.T) (*Server, *httptest.Server, *SandboxRecord) {
	t.Helper()
	reconciler := controller.NewReconciler()
	srv := NewServer(reconciler)
	tempDir := t.TempDir()
	srv.SetWorkspaceBaseDir(tempDir)

	now := time.Now()
	rec := &SandboxRecord{
		SandboxID:        "sbx-connect-test-01",
		TemplateID:       "base",
		State:            "Running",
		StartedAt:        now,
		EndAt:            now.Add(time.Hour),
		WorkspaceDir:     filepath.Join(tempDir, "sbx-connect-test-01", "workspace"),
		RootfsGeneration: 1,
	}
	_ = os.MkdirAll(rec.WorkspaceDir, 0755)

	srv.mu.Lock()
	srv.sandboxes[rec.SandboxID] = rec
	srv.mu.Unlock()

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts, rec
}

func TestConnectRPC_ProcessStart_Success(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	reqPayload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", "echo 'hello connectrpc stdout'"},
			"cwd":  "/workspace",
		},
	}
	rawJSON, _ := json.Marshal(reqPayload)
	framedBody := makeConnectFrame(rawJSON, false)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(framedBody))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d; body=%s", resp.StatusCode, string(b))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	frames, err := parseConnectFrames(body)
	if err != nil {
		t.Fatalf("failed to parse connect frames: %v", err)
	}
	if len(frames) < 3 {
		t.Fatalf("expected at least 3 frames (start, data, end), got %d frames", len(frames))
	}

	// Verify Start frame (flag 0)
	var startObj struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	_ = json.Unmarshal(frames[0].Payload, &startObj)
	if startObj.Event.Start.Pid <= 0 {
		t.Errorf("expected positive pid in start event, got %d", startObj.Event.Start.Pid)
	}

	// Verify Data frame contains base64 stdout
	var stdoutFound bool
	for _, f := range frames {
		if f.Flag == 0 {
			var dataObj struct {
				Event struct {
					Data struct {
						Stdout string `json:"stdout"`
					} `json:"data"`
				} `json:"event"`
			}
			_ = json.Unmarshal(f.Payload, &dataObj)
			if dataObj.Event.Data.Stdout != "" {
				decoded, _ := base64.StdEncoding.DecodeString(dataObj.Event.Data.Stdout)
				if strings.Contains(string(decoded), "hello connectrpc stdout") {
					stdoutFound = true
					break
				}
			}
		}
	}
	if !stdoutFound {
		t.Errorf("stdout 'hello connectrpc stdout' not found in stream frames")
	}

	// Verify End frame has exitCode 0
	lastMsgFrame := frames[len(frames)-2]
	var endObj struct {
		Event struct {
			End struct {
				ExitCode int  `json:"exitCode"`
				Exited   bool `json:"exited"`
			} `json:"end"`
		} `json:"event"`
	}
	_ = json.Unmarshal(lastMsgFrame.Payload, &endObj)
	if endObj.Event.End.ExitCode != 0 || !endObj.Event.End.Exited {
		t.Errorf("expected exitCode 0 and exited true, got %+v", endObj.Event.End)
	}

	// Verify trailer frame (flag 2, length 2: "{}")
	trailerFrame := frames[len(frames)-1]
	if trailerFrame.Flag != 2 {
		t.Errorf("expected trailer frame flag 2, got %d", trailerFrame.Flag)
	}
}

func TestConnectRPC_ProcessStart_StderrAndExitCode(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	reqPayload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "sh",
			"args": []string{"-c", "echo 'critical error' >&2; exit 42"},
		},
	}
	rawJSON, _ := json.Marshal(reqPayload)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(rawJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sandbox-ID", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d; body=%s", resp.StatusCode, string(b))
	}

	body, _ := io.ReadAll(resp.Body)
	frames, err := parseConnectFrames(body)
	if err != nil {
		t.Fatalf("failed to parse connect frames: %v", err)
	}

	var stderrFound bool
	for _, f := range frames {
		if f.Flag == 0 {
			var dataObj struct {
				Event struct {
					Data struct {
						Stderr string `json:"stderr"`
					} `json:"data"`
				} `json:"event"`
			}
			_ = json.Unmarshal(f.Payload, &dataObj)
			if dataObj.Event.Data.Stderr != "" {
				decoded, _ := base64.StdEncoding.DecodeString(dataObj.Event.Data.Stderr)
				if strings.Contains(string(decoded), "critical error") {
					stderrFound = true
					break
				}
			}
		}
	}
	if !stderrFound {
		t.Errorf("stderr 'critical error' not found in stream frames")
	}

	endFrame := frames[len(frames)-2]
	var endObj struct {
		Event struct {
			End struct {
				ExitCode int `json:"exitCode"`
			} `json:"end"`
		} `json:"event"`
	}
	_ = json.Unmarshal(endFrame.Payload, &endObj)
	if endObj.Event.End.ExitCode != 42 {
		t.Errorf("expected exitCode 42, got %d", endObj.Event.End.ExitCode)
	}
}

func TestConnectRPC_ProcessSendSignalAndConnect(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	// Spawn a background command in server
	cmdID := "cmd-9999"
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmdEntry := &commandEntry{
		id:       cmdID,
		cmdID:    cmdID,
		pid:      9999,
		running:  true,
		buf:      bytes.NewBufferString("background output"),
		cancel:   cancel,
		exitCode: 0,
	}
	srv.mu.Lock()
	srv.commands["9999"] = cmdEntry
	srv.mu.Unlock()

	// 1. SendSignal
	sigPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{
			"pid": 9999,
		},
		"signal": 15,
	})
	sigReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendSignal", bytes.NewReader(sigPayload))
	sigReq.Header.Set("Content-Type", "application/json")
	sigReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	sigResp, err := http.DefaultClient.Do(sigReq)
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}
	sigResp.Body.Close()
	if sigResp.StatusCode != http.StatusOK {
		t.Errorf("expected SendSignal 200 OK, got %d", sigResp.StatusCode)
	}

	// 2. Connect
	connPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{
			"pid": 9999,
		},
	})
	connReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Connect", bytes.NewReader(connPayload))
	connReq.Header.Set("Content-Type", "application/connect+json")
	connReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	connResp, err := http.DefaultClient.Do(connReq)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer connResp.Body.Close()
	if connResp.StatusCode != http.StatusOK {
		t.Fatalf("expected Connect 200 OK, got %d", connResp.StatusCode)
	}

	body, _ := io.ReadAll(connResp.Body)
	frames, err := parseConnectFrames(body)
	if err != nil {
		t.Fatalf("failed to parse connect frames: %v", err)
	}
	if len(frames) < 3 {
		t.Fatalf("expected at least 3 frames, got %d", len(frames))
	}
}

func TestConnectRPC_ProcessList(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	srv.mu.Lock()
	srv.commands["1001"] = &commandEntry{
		id:      "cmd-1001",
		cmdID:   "cmd-1001",
		pid:     1001,
		running: true,
		buf:     &bytes.Buffer{},
	}
	srv.mu.Unlock()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/List", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Process/List failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d; body=%s", resp.StatusCode, string(b))
	}

	var listResp struct {
		Processes []struct {
			Pid int `json:"pid"`
		} `json:"processes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listResp)
	if len(listResp.Processes) != 1 || listResp.Processes[0].Pid != 1001 {
		t.Errorf("expected 1 process with pid 1001, got %+v", listResp.Processes)
	}
}

func TestConnectRPC_Filesystem_CRUD(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	// 1. Write file
	writePayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace/connect_note.txt",
		"data": "Hello ConnectRPC Filesystem!",
	})
	wReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/Write", bytes.NewReader(writePayload))
	wReq.Header.Set("Content-Type", "application/json")
	wReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	wResp, err := http.DefaultClient.Do(wReq)
	if err != nil {
		t.Fatalf("Filesystem/Write failed: %v", err)
	}
	wResp.Body.Close()
	if wResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Write, got %d", wResp.StatusCode)
	}

	// 2. Read file
	readPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace/connect_note.txt",
	})
	rReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/Read", bytes.NewReader(readPayload))
	rReq.Header.Set("Content-Type", "application/json")
	rReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	rResp, err := http.DefaultClient.Do(rReq)
	if err != nil {
		t.Fatalf("Filesystem/Read failed: %v", err)
	}
	defer rResp.Body.Close()
	if rResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Read, got %d", rResp.StatusCode)
	}

	var readRes struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(rResp.Body).Decode(&readRes)
	if readRes.Content != "Hello ConnectRPC Filesystem!" {
		t.Errorf("expected 'Hello ConnectRPC Filesystem!', got %q", readRes.Content)
	}

	// 3. Stat file
	statPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace/connect_note.txt",
	})
	statReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/Stat", bytes.NewReader(statPayload))
	statReq.Header.Set("Content-Type", "application/json")
	statReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	statResp, err := http.DefaultClient.Do(statReq)
	if err != nil {
		t.Fatalf("Filesystem/Stat failed: %v", err)
	}
	defer statResp.Body.Close()
	if statResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Stat, got %d", statResp.StatusCode)
	}

	var statRes struct {
		Entry struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"entry"`
	}
	_ = json.NewDecoder(statResp.Body).Decode(&statRes)
	if statRes.Entry.Name != "connect_note.txt" || statRes.Entry.Type != "FILE_TYPE_FILE" {
		t.Errorf("unexpected stat entry: %+v", statRes.Entry)
	}

	// 4. MakeDir
	mkPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace/sub_docs",
	})
	mkReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/MakeDir", bytes.NewReader(mkPayload))
	mkReq.Header.Set("Content-Type", "application/json")
	mkReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	mkResp, err := http.DefaultClient.Do(mkReq)
	if err != nil {
		t.Fatalf("Filesystem/MakeDir failed: %v", err)
	}
	mkResp.Body.Close()
	if mkResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for MakeDir, got %d", mkResp.StatusCode)
	}

	// 5. List directory
	listPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace",
	})
	lReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/List", bytes.NewReader(listPayload))
	lReq.Header.Set("Content-Type", "application/json")
	lReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	lResp, err := http.DefaultClient.Do(lReq)
	if err != nil {
		t.Fatalf("Filesystem/List failed: %v", err)
	}
	defer lResp.Body.Close()
	if lResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for List, got %d", lResp.StatusCode)
	}

	var listRes struct {
		Entries []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"isDir"`
		} `json:"entries"`
	}
	_ = json.NewDecoder(lResp.Body).Decode(&listRes)
	if len(listRes.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(listRes.Entries))
	}

	// 6. Remove file
	rmPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace/connect_note.txt",
	})
	rmReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/Remove", bytes.NewReader(rmPayload))
	rmReq.Header.Set("Content-Type", "application/json")
	rmReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	rmResp, err := http.DefaultClient.Do(rmReq)
	if err != nil {
		t.Fatalf("Filesystem/Remove failed: %v", err)
	}
	rmResp.Body.Close()
	if rmResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for Remove, got %d", rmResp.StatusCode)
	}

	// 7. Verify Stat returns 404
	statReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/Stat", bytes.NewReader(statPayload))
	statReq2.Header.Set("Content-Type", "application/json")
	statReq2.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	statResp2, err := http.DefaultClient.Do(statReq2)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	defer statResp2.Body.Close()
	if statResp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for removed file, got %d", statResp2.StatusCode)
	}
}

func TestConnectRPC_LivePoddForwarding(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	// Spin up mock upstream podd daemon
	poddHit := false
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poddHit = true
		if r.URL.Path == "/process.Process/Start" {
			w.Header().Set("Content-Type", "application/connect+json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(makeConnectFrame([]byte(`{"event":{"start":{"pid":7777}}}`), false))
			_, _ = w.Write(makeConnectFrame([]byte(`{"event":{"end":{"exitCode":0,"exited":true}}}`), false))
			_, _ = w.Write(makeConnectFrame([]byte("{}"), true))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(mockPodd.Close)

	// Set PoddURL on record
	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	reqPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"cmd": "echo test"},
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from forwarded podd, got %d", resp.StatusCode)
	}
	if !poddHit {
		t.Errorf("expected request to be forwarded to upstream mock podd")
	}

	body, _ := io.ReadAll(resp.Body)
	frames, err := parseConnectFrames(body)
	if err != nil || len(frames) != 3 {
		t.Errorf("unexpected forwarded frames: len=%d err=%v", len(frames), err)
	}
}

func TestConnectRPC_SubdomainRouting(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	reqPayload, _ := json.Marshal(map[string]interface{}{
		"path": "/workspace",
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/List", bytes.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	// Host header with E2B subdomain format: 49983-<sandboxID>.example.com
	req.Host = fmt.Sprintf("49983-%s.example.com", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subdomain request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK via subdomain routing, got %d", resp.StatusCode)
	}
}

func TestConnectRPC_DirectRESTFiles_Compatibility(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	// 1. POST /files write
	postReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/files?path=/workspace/direct_file.txt", strings.NewReader("direct file content"))
	postReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST /files failed: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for POST /files, got %d", postResp.StatusCode)
	}

	var writeRes []map[string]interface{}
	_ = json.NewDecoder(postResp.Body).Decode(&writeRes)
	if len(writeRes) != 1 || writeRes[0]["name"] != "direct_file.txt" {
		t.Errorf("unexpected write response: %+v", writeRes)
	}

	// 2. GET /files read
	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/files?path=/workspace/direct_file.txt", nil)
	getReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET /files failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for GET /files, got %d", getResp.StatusCode)
	}
	content, _ := io.ReadAll(getResp.Body)
	if string(content) != "direct file content" {
		t.Errorf("expected 'direct file content', got %q", string(content))
	}

	// 3. GET /files/list
	listReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/files/list?path=/workspace", nil)
	listReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET /files/list failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for GET /files/list, got %d", listResp.StatusCode)
	}
}

func TestFiles_OfficialSDK_POSTFiles_MultipartAndOctetStream(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	// 1. Official-style multipart upload (single file with path query parameter)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "/workspace/compat-smoke.txt")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = part.Write([]byte("hello-from-compat-smoke"))
	writer.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/files?path=/workspace/compat-smoke.txt", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /files multipart failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for multipart POST /files, got %d", resp.StatusCode)
	}

	var writeRes []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&writeRes); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(writeRes) != 1 || writeRes[0]["name"] != "compat-smoke.txt" || writeRes[0]["path"] != "/workspace/compat-smoke.txt" || writeRes[0]["type"] != "file" {
		t.Fatalf("unexpected write response: %+v", writeRes)
	}

	// Read back to verify
	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/files?path=/workspace/compat-smoke.txt", nil)
	getReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /files failed: status=%d err=%v", getResp.StatusCode, err)
	}
	content, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if string(content) != "hello-from-compat-smoke" {
		t.Fatalf("expected 'hello-from-compat-smoke', got %q", string(content))
	}

	// 2. Official-style multipart upload with multiple files
	bodyMulti := &bytes.Buffer{}
	writerMulti := multipart.NewWriter(bodyMulti)
	p1, _ := writerMulti.CreateFormFile("file", "/workspace/multi1.txt")
	_, _ = p1.Write([]byte("file1-content"))
	p2, _ := writerMulti.CreateFormFile("file", "/workspace/multi2.txt")
	_, _ = p2.Write([]byte("file2-content"))
	writerMulti.Close()

	reqMulti, _ := http.NewRequest(http.MethodPost, ts.URL+"/files", bodyMulti)
	reqMulti.Header.Set("Content-Type", writerMulti.FormDataContentType())
	reqMulti.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	respMulti, err := http.DefaultClient.Do(reqMulti)
	if err != nil {
		t.Fatalf("POST /files multi failed: %v", err)
	}
	defer respMulti.Body.Close()
	if respMulti.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for multi POST /files, got %d", respMulti.StatusCode)
	}
	var multiRes []map[string]interface{}
	if err := json.NewDecoder(respMulti.Body).Decode(&multiRes); err != nil {
		t.Fatalf("failed to decode multi response: %v", err)
	}
	if len(multiRes) != 2 {
		t.Fatalf("expected 2 written files, got %+v", multiRes)
	}

	// 3. Octet-stream upload with gzip compression
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte("streamed-gzipped-content"))
	gw.Close()

	reqGz, _ := http.NewRequest(http.MethodPost, ts.URL+"/files?path=/workspace/gzipped.txt", &gzBuf)
	reqGz.Header.Set("Content-Type", "application/octet-stream")
	reqGz.Header.Set("Content-Encoding", "gzip")
	reqGz.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	respGz, err := http.DefaultClient.Do(reqGz)
	if err != nil {
		t.Fatalf("POST /files gzip failed: %v", err)
	}
	defer respGz.Body.Close()
	if respGz.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for gzip POST /files, got %d", respGz.StatusCode)
	}
	var gzRes []map[string]interface{}
	if err := json.NewDecoder(respGz.Body).Decode(&gzRes); err != nil {
		t.Fatalf("failed to decode gzip response: %v", err)
	}
	if len(gzRes) != 1 || gzRes[0]["name"] != "gzipped.txt" || gzRes[0]["type"] != "file" {
		t.Fatalf("unexpected gzip write response: %+v", gzRes)
	}

	// Read back to verify decompressed file
	getGzReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/files?path=/workspace/gzipped.txt", nil)
	getGzReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	getGzResp, err := http.DefaultClient.Do(getGzReq)
	if err != nil || getGzResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /files gzipped failed: status=%d err=%v", getGzResp.StatusCode, err)
	}
	gzContent, _ := io.ReadAll(getGzResp.Body)
	getGzResp.Body.Close()
	if string(gzContent) != "streamed-gzipped-content" {
		t.Fatalf("expected 'streamed-gzipped-content', got %q", string(gzContent))
	}
}

func TestFiles_MockPoddForwarding(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	var (
		poddPostHit  bool
		receivedPath string
	)
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/files" {
			poddPostHit = true
			receivedPath = r.URL.Query().Get("path")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"name": filepath.Base(receivedPath),
					"path": receivedPath,
					"type": "file",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockPodd.Close()

	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "/workspace/forward-smoke.txt")
	_, _ = part.Write([]byte("forwarded content"))
	writer.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/files?path=/workspace/forward-smoke.txt", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /files forward failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from forwarded podd, got %d", resp.StatusCode)
	}
	if !poddPostHit {
		t.Fatalf("expected mock podd to receive POST /v1/files")
	}
	if receivedPath != "/workspace/forward-smoke.txt" {
		t.Fatalf("expected path /workspace/forward-smoke.txt, got %s", receivedPath)
	}
	var fwdRes []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&fwdRes); err != nil {
		t.Fatalf("failed to decode forwarded response: %v", err)
	}
	if len(fwdRes) != 1 || fwdRes[0]["name"] != "forward-smoke.txt" {
		t.Fatalf("unexpected forwarded response: %+v", fwdRes)
	}
}

func readNextConnectFrame(r io.Reader) (*parsedConnectFrame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	flag := header[0]
	length := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return &parsedConnectFrame{
		Flag:    flag,
		Length:  length,
		Payload: payload,
	}, nil
}

func waitForPtyOutput(r io.Reader, pred func(string) bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	accumulated := ""
	for time.Now().Before(deadline) {
		frame, err := readNextConnectFrame(r)
		if err != nil {
			return accumulated, err
		}
		if frame.Flag == 0 {
			var event struct {
				Event struct {
					Data struct {
						Pty string `json:"pty"`
					} `json:"data"`
				} `json:"event"`
			}
			if err := json.Unmarshal(frame.Payload, &event); err == nil {
				if event.Event.Data.Pty != "" {
					decoded, _ := base64.StdEncoding.DecodeString(event.Event.Data.Pty)
					accumulated += string(decoded)
					if pred(accumulated) {
						return accumulated, nil
					}
				}
			}
		} else if frame.Flag == 2 {
			break
		}
	}
	return accumulated, fmt.Errorf("timeout waiting for output, accumulated: %q", accumulated)
}

func TestConnectRPC_PTY_Start_RealOSPTY_And_SendInput_And_Resize(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)
	_ = srv

	reqPayload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
		},
		"pty": map[string]interface{}{
			"size": map[string]interface{}{
				"cols": 80,
				"rows": 24,
			},
		},
	}
	rawJSON, _ := json.Marshal(reqPayload)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(rawJSON))
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PTY Start failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d; body=%s", resp.StatusCode, string(b))
	}

	// 1. Frame 0 must be Start event with pid
	frame0, err := readNextConnectFrame(resp.Body)
	if err != nil {
		t.Fatalf("failed to read start frame: %v", err)
	}
	var startEvent struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	if err := json.Unmarshal(frame0.Payload, &startEvent); err != nil {
		t.Fatalf("failed to unmarshal start event: %v", err)
	}
	pid := startEvent.Event.Start.Pid
	if pid <= 0 {
		t.Fatalf("expected valid pid > 0, got %d", pid)
	}

	// 2. Test isatty
	sendInputPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input": map[string]interface{}{
			"pty": base64.StdEncoding.EncodeToString([]byte("if [ -t 0 ] && [ -t 1 ]; then echo GATEWAY_PTY_ISATTY; fi\n")),
		},
	})
	inReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendInputPayload))
	inReq.Header.Set("Content-Type", "application/json")
	inReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inResp, err := http.DefaultClient.Do(inReq)
	if err != nil {
		t.Fatalf("SendInput failed: %v", err)
	}
	inResp.Body.Close()
	if inResp.StatusCode != http.StatusOK {
		t.Fatalf("expected SendInput 200 OK, got %d", inResp.StatusCode)
	}

	out, err := waitForPtyOutput(resp.Body, func(s string) bool {
		return strings.Contains(s, "GATEWAY_PTY_ISATTY")
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("failed waiting for GATEWAY_PTY_ISATTY: %v", err)
	}
	if !strings.Contains(out, "GATEWAY_PTY_ISATTY") {
		t.Fatalf("GATEWAY_PTY_ISATTY not in output: %q", out)
	}

	// 3. Test initial stty size
	sendInputStty, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input": map[string]interface{}{
			"pty": base64.StdEncoding.EncodeToString([]byte("stty size\n")),
		},
	})
	inReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendInputStty))
	inReq2.Header.Set("Content-Type", "application/json")
	inReq2.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inResp2, err := http.DefaultClient.Do(inReq2)
	if err != nil {
		t.Fatalf("SendInput failed: %v", err)
	}
	inResp2.Body.Close()

	out, err = waitForPtyOutput(resp.Body, func(s string) bool {
		return strings.Contains(s, "24 80")
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("failed waiting for 24 80: %v", err)
	}

	// 4. Update resize to 40 cols, 12 rows
	updatePayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"pty": map[string]interface{}{
			"size": map[string]interface{}{
				"cols": 40,
				"rows": 12,
			},
		},
	})
	upReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Update", bytes.NewReader(updatePayload))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	upResp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		t.Fatalf("expected Update 200 OK, got %d", upResp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)

	// Send stty size again to verify resize
	inReq3, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendInputStty))
	inReq3.Header.Set("Content-Type", "application/json")
	inReq3.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inResp3, _ := http.DefaultClient.Do(inReq3)
	inResp3.Body.Close()

	out, err = waitForPtyOutput(resp.Body, func(s string) bool {
		return strings.Contains(s, "12 40")
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("failed waiting for 12 40: %v", err)
	}

	// 5. SendSignal to kill
	killPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"signal":  9,
	})
	sigReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendSignal", bytes.NewReader(killPayload))
	sigReq.Header.Set("Content-Type", "application/json")
	sigReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	sigResp, err := http.DefaultClient.Do(sigReq)
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}
	sigResp.Body.Close()
	if sigResp.StatusCode != http.StatusOK {
		t.Fatalf("expected SendSignal 200 OK, got %d", sigResp.StatusCode)
	}

	// Verify EndEvent on resp.Body
	sawEnd := false
	sawEOS := false
	for {
		frame, err := readNextConnectFrame(resp.Body)
		if err != nil {
			break
		}
		if frame.Flag == 0 {
			var endEvent struct {
				Event struct {
					End *struct {
						ExitCode int  `json:"exitCode"`
						Exited   bool `json:"exited"`
					} `json:"end"`
				} `json:"event"`
			}
			if err := json.Unmarshal(frame.Payload, &endEvent); err == nil && endEvent.Event.End != nil {
				sawEnd = true
				if !endEvent.Event.End.Exited {
					t.Errorf("expected exited: true")
				}
			}
		} else if frame.Flag == 2 {
			sawEOS = true
			break
		}
	}
	if !sawEnd {
		t.Errorf("expected EndEvent after kill")
	}
	if !sawEOS {
		t.Errorf("expected EOS trailer frame")
	}

	// Second kill must return 404
	sigReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendSignal", bytes.NewReader(killPayload))
	sigReq2.Header.Set("Content-Type", "application/json")
	sigReq2.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	sigResp2, err := http.DefaultClient.Do(sigReq2)
	if err != nil {
		t.Fatalf("SendSignal 2 failed: %v", err)
	}
	sigResp2.Body.Close()
	if sigResp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected second SendSignal to return 404, got %d", sigResp2.StatusCode)
	}
}

func TestConnectRPC_PTY_Forwarding_Routes(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	hits := make(map[string]int)
	mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(mockPodd.Close)

	srv.SetPoddURL(rec.SandboxID, mockPodd.URL)

	endpoints := []string{
		"/process.Process/SendInput",
		"/process.Process/Update",
		"/process.Process/StreamInput",
		"/process.Process/CloseStdin",
		"/sandboxes/" + rec.SandboxID + "/process.Process/SendInput",
		"/sandboxes/" + rec.SandboxID + "/process.Process/Update",
		"/sandboxes/" + rec.SandboxID + "/process.Process/StreamInput",
		"/sandboxes/" + rec.SandboxID + "/process.Process/CloseStdin",
	}

	for _, ep := range endpoints {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ep, bytes.NewReader([]byte("{}")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request to %s failed: %v", ep, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for %s, got %d", ep, resp.StatusCode)
		}
	}

	expectedPaths := []string{
		"/process.Process/SendInput",
		"/process.Process/Update",
		"/process.Process/StreamInput",
		"/process.Process/CloseStdin",
	}
	for _, p := range expectedPaths {
		if hits[p] != 2 {
			t.Errorf("expected 2 hits for forwarded path %s, got %d", p, hits[p])
		}
	}
}

func TestConnectRPC_PTY_Connect_And_Multibyte_And_ExitCode(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	// 1. Start PTY
	reqPayload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
		},
		"pty": map[string]interface{}{
			"size": map[string]interface{}{
				"cols": 80,
				"rows": 24,
			},
		},
	}
	rawJSON, _ := json.Marshal(reqPayload)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(rawJSON))
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PTY Start failed: %v", err)
	}

	frame0, err := readNextConnectFrame(resp.Body)
	if err != nil {
		resp.Body.Close()
		t.Fatalf("failed to read start frame: %v", err)
	}
	var startEvent struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	_ = json.Unmarshal(frame0.Payload, &startEvent)
	pid := startEvent.Event.Start.Pid

	// Send input before disconnect
	sendInputPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input": map[string]interface{}{
			"pty": base64.StdEncoding.EncodeToString([]byte("echo BEFORE_DISCONNECT\n")),
		},
	})
	inReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendInputPayload))
	inReq.Header.Set("Content-Type", "application/json")
	inReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inResp, _ := http.DefaultClient.Do(inReq)
	inResp.Body.Close()

	_, err = waitForPtyOutput(resp.Body, func(s string) bool {
		return strings.Contains(s, "BEFORE_DISCONNECT")
	}, 10*time.Second)
	if err != nil {
		resp.Body.Close()
		t.Fatalf("failed waiting for BEFORE_DISCONNECT: %v", err)
	}

	// Disconnect first stream
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)

	// 2. Connect second stream
	connPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
	})
	connReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Connect", bytes.NewReader(connPayload))
	connReq.Header.Set("Content-Type", "application/connect+json")
	connReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	connResp, err := http.DefaultClient.Do(connReq)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer connResp.Body.Close()

	if connResp.StatusCode != http.StatusOK {
		t.Fatalf("expected Connect 200 OK, got %d", connResp.StatusCode)
	}

	frameConn0, err := readNextConnectFrame(connResp.Body)
	if err != nil {
		t.Fatalf("failed to read start frame on connect: %v", err)
	}
	var connStartEvent struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	_ = json.Unmarshal(frameConn0.Payload, &connStartEvent)
	if connStartEvent.Event.Start.Pid != pid {
		t.Fatalf("expected connect pid %d, got %d", pid, connStartEvent.Event.Start.Pid)
	}

	// Send multibyte UTF-8 input
	sendInputMB, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input": map[string]interface{}{
			"pty": base64.StdEncoding.EncodeToString([]byte("echo 你好世界\n")),
		},
	})
	inReqMB, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendInputMB))
	inReqMB.Header.Set("Content-Type", "application/json")
	inReqMB.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inRespMB, _ := http.DefaultClient.Do(inReqMB)
	inRespMB.Body.Close()

	_, err = waitForPtyOutput(connResp.Body, func(s string) bool {
		return strings.Contains(s, "你好世界")
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("failed waiting for 你好世界: %v", err)
	}

	// 3. Send exit 7 to test non-zero exit code
	sendExit, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"input": map[string]interface{}{
			"pty": base64.StdEncoding.EncodeToString([]byte("exit 7\n")),
		},
	})
	inReqExit, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendInput", bytes.NewReader(sendExit))
	inReqExit.Header.Set("Content-Type", "application/json")
	inReqExit.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	inRespExit, _ := http.DefaultClient.Do(inReqExit)
	inRespExit.Body.Close()

	sawExitCode := -1
	for {
		frame, err := readNextConnectFrame(connResp.Body)
		if err != nil {
			break
		}
		if frame.Flag == 0 {
			var endEvent struct {
				Event struct {
					End *struct {
						ExitCode int  `json:"exitCode"`
						Exited   bool `json:"exited"`
					} `json:"end"`
				} `json:"event"`
			}
			if err := json.Unmarshal(frame.Payload, &endEvent); err == nil && endEvent.Event.End != nil {
				sawExitCode = endEvent.Event.End.ExitCode
			}
		} else if frame.Flag == 2 {
			break
		}
	}
	if sawExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", sawExitCode)
	}
}

func TestConnectRPC_PTY_StreamInput(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	reqPayload := map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
		},
		"pty": map[string]interface{}{
			"size": map[string]interface{}{
				"cols": 80,
				"rows": 24,
			},
		},
	}
	rawJSON, _ := json.Marshal(reqPayload)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(rawJSON))
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PTY Start failed: %v", err)
	}
	defer resp.Body.Close()

	frame0, err := readNextConnectFrame(resp.Body)
	if err != nil {
		t.Fatalf("failed to read start frame: %v", err)
	}
	var startEvent struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	_ = json.Unmarshal(frame0.Payload, &startEvent)
	pid := startEvent.Event.Start.Pid

	// Send StreamInput
	f1, _ := json.Marshal(map[string]interface{}{
		"start": map[string]interface{}{
			"process": map[string]interface{}{"pid": pid},
		},
	})
	f2, _ := json.Marshal(map[string]interface{}{
		"data": map[string]interface{}{
			"input": map[string]interface{}{
				"pty": base64.StdEncoding.EncodeToString([]byte("echo GW_STREAM_INPUT_OK\n")),
			},
		},
	})

	var streamBody bytes.Buffer
	streamBody.Write(makeConnectFrame(f1, false))
	streamBody.Write(makeConnectFrame(f2, false))
	streamBody.Write(makeConnectFrame([]byte("{}"), true))

	siReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/StreamInput", &streamBody)
	siReq.Header.Set("Content-Type", "application/connect+json")
	siReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)

	siResp, err := http.DefaultClient.Do(siReq)
	if err != nil {
		t.Fatalf("StreamInput failed: %v", err)
	}
	siResp.Body.Close()
	if siResp.StatusCode != http.StatusOK {
		t.Fatalf("expected StreamInput 200 OK, got %d", siResp.StatusCode)
	}

	out, err := waitForPtyOutput(resp.Body, func(s string) bool {
		return strings.Contains(s, "GW_STREAM_INPUT_OK")
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("failed waiting for GW_STREAM_INPUT_OK: %v", err)
	}
	if !strings.Contains(out, "GW_STREAM_INPUT_OK") {
		t.Fatalf("GW_STREAM_INPUT_OK not found in output: %q", out)
	}

	// Cleanup
	killPayload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{"pid": pid},
		"signal":  9,
	})
	sigReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/process.Process/SendSignal", bytes.NewReader(killPayload))
	sigReq.Header.Set("Content-Type", "application/json")
	sigReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	sigResp, _ := http.DefaultClient.Do(sigReq)
	sigResp.Body.Close()
}
