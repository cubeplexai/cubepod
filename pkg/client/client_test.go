package client_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

	"github.com/cubeplexai/cubepod/pkg/client"
)

func TestClient_AllMethods(t *testing.T) {
	mux := http.NewServeMux()

	// 1. Health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "cubepod-gateway"})
	})

	// 2. Sandboxes collection
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req client.CreateSandboxRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:        "cpod-123",
			TemplateID:       req.TemplateID,
			State:            "Running",
			EnvdPort:         49983,
			RootfsGeneration: 1,
			VolumeKey:        req.VolumeKey,
		})
	})

	mux.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]client.SandboxRecord{
			{
				SandboxID:  "cpod-123",
				TemplateID: "cubeplex-desktop",
				State:      "Running",
			},
		})
	})

	// 3. Sandboxes item & lifecycle
	mux.HandleFunc("GET /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:  id,
			TemplateID: "cubeplex-desktop",
			State:      "Running",
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:  id,
			TemplateID: "cubeplex-desktop",
			State:      "Paused",
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/connect", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:  id,
			TemplateID: "cubeplex-desktop",
			State:      "Running",
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/reset-rootfs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:        id,
			TemplateID:       "cubeplex-desktop",
			State:            "Running",
			RootfsGeneration: 2,
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/fork", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.SandboxRecord{
			SandboxID:  "cpod-child-456",
			TemplateID: "cubeplex-desktop",
			State:      "Running",
		})
	})

	mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /sandboxes/{id}/timeout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /sandboxes/{id}/lease", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// 4. Commands
	mux.HandleFunc("POST /sandboxes/{id}/commands/run", func(w http.ResponseWriter, r *http.Request) {
		exitCode := 0
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CommandResult{
			ID:       "cmd-99",
			ExitCode: &exitCode,
			Output:   "echo output",
			Running:  false,
			PID:      5432,
		})
	})

	mux.HandleFunc("GET /sandboxes/{id}/commands/{cmdID}/status", func(w http.ResponseWriter, r *http.Request) {
		cmdID := r.PathValue("cmdID")
		exitCode := 0
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CommandInfo{
			ID:       cmdID,
			PID:      5432,
			Running:  false,
			ExitCode: &exitCode,
			Output:   "completed",
		})
	})

	mux.HandleFunc("GET /sandboxes/{id}/commands/{cmdID}/logs", func(w http.ResponseWriter, r *http.Request) {
		cmdID := r.PathValue("cmdID")
		cursorStr := r.URL.Query().Get("cursor")
		var cursor int
		fmt.Sscanf(cursorStr, "%d", &cursor)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CommandLogs{
			ID:      cmdID,
			Content: "log line\n",
			Cursor:  cursor + 9,
			Running: false,
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/commands/{cmdID}/kill", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "killed"})
	})

	// 5. Files
	mux.HandleFunc("PUT /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		data, _ := io.ReadAll(r.Body)
		if path == "" || len(data) == 0 {
			http.Error(w, "invalid file put", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("file binary content"))
	})

	mux.HandleFunc("DELETE /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})

	mux.HandleFunc("GET /sandboxes/{id}/files/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entries": []client.FileEntry{
				{
					Name:  "main.py",
					Path:  "/workspace/main.py",
					IsDir: false,
					Size:  42,
					Mode:  "0644",
				},
			},
		})
	})

	mux.HandleFunc("POST /sandboxes/{id}/files/archive-sync", func(w http.ResponseWriter, r *http.Request) {
		dest := r.URL.Query().Get("dest")
		if dest == "" {
			http.Error(w, "missing dest", http.StatusBadRequest)
			return
		}
		gzr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip", http.StatusBadRequest)
			return
		}
		defer gzr.Close()
		tr := tar.NewReader(gzr)
		var filesFound int
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "tar read error", http.StatusBadRequest)
				return
			}
			if hdr.Name != "" {
				filesFound++
			}
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "extracted", "extracted": filesFound})
	})

	// 6. Endpoints & WarmPools
	mux.HandleFunc("POST /sandboxes/{id}/endpoints/{port}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.PanelEndpoint{
			URL:   "http://localhost:8080/panel/test-token/",
			Token: "test-token",
		})
	})

	mux.HandleFunc("POST /warmpools", func(w http.ResponseWriter, r *http.Request) {
		var wp client.WarmPool
		_ = json.NewDecoder(r.Body).Decode(&wp)
		wp.ReadyReplicas = wp.Replicas
		wp.AvailableReplicas = wp.Replicas
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(wp)
	})

	mux.HandleFunc("GET /warmpools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]client.WarmPool{
			{Name: "pool-1", TemplateID: "cubeplex-desktop", Replicas: 2, ReadyReplicas: 2},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(
		server.URL,
		client.WithAPIKey("secret-token"),
		client.WithTimeout(10*time.Second),
	)

	ctx := context.Background()

	// 1. Health
	health, err := c.Health(ctx)
	if err != nil || health["status"] != "ok" {
		t.Fatalf("Health failed: %v", err)
	}

	// 2. CreateSandbox
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: "cubeplex-desktop",
		VolumeKey:  "vol-key-1",
	})
	if err != nil || sb.SandboxID != "cpod-123" {
		t.Fatalf("CreateSandbox failed: %v", err)
	}

	// 3. Get & List
	sbGet, err := c.GetSandbox(ctx, "cpod-123")
	if err != nil || sbGet.State != "Running" {
		t.Fatalf("GetSandbox failed: %v", err)
	}

	sbs, err := c.ListSandboxes(ctx, "cubeplex-desktop")
	if err != nil || len(sbs) != 1 {
		t.Fatalf("ListSandboxes failed: %v", err)
	}

	// 4. Pause, Connect, Reset, Fork
	sbPaused, err := c.PauseSandbox(ctx, "cpod-123")
	if err != nil || sbPaused.State != "Paused" {
		t.Fatalf("PauseSandbox failed: %v", err)
	}

	sbConnected, err := c.ConnectSandbox(ctx, "cpod-123")
	if err != nil || sbConnected.State != "Running" {
		t.Fatalf("ConnectSandbox failed: %v", err)
	}

	sbReset, err := c.ResetRootFS(ctx, "cpod-123")
	if err != nil || sbReset.RootfsGeneration != 2 {
		t.Fatalf("ResetRootFS failed: %v", err)
	}

	sbFork, err := c.ForkSandbox(ctx, "cpod-123")
	if err != nil || sbFork.SandboxID != "cpod-child-456" {
		t.Fatalf("ForkSandbox failed: %v", err)
	}

	// 5. Timeout, Lease, Delete
	if err := c.SetTimeout(ctx, "cpod-123", 1800); err != nil {
		t.Fatalf("SetTimeout failed: %v", err)
	}
	if err := c.AcquireLease(ctx, "cpod-123", 120); err != nil {
		t.Fatalf("AcquireLease failed: %v", err)
	}
	if err := c.DeleteSandbox(ctx, "cpod-123"); err != nil {
		t.Fatalf("DeleteSandbox failed: %v", err)
	}

	// 6. Commands
	cmdRes, err := c.RunCommand(ctx, "cpod-123", client.RunCommandRequest{
		Command: "echo hi",
	})
	if err != nil || cmdRes.Output != "echo output" {
		t.Fatalf("RunCommand failed: %v", err)
	}

	cmdInfo, err := c.GetCommandStatus(ctx, "cpod-123", "cmd-99")
	if err != nil || cmdInfo.ID != "cmd-99" {
		t.Fatalf("GetCommandStatus failed: %v", err)
	}

	cmdLogs, err := c.GetCommandLogs(ctx, "cpod-123", "cmd-99", 0)
	if err != nil || !strings.Contains(cmdLogs.Content, "log line") {
		t.Fatalf("GetCommandLogs failed: %v", err)
	}

	if err := c.KillCommand(ctx, "cpod-123", "cmd-99"); err != nil {
		t.Fatalf("KillCommand failed: %v", err)
	}

	// 7. Files
	if err := c.WriteFile(ctx, "cpod-123", "/workspace/main.py", []byte("print('hello')"), "0644"); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	readData, err := c.ReadFile(ctx, "cpod-123", "/workspace/main.py")
	if err != nil || string(readData) != "file binary content" {
		t.Fatalf("ReadFile failed: %v", err)
	}

	fileEntries, err := c.ListFiles(ctx, "cpod-123", "/workspace")
	if err != nil || len(fileEntries) != 1 || fileEntries[0].Name != "main.py" {
		t.Fatalf("ListFiles failed: %v", err)
	}

	if err := c.DeleteFile(ctx, "cpod-123", "/workspace/main.py"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}

	// 8. ArchiveSync & ArchiveSyncDir
	if err := c.ArchiveSync(ctx, "cpod-123", "/workspace", map[string][]byte{
		"a.txt": []byte("hello"),
		"b.txt": []byte("world"),
	}); err != nil {
		t.Fatalf("ArchiveSync failed: %v", err)
	}

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("content 1"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "file2.txt"), []byte("content 2"), 0644)
	if err := c.ArchiveSyncDir(ctx, "cpod-123", "/workspace", tmpDir); err != nil {
		t.Fatalf("ArchiveSyncDir failed: %v", err)
	}

	// 9. Panel endpoint
	panel, err := c.GetPanelEndpoint(ctx, "cpod-123", 8080, 3600)
	if err != nil || panel.Token != "test-token" {
		t.Fatalf("GetPanelEndpoint failed: %v", err)
	}

	// 10. WarmPools
	wpCreated, err := c.CreateWarmPool(ctx, "pool-1", "cubeplex-desktop", 2)
	if err != nil || wpCreated.Name != "pool-1" {
		t.Fatalf("CreateWarmPool failed: %v", err)
	}

	wpList, err := c.ListWarmPools(ctx)
	if err != nil || len(wpList) != 1 {
		t.Fatalf("ListWarmPools failed: %v", err)
	}
}

func TestClient_ErrorHandling(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sandbox not found", http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(server.URL)
	_, err := c.GetSandbox(context.Background(), "non-existent")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected error containing 404, got %v", err)
	}
}

func TestClient_ListWarmPools_WrappedResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /warmpools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"warmPools": []map[string]interface{}{
				{
					"name":              "wrapped-pool",
					"templateRef":       "cubeplex-desktop",
					"replicas":          3,
					"readyReplicas":     3,
					"availableReplicas": 3,
				},
			},
			"warmPoolReadyCount": 3,
			"totalReady":         3,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(server.URL)
	pools, err := c.ListWarmPools(context.Background())
	if err != nil {
		t.Fatalf("ListWarmPools with wrapped response failed: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(pools))
	}
	if pools[0].Name != "wrapped-pool" || pools[0].AvailableReplicas != 3 {
		t.Fatalf("unexpected pool data: %+v", pools[0])
	}
}
