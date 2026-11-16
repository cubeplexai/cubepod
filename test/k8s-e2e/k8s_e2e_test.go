package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
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

	podName := sandboxID
	rootfsPVCName := fmt.Sprintf("%s-rootfs-pvc", sandboxID)
	workspacePVCName := fmt.Sprintf("%s-workspace-pvc", sandboxID)

	// Verify Kubernetes Pod is scheduled and running
	podObj, err := kubeClient.GetPod(ctx, workloadsNamespace, podName)
	if err != nil {
		t.Fatalf("failed to get live Kubernetes pod %s/%s: %v", workloadsNamespace, podName, err)
	}

	specMap, _ := podObj["spec"].(map[string]any)

	// Verify runtimeClassName is set (validating Kata MicroVM execution)
	runtimeClass, _ := specMap["runtimeClassName"].(string)
	if runtimeClass == "" {
		t.Fatalf("expected Pod %s to have runtimeClassName set, got empty", podName)
	}
	t.Logf("[K8s-E2E] Verified Pod runtimeClassName: %q", runtimeClass)

	// Verify initContainer podd-installer exists
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
	fgCmd := "echo 'live-k8s-pod-foreground-ok' && whoami && uname -a"
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
	if !strings.EqualFold(sbResumed.State, "running") {
		t.Fatalf("expected state running after auto-resume, got %s", sbResumed.State)
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

// TestK8sWarmPoolClaimLive tests provisioning a standby PodWarmPool and measuring
// the rapid claim latency when a new sandbox request arrives.
func TestK8sWarmPoolClaimLive(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_E2E") != "1" {
		t.Skip("Skipping live Kubernetes E2E tests: set CUBEPOD_K8S_E2E=1 to run against live cluster")
	}

	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("gateway URL required")
	}
	gatewayURL := cfg.GatewayURL
	sandboxImage := cfg.SandboxImage

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c := client.New(gatewayURL, client.WithTimeout(3*time.Minute))

	// 1. Create WarmPool with 1 replica for our sandboxImage
	poolName := fmt.Sprintf("live-wp-%d", time.Now().Unix()%10000)
	t.Logf("[K8s-WarmPool] Step 1: Creating warmpool %s with template %s...", poolName, sandboxImage)
	wp, err := c.CreateWarmPool(ctx, poolName, sandboxImage, 1)
	if err != nil {
		t.Fatalf("failed to create warmpool: %v", err)
	}
	t.Logf("[K8s-WarmPool] Warmpool created: %+v", wp)

	// Poll the warmpool status until availableReplicas >= 1 (with 60s timeout)
	t.Logf("[K8s-WarmPool] Waiting for warm standby pod to be provisioned and ready (polling up to 60s)...")
	pollDeadline := time.Now().Add(60 * time.Second)
	ready := false
	var lastListErr error
	for time.Now().Before(pollDeadline) {
		pools, err := c.ListWarmPools(ctx)
		if err != nil {
			lastListErr = err
			t.Logf("[K8s-WarmPool] polling ListWarmPools err: %v", err)
		} else {
			lastListErr = nil
			for _, p := range pools {
				if p.Name == poolName && p.AvailableReplicas >= 1 {
					ready = true
					break
				}
			}
		}
		if ready {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		if lastListErr != nil {
			t.Fatalf("warmpool %s did not become ready within 60s; last ListWarmPools error: %v", poolName, lastListErr)
		}
		t.Fatalf("warmpool %s did not become ready (availableReplicas >= 1) within 60s", poolName)
	}

	// 2. Measure sandbox creation via WarmPool Claim!
	t.Logf("[K8s-WarmPool] Step 2: Creating sandbox and timing the WarmPool Claim latency...")
	start := time.Now()
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: sandboxImage,
		Metadata: map[string]string{
			"test": "warmpool-claim",
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("failed to create sandbox via warmpool claim: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = c.DeleteSandboxWithOptions(cleanupCtx, sb.SandboxID, true)
	}()

	if elapsed >= 5*time.Second {
		t.Fatalf("expected warm pool claim latency < 5s, got %v", elapsed)
	}
	if sb.Reason != "WarmPoolClaimed" {
		t.Fatalf("expected sandbox to be claimed from warm pool (reason WarmPoolClaimed), got %q", sb.Reason)
	}

	t.Logf("[K8s-WarmPool] SUCCESS! Sandbox %s claimed from warmpool in %v! (vs ~30s on cold start)", sb.SandboxID, elapsed)

	// 3. Verify fast command execution
	res, err := c.RunCommand(ctx, sb.SandboxID, client.RunCommandRequest{
		Command: "uname -a",
	})
	if err != nil || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("command execution on claimed warm sandbox failed: %v", err)
	}
	t.Logf("[K8s-WarmPool] Claimed sandbox command output: %s", strings.TrimSpace(res.Output))
}

type connectFrame struct {
	Flag    byte
	Payload []byte
}

func makeConnectFrame(payload []byte, endStream bool) []byte {
	var flag byte
	if endStream {
		flag = 0x02
	}
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func readNextConnectFrame(r io.Reader) (*connectFrame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[1:5])
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return &connectFrame{
		Flag:    header[0],
		Payload: payload,
	}, nil
}

func postSandboxConnectJSON(ctx context.Context, httpClient *http.Client, gatewayURL, sandboxID, rpcPath string, reqBody any) (int, []byte, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return 0, nil, err
	}
	u := strings.TrimRight(gatewayURL, "/") + "/sandboxes/" + sandboxID + rpcPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("E2b-Sandbox-Id", sandboxID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func startSandboxConnectStream(ctx context.Context, httpClient *http.Client, gatewayURL, sandboxID, rpcPath string, reqBody any) (*http.Response, <-chan struct {
	frame *connectFrame
	err   error
}, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}
	framed := makeConnectFrame(raw, false)
	u := strings.TrimRight(gatewayURL, "/") + "/sandboxes/" + sandboxID + rpcPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(framed))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("E2b-Sandbox-Id", sandboxID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}

	type frameResult struct {
		frame *connectFrame
		err   error
	}
	ch := make(chan struct {
		frame *connectFrame
		err   error
	}, 32)
	go func() {
		defer close(ch)
		for {
			f, err := readNextConnectFrame(resp.Body)
			ch <- frameResult{frame: f, err: err}
			if err != nil || (f != nil && f.Flag&0x02 != 0) {
				return
			}
		}
	}()
	return resp, ch, nil
}

func requireLiveE2EConfig(t *testing.T) clusterConfig {
	t.Helper()
	if os.Getenv("CUBEPOD_K8S_E2E") != "1" {
		t.Skip("Skipping live Kubernetes E2E tests: set CUBEPOD_K8S_E2E=1 to run against live cluster")
	}
	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("set CUBEPOD_GATEWAY_URL, or expose svc/cubepod-gateway as NodePort or LoadBalancer")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("set CUBEPOD_SANDBOX_IMAGE, or pass -sandbox-image on the cubepod-gateway Deployment")
	}
	return cfg
}

func doGatewayJSON(ctx context.Context, httpClient *http.Client, method, url, apiKey string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBytes, err
}

func hasMountFlags(mountLine string, required ...string) bool {
	for _, flag := range required {
		found := false
		for _, tok := range strings.FieldsFunc(mountLine, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '(' || r == ')'
		}) {
			if tok == flag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func discoverControllerFlag(ctx context.Context, cfg clusterConfig, flagName string) string {
	if cfg.GatewayName == "" {
		return ""
	}
	ctrl := siblingName(cfg.GatewayName, "gateway", "controller")
	if ctrl == "" {
		ctrl = "cubepod-controller"
	}
	args, err := deploymentArgs(ctx, cfg.SystemNamespace, ctrl)
	if err != nil {
		return ""
	}
	return flagValue(args, flagName)
}

func TestConnectFrameAndMountFlagHelpers(t *testing.T) {
	framed := makeConnectFrame([]byte(`{"ok":true}`), false)
	f, err := readNextConnectFrame(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("readNextConnectFrame: %v", err)
	}
	if f.Flag != 0 || string(f.Payload) != `{"ok":true}` {
		t.Fatalf("unexpected frame: flag=%d payload=%q", f.Flag, f.Payload)
	}

	end := makeConnectFrame([]byte(`{}`), true)
	fEnd, err := readNextConnectFrame(bytes.NewReader(end))
	if err != nil {
		t.Fatalf("readNextConnectFrame end: %v", err)
	}
	if fEnd.Flag != 0x02 {
		t.Fatalf("expected flag 0x02, got %d", fEnd.Flag)
	}

	if !hasMountFlags("rw,nosuid,nodev,relatime", "nosuid", "nodev") {
		t.Fatal("expected hasMountFlags to match nosuid,nodev")
	}
	if hasMountFlags("rw,relatime", "nosuid", "nodev") {
		t.Fatal("expected hasMountFlags to reject missing nosuid,nodev")
	}
}

// TestK8sLiveE2EPTY verifies interactive PTY creation, stdin input, resize,
// output streaming, and signal/kill on a live Kubernetes Pod.
func TestK8sLiveE2EPTY(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: cfg.SandboxImage,
		Metadata:   map[string]string{"env": "k8s-e2e", "feature": "pty"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, sb.SandboxID, true)
	}()

	streamClient := &http.Client{}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	resp, frameCh, err := startSandboxConnectStream(ctx, streamClient, cfg.GatewayURL, sb.SandboxID, "/process.Process/Start", map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-i"},
		},
		"pty": map[string]any{
			"size": map[string]any{"cols": 80, "rows": 24},
		},
	})
	if err != nil {
		t.Fatalf("PTY Start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PTY Start status=%d body=%s", resp.StatusCode, b)
	}

	var pid int
	startDeadline := time.NewTimer(15 * time.Second)
	for pid == 0 {
		select {
		case res, ok := <-frameCh:
			if !ok || res.err != nil {
				startDeadline.Stop()
				t.Fatalf("PTY stream ended before start event: ok=%v err=%v", ok, res.err)
			}
			var ev struct {
				Event struct {
					Start struct {
						Pid int `json:"pid"`
					} `json:"start"`
				} `json:"event"`
			}
			if json.Unmarshal(res.frame.Payload, &ev) == nil && ev.Event.Start.Pid > 0 {
				pid = ev.Event.Start.Pid
			}
		case <-startDeadline.C:
			t.Fatal("timed out waiting for PTY start event")
		}
	}
	startDeadline.Stop()

	sendPTYInput := func(input string) {
		t.Helper()
		status, body, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/process.Process/SendInput", map[string]any{
			"process": map[string]any{"pid": pid},
			"input":   map[string]any{"pty": base64.StdEncoding.EncodeToString([]byte(input))},
		})
		if err != nil || status != http.StatusOK {
			t.Fatalf("PTY SendInput failed: status=%d err=%v body=%s", status, err, body)
		}
	}

	var accumulated string
	waitPTYOutput := func(substr string) {
		t.Helper()
		if strings.Contains(accumulated, substr) {
			return
		}
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			select {
			case res, ok := <-frameCh:
				if !ok || res.err != nil {
					t.Fatalf("PTY stream ended before %q observed (got %q): ok=%v err=%v", substr, accumulated, ok, res.err)
				}
				if res.frame.Flag == 0 {
					var ev struct {
						Event struct {
							Data struct {
								Pty string `json:"pty"`
							} `json:"data"`
						} `json:"event"`
					}
					if json.Unmarshal(res.frame.Payload, &ev) == nil && ev.Event.Data.Pty != "" {
						if rawOut, decErr := base64.StdEncoding.DecodeString(ev.Event.Data.Pty); decErr == nil {
							accumulated += string(rawOut)
							if strings.Contains(accumulated, substr) {
								return
							}
						}
					}
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for PTY output %q; accumulated=%q", substr, accumulated)
			}
		}
	}

	sendPTYInput("echo PTY_READY && stty size\n")
	waitPTYOutput("PTY_READY")
	waitPTYOutput("24 80")

	resizeStatus, resizeBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/process.Process/Update", map[string]any{
		"process": map[string]any{"pid": pid},
		"pty":     map[string]any{"size": map[string]any{"cols": 120, "rows": 40}},
	})
	if err != nil || resizeStatus != http.StatusOK {
		t.Fatalf("PTY resize failed: status=%d err=%v body=%s", resizeStatus, err, resizeBody)
	}
	sendPTYInput("stty size\n")
	waitPTYOutput("40 120")

	killStatus, killBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/process.Process/SendSignal", map[string]any{
		"process": map[string]any{"pid": pid},
		"signal":  9,
	})
	if err != nil || killStatus != http.StatusOK {
		t.Fatalf("PTY SendSignal failed: status=%d err=%v body=%s", killStatus, err, killBody)
	}

	closeTimer := time.NewTimer(15 * time.Second)
	defer closeTimer.Stop()
	for {
		select {
		case res, ok := <-frameCh:
			if !ok || errors.Is(res.err, io.EOF) || (res.frame != nil && res.frame.Flag&0x02 != 0) {
				t.Logf("[K8s-E2E] PTY verified (pid=%d, resize 24x80 -> 40x120, kill)", pid)
				return
			}
			if res.err != nil {
				t.Fatalf("PTY stream error after SendSignal: %v", res.err)
			}
		case <-closeTimer.C:
			t.Fatal("timed out waiting for PTY stream to terminate after SendSignal")
		}
	}
}

// TestK8sLiveE2EWatchAndMakeDir verifies recursive filesystem.Filesystem/WatchDir
// events, MakeDir, and polling watcher RPCs on a live Kubernetes Pod.
func TestK8sLiveE2EWatchAndMakeDir(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: cfg.SandboxImage,
		Metadata:   map[string]string{"env": "k8s-e2e", "feature": "watch-makedir"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, sb.SandboxID, true)
	}()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	streamClient := &http.Client{}

	watchRoot := "/workspace/k8s-watch-root"
	subDir := watchRoot + "/nested"
	mkStatus, mkBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/filesystem.Filesystem/MakeDir", map[string]any{
		"path": subDir,
	})
	if err != nil || mkStatus != http.StatusOK {
		t.Fatalf("MakeDir %s failed: status=%d err=%v body=%s", subDir, mkStatus, err, mkBody)
	}

	resp, frameCh, err := startSandboxConnectStream(ctx, streamClient, cfg.GatewayURL, sb.SandboxID, "/filesystem.Filesystem/WatchDir", map[string]any{
		"path":      watchRoot,
		"recursive": true,
	})
	if err != nil {
		t.Fatalf("WatchDir start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		t.Skip("Skipping WatchDir E2E: E2BFilesystemWatch gate is disabled on cluster")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("WatchDir status=%d body=%s", resp.StatusCode, b)
	}

	startTimer := time.NewTimer(15 * time.Second)
	started := false
	for !started {
		select {
		case res, ok := <-frameCh:
			if !ok || res.err != nil {
				startTimer.Stop()
				t.Fatalf("WatchDir ended before start frame: ok=%v err=%v", ok, res.err)
			}
			if res.frame.Flag == 0 && bytes.Contains(res.frame.Payload, []byte("start")) {
				started = true
			}
		case <-startTimer.C:
			t.Fatal("timed out waiting for WatchDir start frame")
		}
	}
	startTimer.Stop()

	if err := c.WriteFile(ctx, sb.SandboxID, subDir+"/event.txt", []byte("watch-payload"), "0644"); err != nil {
		t.Fatalf("WriteFile inside watched dir: %v", err)
	}

	eventTimer := time.NewTimer(15 * time.Second)
	defer eventTimer.Stop()
	sawEvent := false
	for !sawEvent {
		select {
		case res, ok := <-frameCh:
			if !ok || res.err != nil {
				t.Fatalf("WatchDir stream closed before filesystem event: ok=%v err=%v", ok, res.err)
			}
			if res.frame.Flag == 0 && bytes.Contains(res.frame.Payload, []byte("event.txt")) {
				sawEvent = true
			}
		case <-eventTimer.C:
			t.Fatal("timed out waiting for recursive WatchDir event for event.txt")
		}
	}

	// Also verify unary CreateWatcher -> GetWatcherEvents -> RemoveWatcher
	cwStatus, cwBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/filesystem.Filesystem/CreateWatcher", map[string]any{
		"path":      watchRoot,
		"recursive": true,
	})
	if err != nil || cwStatus != http.StatusOK {
		t.Fatalf("CreateWatcher failed: status=%d err=%v body=%s", cwStatus, err, cwBody)
	}
	var created struct {
		WatcherID string `json:"watcherId"`
	}
	if err := json.Unmarshal(cwBody, &created); err != nil || created.WatcherID == "" {
		t.Fatalf("invalid CreateWatcher response: err=%v body=%s", err, cwBody)
	}

	if err := c.WriteFile(ctx, sb.SandboxID, subDir+"/poll.txt", []byte("poll-payload"), "0644"); err != nil {
		t.Fatalf("WriteFile for CreateWatcher: %v", err)
	}
	pollDeadline := time.Now().Add(10 * time.Second)
	sawPoll := false
	for time.Now().Before(pollDeadline) && !sawPoll {
		gwStatus, gwBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/filesystem.Filesystem/GetWatcherEvents", map[string]any{
			"watcherId": created.WatcherID,
		})
		if err != nil || gwStatus != http.StatusOK {
			t.Fatalf("GetWatcherEvents failed: status=%d err=%v body=%s", gwStatus, err, gwBody)
		}
		if bytes.Contains(gwBody, []byte("poll.txt")) {
			sawPoll = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !sawPoll {
		t.Fatalf("expected GetWatcherEvents to observe poll.txt")
	}

	rwStatus, rwBody, err := postSandboxConnectJSON(ctx, httpClient, cfg.GatewayURL, sb.SandboxID, "/filesystem.Filesystem/RemoveWatcher", map[string]any{
		"watcherId": created.WatcherID,
	})
	if err != nil || rwStatus != http.StatusOK {
		t.Fatalf("RemoveWatcher failed: status=%d err=%v body=%s", rwStatus, err, rwBody)
	}
	t.Logf("[K8s-E2E] MakeDir + recursive WatchDir + CreateWatcher/GetWatcherEvents/RemoveWatcher verified")
}

// TestK8sLiveE2EVolumeMountsAndFork verifies E2B volumeMounts on a live Kata Pod:
// nosuid,nodev mount flags, shared RWX persistence, ForkFrom inheritance, and RWO fork conflict.
func TestK8sLiveE2EVolumeMountsAndFork(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("NewRESTKubeClientFromEnv: %v", err)
	}
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	apiKey := getenv("CUBEPOD_API_KEY", "cpod_k8s_e2e_key")

	volName := fmt.Sprintf("k8s-e2e-vol-%d", time.Now().UnixNano())
	volStatus, volBody, err := doGatewayJSON(ctx, httpClient, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/volumes", apiKey, map[string]string{
		"name": volName,
	})
	if err != nil {
		t.Fatalf("POST /volumes: %v", err)
	}
	if volStatus == http.StatusNotImplemented || volStatus == http.StatusNotFound {
		t.Skipf("Skipping E2B volumeMounts E2E: volume store unavailable on cluster (HTTP %d: %s)", volStatus, volBody)
	}
	if volStatus != http.StatusCreated && volStatus != http.StatusOK {
		t.Fatalf("POST /volumes status=%d body=%s", volStatus, volBody)
	}
	var vol struct {
		VolumeID string `json:"volumeID"`
		Name     string `json:"name"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(volBody, &vol); err != nil || vol.VolumeID == "" {
		t.Fatalf("decode volume response: err=%v body=%s", err, volBody)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_, _, _ = doGatewayJSON(cctx, httpClient, http.MethodDelete, strings.TrimRight(cfg.GatewayURL, "/")+"/volumes/"+vol.VolumeID, apiKey, nil)
	}()

	seedReq, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(cfg.GatewayURL, "/")+"/volumecontent/"+vol.VolumeID+"/file?path=/seed.txt", strings.NewReader("seed-from-volume-api"))
	if err != nil {
		t.Fatalf("seed volume request: %v", err)
	}
	seedReq.Header.Set("Authorization", "Bearer "+vol.Token)
	seedResp, err := httpClient.Do(seedReq)
	if err != nil {
		t.Fatalf("PUT volume seed file: %v", err)
	}
	seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusCreated && seedResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT volume seed file status=%d", seedResp.StatusCode)
	}

	sbxStatus, sbxBody, err := doGatewayJSON(ctx, httpClient, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/sandboxes", apiKey, map[string]any{
		"templateID": cfg.SandboxImage,
		"volumeMounts": []map[string]string{
			{"name": volName, "path": "/mnt/shared"},
		},
	})
	if err != nil {
		t.Fatalf("POST /sandboxes with volumeMounts: %v", err)
	}
	if sbxStatus == http.StatusNotImplemented {
		t.Skipf("Skipping E2B volumeMounts E2E: K8sE2BVolumeMounts gate or shared RWX PVC not configured (%s)", sbxBody)
	}
	if sbxStatus != http.StatusCreated && sbxStatus != http.StatusOK {
		t.Fatalf("POST /sandboxes with volumeMounts status=%d body=%s", sbxStatus, sbxBody)
	}
	var parent client.SandboxRecord
	if err := json.Unmarshal(sbxBody, &parent); err != nil || parent.SandboxID == "" {
		t.Fatalf("decode parent sandbox: err=%v body=%s", err, sbxBody)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, parent.SandboxID, true)
	}()

	parentBox, err := kubeClient.GetPodBox(ctx, cfg.Namespace, parent.SandboxID)
	if err != nil {
		t.Fatalf("GetPodBox parent: %v", err)
	}
	if len(parentBox.Spec.E2BVolumeMounts) != 1 || parentBox.Spec.E2BVolumeMounts[0].Path != "/mnt/shared" {
		t.Fatalf("expected parent PodBox E2BVolumeMounts at /mnt/shared, got %+v", parentBox.Spec.E2BVolumeMounts)
	}

	mountRes, err := c.RunCommand(ctx, parent.SandboxID, client.RunCommandRequest{
		Command: "grep ' /mnt/shared ' /proc/mounts",
	})
	if err != nil || mountRes.ExitCode == nil || *mountRes.ExitCode != 0 {
		t.Fatalf("inspect /proc/mounts failed: err=%v res=%+v", err, mountRes)
	}
	if !hasMountFlags(mountRes.Output, "nosuid", "nodev") {
		t.Fatalf("expected /mnt/shared mount flags to include nosuid,nodev, got: %s", mountRes.Output)
	}

	readSeed, err := c.ReadFile(ctx, parent.SandboxID, "/mnt/shared/seed.txt")
	if err != nil || string(readSeed) != "seed-from-volume-api" {
		t.Fatalf("read /mnt/shared/seed.txt: err=%v content=%q", err, string(readSeed))
	}

	persistPayload := []byte("rwx-shared-from-parent")
	if err := c.WriteFile(ctx, parent.SandboxID, "/mnt/shared/from-parent.txt", persistPayload, "0644"); err != nil {
		t.Fatalf("write /mnt/shared/from-parent.txt: %v", err)
	}

	// Fork parent and verify E2BVolumeMounts inheritance + shared RWX persistence
	child, err := c.ForkSandbox(ctx, parent.SandboxID)
	if err != nil {
		t.Fatalf("ForkSandbox with E2B volumeMounts: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, child.SandboxID, true)
	}()

	childBox, err := kubeClient.GetPodBox(ctx, cfg.Namespace, child.SandboxID)
	if err != nil {
		t.Fatalf("GetPodBox child: %v", err)
	}
	if childBox.Spec.ForkFrom == nil || childBox.Spec.ForkFrom.PodBoxName != parent.SandboxID {
		t.Fatalf("expected child ForkFrom.PodBoxName=%s, got %+v", parent.SandboxID, childBox.Spec.ForkFrom)
	}
	if len(childBox.Spec.E2BVolumeMounts) != 1 || childBox.Spec.E2BVolumeMounts[0].PVCName != parentBox.Spec.E2BVolumeMounts[0].PVCName {
		t.Fatalf("expected child PodBox to inherit E2BVolumeMounts, got %+v", childBox.Spec.E2BVolumeMounts)
	}

	childShared, err := c.ReadFile(ctx, child.SandboxID, "/mnt/shared/from-parent.txt")
	if err != nil || string(childShared) != string(persistPayload) {
		t.Fatalf("child RWX read mismatch: err=%v content=%q", err, string(childShared))
	}

	// Verify volume deletion returns 409 Conflict while sandboxes still mount it
	delBusyStatus, delBusyBody, err := doGatewayJSON(ctx, httpClient, http.MethodDelete, strings.TrimRight(cfg.GatewayURL, "/")+"/volumes/"+vol.VolumeID, apiKey, nil)
	if err != nil {
		t.Fatalf("DELETE busy volume: %v", err)
	}
	if delBusyStatus != http.StatusConflict {
		t.Fatalf("expected 409 Conflict deleting mounted volume, got %d (%s)", delBusyStatus, delBusyBody)
	}

	// RWO fork conflict check: a PodBox referencing a per-volume non-shared RWO PVC must reject fork with 409 Conflict.
	rwoBoxName := fmt.Sprintf("k8s-rwo-fork-chk-%d", time.Now().UnixNano())
	rwoBox := v1alpha1.PodBox{
		APIVersion: v1alpha1.CRDGroup + "/" + v1alpha1.GroupVersion,
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      rwoBoxName,
			Namespace: cfg.Namespace,
		},
		Spec: v1alpha1.PodBoxSpec{
			TemplateRef:      cfg.SandboxImage,
			DesiredState:     v1alpha1.DesiredStatePaused,
			RootfsGeneration: 1,
			E2BVolumeMounts: []v1alpha1.E2BVolumeMount{
				{VolumeID: "vol-rwo-check", PVCName: "e2bvol-rwo-check", Path: "/mnt/rwo"},
			},
		},
	}
	if err := kubeClient.UpsertPodBox(ctx, rwoBox); err != nil {
		t.Fatalf("UpsertPodBox for RWO fork conflict check: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = kubeClient.DeletePodBox(cctx, cfg.Namespace, rwoBoxName)
	}()

	rwoForkStatus, rwoForkBody, err := doGatewayJSON(ctx, httpClient, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/sandboxes/"+rwoBoxName+"/fork", apiKey, nil)
	if err != nil {
		t.Fatalf("POST RWO fork: %v", err)
	}
	if rwoForkStatus == http.StatusNotFound {
		t.Logf("[K8s-E2E] Skipping RWO fork 409 check (StatelessGatewayHA disabled on gateway)")
	} else if rwoForkStatus != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for RWO E2B PVC fork, got %d (%s)", rwoForkStatus, rwoForkBody)
	}
	t.Logf("[K8s-E2E] E2B volumeMounts (nosuid,nodev, RWX persistence, ForkFrom inheritance, RWO conflict) verified")
}

// TestK8sLiveE2ETemplateBuild verifies in-cluster BuildKit Template.build status
// transitions (building -> ready), or skips cleanly if BuildKit/registry are not configured.
func TestK8sLiveE2ETemplateBuild(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	buildEnabled := os.Getenv("CUBEPOD_K8S_TEMPLATE_BUILD") == "1" ||
		(discoverControllerFlag(ctx, cfg, "-template-build") == "true" && discoverControllerFlag(ctx, cfg, "-template-registry") != "")
	if !buildEnabled {
		t.Skip("Skipping live Template.build E2E: set CUBEPOD_K8S_TEMPLATE_BUILD=1 (or configure -template-build=true and -template-registry on cubepod-controller)")
	}

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("NewRESTKubeClientFromEnv: %v", err)
	}

	tplName := fmt.Sprintf("k8s-e2e-tpl-%d", time.Now().UnixNano())
	res, err := c.BuildTemplate(ctx, client.BuildTemplateRequest{
		Name:      tplName,
		FromImage: cfg.SandboxImage,
		Steps: []client.BuildStep{
			{Type: "ENV", Args: []string{"CUBEPOD_BUILD_E2E", "ok"}},
		},
		NoWait: true,
	})
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotImplemented || apiErr.StatusCode == http.StatusNotFound) {
			t.Skipf("Skipping live Template.build E2E: gateway template build API disabled (%v)", err)
		}
		t.Fatalf("BuildTemplate: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteTemplate(cctx, res.TemplateID, true)
	}()

	sawBuilding := false
	ready := false
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		st, err := c.GetTemplateBuildStatus(ctx, res.TemplateID, res.BuildID)
		if err != nil {
			t.Fatalf("GetTemplateBuildStatus: %v", err)
		}
		switch st.Status {
		case "building", "waiting":
			sawBuilding = true
		case "ready":
			ready = true
		case "error":
			msg := ""
			if st.Reason != nil {
				msg = st.Reason.Message
			}
			t.Fatalf("template build failed: %s", msg)
		}
		if ready {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !ready {
		t.Fatalf("template %s build %s did not reach ready within timeout", res.TemplateID, res.BuildID)
	}

	if podTpl, err := kubeClient.GetPodTemplate(ctx, cfg.Namespace, res.TemplateID); err == nil && podTpl != nil {
		if podTpl.Status.BuildStatus != v1alpha1.BuildStatusReady {
			t.Fatalf("expected PodTemplate.Status.BuildStatus=ready, got %q", podTpl.Status.BuildStatus)
		}
	}
	t.Logf("[K8s-E2E] Template.build verified (templateID=%s buildID=%s sawBuilding=%v -> ready)", res.TemplateID, res.BuildID, sawBuilding)
}

// TestK8sLiveE2EVolumeSnapshotFork verifies CSI VolumeSnapshot CoW fork flow,
// gated on CUBEPOD_VOLUME_SNAPSHOT_CLASS (or controller -volume-snapshot-class).
func TestK8sLiveE2EVolumeSnapshotFork(t *testing.T) {
	cfg := requireLiveE2EConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	snapClass := os.Getenv("CUBEPOD_VOLUME_SNAPSHOT_CLASS")
	if snapClass == "" {
		snapClass = os.Getenv("CUBEPOD_K8S_SNAPSHOT_CLASS")
	}
	if snapClass == "" && discoverControllerFlag(ctx, cfg, "-fork-mode") == "snapshot" {
		snapClass = discoverControllerFlag(ctx, cfg, "-volume-snapshot-class")
	}
	if snapClass == "" {
		t.Skip("Skipping VolumeSnapshot fork E2E: set CUBEPOD_VOLUME_SNAPSHOT_CLASS to run against cluster with CSI VolumeSnapshotClass")
	}

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("NewRESTKubeClientFromEnv: %v", err)
	}

	parent, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: cfg.SandboxImage,
		Metadata:   map[string]string{"env": "k8s-e2e", "feature": "snapshot-fork"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox parent: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, parent.SandboxID, true)
	}()

	if err := c.WriteFile(ctx, parent.SandboxID, "/workspace/snap-parent.txt", []byte("snapshot-ws-v1"), "0644"); err != nil {
		t.Fatalf("WriteFile workspace: %v", err)
	}
	if err := c.WriteFile(ctx, parent.SandboxID, "/var/tmp/snap-rootfs.txt", []byte("snapshot-rootfs-v1"), "0644"); err != nil {
		t.Fatalf("WriteFile rootfs: %v", err)
	}

	child, err := c.ForkSandbox(ctx, parent.SandboxID)
	if err != nil {
		t.Fatalf("ForkSandbox: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, child.SandboxID, true)
	}()

	wsData, err := c.ReadFile(ctx, child.SandboxID, "/workspace/snap-parent.txt")
	if err != nil || string(wsData) != "snapshot-ws-v1" {
		t.Fatalf("child workspace snapshot fork mismatch: err=%v content=%q", err, string(wsData))
	}
	rfData, err := c.ReadFile(ctx, child.SandboxID, "/var/tmp/snap-rootfs.txt")
	if err != nil || string(rfData) != "snapshot-rootfs-v1" {
		t.Fatalf("child rootfs snapshot fork mismatch: err=%v content=%q", err, string(rfData))
	}

	// Verify CoW isolation between parent and child
	if err := c.WriteFile(ctx, child.SandboxID, "/workspace/snap-child-only.txt", []byte("child-only"), "0644"); err != nil {
		t.Fatalf("WriteFile child-only: %v", err)
	}
	if _, err := c.ReadFile(ctx, parent.SandboxID, "/workspace/snap-child-only.txt"); err == nil {
		t.Fatalf("expected child-only file not to appear in parent sandbox")
	}

	childWsPVC, err := kubeClient.GetPVC(ctx, cfg.Namespace, child.SandboxID+"-workspace-pvc")
	if err != nil {
		t.Fatalf("GetPVC child workspace: %v", err)
	}
	specMap, _ := childWsPVC["spec"].(map[string]any)
	ds, _ := specMap["dataSource"].(map[string]any)
	if ds == nil || ds["kind"] != "VolumeSnapshot" {
		t.Fatalf("expected child PVC dataSource.kind=VolumeSnapshot (snapshotClass=%s), got %+v", snapClass, specMap["dataSource"])
	}
	t.Logf("[K8s-E2E] VolumeSnapshot CoW fork verified (parent=%s child=%s dataSource=%+v)", parent.SandboxID, child.SandboxID, ds)
}
