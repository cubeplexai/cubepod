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

func TestClient_Templates(t *testing.T) {
	type recordedReq struct {
		method     string
		path       string
		requestURI string
		query      string
		headers    http.Header
		body       map[string]interface{}
		rawBody    string
	}

	var recorded []recordedReq
	record := func(r *http.Request) recordedReq {
		rec := recordedReq{
			method:     r.Method,
			path:       r.URL.Path,
			requestURI: r.RequestURI,
			query:      r.URL.RawQuery,
			headers:    r.Header.Clone(),
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		rec.rawBody = string(bodyBytes)
		if len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &rec.body)
		}
		recorded = append(recorded, rec)
		return rec
	}

	legacyList := false
	var statusSeq []map[string]interface{}
	statusIdx := 0

	mux := http.NewServeMux()

	// 1. List templates
	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		if legacyList {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"templates": []map[string]interface{}{
					{
						"id":          "cubeplex-ubuntu24-desktop",
						"name":        "Desktop",
						"image":       "cubeplex-ubuntu24-desktop:latest",
						"description": "Standard Ubuntu 24.04 desktop",
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"templateID":  "tpl_abc123",
				"buildID":     "bld_xyz789",
				"cpuCount":    4,
				"memoryMB":    4096,
				"diskSizeMB":  20480,
				"public":      true,
				"aliases":     []string{"my-alias", "my-alias:v1"},
				"names":       []string{"my-alias", "my-alias:v1"},
				"buildStatus": "uploaded",
				"envdVersion": "cubepod-podd",
				"spawnCount":  5,
				"buildCount":  2,
			},
		})
	})

	// 2. Get template by ID
	mux.HandleFunc("GET /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		id := r.PathValue("id")
		if id == "not-found" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"template not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		lastSpawn := "2026-11-12T12:00:00Z"
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID":    id,
			"buildID":       "bld_get_1",
			"cpuCount":      2,
			"memoryMB":      2048,
			"diskSizeMB":    10240,
			"public":        false,
			"aliases":       []string{"alias-get"},
			"names":         []string{"alias-get"},
			"buildStatus":   "ready",
			"envdVersion":   "cubepod-podd",
			"spawnCount":    1,
			"buildCount":    1,
			"lastSpawnedAt": lastSpawn,
			"builds": []map[string]interface{}{
				{
					"buildID":   "bld_get_1",
					"status":    "ready",
					"createdAt": "2026-11-12T10:00:00Z",
					"updatedAt": "2026-11-12T10:01:00Z",
					"cpuCount":  2,
					"memoryMB":  2048,
				},
			},
		})
	})

	// 3. Get template by alias
	mux.HandleFunc("GET /templates/aliases/{alias}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		alias := r.PathValue("alias")
		if alias == "unknown-alias" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"alias not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID":  "tpl_by_alias",
			"buildID":     "bld_by_alias",
			"cpuCount":    2,
			"memoryMB":    2048,
			"diskSizeMB":  10240,
			"public":      true,
			"aliases":     []string{alias},
			"names":       []string{alias},
			"buildStatus": "ready",
		})
	})

	// 4. Create template v3
	mux.HandleFunc("POST /v3/templates", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID": "tpl_new_123",
			"buildID":    "bld_new_456",
			"public":     false,
			"aliases":    []string{"custom-alias"},
			"names":      []string{"custom-alias"},
			"tags":       []string{"v1"},
		})
	})

	// 5. Start build v2
	mux.HandleFunc("POST /v2/templates/{templateID}/builds/{buildID}", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		steps, _ := rec.body["steps"].([]interface{})
		for _, s := range steps {
			if sm, ok := s.(map[string]interface{}); ok {
				if sm["type"] == "UNKNOWN_STEP" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"code":400,"message":"unknown step type: UNKNOWN_STEP"}`))
					return
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})

	// 5b. Poll build status
	mux.HandleFunc("GET /templates/{templateID}/builds/{buildID}/status", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		if len(statusSeq) > 0 {
			idx := statusIdx
			if idx >= len(statusSeq) {
				idx = len(statusSeq) - 1
			}
			statusIdx++
			_ = json.NewEncoder(w).Encode(statusSeq[idx])
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID": r.PathValue("templateID"),
			"buildID":    r.PathValue("buildID"),
			"status":     "ready",
			"logs":       []string{},
			"logEntries": []interface{}{},
		})
	})

	// 6. Delete template (204 No Content)
	mux.HandleFunc("DELETE /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		id := r.PathValue("id")
		if id == "tpl-in-use" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":409,"message":"template is referenced by active podbox cpod-active"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// 7. Patch template visibility
	mux.HandleFunc("PATCH /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"names":["my-tpl"]}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(server.URL)
	ctx := context.Background()

	// A. ListTemplates (standard shape)
	tpls, err := c.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates failed: %v", err)
	}
	if len(tpls) != 1 {
		t.Fatalf("expected 1 template, got %d", len(tpls))
	}
	tpl0 := tpls[0]
	if tpl0.TemplateID != "tpl_abc123" || tpl0.BuildID != "bld_xyz789" || tpl0.CPUCount != 4 || tpl0.MemoryMB != 4096 || tpl0.DiskSizeMB != 20480 || !tpl0.Public || tpl0.BuildStatus != "uploaded" || tpl0.EnvdVersion != "cubepod-podd" || tpl0.SpawnCount != 5 || tpl0.BuildCount != 2 {
		t.Fatalf("unexpected fields preserved in template: %+v", tpl0)
	}
	if len(tpl0.Aliases) != 2 || tpl0.Aliases[0] != "my-alias" {
		t.Fatalf("unexpected aliases: %v", tpl0.Aliases)
	}

	// B. ListTemplates (legacy shape)
	legacyList = true
	legacyTpls, err := c.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("legacy ListTemplates failed: %v", err)
	}
	if len(legacyTpls) != 1 || legacyTpls[0].TemplateID != "cubeplex-ubuntu24-desktop" {
		t.Fatalf("expected legacy template decoded, got: %+v", legacyTpls)
	}

	// C. GetTemplate (with builds[] & lastSpawnedAt)
	recBefore := len(recorded)
	getTpl, err := c.GetTemplate(ctx, "tpl_get_123")
	if err != nil {
		t.Fatalf("GetTemplate failed: %v", err)
	}
	if getTpl.TemplateID != "tpl_get_123" || getTpl.BuildStatus != "ready" {
		t.Fatalf("unexpected GetTemplate result: %+v", getTpl)
	}
	if getTpl.LastSpawnedAt == nil || *getTpl.LastSpawnedAt != "2026-11-12T12:00:00Z" {
		t.Fatalf("expected lastSpawnedAt decoded, got: %v", getTpl.LastSpawnedAt)
	}
	if len(getTpl.Builds) != 1 || getTpl.Builds[0].BuildID != "bld_get_1" || getTpl.Builds[0].Status != "ready" {
		t.Fatalf("expected builds[] decoded, got: %+v", getTpl.Builds)
	}
	if recorded[len(recorded)-1].method != http.MethodGet || recorded[len(recorded)-1].path != "/templates/tpl_get_123" {
		t.Fatalf("unexpected request for GetTemplate: %+v", recorded[len(recorded)-1])
	}

	// GetTemplate 404
	_, err = c.GetTemplate(ctx, "not-found")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected error to include status code 404, got: %v", err)
	}

	// D. GetTemplateByAlias (URL-escaped org/tpl:v1)
	aliasTpl, err := c.GetTemplateByAlias(ctx, "org/tpl:v1")
	if err != nil {
		t.Fatalf("GetTemplateByAlias failed: %v", err)
	}
	if aliasTpl.TemplateID != "tpl_by_alias" {
		t.Fatalf("unexpected GetTemplateByAlias result: %+v", aliasTpl)
	}
	if recorded[len(recorded)-1].method != http.MethodGet || recorded[len(recorded)-1].requestURI != "/templates/aliases/org%2Ftpl:v1" {
		t.Fatalf("expected URL-escaped /templates/aliases/org%%2Ftpl:v1, got: %+v", recorded[len(recorded)-1])
	}

	// E. CreateTemplateFromImage with 202 polling (waiting -> building -> ready), tags, registry, idempotency key
	statusSeq = []map[string]interface{}{
		{"templateID": "tpl_new_123", "buildID": "bld_new_456", "status": "waiting"},
		{"templateID": "tpl_new_123", "buildID": "bld_new_456", "status": "building"},
		{"templateID": "tpl_new_123", "buildID": "bld_new_456", "status": "ready"},
	}
	statusIdx = 0

	createReq := client.CreateTemplateFromImageRequest{
		FromImage: "python:3.13",
		FromImageRegistry: &client.RegistryAuth{
			Type:     "registry",
			Username: "ci",
			Password: "secret",
		},
		Alias:          "custom-alias",
		Tags:           []string{"v1", "latest"},
		CPUCount:       2,
		MemoryMB:       2048,
		MinFreeDiskMB:  10240,
		IdempotencyKey: "idem-key-1",
		PollInterval:   1 * time.Millisecond,
	}
	createdRes, err := c.CreateTemplateFromImage(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateTemplateFromImage failed: %v", err)
	}
	if createdRes.TemplateID != "tpl_new_123" || createdRes.BuildID != "bld_new_456" {
		t.Fatalf("unexpected created result: %+v", createdRes)
	}
	if len(createdRes.Tags) != 1 || createdRes.Tags[0] != "v1" {
		t.Fatalf("unexpected tags in create result: %v", createdRes.Tags)
	}

	// Assert v3 request (3rd from end after 3 status polls? Let's locate v3 and v2 before status polls)
	v3Req := recorded[len(recorded)-5]
	if v3Req.method != http.MethodPost || v3Req.path != "/v3/templates" {
		t.Fatalf("expected POST /v3/templates, got: %+v", v3Req)
	}
	if v3Req.headers.Get("Idempotency-Key") != "idem-key-1" {
		t.Fatalf("expected Idempotency-Key header, got: %v", v3Req.headers)
	}
	if v3Req.body["alias"] != "custom-alias" || v3Req.body["cpuCount"] != float64(2) || v3Req.body["memoryMB"] != float64(2048) || v3Req.body["minFreeDiskMb"] != float64(10240) {
		t.Fatalf("unexpected v3 body: %+v", v3Req.body)
	}
	if tags, ok := v3Req.body["tags"].([]interface{}); !ok || len(tags) != 2 || tags[0] != "v1" {
		t.Fatalf("expected tags in v3 body, got: %+v", v3Req.body["tags"])
	}
	if _, exists := v3Req.body["name"]; exists {
		t.Fatalf("expected un-set 'name' to not be in v3 body, got: %+v", v3Req.body)
	}

	v2Req := recorded[len(recorded)-4]
	if v2Req.method != http.MethodPost || v2Req.path != "/v2/templates/tpl_new_123/builds/bld_new_456" {
		t.Fatalf("expected POST /v2/templates/tpl_new_123/builds/bld_new_456, got: %+v", v2Req)
	}
	if v2Req.body["fromImage"] != "python:3.13" {
		t.Fatalf("expected fromImage='python:3.13', got: %+v", v2Req.body)
	}
	if reg, ok := v2Req.body["fromImageRegistry"].(map[string]interface{}); !ok || reg["username"] != "ci" {
		t.Fatalf("expected fromImageRegistry in v2 build body, got: %+v", v2Req.body["fromImageRegistry"])
	}
	steps, ok := v2Req.body["steps"].([]interface{})
	if !ok || len(steps) != 0 {
		t.Fatalf("expected empty steps array [], got: %+v", v2Req.body["steps"])
	}
	if strings.Contains(strings.ToLower(v2Req.rawBody), "dockerfile") {
		t.Fatalf("expected no Dockerfile in create-from-image build body, got: %s", v2Req.rawBody)
	}
	if statusIdx != 3 {
		t.Fatalf("expected 3 status polls (waiting -> building -> ready), got %d", statusIdx)
	}

	// F. DeleteTemplate (204 No Content, force=true and force=false)
	if err := c.DeleteTemplate(ctx, "tpl_to_del", true); err != nil {
		t.Fatalf("DeleteTemplate(force=true) failed: %v", err)
	}
	delRec := recorded[len(recorded)-1]
	if delRec.method != http.MethodDelete || delRec.path != "/templates/tpl_to_del" || delRec.query != "force=true" {
		t.Fatalf("expected DELETE /templates/tpl_to_del?force=true, got: %+v", delRec)
	}

	if err := c.DeleteTemplate(ctx, "tpl_to_del_noforce", false); err != nil {
		t.Fatalf("DeleteTemplate(force=false) failed: %v", err)
	}
	delNoForceRec := recorded[len(recorded)-1]
	if delNoForceRec.query != "" {
		t.Fatalf("expected no force query param, got: %s", delNoForceRec.query)
	}

	// Assert 409 body surfaced
	err = c.DeleteTemplate(ctx, "tpl-in-use", false)
	if err == nil {
		t.Fatal("expected conflict error on 409, got nil")
	}
	if !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "template is referenced by active podbox cpod-active") {
		t.Fatalf("expected error to surface 409 response body, got: %v", err)
	}

	// G. SetTemplatePublic & UpdateTemplateVisibility(false)
	if err := c.SetTemplatePublic(ctx, "tpl_pub_1"); err != nil {
		t.Fatalf("SetTemplatePublic failed: %v", err)
	}
	patchRec := recorded[len(recorded)-1]
	if patchRec.method != http.MethodPatch || patchRec.path != "/templates/tpl_pub_1" || patchRec.body["public"] != true {
		t.Fatalf("expected PATCH /templates/tpl_pub_1 public=true, got: %+v", patchRec)
	}

	if err := c.UpdateTemplateVisibility(ctx, "org/tpl:v1", false); err != nil {
		t.Fatalf("UpdateTemplateVisibility(false) failed: %v", err)
	}
	patchPriv := recorded[len(recorded)-1]
	if patchPriv.requestURI != "/templates/org%2Ftpl:v1" || patchPriv.body["public"] != false {
		t.Fatalf("expected PATCH /templates/org%%2Ftpl:v1 public=false, got: %+v", patchPriv)
	}

	// H. BuildTemplate (NoWait + polling + error with BuildStatusReason)
	statusSeq = nil
	statusIdx = 0
	buildReq := client.BuildTemplateRequest{
		Name:      "built-tpl",
		Alias:     "built-alias",
		FromImage: "node:20",
		Steps: []client.BuildStep{
			{
				Type: "RUN",
				Args: []string{"npm install"},
			},
		},
		StartCmd:     "npm start",
		ReadyCmd:     "curl -sf http://localhost:3000",
		Force:        true,
		PollInterval: 1 * time.Millisecond,
	}
	bldRes, err := c.BuildTemplate(ctx, buildReq)
	if err != nil {
		t.Fatalf("BuildTemplate failed: %v", err)
	}
	if bldRes.TemplateID != "tpl_new_123" {
		t.Fatalf("unexpected BuildTemplate result: %+v", bldRes)
	}
	// recorded[-2] is POST /v2/.../builds/... because recorded[-1] is GET .../status
	lastBldRec := recorded[len(recorded)-2]
	if lastBldRec.body["startCmd"] != "npm start" || lastBldRec.body["readyCmd"] != "curl -sf http://localhost:3000" || lastBldRec.body["force"] != true {
		t.Fatalf("unexpected build body: %+v", lastBldRec.body)
	}
	if bldSteps, ok := lastBldRec.body["steps"].([]interface{}); !ok || len(bldSteps) != 1 {
		t.Fatalf("expected 1 step in build request, got: %+v", lastBldRec.body["steps"])
	}

	// BuildTemplate error status surfaces BuildStatusReason.Message
	statusSeq = []map[string]interface{}{
		{"templateID": "tpl_new_123", "buildID": "bld_new_456", "status": "building"},
		{
			"templateID": "tpl_new_123",
			"buildID":    "bld_new_456",
			"status":     "error",
			"reason": map[string]interface{}{
				"message": "npm install exited with code 1",
				"step":    "RUN npm install",
			},
		},
	}
	statusIdx = 0
	_, err = c.BuildTemplate(ctx, buildReq)
	if err == nil {
		t.Fatal("expected build error when status is error, got nil")
	}
	if !strings.Contains(err.Error(), "npm install exited with code 1") || !strings.Contains(err.Error(), "RUN npm install") {
		t.Fatalf("expected error to include BuildStatusReason message and step, got: %v", err)
	}

	// BuildTemplate unknown step 400 error body surfaced
	badBuildReq := client.BuildTemplateRequest{
		Alias:     "bad-step-tpl",
		FromImage: "node:20",
		Steps: []client.BuildStep{
			{
				Type: "UNKNOWN_STEP",
				Args: []string{"foo"},
			},
		},
	}
	_, err = c.BuildTemplate(ctx, badBuildReq)
	if err == nil {
		t.Fatal("expected error for unknown step type, got nil")
	}
	if !strings.Contains(err.Error(), "unknown step type: UNKNOWN_STEP") {
		t.Fatalf("expected error to surface 400 body, got: %v", err)
	}
	_ = recBefore
}

func TestClient_OmittedUIDGID(t *testing.T) {
	var (
		cmdBodies []map[string]interface{}
		fileQuery []string
		syncQuery []string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes/{id}/commands/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cmdBodies = append(cmdBodies, body)
		exitCode := 0
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CommandResult{ID: "cmd-1", ExitCode: &exitCode})
	})
	mux.HandleFunc("PUT /sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		fileQuery = append(fileQuery, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /sandboxes/{id}/files/archive-sync", func(w http.ResponseWriter, r *http.Request) {
		syncQuery = append(syncQuery, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"extracted"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(server.URL)
	ctx := context.Background()

	// 1. Default RunCommand omits uid and gid (does not inject 1000:1000)
	if _, err := c.RunCommand(ctx, "sb-1", client.RunCommandRequest{Command: "id"}); err != nil {
		t.Fatalf("RunCommand default failed: %v", err)
	}
	if _, hasUID := cmdBodies[0]["uid"]; hasUID {
		t.Fatalf("expected uid to be omitted by default, got: %+v", cmdBodies[0])
	}
	if _, hasGID := cmdBodies[0]["gid"]; hasGID {
		t.Fatalf("expected gid to be omitted by default, got: %+v", cmdBodies[0])
	}

	// 2. Explicit RunAsRoot sends uid=0 and gid=0
	if _, err := c.RunCommand(ctx, "sb-1", client.RunCommandRequest{Command: "id", RunAsRoot: true}); err != nil {
		t.Fatalf("RunCommand RunAsRoot failed: %v", err)
	}
	if cmdBodies[1]["uid"] != float64(0) || cmdBodies[1]["gid"] != float64(0) {
		t.Fatalf("expected explicit uid=0, gid=0 for RunAsRoot, got: %+v", cmdBodies[1])
	}

	// 3. Explicit non-zero UID/GID sends them
	if _, err := c.RunCommand(ctx, "sb-1", client.RunCommandRequest{Command: "id", UID: 1001, GID: 1002}); err != nil {
		t.Fatalf("RunCommand explicit uid/gid failed: %v", err)
	}
	if cmdBodies[2]["uid"] != float64(1001) || cmdBodies[2]["gid"] != float64(1002) {
		t.Fatalf("expected uid=1001, gid=1002, got: %+v", cmdBodies[2])
	}

	// 4. WriteFile and ArchiveSync omit uid/gid by default
	if err := c.WriteFile(ctx, "sb-1", "/tmp/a.txt", []byte("hi"), "0644"); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if strings.Contains(fileQuery[0], "uid=") || strings.Contains(fileQuery[0], "gid=") {
		t.Fatalf("expected WriteFile to omit uid/gid by default, got query: %s", fileQuery[0])
	}

	if err := c.ArchiveSync(ctx, "sb-1", "/workspace", map[string][]byte{"a.txt": []byte("hi")}); err != nil {
		t.Fatalf("ArchiveSync failed: %v", err)
	}
	if strings.Contains(syncQuery[0], "uid=") || strings.Contains(syncQuery[0], "gid=") {
		t.Fatalf("expected ArchiveSync to omit uid/gid by default, got query: %s", syncQuery[0])
	}

	// 5. WriteFileWithOwner / ArchiveSyncWithOwner send explicit uid=0, gid=0
	zero := 0
	if err := c.WriteFileWithOwner(ctx, "sb-1", "/tmp/root.txt", []byte("hi"), "0644", &zero, &zero); err != nil {
		t.Fatalf("WriteFileWithOwner failed: %v", err)
	}
	if !strings.Contains(fileQuery[1], "uid=0") || !strings.Contains(fileQuery[1], "gid=0") {
		t.Fatalf("expected WriteFileWithOwner to send uid=0&gid=0, got query: %s", fileQuery[1])
	}

	if err := c.ArchiveSyncWithOwner(ctx, "sb-1", "/workspace", map[string][]byte{"a.txt": []byte("hi")}, &zero, &zero); err != nil {
		t.Fatalf("ArchiveSyncWithOwner failed: %v", err)
	}
	if !strings.Contains(syncQuery[1], "uid=0") || !strings.Contains(syncQuery[1], "gid=0") {
		t.Fatalf("expected ArchiveSyncWithOwner to send uid=0&gid=0, got query: %s", syncQuery[1])
	}
}
