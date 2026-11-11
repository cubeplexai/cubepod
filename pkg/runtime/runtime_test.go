package runtime

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
