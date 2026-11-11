package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
}
