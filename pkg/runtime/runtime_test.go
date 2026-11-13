package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPoddProcessBackend_LifecycleAndPersistence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	backend, err := NewPoddProcessBackend(PoddProcessBackendConfig{
		BaseDir:          tmpDir,
		EnableSparseExt4: true,
		HealthTimeout:    5 * time.Second,
	})
	if err != nil {
		if strings.Contains(err.Error(), "podd binary not found") {
			t.Skip("skipping test: podd binary not found (run make build-podd first)")
		}
		t.Fatalf("failed to create PoddProcessBackend: %v", err)
	}
	defer backend.Close()

	spec := SandboxSpec{
		SandboxID:        "sb-lifecycle-1",
		TemplateID:       "cubeplex-ubuntu24-desktop",
		RootfsGeneration: 1,
	}

	// 1. EnsureRunning (start)
	info, err := backend.EnsureRunning(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureRunning failed: %v", err)
	}

	if info.SandboxID != spec.SandboxID {
		t.Errorf("expected sandboxID %s, got %s", spec.SandboxID, info.SandboxID)
	}
	if info.PID <= 0 {
		t.Errorf("expected positive PID, got %d", info.PID)
	}
	if info.PoddURL == "" {
		t.Fatalf("expected non-empty PoddURL")
	}

	// Verify /health endpoint returns 200 OK
	resp, err := http.Get(info.PoddURL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /health, got %d", resp.StatusCode)
	}

	// Write files into workspace and rootfs
	wsFile := filepath.Join(info.WorkspaceDir, "test-ws.txt")
	rootfsFile := filepath.Join(info.RootfsDir, "test-rootfs.txt")

	if err := os.WriteFile(wsFile, []byte("workspace-persistent-data"), 0644); err != nil {
		t.Fatalf("failed to write ws file: %v", err)
	}
	if err := os.WriteFile(rootfsFile, []byte("rootfs-persistent-data"), 0644); err != nil {
		t.Fatalf("failed to write rootfs file: %v", err)
	}

	initialPID := info.PID

	// 2. Pause
	if err := backend.Pause(ctx, spec.SandboxID); err != nil {
		t.Fatalf("Pause failed: %v", err)
	}

	// Verify the process stopped
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(initialPID, 0); err == nil {
		t.Errorf("expected initial PID %d to be stopped, but still alive", initialPID)
	}

	// 3. EnsureRunning (Auto-Resume / connect)
	resumedInfo, err := backend.EnsureRunning(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureRunning (resume) failed: %v", err)
	}

	if resumedInfo.PID <= 0 {
		t.Errorf("expected positive PID after resume, got %d", resumedInfo.PID)
	}

	// Verify healthy after resume
	resp, err = http.Get(resumedInfo.PoddURL + "/health")
	if err != nil {
		t.Fatalf("GET /health after resume failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK after resume, got %d", resp.StatusCode)
	}

	// 4. Verify both workspace and rootfs files survived
	wsData, err := os.ReadFile(wsFile)
	if err != nil {
		t.Fatalf("failed to read ws file after resume: %v", err)
	}
	if string(wsData) != "workspace-persistent-data" {
		t.Errorf("workspace data mismatch: got %q", string(wsData))
	}

	rootfsData, err := os.ReadFile(rootfsFile)
	if err != nil {
		t.Fatalf("failed to read rootfs file after resume: %v", err)
	}
	if string(rootfsData) != "rootfs-persistent-data" {
		t.Errorf("rootfs data mismatch: got %q", string(rootfsData))
	}
}

func TestPoddProcessBackend_ResetRootFSPreservesWorkspace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	backend, err := NewPoddProcessBackend(PoddProcessBackendConfig{
		BaseDir:          tmpDir,
		EnableSparseExt4: true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "podd binary not found") {
			t.Skip("skipping test: podd binary not found (run make build-podd first)")
		}
		t.Fatalf("failed to create PoddProcessBackend: %v", err)
	}
	defer backend.Close()

	spec := SandboxSpec{
		SandboxID:        "sb-reset-1",
		TemplateID:       "cubeplex-ubuntu24-desktop",
		RootfsGeneration: 1,
	}

	info, err := backend.EnsureRunning(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureRunning failed: %v", err)
	}

	wsFile := filepath.Join(info.WorkspaceDir, "keep-me.txt")
	rootfsFile := filepath.Join(info.RootfsDir, "wipe-me.txt")

	if err := os.WriteFile(wsFile, []byte("must-preserve"), 0644); err != nil {
		t.Fatalf("write wsFile: %v", err)
	}
	if err := os.WriteFile(rootfsFile, []byte("must-wipe"), 0644); err != nil {
		t.Fatalf("write rootfsFile: %v", err)
	}

	// Reset RootFS
	if err := backend.ResetRootFS(ctx, spec.SandboxID); err != nil {
		t.Fatalf("ResetRootFS failed: %v", err)
	}

	// Verify rootfs file is gone
	if _, err := os.Stat(rootfsFile); !os.IsNotExist(err) {
		t.Errorf("expected rootfs file to be wiped, but still exists")
	}

	// Verify workspace file is preserved
	wsData, err := os.ReadFile(wsFile)
	if err != nil {
		t.Fatalf("failed to read wsFile after reset: %v", err)
	}
	if string(wsData) != "must-preserve" {
		t.Errorf("workspace data mismatch after reset: got %q", string(wsData))
	}
}

func TestPoddProcessBackend_CoWFork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	backend, err := NewPoddProcessBackend(PoddProcessBackendConfig{
		BaseDir:          tmpDir,
		EnableSparseExt4: true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "podd binary not found") {
			t.Skip("skipping test: podd binary not found (run make build-podd first)")
		}
		t.Fatalf("failed to create PoddProcessBackend: %v", err)
	}
	defer backend.Close()

	parentSpec := SandboxSpec{
		SandboxID:        "sb-parent",
		TemplateID:       "cubeplex-ubuntu24-desktop",
		RootfsGeneration: 1,
	}

	parentInfo, err := backend.EnsureRunning(ctx, parentSpec)
	if err != nil {
		t.Fatalf("EnsureRunning parent failed: %v", err)
	}

	parentWsFile := filepath.Join(parentInfo.WorkspaceDir, "parent-data.txt")
	if err := os.WriteFile(parentWsFile, []byte("hello-from-parent"), 0644); err != nil {
		t.Fatalf("write parentWsFile: %v", err)
	}

	// Fork parent into child
	childInfo, err := backend.Fork(ctx, "sb-parent", "sb-child")
	if err != nil {
		t.Fatalf("Fork failed: %v", err)
	}

	if childInfo.SandboxID != "sb-child" {
		t.Errorf("expected child ID sb-child, got %s", childInfo.SandboxID)
	}
	if childInfo.PoddURL == parentInfo.PoddURL {
		t.Errorf("child should have a different PoddURL than parent: %s", childInfo.PoddURL)
	}

	// Verify child has copied parent data
	childWsFile := filepath.Join(childInfo.WorkspaceDir, "parent-data.txt")
	childData, err := os.ReadFile(childWsFile)
	if err != nil {
		t.Fatalf("failed to read childWsFile: %v", err)
	}
	if string(childData) != "hello-from-parent" {
		t.Errorf("child data mismatch: got %q", string(childData))
	}

	// Write new file in child
	childOnlyFile := filepath.Join(childInfo.WorkspaceDir, "child-only.txt")
	if err := os.WriteFile(childOnlyFile, []byte("private-to-child"), 0644); err != nil {
		t.Fatalf("write childOnlyFile: %v", err)
	}

	// Verify childOnlyFile does NOT exist in parent
	parentCheck := filepath.Join(parentInfo.WorkspaceDir, "child-only.txt")
	if _, err := os.Stat(parentCheck); !os.IsNotExist(err) {
		t.Errorf("child-only file leaked into parent workspace!")
	}

	// Verify both parent and child /health endpoints work
	for _, url := range []string{parentInfo.PoddURL, childInfo.PoddURL} {
		resp, err := http.Get(url + "/health")
		if err != nil {
			t.Fatalf("GET /health failed for %s: %v", url, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK from %s, got %d", url, resp.StatusCode)
		}
	}
}

func TestDockerBackend_CommandBuilder(t *testing.T) {
	docker := NewDockerBackend(DockerConfig{
		BinDir:           "/host/opt/cubepod/bin",
		BaseDir:          "/var/lib/cubepod/sandboxes",
		Network:          "cubepod-net",
		EnableSparseExt4: true,
	})

	spec := SandboxSpec{
		SandboxID:  "sb-docker-test",
		TemplateID: "cubeplex-ubuntu24-desktop",
		Env: map[string]string{
			"CUSTOM_VAR": "val123",
		},
	}

	args, err := docker.BuildRunArgs(spec)
	if err != nil {
		t.Fatalf("BuildRunArgs failed: %v", err)
	}

	// Check core flags
	if args[0] != "run" || args[1] != "-d" {
		t.Errorf("expected 'run -d' prefix, got %v", args[:2])
	}

	expectedInjections := []struct {
		name string
		val  string
	}{
		{"container name", "--name"},
		{"container name val", "cubepod-sb-docker-test"},
		{"bin volume", "/host/opt/cubepod/bin:/opt/cubepod/bin:ro"},
		{"entrypoint flag", "--entrypoint"},
		{"entrypoint target", "/opt/cubepod/bin/podd-init"},
		{"network flag", "--network"},
		{"network val", "cubepod-net"},
		{"custom env", "CUSTOM_VAR=val123"},
		{"podd addr env", "CUBEPOD_ADDR=0.0.0.0:49983"},
		{"podd workdir env", "CUBEPOD_WORKDIR=/workspace"},
		{"podd binary arg", "/opt/cubepod/bin/podd"},
	}

	for _, exp := range expectedInjections {
		found := slices.Contains(args, exp.val)
		if !found {
			t.Errorf("expected %s (%q) in BuildRunArgs, args: %v", exp.name, exp.val, args)
		}
	}

	// Check sparse ext4 mounts
	sparseMountFound := false
	for _, arg := range args {
		if arg == "/var/lib/cubepod/sandboxes/sb-docker-test/rootfs.ext4:/opt/cubepod/disks/rootfs.ext4" {
			sparseMountFound = true
			break
		}
	}
	if !sparseMountFound {
		t.Errorf("expected sparse rootfs.ext4 mount in args: %v", args)
	}
}

func TestPoddProcessBackend_DeleteAndInspect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	backend, err := NewPoddProcessBackend(PoddProcessBackendConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		if strings.Contains(err.Error(), "podd binary not found") {
			t.Skip("skipping test: podd binary not found (run make build-podd first)")
		}
		t.Fatalf("create backend: %v", err)
	}
	defer backend.Close()

	spec := SandboxSpec{
		SandboxID: "sb-del-1",
	}

	info, err := backend.EnsureRunning(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	inspected, err := backend.Inspect(ctx, "sb-del-1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inspected.PoddURL != info.PoddURL {
		t.Errorf("expected URL %s, got %s", info.PoddURL, inspected.PoddURL)
	}

	if err := backend.Delete(ctx, "sb-del-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = backend.Inspect(ctx, "sb-del-1")
	if err != ErrSandboxNotFound {
		t.Errorf("expected ErrSandboxNotFound, got %v", err)
	}

	// Verify sandbox directory was deleted
	sandboxDir := filepath.Join(tmpDir, "sb-del-1")
	if _, err := os.Stat(sandboxDir); !os.IsNotExist(err) {
		t.Errorf("expected sandbox directory to be deleted")
	}
}

func TestLocalTemplateTag(t *testing.T) {
	t.Run("local tag cubepod-templates/<slug>:<buildID>", func(t *testing.T) {
		slug := "my-python-app"
		buildID := "bld_12345678"
		expected := "cubepod-templates/my-python-app:bld_12345678"
		got := LocalTemplateTag(slug, buildID)
		if got != expected {
			t.Fatalf("expected tag %q, got %q", expected, got)
		}
	})
}

func TestDockerBackend_CoWForkIsolationAndQuiesceThawOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		mu         sync.Mutex
		events     []string
		quiesceReq map[string]any
	)
	recordEvent := func(ev string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/lifecycle/quiesce":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &quiesceReq)
			recordEvent("quiesce")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"quiesced","synced":true,"frozen":true,"maxFreezeMs":1500}`))
		case "/v1/lifecycle/thaw":
			recordEvent("thaw")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"thawed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	baseDir := t.TempDir()
	docker := NewDockerBackend(DockerConfig{
		BinDir:           "/opt/cubepod/bin",
		BaseDir:          baseDir,
		EnableSparseExt4: true,
		SparseExt4Size:   1024 * 1024,
	})
	docker.runDocker = func(_ context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			recordEvent("docker_run")
		}
		return []byte("mock-container-id"), nil
	}

	parentDir := filepath.Join(baseDir, "sb-parent")
	parentWs := filepath.Join(parentDir, "workspace")
	parentRootfs := filepath.Join(parentDir, "rootfs")
	if err := os.MkdirAll(parentWs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parentRootfs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentWs, "hello.txt"), []byte("parent-ws"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentRootfs, "pkg.conf"), []byte("parent-rootfs"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := createSparseFile(filepath.Join(parentDir, "workspace.ext4"), 1024*1024); err != nil {
		t.Fatal(err)
	}
	if err := createSparseFile(filepath.Join(parentDir, "rootfs.ext4"), 1024*1024); err != nil {
		t.Fatal(err)
	}
	// Write non-zero bytes into parent workspace.ext4 to verify loopback image contents clone cleanly.
	f, err := os.OpenFile(filepath.Join(parentDir, "workspace.ext4"), os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("EXT4-PARENT-MARKER"), 4096); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	docker.mu.Lock()
	docker.instances["sb-parent"] = &dockerInstance{
		spec: SandboxSpec{
			SandboxID:  "sb-parent",
			TemplateID: "ubuntu-test",
		},
		info: InstanceInfo{
			SandboxID:    "sb-parent",
			PoddURL:      srv.URL,
			WorkspaceDir: parentWs,
			RootfsDir:    parentRootfs,
			ContainerID:  "cubepod-sb-parent",
		},
		running:     true,
		containerID: "cubepod-sb-parent",
		sandboxDir:  parentDir,
	}
	docker.mu.Unlock()

	childInfo, err := docker.Fork(ctx, "sb-parent", "sb-child")
	if err != nil {
		t.Fatalf("DockerBackend.Fork failed: %v", err)
	}

	// Verify quiesce -> thaw -> docker_run ordering and maxFreezeMs=1500
	expectedEvents := []string{"quiesce", "thaw", "docker_run"}
	if !slices.Equal(events, expectedEvents) {
		t.Fatalf("unexpected call order: got %v, want %v", events, expectedEvents)
	}
	if got, ok := quiesceReq["maxFreezeMs"].(float64); !ok || int(got) != 1500 {
		t.Fatalf("expected quiesce maxFreezeMs=1500, got %v", quiesceReq)
	}

	// Verify child inherited workspace, rootfs, and sparse loopback .ext4 contents
	gotWs, err := os.ReadFile(filepath.Join(childInfo.WorkspaceDir, "hello.txt"))
	if err != nil || string(gotWs) != "parent-ws" {
		t.Fatalf("child workspace mismatch: %q, err=%v", string(gotWs), err)
	}
	gotRootfs, err := os.ReadFile(filepath.Join(childInfo.RootfsDir, "pkg.conf"))
	if err != nil || string(gotRootfs) != "parent-rootfs" {
		t.Fatalf("child rootfs mismatch: %q, err=%v", string(gotRootfs), err)
	}

	childExt4 := filepath.Join(baseDir, "sb-child", "workspace.ext4")
	cf, err := os.OpenFile(childExt4, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open child workspace.ext4: %v", err)
	}
	buf := make([]byte, len("EXT4-PARENT-MARKER"))
	if _, err := cf.ReadAt(buf, 4096); err != nil {
		_ = cf.Close()
		t.Fatal(err)
	}
	if string(buf) != "EXT4-PARENT-MARKER" {
		_ = cf.Close()
		t.Fatalf("child ext4 marker mismatch: got %q", string(buf))
	}

	// Mutate child files and ext4 image; verify parent remains completely isolated
	if _, err := cf.WriteAt([]byte("CHILD-MUTATED-DATA"), 4096); err != nil {
		_ = cf.Close()
		t.Fatal(err)
	}
	_ = cf.Close()

	if err := os.WriteFile(filepath.Join(childInfo.WorkspaceDir, "hello.txt"), []byte("child-only"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childInfo.RootfsDir, "child-file.txt"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}

	parentWsAfter, _ := os.ReadFile(filepath.Join(parentWs, "hello.txt"))
	if string(parentWsAfter) != "parent-ws" {
		t.Fatalf("child mutation leaked into parent workspace: %q", string(parentWsAfter))
	}
	if _, err := os.Stat(filepath.Join(parentRootfs, "child-file.txt")); !os.IsNotExist(err) {
		t.Fatalf("child rootfs file leaked into parent rootfs")
	}

	pf, err := os.Open(filepath.Join(parentDir, "workspace.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	pbuf := make([]byte, len("EXT4-PARENT-MARKER"))
	_, _ = pf.ReadAt(pbuf, 4096)
	_ = pf.Close()
	if string(pbuf) != "EXT4-PARENT-MARKER" {
		t.Fatalf("child ext4 write mutated parent ext4: got %q", string(pbuf))
	}
}

func TestQuiesceSandbox_ThawsOnCloneError(t *testing.T) {
	ctx := context.Background()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := cloneWithQuiesce(ctx, srv.Client(), srv.URL, true, "/nonexistent-cubepod-src-dir", filepath.Join(t.TempDir(), "dst"))
	if err == nil {
		t.Fatalf("expected cloneWithQuiesce to fail for nonexistent source dir")
	}
	expected := []string{"/v1/lifecycle/quiesce", "/v1/lifecycle/thaw"}
	if !slices.Equal(calls, expected) {
		t.Fatalf("expected thaw to still execute on clone failure: got %v, want %v", calls, expected)
	}
}
