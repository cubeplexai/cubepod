package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
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
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
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
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
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
