package k8s_e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

// TestK8sLiveE2E executes the end-to-end verification of CubePod against a live
// Kubernetes cluster with real persistent volumes and container execution.
func TestK8sLiveE2E(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_E2E") != "1" {
		t.Skip("Skipping live Kubernetes E2E tests: set CUBEPOD_K8S_E2E=1 to run against live cluster")
	}

	gatewayURL := os.Getenv("CUBEPOD_GATEWAY_URL")
	if gatewayURL == "" {
		gatewayURL = "http://192.168.1.207:32380"
	}

	workloadsNamespace := os.Getenv("CUBEPOD_WORKLOADS_NAMESPACE")
	if workloadsNamespace == "" {
		workloadsNamespace = "cubepod-workloads"
	}

	sandboxImage := os.Getenv("CUBEPOD_SANDBOX_IMAGE")
	if sandboxImage == "" {
		sandboxImage = "hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 1. Initialize API client and Kubernetes REST client
	c := client.New(gatewayURL, client.WithTimeout(60*time.Second))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("failed to initialize RESTKubeClient from environment/kubeconfig: %v", err)
	}

	health, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("gateway health check failed at %s: %v", gatewayURL, err)
	}
	t.Logf("[K8s-E2E] Gateway is healthy: %v", health)

	// Step a: Create Sandbox and verify real Kubernetes Pod & Bound PVCs
	t.Logf("[K8s-E2E] Step a: Creating sandbox with template %s...", sandboxImage)
	createReq := client.CreateSandboxRequest{
		TemplateID: sandboxImage,
		Metadata: map[string]string{
			"env":  "k8s-e2e",
			"tier": "live-test",
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

	// Verify 2 Bound PVCs (rootfs-pvc and workspace-pvc)
	rfPVC, err := kubeClient.GetPVC(ctx, workloadsNamespace, rootfsPVCName)
	if err != nil {
		t.Fatalf("failed to get rootfs PVC %s: %v", rootfsPVCName, err)
	}
	if rfStatus, ok := rfPVC["status"].(map[string]any); !ok || rfStatus["phase"] != "Bound" {
		t.Fatalf("expected rootfs PVC %s to be Bound, got: %+v", rootfsPVCName, rfPVC["status"])
	}

	wsPVC, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
	if err != nil {
		t.Fatalf("failed to get workspace PVC %s: %v", workspacePVCName, err)
	}
	if wsStatus, ok := wsPVC["status"].(map[string]any); !ok || wsStatus["phase"] != "Bound" {
		t.Fatalf("expected workspace PVC %s to be Bound, got: %+v", workspacePVCName, wsPVC["status"])
	}
	t.Logf("[K8s-E2E] Verified dual Bound PVCs: %s and %s", rootfsPVCName, workspacePVCName)

	// Step b: Foreground and background command execution and cursor log polling
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

	// Step c: File write/read/list/delete and archive-sync on /workspace
	t.Logf("[K8s-E2E] Step c: Testing file operations on persistent /workspace...")
	testFilePath := "/workspace/k8s-e2e-data.txt"
	testFileContent := []byte("persistent-data-saved-on-nfs-workspace")
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
	t.Logf("[K8s-E2E] Step d: Testing Pause (Pod deleted, PVCs retained)...")
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

	// Verify workspace PVC is still intact and Bound
	wsPVCPaused, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
	if err != nil {
		t.Fatalf("expected workspace PVC %s to persist across Pause, got error: %v", workspacePVCName, err)
	}
	if statusMap, ok := wsPVCPaused["status"].(map[string]any); !ok || statusMap["phase"] != "Bound" {
		t.Fatalf("workspace PVC %s is not Bound during Pause: %+v", workspacePVCName, wsPVCPaused["status"])
	}
	t.Logf("[K8s-E2E] Verified workspace PVC persists intact while paused")

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
	t.Logf("[K8s-E2E] Auto-resume verified! New Pod running, AutoResumeCount=%d, /workspace data intact", sbResumed.AutoResumeCount)

	// Step e: ResetRootFS (rootfsGeneration 1 -> 2, workspace PVC intact)
	t.Logf("[K8s-E2E] Step e: Testing ResetRootFS (rootfsGeneration 1 -> 2)...")
	// Write ephemeral data into rootfs outside /workspace
	ephemeralPath := "/tmp/ephemeral-k8s-rootfs.txt"
	if err := c.WriteFile(ctx, sandboxID, ephemeralPath, []byte("ephemeral-rootfs-payload"), "0644"); err != nil {
		t.Fatalf("failed to write ephemeral file: %v", err)
	}

	resetRecord, err := c.ResetRootFS(ctx, sandboxID)
	if err != nil {
		t.Fatalf("failed to ResetRootFS: %v", err)
	}
	if resetRecord.RootfsGeneration != 2 {
		t.Fatalf("expected rootfsGeneration=2 after reset, got %d", resetRecord.RootfsGeneration)
	}

	// Verify persistent /workspace file remains intact after RootFS reset!
	persistedData, err := c.ReadFile(ctx, sandboxID, testFilePath)
	if err != nil {
		t.Fatalf("failed to read /workspace file after ResetRootFS: %v", err)
	}
	if string(persistedData) != string(testFileContent) {
		t.Fatalf("workspace data corrupted across RootFS reset: expected %q, got %q", string(testFileContent), string(persistedData))
	}

	// Verify ephemeral file in RootFS is gone
	_, err = c.ReadFile(ctx, sandboxID, ephemeralPath)
	if err == nil {
		t.Fatalf("expected ephemeral file %s to be wiped out after RootFS reset, but it still exists", ephemeralPath)
	}
	t.Logf("[K8s-E2E] ResetRootFS verified! /workspace data preserved intact, ephemeral rootfs wiped")

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
	for i := 0; i < 40; i++ {
		_, err := kubeClient.GetPVC(ctx, workloadsNamespace, workspacePVCName)
		if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")) {
			wsCleaned = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !wsCleaned {
		t.Fatalf("expected workspace PVC %s to be deleted after teardown", workspacePVCName)
	}

	t.Logf("[K8s-E2E] Teardown verified! Pod and PVCs cleanly removed from %s", workloadsNamespace)
}
