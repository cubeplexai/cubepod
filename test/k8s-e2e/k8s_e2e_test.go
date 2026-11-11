package k8s_e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

// InitState models the in-guest initialization marker written to /run/cubepod/init-state.json.
type InitState struct {
	Pivoted         bool   `json:"pivoted"`
	RootfsDevice    string `json:"rootfs_device"`
	RootfsAction    string `json:"rootfs_action"`
	WorkspaceDevice string `json:"workspace_device"`
	WorkspaceAction string `json:"workspace_action"`
	DurationMs      uint64 `json:"duration_ms"`
}

// TestK8sLiveE2E runs the CubePod sandbox lifecycle against whatever cluster
// the current kubeconfig and deploy/k8s/env.local point at. Gateway URL, sandbox
// image, namespace, and storage class are not tied to one environment.
func TestK8sLiveE2E(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_E2E") != "1" {
		t.Skip("Skipping live Kubernetes E2E tests: set CUBEPOD_K8S_E2E=1 to run against live cluster")
	}

	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("set CUBEPOD_GATEWAY_URL, or expose svc/cubepod-gateway as NodePort or LoadBalancer. For NodePort set CUBEPOD_NODE_IP when no Ready node has an InternalIP")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("set CUBEPOD_SANDBOX_IMAGE, or pass -sandbox-image on the cubepod-gateway Deployment")
	}
	gatewayURL := cfg.GatewayURL
	workloadsNamespace := cfg.Namespace
	sandboxImage := cfg.SandboxImage

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Longhorn V2 attach plus ext4 format can outlast the default minute.
	c := client.New(gatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("failed to initialize RESTKubeClient from environment/kubeconfig: %v", err)
	}

	health, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("gateway health check failed at %s: %v", gatewayURL, err)
	}
	t.Logf("[K8s-E2E] Gateway is healthy: %v", health)

	// Step a: Create Sandbox and verify real Kubernetes Pod, volumeDevices, & Bound Block PVCs
	t.Logf("[K8s-E2E] Step a: Creating sandbox with template %s...", sandboxImage)
	createReq := client.CreateSandboxRequest{
		TemplateID: sandboxImage,
		Metadata: map[string]string{
			"env":  "k8s-e2e",
			"tier": "live-block-csi",
		},
	}
	sb, err := c.CreateSandbox(ctx, createReq)
	if err != nil {
		t.Fatalf("failed to create sandbox: %v", err)
	}
	sandboxID := sb.SandboxID
	t.Logf("[K8s-E2E] Sandbox created: ID=%s, State=%s, EnvdPort=%d", sandboxID, sb.State, sb.EnvdPort)

	// Register teardown to ensure cleanup even if assertions fail
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = c.DeleteSandboxWithOptions(cleanupCtx, sandboxID, true)
	}()

	podName := fmt.Sprintf("%s-kata-pod", sandboxID)
	rootfsPVCName := fmt.Sprintf("%s-rootfs-pvc", sandboxID)
	workspacePVCName := fmt.Sprintf("%s-workspace-pvc", sandboxID)

	// Verify Kubernetes Pod is scheduled and running
	podObj, err := kubeClient.GetPod(ctx, workloadsNamespace, podName)
	if err != nil {
		t.Fatalf("failed to get live Kubernetes pod %s/%s: %v", workloadsNamespace, podName, err)
	}

	// Verify initContainer podd-installer exists
	specMap, _ := podObj["spec"].(map[string]any)
	initContainers, _ := specMap["initContainers"].([]any)
	foundPoddInstaller := false
	for _, ic := range initContainers {
		if icMap, ok := ic.(map[string]any); ok {
			if icMap["name"] == "podd-installer" {
				foundPoddInstaller = true
				break
			}
		}
	}
	if !foundPoddInstaller {
		t.Fatalf("expected initContainer 'podd-installer' in Pod spec, got: %+v", initContainers)
	}
	t.Logf("[K8s-E2E] Verified initContainer 'podd-installer' exists in Pod %s", podName)

	// Verify volumeDevices on sandbox container (/dev/cubepod-rootfs and /dev/cubepod-workspace)
	containers, _ := specMap["containers"].([]any)
	foundRootfsDev := false
	foundWorkspaceDev := false
	for _, cObj := range containers {
		if cMap, ok := cObj.(map[string]any); ok && cMap["name"] == "sandbox" {
			if vdevs, ok := cMap["volumeDevices"].([]any); ok {
				for _, vd := range vdevs {
					if vdMap, ok := vd.(map[string]any); ok {
						if vdMap["devicePath"] == "/dev/cubepod-rootfs" {
							foundRootfsDev = true
						}
						if vdMap["devicePath"] == "/dev/cubepod-workspace" {
							foundWorkspaceDev = true
						}
					}
				}
			}
		}
	}
	if !foundRootfsDev || !foundWorkspaceDev {
		t.Fatalf("expected volumeDevices /dev/cubepod-rootfs and /dev/cubepod-workspace in Pod spec, got rootfs=%v, workspace=%v", foundRootfsDev, foundWorkspaceDev)
	}
	t.Logf("[K8s-E2E] Verified volumeDevices (/dev/cubepod-rootfs and /dev/cubepod-workspace) attached to sandbox container")

	// Verify 2 Bound PVCs (rootfs-pvc and workspace-pvc) with volumeMode: Block.
	rfPVC, err := kubeClient.GetPVC(ctx, workloadsNamespace, rootfsPVCName)
	if err != nil {
		t.Fatalf("failed to get rootfs PVC %s: %v", rootfsPVCName, err)
	}
	if rfStatus, ok := rfPVC["status"].(map[string]any); !ok || rfStatus["phase"] != "Bound" {
		t.Fatalf("expected rootfs PVC %s to be Bound, got: %+v", rootfsPVCName, rfPVC["status"])
	}
	rfSpec, _ := rfPVC["spec"].(map[string]any)
	if vm, _ := rfSpec["volumeMode"].(string); vm != "Block" {
		t.Fatalf("expected rootfs PVC volumeMode Block, got %v", vm)
	}

	wsPVC, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
	if err != nil {
		t.Fatalf("failed to get workspace PVC %s: %v", workspacePVCName, err)
	}
	if wsStatus, ok := wsPVC["status"].(map[string]any); !ok || wsStatus["phase"] != "Bound" {
		t.Fatalf("expected workspace PVC %s to be Bound, got: %+v", workspacePVCName, wsPVC["status"])
	}
	wsSpec, _ := wsPVC["spec"].(map[string]any)
	if vm, _ := wsSpec["volumeMode"].(string); vm != "Block" {
		t.Fatalf("expected workspace PVC volumeMode Block, got %v", vm)
	}
	if cfg.StorageClass != "" {
		if got, _ := rfSpec["storageClassName"].(string); got != cfg.StorageClass {
			t.Fatalf("expected rootfs PVC storageClass %s, got %q", cfg.StorageClass, got)
		}
		if got, _ := wsSpec["storageClassName"].(string); got != cfg.StorageClass {
			t.Fatalf("expected workspace PVC storageClass %s, got %q", cfg.StorageClass, got)
		}
	}
	t.Logf("[K8s-E2E] Verified dual Bound Block PVCs %s and %s (class=%v)", rootfsPVCName, workspacePVCName, rfSpec["storageClassName"])

	// Step b: Verify /run/cubepod/init-state.json inside sandbox reports pivoted=true & rootfs_action=formatted
	t.Logf("[K8s-E2E] Step b: Verifying in-guest init-state.json (OverlayFS pivot & block formatting)...")
	initStateBytes, err := c.ReadFile(ctx, sandboxID, "/run/cubepod/init-state.json")
	if err != nil {
		t.Fatalf("failed to read /run/cubepod/init-state.json: %v", err)
	}
	var initState InitState
	if err := json.Unmarshal(initStateBytes, &initState); err != nil {
		t.Fatalf("failed to parse init-state.json: %v (raw: %s)", err, string(initStateBytes))
	}
	if !initState.Pivoted {
		t.Fatalf("expected initState.Pivoted=true, got false: %+v", initState)
	}
	if initState.RootfsAction != "formatted" {
		t.Fatalf("expected initState.RootfsAction='formatted', got %q: %+v", initState.RootfsAction, initState)
	}
	if initState.WorkspaceAction != "formatted" {
		t.Fatalf("expected initState.WorkspaceAction='formatted', got %q: %+v", initState.WorkspaceAction, initState)
	}
	t.Logf("[K8s-E2E] Verified init-state.json: pivoted=%v, rootfs_action=%s, workspace_action=%s, duration=%dms",
		initState.Pivoted, initState.RootfsAction, initState.WorkspaceAction, initState.DurationMs)

	// Foreground and background command execution and cursor log polling
	t.Logf("[K8s-E2E] Step b: Testing foreground command execution...")
	fgCmd := "echo 'live-k8s-pod-foreground-ok' && whoami && uname -m"
	fgRes, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{Command: fgCmd})
	if err != nil {
		t.Fatalf("failed to run foreground command: %v", err)
	}
	if fgRes.ExitCode == nil || *fgRes.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %+v, output: %s", fgRes.ExitCode, fgRes.Output)
	}
	if !strings.Contains(fgRes.Output, "live-k8s-pod-foreground-ok") {
		t.Fatalf("expected output to contain 'live-k8s-pod-foreground-ok', got: %s", fgRes.Output)
	}
	t.Logf("[K8s-E2E] Foreground command succeeded with output: %s", strings.TrimSpace(fgRes.Output))

	t.Logf("[K8s-E2E] Step b: Testing background command with cursor log polling...")
	bgCmd := "for i in $(seq 1 4); do echo step-$i; sleep 0.1; done"
	bgRes, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
		Command:    bgCmd,
		Background: true,
	})
	if err != nil {
		t.Fatalf("failed to spawn background command: %v", err)
	}
	if bgRes.ID == "" {
		t.Fatalf("expected background command ID, got empty")
	}

	cursor := 0
	accumulatedOutput := ""
	pollDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(pollDeadline) {
		logs, err := c.GetCommandLogs(ctx, sandboxID, bgRes.ID, cursor)
		if err != nil {
			t.Fatalf("failed to get command logs: %v", err)
		}
		if logs.Content != "" {
			accumulatedOutput += logs.Content
			cursor = logs.Cursor
		}
		if !logs.Running {
			if logs.ExitCode == nil || *logs.ExitCode != 0 {
				t.Fatalf("background command exited with non-zero: %+v, output: %s", logs.ExitCode, accumulatedOutput)
			}
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !strings.Contains(accumulatedOutput, "step-1") || !strings.Contains(accumulatedOutput, "step-4") {
		t.Fatalf("background command output missing expected steps, got: %s", accumulatedOutput)
	}
	t.Logf("[K8s-E2E] Background command finished and verified with cursor logs: %s", strings.TrimSpace(accumulatedOutput))

	// Step c: Write persistent files to RootFS upperdir AND /workspace
	t.Logf("[K8s-E2E] Step c: Writing test files to RootFS upperdir and /workspace...")
	rootfsPersistPath := "/var/tmp/rootfs-persisted.txt"
	rootfsPersistContent := []byte("persistent-data-saved-on-block-rootfs-upperdir")
	if err := c.WriteFile(ctx, sandboxID, rootfsPersistPath, rootfsPersistContent, "0644"); err != nil {
		t.Fatalf("failed to write %s to RootFS upperdir: %v", rootfsPersistPath, err)
	}

	testFilePath := "/workspace/k8s-e2e-data.txt"
	testFileContent := []byte("persistent-data-saved-on-block-workspace")
	if err := c.WriteFile(ctx, sandboxID, testFilePath, testFileContent, "0644"); err != nil {
		t.Fatalf("failed to write file %s: %v", testFilePath, err)
	}

	readContent, err := c.ReadFile(ctx, sandboxID, testFilePath)
	if err != nil {
		t.Fatalf("failed to read file %s: %v", testFilePath, err)
	}
	if string(readContent) != string(testFileContent) {
		t.Fatalf("file content mismatch: expected %q, got %q", string(testFileContent), string(readContent))
	}

	readRootfsContent, err := c.ReadFile(ctx, sandboxID, rootfsPersistPath)
	if err != nil {
		t.Fatalf("failed to read rootfs file %s: %v", rootfsPersistPath, err)
	}
	if string(readRootfsContent) != string(rootfsPersistContent) {
		t.Fatalf("rootfs file content mismatch: expected %q, got %q", string(rootfsPersistContent), string(readRootfsContent))
	}

	filesList, err := c.ListFiles(ctx, sandboxID, "/workspace")
	if err != nil {
		t.Fatalf("failed to list files in /workspace: %v", err)
	}
	foundTestFile := false
	for _, f := range filesList {
		if strings.HasSuffix(f.Path, "k8s-e2e-data.txt") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Fatalf("expected %s in /workspace file list, got: %+v", testFilePath, filesList)
	}

	// ArchiveSync tar.gz multi-file sync
	t.Logf("[K8s-E2E] Step c: Testing ArchiveSync tar.gz extraction...")
	syncFiles := map[string][]byte{
		"sync1.txt":     []byte("archive-sync-file-1"),
		"sub/sync2.txt": []byte("archive-sync-file-2"),
	}
	if err := c.ArchiveSync(ctx, sandboxID, "/workspace/synced", syncFiles); err != nil {
		t.Fatalf("ArchiveSync failed: %v", err)
	}
	sync1Data, err := c.ReadFile(ctx, sandboxID, "/workspace/synced/sync1.txt")
	if err != nil || string(sync1Data) != "archive-sync-file-1" {
		t.Fatalf("ArchiveSync file 1 verification failed: err=%v, content=%q", err, string(sync1Data))
	}
	sync2Data, err := c.ReadFile(ctx, sandboxID, "/workspace/synced/sub/sync2.txt")
	if err != nil || string(sync2Data) != "archive-sync-file-2" {
		t.Fatalf("ArchiveSync file 2 verification failed: err=%v, content=%q", err, string(sync2Data))
	}

	if err := c.DeleteFile(ctx, sandboxID, "/workspace/synced/sync1.txt"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	t.Logf("[K8s-E2E] File write/read/list/archive-sync/delete verified successfully")

	// Step d: Pause followed by Auto-Resume
	t.Logf("[K8s-E2E] Step d: Testing Pause (Pod deleted, Block PVCs retained)...")
	pausedRecord, err := c.PauseSandbox(ctx, sandboxID)
	if err != nil {
		t.Fatalf("failed to pause sandbox: %v", err)
	}
	if pausedRecord.State != "Paused" {
		t.Fatalf("expected state Paused, got %s", pausedRecord.State)
	}

	// Wait for Pod to be deleted from Kubernetes cluster
	podDeleted := false
	for i := 0; i < 40; i++ {
		_, err := kubeClient.GetPod(ctx, workloadsNamespace, podName)
		if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")) {
			podDeleted = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !podDeleted {
		t.Fatalf("expected Pod %s to be terminated after Pause, but still exists", podName)
	}
	t.Logf("[K8s-E2E] Verified compute Pod %s terminated on Pause", podName)

	// Verify workspace and rootfs PVCs are still intact and Bound
	wsPVCPaused, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
	if err != nil {
		t.Fatalf("expected workspace PVC %s to persist across Pause, got error: %v", workspacePVCName, err)
	}
	if statusMap, ok := wsPVCPaused["status"].(map[string]any); !ok || statusMap["phase"] != "Bound" {
		t.Fatalf("workspace PVC %s is not Bound during Pause: %+v", workspacePVCName, wsPVCPaused["status"])
	}

	rfPVCPaused, err := kubeClient.GetPVC(ctx, workloadsNamespace, rootfsPVCName)
	if err != nil {
		t.Fatalf("expected rootfs PVC %s to persist across Pause, got error: %v", rootfsPVCName, err)
	}
	if statusMap, ok := rfPVCPaused["status"].(map[string]any); !ok || statusMap["phase"] != "Bound" {
		t.Fatalf("rootfs PVC %s is not Bound during Pause: %+v", rootfsPVCName, rfPVCPaused["status"])
	}
	t.Logf("[K8s-E2E] Verified dual Block PVCs persist intact while paused")
	time.Sleep(2 * time.Second)

	// Auto-Resume: access data on paused sandbox, triggering automatic resurrection
	t.Logf("[K8s-E2E] Step d: Triggering Auto-Resume via data access on paused sandbox...")
	resumedData, err := c.ReadFile(ctx, sandboxID, testFilePath)
	if err != nil {
		t.Fatalf("auto-resume failed to read %s: %v", testFilePath, err)
	}
	if string(resumedData) != string(testFileContent) {
		t.Fatalf("auto-resume workspace content mismatch: expected %q, got %q", string(testFileContent), string(resumedData))
	}

	// Verify sandbox is Running and AutoResumeCount incremented
	sbResumed, err := c.GetSandbox(ctx, sandboxID)
	if err != nil {
		t.Fatalf("failed to get sandbox after auto-resume: %v", err)
	}
	if sbResumed.State != "Running" {
		t.Fatalf("expected state Running after auto-resume, got %s", sbResumed.State)
	}
	if sbResumed.AutoResumeCount < 1 {
		t.Fatalf("expected AutoResumeCount >= 1, got %d", sbResumed.AutoResumeCount)
	}

	// Verify init-state.json shows pivoted=true and rootfs_action='mounted' or 'healed'
	resumedStateBytes, err := c.ReadFile(ctx, sandboxID, "/run/cubepod/init-state.json")
	if err != nil {
		t.Fatalf("failed to read /run/cubepod/init-state.json after auto-resume: %v", err)
	}
	var resumedState InitState
	if err := json.Unmarshal(resumedStateBytes, &resumedState); err != nil {
		t.Fatalf("failed to parse init-state.json after auto-resume: %v", err)
	}
	if !resumedState.Pivoted {
		t.Fatalf("expected pivoted=true after auto-resume, got false: %+v", resumedState)
	}
	if resumedState.RootfsAction != "mounted" && resumedState.RootfsAction != "healed" {
		t.Fatalf("expected rootfs_action 'mounted' or 'healed' after auto-resume, got %q: %+v", resumedState.RootfsAction, resumedState)
	}
	t.Logf("[K8s-E2E] Auto-resume verified! New Pod running, AutoResumeCount=%d, init-state: rootfs=%s, ws=%s",
		sbResumed.AutoResumeCount, resumedState.RootfsAction, resumedState.WorkspaceAction)

	// Verify BOTH the RootFS persistent file AND /workspace file persisted across Pause & Auto-Resume!
	resumedRootfsFile, err := c.ReadFile(ctx, sandboxID, rootfsPersistPath)
	if err != nil {
		t.Fatalf("failed to read RootFS persisted file %s after auto-resume: %v", rootfsPersistPath, err)
	}
	if string(resumedRootfsFile) != string(rootfsPersistContent) {
		t.Fatalf("RootFS persistent file corrupted after auto-resume: expected %q, got %q", string(rootfsPersistContent), string(resumedRootfsFile))
	}
	t.Logf("[K8s-E2E] RootFS persistence verified across scale-to-zero! %s survived with %q", rootfsPersistPath, string(resumedRootfsFile))

	// Step e: ResetRootFS (rootfsGeneration 1 -> 2: wipes RootFS file, preserves /workspace)
	t.Logf("[K8s-E2E] Step e: Testing ResetRootFS (rootfsGeneration 1 -> 2)...")
	resetRecord, err := c.ResetRootFS(ctx, sandboxID)
	if err != nil {
		t.Fatalf("failed to ResetRootFS: %v", err)
	}
	if resetRecord.RootfsGeneration != 2 {
		t.Fatalf("expected rootfsGeneration=2 after reset, got %d", resetRecord.RootfsGeneration)
	}

	// Verify persistent /workspace file remains 100% intact after RootFS reset!
	persistedData, err := c.ReadFile(ctx, sandboxID, testFilePath)
	if err != nil {
		t.Fatalf("failed to read /workspace file after ResetRootFS: %v", err)
	}
	if string(persistedData) != string(testFileContent) {
		t.Fatalf("workspace data corrupted across RootFS reset: expected %q, got %q", string(testFileContent), string(persistedData))
	}

	// Verify the RootFS persistent file outside /workspace is WIPED on the fresh RootFS block volume!
	_, err = c.ReadFile(ctx, sandboxID, rootfsPersistPath)
	if err == nil {
		t.Fatalf("expected RootFS file %s to be wiped out after RootFS reset, but it still exists", rootfsPersistPath)
	}
	t.Logf("[K8s-E2E] ResetRootFS verified! /workspace data preserved intact, RootFS upperdir wiped")

	// Verify fresh init-state on new RootFS generation reports rootfs_action='formatted'
	resetStateBytes, err := c.ReadFile(ctx, sandboxID, "/run/cubepod/init-state.json")
	if err != nil {
		t.Fatalf("failed to read init-state.json after ResetRootFS: %v", err)
	}
	var resetState InitState
	if err := json.Unmarshal(resetStateBytes, &resetState); err != nil {
		t.Fatalf("failed to parse init-state.json after ResetRootFS: %v", err)
	}
	if !resetState.Pivoted {
		t.Fatalf("expected pivoted=true after ResetRootFS, got: %+v", resetState)
	}
	t.Logf("[K8s-E2E] Fresh RootFS generation 2 verified: pivoted=%v, rootfs_action=%s, ws_action=%s",
		resetState.Pivoted, resetState.RootfsAction, resetState.WorkspaceAction)

	// Step f: Teardown (DELETE /sandboxes/{id}?deleteVolumes=true)
	t.Logf("[K8s-E2E] Step f: Testing Teardown with volume deletion...")
	if err := c.DeleteSandboxWithOptions(ctx, sandboxID, true); err != nil {
		t.Fatalf("failed to delete sandbox with volumes: %v", err)
	}

	// Verify Pod and PVCs are cleaned up
	podCleaned := false
	for i := 0; i < 40; i++ {
		_, err := kubeClient.GetPod(ctx, workloadsNamespace, podName)
		if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")) {
			podCleaned = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !podCleaned {
		t.Fatalf("expected Pod %s to be deleted after teardown", podName)
	}

	wsCleaned := false
	for i := 0; i < 90; i++ {
		_, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
		if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")) {
			wsCleaned = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !wsCleaned {
		t.Fatalf("expected workspace PVC %s to be deleted after teardown", workspacePVCName)
	}

	t.Logf("[K8s-E2E] Teardown verified! Pod and Block PVCs cleanly removed from %s", workloadsNamespace)
}
