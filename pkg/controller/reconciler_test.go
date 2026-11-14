package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

func sampleTemplate() v1alpha1.PodTemplateSpec {
	return v1alpha1.PodTemplateSpec{
		RuntimeClassName: "kata-qemu",
		Image:            "cubeplex/sandbox-ubuntu24:latest",
		PoddImage:        "cubepod-podd:v0.1.0",
		Resources: v1alpha1.ResourceRequirements{
			CPURequest:    "2",
			CPULimit:      "4",
			MemoryRequest: "4Gi",
			MemoryLimit:   "8Gi",
		},
		Storage: v1alpha1.StorageSpec{
			StorageClassName: "mayastor-nvmeof",
			RootfsSize:       "15Gi",
			WorkspaceSize:    "30Gi",
		},
		Egress: v1alpha1.EgressConfig{
			Enabled:     false,
			Image:       "cubepod-egress:v0.1.0",
			ExchangeURL: "https://egress.cubepod.internal:8443",
		},
	}
}

// 1. Tests for BuildBlockPVCs
func TestBuildBlockPVCs_NormalAndDefaults(t *testing.T) {
	// With empty template defaults
	boxName := "test-box-1"
	emptyTpl := v1alpha1.PodTemplateSpec{}
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
	}

	rootfsPVC, wsPVC := controller.BuildBlockPVCs(boxName, emptyTpl, spec)

	if rootfsPVC.Name != "test-box-1-rootfs-pvc" {
		t.Fatalf("expected rootfs PVC name test-box-1-rootfs-pvc, got %s", rootfsPVC.Name)
	}
	if rootfsPVC.VolumeMode != "Block" {
		t.Fatalf("expected rootfs volumeMode Block, got %s", rootfsPVC.VolumeMode)
	}
	if rootfsPVC.StorageClassName != "standard" {
		t.Fatalf("expected default storageClass standard, got %s", rootfsPVC.StorageClassName)
	}
	if rootfsPVC.Size != "10Gi" {
		t.Fatalf("expected default rootfs size 10Gi, got %s", rootfsPVC.Size)
	}
	if rootfsPVC.DataSourcePVC != "" {
		t.Fatalf("expected empty DataSourcePVC, got %s", rootfsPVC.DataSourcePVC)
	}

	if wsPVC.Name != "test-box-1-workspace-pvc" {
		t.Fatalf("expected workspace PVC name test-box-1-workspace-pvc, got %s", wsPVC.Name)
	}
	if wsPVC.VolumeMode != "Block" {
		t.Fatalf("expected workspace volumeMode Block, got %s", wsPVC.VolumeMode)
	}
	if wsPVC.Size != "20Gi" {
		t.Fatalf("expected default workspace size 20Gi, got %s", wsPVC.Size)
	}
	if wsPVC.DataSourcePVC != "" {
		t.Fatalf("expected empty DataSourcePVC for workspace, got %s", wsPVC.DataSourcePVC)
	}
}

func TestBuildBlockPVCs_CustomStorageAndGeneration(t *testing.T) {
	boxName := "custom-box"
	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 3,
	}

	rootfsPVC, wsPVC := controller.BuildBlockPVCs(boxName, tpl, spec)

	if rootfsPVC.Name != "custom-box-rootfs-gen3-pvc" {
		t.Fatalf("expected rootfs PVC name custom-box-rootfs-gen3-pvc, got %s", rootfsPVC.Name)
	}
	if rootfsPVC.StorageClassName != "mayastor-nvmeof" {
		t.Fatalf("expected storageClass mayastor-nvmeof, got %s", rootfsPVC.StorageClassName)
	}
	if rootfsPVC.Size != "15Gi" {
		t.Fatalf("expected rootfs size 15Gi, got %s", rootfsPVC.Size)
	}

	// Workspace PVC must NOT have gen in name
	if wsPVC.Name != "custom-box-workspace-pvc" {
		t.Fatalf("expected workspace PVC name custom-box-workspace-pvc, got %s", wsPVC.Name)
	}
	if wsPVC.Size != "30Gi" {
		t.Fatalf("expected workspace size 30Gi, got %s", wsPVC.Size)
	}
}

func TestBuildBlockPVCs_ForkFrom(t *testing.T) {
	parentName := "parent-sandbox-42"
	childName := "child-sandbox-99"
	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: parentName},
	}

	rootfsPVC, wsPVC := controller.BuildBlockPVCs(childName, tpl, spec)

	if rootfsPVC.DataSourcePVC != "parent-sandbox-42-rootfs-pvc" {
		t.Fatalf("expected rootfs DataSourcePVC parent-sandbox-42-rootfs-pvc, got %s", rootfsPVC.DataSourcePVC)
	}
	if wsPVC.DataSourcePVC != "parent-sandbox-42-workspace-pvc" {
		t.Fatalf("expected workspace DataSourcePVC parent-sandbox-42-workspace-pvc, got %s", wsPVC.DataSourcePVC)
	}
}

// 2. Tests for BuildKataPod
func TestBuildKataPod_RuntimeInjectionAndDevices(t *testing.T) {
	boxName := "kata-box"
	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
	}
	status := v1alpha1.PodBoxStatus{
		PodName:          "kata-box-kata-pod",
		RootfsPVCName:    "kata-box-rootfs-pvc",
		WorkspacePVCName: "kata-box-workspace-pvc",
	}

	pod := controller.BuildKataPod(boxName, tpl, spec, status)

	// RuntimeClass check
	if pod.RuntimeClassName != "kata-qemu" {
		t.Fatalf("expected RuntimeClassName kata-qemu, got %s", pod.RuntimeClassName)
	}
	if pod.Name != "kata-box-kata-pod" {
		t.Fatalf("expected Pod name kata-box-kata-pod, got %s", pod.Name)
	}

	// Volumes check: must have emptyDir for /opt/cubepod/bin and PVCs for rootfs and workspace
	var foundBinVol, foundRootfsVol, foundWsVol bool
	for _, vol := range pod.Volumes {
		if vol.Name == "cubepod-bin" && vol.EmptyDir {
			foundBinVol = true
		}
		if vol.Name == "rootfs-volume" && vol.PersistentVolumeClaim == "kata-box-rootfs-pvc" {
			foundRootfsVol = true
		}
		if vol.Name == "workspace-volume" && vol.PersistentVolumeClaim == "kata-box-workspace-pvc" {
			foundWsVol = true
		}
	}
	if !foundBinVol {
		t.Fatal("expected emptyDir volume cubepod-bin")
	}
	if !foundRootfsVol {
		t.Fatal("expected PVC volume rootfs-volume")
	}
	if !foundWsVol {
		t.Fatal("expected PVC volume workspace-volume")
	}

	// InitContainer check: podd-installer
	if len(pod.InitContainers) != 1 {
		t.Fatalf("expected exactly 1 initContainer, got %d", len(pod.InitContainers))
	}
	initC := pod.InitContainers[0]
	if initC.Name != "podd-installer" {
		t.Fatalf("expected initContainer name podd-installer, got %s", initC.Name)
	}
	if initC.Image != "cubepod-podd:v0.1.0" {
		t.Fatalf("expected initContainer image cubepod-podd:v0.1.0, got %s", initC.Image)
	}
	var initMountedBin bool
	for _, m := range initC.VolumeMounts {
		if m.Name == "cubepod-bin" && m.MountPath == "/opt/cubepod/bin" {
			initMountedBin = true
		}
	}
	if !initMountedBin {
		t.Fatal("expected podd-installer to mount cubepod-bin to /opt/cubepod/bin")
	}

	// Main container check
	if len(pod.Containers) != 1 {
		t.Fatalf("expected exactly 1 container without egress, got %d", len(pod.Containers))
	}
	mainC := pod.Containers[0]
	if mainC.Name != "sandbox" {
		t.Fatalf("expected container name sandbox, got %s", mainC.Name)
	}
	if mainC.Image != "cubeplex/sandbox-ubuntu24:latest" {
		t.Fatalf("expected container image cubeplex/sandbox-ubuntu24:latest, got %s", mainC.Image)
	}
	expectedCmd := []string{"/opt/cubepod/bin/podd-init", "/opt/cubepod/bin/podd"}
	if !slices.Equal(mainC.Command, expectedCmd) {
		t.Fatalf("expected command %v, got %v", expectedCmd, mainC.Command)
	}

	var mainMountedBin bool
	for _, m := range mainC.VolumeMounts {
		if m.Name == "cubepod-bin" && m.MountPath == "/opt/cubepod/bin" {
			mainMountedBin = true
		}
	}
	if !mainMountedBin {
		t.Fatal("expected main container to mount /opt/cubepod/bin")
	}

	// Check volumeDevices: /dev/cubepod-rootfs & /dev/cubepod-workspace
	var foundRootfsDev, foundWsDev bool
	for _, d := range mainC.VolumeDevices {
		if d.Name == "rootfs-volume" && d.DevicePath == "/dev/cubepod-rootfs" {
			foundRootfsDev = true
		}
		if d.Name == "workspace-volume" && d.DevicePath == "/dev/cubepod-workspace" {
			foundWsDev = true
		}
	}
	if !foundRootfsDev {
		t.Fatal("expected main container volumeDevice /dev/cubepod-rootfs")
	}
	if !foundWsDev {
		t.Fatal("expected main container volumeDevice /dev/cubepod-workspace")
	}
}

func TestBuildKataPod_EgressSidecar(t *testing.T) {
	boxName := "egress-box"
	tpl := sampleTemplate()
	tpl.Egress.Enabled = true

	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
		NetworkPolicy: &v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			SecretBindings: []v1alpha1.SecretBinding{
				{
					EnvName:     "GITHUB_TOKEN",
					Placeholder: "cpodref_ABC1234567890",
				},
			},
		},
	}
	status := v1alpha1.PodBoxStatus{
		PodName:          "egress-box-kata-pod",
		RootfsPVCName:    "egress-box-rootfs-pvc",
		WorkspacePVCName: "egress-box-workspace-pvc",
	}

	pod := controller.BuildKataPod(boxName, tpl, spec, status)

	// Volumes must contain egress-mtls secret volume
	var foundEgressMtlsVol bool
	for _, vol := range pod.Volumes {
		if vol.Name == "egress-mtls" && vol.SecretName == "egress-box-egress-mtls" {
			foundEgressMtlsVol = true
		}
	}
	if !foundEgressMtlsVol {
		t.Fatal("expected secret volume egress-mtls")
	}

	// Must have 2 containers: sandbox + egress
	if len(pod.Containers) != 2 {
		t.Fatalf("expected 2 containers (main + egress sidecar), got %d", len(pod.Containers))
	}

	egressC := pod.Containers[1]
	if egressC.Name != "egress" {
		t.Fatalf("expected sidecar name egress, got %s", egressC.Name)
	}
	if egressC.Image != "cubepod-egress:v0.1.0" {
		t.Fatalf("expected sidecar image cubepod-egress:v0.1.0, got %s", egressC.Image)
	}

	var mountedCerts bool
	for _, m := range egressC.VolumeMounts {
		if m.Name == "egress-mtls" && m.MountPath == "/etc/cubepod/certs" && m.ReadOnly {
			mountedCerts = true
		}
	}
	if !mountedCerts {
		t.Fatal("expected egress sidecar to mount /etc/cubepod/certs as read-only")
	}

	var hasExchangeURL, hasSandboxID bool
	for _, env := range egressC.Env {
		if env.Name == "EXCHANGE_URL" && env.Value == "https://egress.cubepod.internal:8443" {
			hasExchangeURL = true
		}
		if env.Name == "SANDBOX_ID" && env.Value == boxName {
			hasSandboxID = true
		}
	}
	if !hasExchangeURL || !hasSandboxID {
		t.Fatalf("missing required egress env vars: %v", egressC.Env)
	}

	// Main container must have secret binding env var placeholder
	mainC := pod.Containers[0]
	var foundSecretBinding bool
	for _, env := range mainC.Env {
		if env.Name == "GITHUB_TOKEN" && env.Value == "cpodref_ABC1234567890" {
			foundSecretBinding = true
		}
	}
	if !foundSecretBinding {
		t.Fatal("expected GITHUB_TOKEN secret binding in main container env")
	}
}

// 3. Tests for Pre-attached PodWarmPool manager
func TestPodWarmPool_ReconcileAndClaim(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	tpl := sampleTemplate()
	r.RegisterTemplate("base-tpl", tpl)

	storedTpl, ok := r.GetTemplate("base-tpl")
	if !ok || storedTpl.RuntimeClassName != "kata-qemu" {
		t.Fatalf("expected registered template base-tpl")
	}

	// Reconcile warm pool with 3 replicas
	poolSpec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    3,
	}
	wpStatus, err := r.ReconcileWarmPool(ctx, "pool-a", poolSpec)
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if wpStatus.ReadyReplicas != 3 || wpStatus.AvailableReplicas != 3 {
		t.Fatalf("expected 3 ready/available replicas, got ready=%d avail=%d",
			wpStatus.ReadyReplicas, wpStatus.AvailableReplicas)
	}

	// Claim 1 warm pod via fresh PodBox creation with WarmPoolRef
	boxSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "base-tpl",
		WarmPoolRef:      "pool-a",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	boxStatus, err := r.Reconcile(ctx, "agent-box-1", boxSpec)
	if err != nil {
		t.Fatalf("Reconcile with WarmPoolRef failed: %v", err)
	}

	if boxStatus.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected phase Running, got %s", boxStatus.Phase)
	}
	if boxStatus.Reason != "WarmPoolClaimed" {
		t.Fatalf("expected reason WarmPoolClaimed, got %s", boxStatus.Reason)
	}
	if boxStatus.PodName == "" || boxStatus.RootfsPVCName == "" || boxStatus.WorkspacePVCName == "" {
		t.Fatalf("warm pod values not assigned: %+v", boxStatus)
	}

	// Warm pool should now have 2 available replicas
	wpStatus2, ok := r.GetWarmPoolStatus("pool-a")
	if !ok {
		t.Fatal("failed to get warm pool status")
	}
	if wpStatus2.AvailableReplicas != 2 || wpStatus2.ReadyReplicas != 2 {
		t.Fatalf("expected 2 available replicas after claim, got %d", wpStatus2.AvailableReplicas)
	}

	// Claim second warm pod directly via ClaimWarmPod
	entry, err := r.ClaimWarmPod(ctx, "pool-a", "agent-box-2")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}
	if entry.ClaimedBy != "agent-box-2" || !entry.Claimed {
		t.Fatalf("entry not properly marked claimed: %+v", entry)
	}
	if entry.PodManifest.Labels["cubepod.io/warmpool"] != "pool-a" ||
		entry.PodManifest.Labels["cubepod.io/template"] != "base-tpl" ||
		entry.PodManifest.Labels["cubepod.io/claimed"] != "true" {
		t.Fatalf("unexpected entry podManifest labels: %+v", entry.PodManifest.Labels)
	}

	// Warm pool should now have 1 available replica
	wpStatus3, _ := r.GetWarmPoolStatus("pool-a")
	if wpStatus3.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica, got %d", wpStatus3.AvailableReplicas)
	}

	// Claim third
	_, err = r.ClaimWarmPod(ctx, "pool-a", "agent-box-3")
	if err != nil {
		t.Fatalf("third claim failed: %v", err)
	}

	// Claiming fourth should fail because pool is exhausted
	_, err = r.ClaimWarmPod(ctx, "pool-a", "agent-box-4")
	if err == nil {
		t.Fatal("expected error claiming from exhausted warm pool, got nil")
	}

	// Reconcile a PodBox targeting exhausted pool should fail
	_, err = r.Reconcile(ctx, "agent-box-5", boxSpec)
	if err == nil {
		t.Fatal("expected error reconciling box with exhausted warm pool, got nil")
	}

	// Scale up warm pool to 2 new replicas
	poolSpec.Replicas = 2
	wpStatusScaled, err := r.ReconcileWarmPool(ctx, "pool-a", poolSpec)
	if err != nil {
		t.Fatalf("scaling warm pool failed: %v", err)
	}
	if wpStatusScaled.AvailableReplicas != 2 {
		t.Fatalf("expected 2 available replicas after scaling, got %d", wpStatusScaled.AvailableReplicas)
	}
}

// 4. Tests for CoW Fork support
func TestReconcile_CoWFork(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	// 1. Reconcile parent sandbox
	parentSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "standard-node",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	parentStatus, err := r.Reconcile(ctx, "parent-box", parentSpec)
	if err != nil {
		t.Fatalf("create parent failed: %v", err)
	}
	if parentStatus.RootfsPVCName != "parent-box-rootfs-pvc" {
		t.Fatalf("unexpected parent rootfs PVC: %s", parentStatus.RootfsPVCName)
	}

	// 2. Reconcile child sandbox with ForkFrom
	childSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "standard-node",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "parent-box"},
	}
	childStatus, err := r.Reconcile(ctx, "child-box", childSpec)
	if err != nil {
		t.Fatalf("create child fork failed: %v", err)
	}

	if childStatus.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected child Running, got %s", childStatus.Phase)
	}
	if childStatus.Reason != "ForkCloned" {
		t.Fatalf("expected child reason ForkCloned, got %s", childStatus.Reason)
	}

	// 3. Verify BuildBlockPVCs generates DataSourcePVC for child pointing to parent's PVCs
	tpl := sampleTemplate()
	rootfsPVC, wsPVC := controller.BuildBlockPVCs("child-box", tpl, childSpec)
	if rootfsPVC.DataSourcePVC != "parent-box-rootfs-pvc" {
		t.Fatalf("expected child rootfs DataSourcePVC parent-box-rootfs-pvc, got %s", rootfsPVC.DataSourcePVC)
	}
	if wsPVC.DataSourcePVC != "parent-box-workspace-pvc" {
		t.Fatalf("expected child workspace DataSourcePVC parent-box-workspace-pvc, got %s", wsPVC.DataSourcePVC)
	}
}

// 5. Tests for RootFS Reset preserving Workspace PVC
func TestReconcile_RootFSResetPreservesWorkspace(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	spec := v1alpha1.PodBoxSpec{
		TemplateRef:      "ubuntu-node",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	status1, err := r.Reconcile(ctx, "reset-box", spec)
	if err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}

	origWorkspacePVC := status1.WorkspacePVCName
	if status1.RootfsPVCName != "reset-box-rootfs-pvc" {
		t.Fatalf("unexpected gen1 rootfs PVC: %s", status1.RootfsPVCName)
	}

	// Reset RootFS to generation 2
	spec.RootfsGeneration = 2
	status2, err := r.Reconcile(ctx, "reset-box", spec)
	if err != nil {
		t.Fatalf("gen2 reconcile failed: %v", err)
	}

	if status2.RootfsPVCName != "reset-box-rootfs-gen2-pvc" {
		t.Fatalf("expected gen2 rootfs PVC reset-box-rootfs-gen2-pvc, got %s", status2.RootfsPVCName)
	}
	if status2.WorkspacePVCName != origWorkspacePVC {
		t.Fatalf("Workspace PVC MUST be preserved across resets! Expected %s, got %s",
			origWorkspacePVC, status2.WorkspacePVCName)
	}
	if status2.Reason != "RootFSReset" {
		t.Fatalf("expected reason RootFSReset, got %s", status2.Reason)
	}
}

// 6. Tests for ReapIdle respecting IdleTimeoutSeconds and LeaseExpiresAt
func TestReapIdle_IdleTimeoutAndLeaseProtection(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	now := time.Now().UTC()
	past := now.Add(-120 * time.Second)
	activeLease := now.Add(300 * time.Second)
	expiredLease := now.Add(-30 * time.Second)

	// Box 1: Idle timeout 60s, last activity 120s ago, no lease -> SHOULD PAUSE
	spec1 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 60,
		},
	}
	_, _ = r.Reconcile(ctx, "idle-unleased-box", spec1)
	_ = r.TouchActivity("idle-unleased-box", past)

	// Box 2: Idle timeout 60s, last activity 120s ago, ACTIVE LEASE -> MUST NOT PAUSE
	spec2 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 60,
			LeaseExpiresAt:     &activeLease,
		},
	}
	_, _ = r.Reconcile(ctx, "idle-leased-box", spec2)
	_ = r.TouchActivity("idle-leased-box", past)

	// Box 3: Idle timeout 60s, last activity 120s ago, EXPIRED LEASE -> SHOULD PAUSE
	spec3 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 60,
			LeaseExpiresAt:     &expiredLease,
		},
	}
	_, _ = r.Reconcile(ctx, "idle-expired-lease-box", spec3)
	_ = r.TouchActivity("idle-expired-lease-box", past)

	// Box 4: Active box, last activity 10s ago, idle timeout 60s -> MUST NOT PAUSE
	recent := now.Add(-10 * time.Second)
	spec4 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 60,
		},
	}
	_, _ = r.Reconcile(ctx, "active-box", spec4)
	_ = r.TouchActivity("active-box", recent)

	// Run ReapIdle at 'now'
	paused, terminated := r.ReapIdle(ctx, now)

	if len(terminated) != 0 {
		t.Fatalf("expected 0 terminated sandboxes, got %v", terminated)
	}

	expectedPaused := []string{"idle-expired-lease-box", "idle-unleased-box"}
	slices.Sort(expectedPaused)
	if !slices.Equal(paused, expectedPaused) {
		t.Fatalf("expected paused %v, got %v", expectedPaused, paused)
	}

	// Verify status of paused boxes
	box1Status, _ := r.GetBox("idle-unleased-box")
	if box1Status.Phase != v1alpha1.PodBoxPhasePaused || box1Status.Reason != "IdlePaused" {
		t.Fatalf("unexpected box1 status: %+v", box1Status)
	}

	// Verify status of leased box remains Running
	box2Status, _ := r.GetBox("idle-leased-box")
	if box2Status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("leased box was unexpectedly paused! Status: %+v", box2Status)
	}
}

// 7. Tests for ReapIdle PausedTTL expiration
func TestReapIdle_PausedTTL(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	now := time.Now().UTC()

	// Box 1: Paused, TTL 100s, paused 200s ago -> SHOULD TERMINATE
	spec1 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStatePaused,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			PausedTTLSeconds: 100,
		},
	}
	_, _ = r.Reconcile(ctx, "expired-paused-box", spec1)
	oldPause := now.Add(-200 * time.Second)
	_ = r.SetPausedAt("expired-paused-box", oldPause)

	// Box 2: Paused, TTL 100s, paused 30s ago -> MUST NOT TERMINATE
	spec2 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStatePaused,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			PausedTTLSeconds: 100,
		},
	}
	_, _ = r.Reconcile(ctx, "recent-paused-box", spec2)

	paused, terminated := r.ReapIdle(ctx, now)
	if len(paused) != 0 {
		t.Fatalf("expected 0 paused, got %v", paused)
	}
	if !slices.Equal(terminated, []string{"expired-paused-box"}) {
		t.Fatalf("expected terminated [expired-paused-box], got %v", terminated)
	}

	stTerm, _ := r.GetBox("expired-paused-box")
	if stTerm.Phase != v1alpha1.PodBoxPhaseTerminated || stTerm.Reason != "PausedTTLExpired" {
		t.Fatalf("unexpected terminated box status: %+v", stTerm)
	}
}

// 8. Test Concurrency and Race Safety
func TestConcurrentWarmPoolAndClaims(t *testing.T) {
	ctx := context.Background()
	r := controller.NewReconciler()

	r.RegisterTemplate("conc-tpl", sampleTemplate())
	_, err := r.ReconcileWarmPool(ctx, "conc-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "conc-tpl",
		Replicas:    20,
	})
	if err != nil {
		t.Fatalf("warm pool setup failed: %v", err)
	}

	var wg sync.WaitGroup
	claimedBoxes := make([]string, 15)
	for i := 0; i < 15; i++ {
		wg.Add(1)
		boxName := fmt.Sprintf("conc-box-%d", i)
		claimedBoxes[i] = boxName
		go func(name string) {
			defer wg.Done()
			spec := v1alpha1.PodBoxSpec{
				TemplateRef:      "conc-tpl",
				WarmPoolRef:      "conc-pool",
				DesiredState:     v1alpha1.DesiredStateRunning,
				RootfsGeneration: 1,
			}
			_, claimErr := r.Reconcile(ctx, name, spec)
			if claimErr != nil {
				t.Errorf("concurrent reconcile error for %s: %v", name, claimErr)
			}
		}(boxName)
	}
	wg.Wait()

	st, ok := r.GetWarmPoolStatus("conc-pool")
	if !ok {
		t.Fatal("failed to get status")
	}
	if st.AvailableReplicas != 5 {
		t.Fatalf("expected 5 remaining available replicas, got %d", st.AvailableReplicas)
	}
}

func TestReconciler_SyncFromKube_WarmPoolDeletionCleanup(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// 1. Create warmpool with 2 replicas
	poolSpec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	}
	_, err := r.ReconcileWarmPool(ctx, "delete-pool", poolSpec)
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// 2. Claim 1 replica
	claimed, err := r.ClaimWarmPod(ctx, "delete-pool", "claimer-box")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}

	// Identify unclaimed entry
	specs := r.GetWarmPoolSpecs()
	if _, ok := specs["delete-pool"]; !ok {
		t.Fatal("expected delete-pool in specs")
	}

	var unclaimedPodName, unclaimedRootfsPVC, unclaimedWorkspacePVC string
	pods, err := memKube.ListPods(ctx, "cubepod-workloads", "")
	if err != nil {
		t.Fatalf("ListPods failed: %v", err)
	}
	for _, pod := range pods {
		if strings.HasPrefix(pod.Name, "delete-pool-warm-") && pod.Name != claimed.PodName {
			unclaimedPodName = pod.Name
			break
		}
	}
	if unclaimedPodName == "" {
		t.Fatal("expected to find unclaimed pod in kubeClient")
	}
	// PodName is <entryID>-kata-pod, PVCs are <entryID>-rootfs-pvc and <entryID>-workspace-pvc
	entryID := strings.TrimSuffix(unclaimedPodName, "-kata-pod")
	unclaimedRootfsPVC = entryID + "-rootfs-pvc"
	unclaimedWorkspacePVC = entryID + "-workspace-pvc"

	// 3. Delete the PodWarmPool CR from Kubernetes (e.g. via kubectl delete podwarmpool)
	if err := memKube.DeletePodWarmPool(ctx, "cubepod-workloads", "delete-pool"); err != nil {
		t.Fatalf("DeletePodWarmPool failed: %v", err)
	}

	// 4. Set pool createdAt to > 10s ago to pass the stale deletion window guard
	r.SetPoolCreatedAt("delete-pool", time.Now().Add(-15*time.Second))

	// SyncFromKube should detect that delete-pool is gone, delete unclaimed Pod & PVCs, and remove from r.warmPools
	_, err = r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}

	// Verify delete-pool is deleted from r.warmPools
	if _, exists := r.GetWarmPoolSpecs()["delete-pool"]; exists {
		t.Fatal("expected delete-pool to be removed from reconciler warmPools")
	}

	// Verify unclaimed pod and PVCs were deleted from Kubernetes
	if _, exists := memKube.GetPod("cubepod-workloads", unclaimedPodName); exists {
		t.Fatalf("expected unclaimed pod %s to be deleted from K8s", unclaimedPodName)
	}
	if _, exists := memKube.GetPVC("cubepod-workloads", unclaimedRootfsPVC); exists {
		t.Fatalf("expected unclaimed rootfs PVC %s to be deleted from K8s", unclaimedRootfsPVC)
	}
	if _, exists := memKube.GetPVC("cubepod-workloads", unclaimedWorkspacePVC); exists {
		t.Fatalf("expected unclaimed workspace PVC %s to be deleted from K8s", unclaimedWorkspacePVC)
	}

	// Verify claimed pod and PVCs were NOT deleted
	if _, exists := memKube.GetPod("cubepod-workloads", claimed.PodName); !exists {
		t.Fatalf("expected claimed pod %s to be preserved in K8s", claimed.PodName)
	}
	if _, exists := memKube.GetPVC("cubepod-workloads", claimed.RootfsPVCName); !exists {
		t.Fatalf("expected claimed rootfs PVC %s to be preserved in K8s", claimed.RootfsPVCName)
	}
	if _, exists := memKube.GetPVC("cubepod-workloads", claimed.WorkspacePVCName); !exists {
		t.Fatalf("expected claimed workspace PVC %s to be preserved in K8s", claimed.WorkspacePVCName)
	}
}

func TestReconciler_ReconcileWarmPool_Status404_DoesNotRecreate(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	poolSpec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	}

	// 1. Initial creation via ReconcileWarmPool: creates the CR in KubeClient and marks persisted = true
	st, err := r.ReconcileWarmPool(ctx, "deleted-pool", poolSpec)
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica, got %d", st.AvailableReplicas)
	}
	if _, err := memKube.GetPodWarmPool(ctx, "cubepod-workloads", "deleted-pool"); err != nil {
		t.Fatalf("expected initial ReconcileWarmPool to persist CR to KubeClient: %v", err)
	}

	// 2. Simulate deletion from Kubernetes (e.g. kubectl delete podwarmpool)
	if err := memKube.DeletePodWarmPool(ctx, "cubepod-workloads", "deleted-pool"); err != nil {
		t.Fatalf("DeletePodWarmPool failed: %v", err)
	}

	// 3. Subsequent tick: pool.persisted is true, UpdatePodWarmPoolStatus returns 404, skips recreation!
	st2, err := r.ReconcileWarmPool(ctx, "deleted-pool", poolSpec)
	if err != nil {
		t.Fatalf("subsequent ReconcileWarmPool failed: %v", err)
	}
	if st2.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica, got %d", st2.AvailableReplicas)
	}

	// Ensure memKube did NOT have UpsertPodWarmPool called (it was not recreated)
	if _, err := memKube.GetPodWarmPool(ctx, "cubepod-workloads", "deleted-pool"); err == nil {
		t.Fatal("expected deleted-pool NOT to be recreated in KubeClient on status 404")
	}
}

func TestReconciler_ClaimWarmPod_PatchesLabels(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	_, err := r.ReconcileWarmPool(ctx, "label-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// Before claim, check standby pod labels
	pods, err := memKube.ListPods(ctx, "cubepod-workloads", "")
	if err != nil {
		t.Fatalf("ListPods failed: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod in kubeClient, got %d", len(pods))
	}
	if pods[0].Labels["cubepod.io/claimed"] != "false" {
		t.Fatalf("expected claimed=false before claim, got %s", pods[0].Labels["cubepod.io/claimed"])
	}

	// Claim the pod
	entry, err := r.ClaimWarmPod(ctx, "label-pool", "claimer-box-123")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}

	// Entry labels updated
	if entry.PodManifest.Labels["cubepod.io/claimed"] != "true" {
		t.Fatalf("expected entry label claimed=true, got %s", entry.PodManifest.Labels["cubepod.io/claimed"])
	}
	if entry.PodManifest.Labels["cubepod.io/claimed-by"] != "claimer-box-123" {
		t.Fatalf("expected entry label claimed-by=claimer-box-123, got %s", entry.PodManifest.Labels["cubepod.io/claimed-by"])
	}

	// KubeClient pod labels patched
	patchedPod, ok := memKube.GetPod("cubepod-workloads", entry.PodName)
	if !ok {
		t.Fatalf("expected pod %s in kubeClient", entry.PodName)
	}
	if patchedPod.Labels["cubepod.io/claimed"] != "true" {
		t.Fatalf("expected patched pod label claimed=true, got %s", patchedPod.Labels["cubepod.io/claimed"])
	}
	if patchedPod.Labels["cubepod.io/claimed-by"] != "claimer-box-123" {
		t.Fatalf("expected patched pod label claimed-by=claimer-box-123, got %s", patchedPod.Labels["cubepod.io/claimed-by"])
	}
}

func TestBuildKataPod_ClaimLabels(t *testing.T) {
	tpl := v1alpha1.PodTemplateSpec{
		RuntimeClassName: "none",
	}

	// 1. Standby pod in pool: boxName starts with pool-warm-
	standbySpec := v1alpha1.PodBoxSpec{
		WarmPoolRef: "my-pool",
		TemplateRef: "my-tpl",
	}
	standbyStatus := v1alpha1.PodBoxStatus{}
	standbyPod := controller.BuildKataPod("my-pool-warm-1", tpl, standbySpec, standbyStatus, "Filesystem")
	if standbyPod.Labels["cubepod.io/claimed"] != "false" {
		t.Fatalf("expected standby pod claimed=false, got %s", standbyPod.Labels["cubepod.io/claimed"])
	}
	if _, ok := standbyPod.Labels["cubepod.io/claimed-by"]; ok {
		t.Fatalf("standby pod should not have claimed-by label, got %s", standbyPod.Labels["cubepod.io/claimed-by"])
	}

	// 2. Claimed pod: boxName is the sandbox name
	claimSpec := v1alpha1.PodBoxSpec{
		WarmPoolRef: "my-pool",
		TemplateRef: "my-tpl",
	}
	claimStatus := v1alpha1.PodBoxStatus{
		Reason:  "WarmPoolClaimed",
		PodName: "my-pool-warm-1-kata-pod",
	}
	claimedPod := controller.BuildKataPod("sandbox-claimer", tpl, claimSpec, claimStatus, "Filesystem")
	if claimedPod.Labels["cubepod.io/claimed"] != "true" {
		t.Fatalf("expected claimed pod claimed=true, got %s", claimedPod.Labels["cubepod.io/claimed"])
	}
	if claimedPod.Labels["cubepod.io/claimed-by"] != "sandbox-claimer" {
		t.Fatalf("expected claimed pod claimed-by=sandbox-claimer, got %s", claimedPod.Labels["cubepod.io/claimed-by"])
	}
}

type failingPatchKubeClient struct {
	*controller.MemoryKubeClient
	failPodNames   map[string]bool
	transientError bool
}

func (f *failingPatchKubeClient) PatchPodLabels(ctx context.Context, ns, name string, labels map[string]string) error {
	if f.failPodNames[name] {
		if f.transientError {
			return fmt.Errorf("injected transient PatchPodLabels failure for %s", name)
		}
		return fmt.Errorf("%w: injected CAS failure for %s", controller.ErrCASConflict, name)
	}
	return f.MemoryKubeClient.PatchPodLabels(ctx, ns, name, labels)
}

func (f *failingPatchKubeClient) ClaimPod(ctx context.Context, ns, name, boxName string) error {
	if f.failPodNames[name] {
		if f.transientError {
			return fmt.Errorf("injected transient ClaimPod failure for %s", name)
		}
		return fmt.Errorf("%w: injected CAS failure for %s", controller.ErrCASConflict, name)
	}
	return f.MemoryKubeClient.ClaimPod(ctx, ns, name, boxName)
}

func TestReconciler_ClaimWarmPod_PatchFailureSkipsPod(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	failingKube := &failingPatchKubeClient{
		MemoryKubeClient: memKube,
		failPodNames:     make(map[string]bool),
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(failingKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Create pool with 2 replicas
	_, err := r.ReconcileWarmPool(ctx, "patch-fail-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	entries := r.GetWarmPoolEntries("patch-fail-pool")
	if len(entries) != 2 {
		t.Fatalf("expected 2 warm pool entries, got %d", len(entries))
	}

	pod1 := entries[0].PodName
	pod2 := entries[1].PodName

	// Inject failure on pod1
	failingKube.failPodNames[pod1] = true

	// Claiming should fail on pod1, skip it, and claim pod2 successfully
	claimedEntry, err := r.ClaimWarmPod(ctx, "patch-fail-pool", "claimer-box-1")
	if err != nil {
		t.Fatalf("ClaimWarmPod should have succeeded by picking pod2, got: %v", err)
	}
	if claimedEntry.PodName != pod2 {
		t.Fatalf("expected claimed entry to be %s, got %s", pod2, claimedEntry.PodName)
	}

	// pod1 CAS claim failed, so it was marked Claimed with ClaimedBy = other-replica in local memory
	status, ok := r.GetWarmPoolStatus("patch-fail-pool")
	if !ok || status.AvailableReplicas != 0 {
		t.Fatalf("expected 0 available replicas remaining, got %d", status.AvailableReplicas)
	}

	// Second claim should fail since pod1 was marked claimed by other replica and no other standby is available
	_, err = r.ClaimWarmPod(ctx, "patch-fail-pool", "claimer-box-2")
	if err == nil {
		t.Fatal("expected ClaimWarmPod to fail when all available pods fail PatchPodLabels")
	}

	// Unclaimed pod1 remains unclaimed
	p1, _ := memKube.GetPod("cubepod-workloads", pod1)
	if p1.Labels["cubepod.io/claimed"] != "false" {
		t.Fatalf("expected pod1 to remain claimed=false in kubeClient, got %s", p1.Labels["cubepod.io/claimed"])
	}
}

func TestReconciler_SyncFromKube_RebuildsEntriesAndCounter(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	// 1. Pre-populate Kubernetes APIServer with a PodWarmPool CR
	poolCR := v1alpha1.PodWarmPool{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodWarmPool",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "rebuild-pool",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodWarmPoolSpec{
			TemplateRef: "ubuntu:24.04",
			Replicas:    2,
		},
	}
	if err := memKube.UpsertPodWarmPool(ctx, poolCR); err != nil {
		t.Fatalf("UpsertPodWarmPool failed: %v", err)
	}

	// 2. Pre-populate 2 pods: pod-1 is claimed, pod-2 is standby
	pod1 := controller.PodManifest{
		Name:      "rebuild-pool-warm-1-kata-pod",
		Namespace: "cubepod-workloads",
		Labels: map[string]string{
			"cubepod.io/warmpool":   "rebuild-pool",
			"cubepod.io/template":   "ubuntu:24.04",
			"cubepod.io/claimed":    "true",
			"cubepod.io/claimed-by": "existing-sandbox-1",
		},
	}
	pod2 := controller.PodManifest{
		Name:      "rebuild-pool-warm-2-kata-pod",
		Namespace: "cubepod-workloads",
		Labels: map[string]string{
			"cubepod.io/warmpool": "rebuild-pool",
			"cubepod.io/template": "ubuntu:24.04",
			"cubepod.io/claimed":  "false",
		},
	}
	if _, err := memKube.EnsurePod(ctx, pod1); err != nil {
		t.Fatalf("EnsurePod 1 failed: %v", err)
	}
	if _, err := memKube.EnsurePod(ctx, pod2); err != nil {
		t.Fatalf("EnsurePod 2 failed: %v", err)
	}

	// 3. Fresh Reconciler (simulating restart)
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Before sync: 0 warm pools in memory
	if _, ok := r.GetWarmPoolStatus("rebuild-pool"); ok {
		t.Fatal("expected rebuild-pool not to exist in memory before sync")
	}

	// Recover state via SyncFromKube
	if _, err := r.SyncFromKube(ctx); err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}

	// Status should show 1 available replica (pod-2)
	st, ok := r.GetWarmPoolStatus("rebuild-pool")
	if !ok {
		t.Fatal("expected rebuild-pool in memory after sync")
	}
	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica after sync, got %d", st.AvailableReplicas)
	}

	// Claim the single available standby pod
	claimed, err := r.ClaimWarmPod(ctx, "rebuild-pool", "new-box-alpha")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}
	if claimed.PodName != "rebuild-pool-warm-2-kata-pod" {
		t.Fatalf("expected to claim pod 2, got %s", claimed.PodName)
	}

	// No standby replicas left
	st, _ = r.GetWarmPoolStatus("rebuild-pool")
	if st.AvailableReplicas != 0 {
		t.Fatalf("expected 0 available replicas after claim, got %d", st.AvailableReplicas)
	}

	// Now reconcile the pool to replenish replicas to desired 2
	st2, err := r.ReconcileWarmPool(ctx, "rebuild-pool", poolCR.Spec)
	if err != nil {
		t.Fatalf("ReconcileWarmPool replenish failed: %v", err)
	}
	if st2.AvailableReplicas != 2 {
		t.Fatalf("expected 2 available replicas after replenishment, got %d", st2.AvailableReplicas)
	}

	// Newly created pods must use index > 2 (e.g. rebuild-pool-warm-3 and 4)
	pods, err := memKube.ListPods(ctx, "cubepod-workloads", "cubepod.io/warmpool=rebuild-pool")
	if err != nil {
		t.Fatalf("ListPods failed: %v", err)
	}
	for _, p := range pods {
		if strings.Contains(p.Name, "-warm-1-") || strings.Contains(p.Name, "-warm-2-") {
			continue
		}
		// Any newly created pod should have index >= 3
		if !strings.Contains(p.Name, "-warm-3-") && !strings.Contains(p.Name, "-warm-4-") {
			t.Errorf("unexpected pod name created: %s", p.Name)
		}
	}
}

func TestMemoryKubeClient_ClaimPod_CAS(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	pod := controller.PodManifest{
		Name:      "test-standby-pod",
		Namespace: "cubepod-workloads",
		Labels: map[string]string{
			"cubepod.io/warmpool": "test-pool",
			"cubepod.io/claimed":  "false",
		},
	}
	if _, err := memKube.EnsurePod(ctx, pod); err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}

	// 1. First claim succeeds
	err := memKube.ClaimPod(ctx, "cubepod-workloads", "test-standby-pod", "box-1")
	if err != nil {
		t.Fatalf("first ClaimPod failed: %v", err)
	}

	p, ok := memKube.GetPod("cubepod-workloads", "test-standby-pod")
	if !ok || p.Labels["cubepod.io/claimed"] != "true" || p.Labels["cubepod.io/claimed-by"] != "box-1" {
		t.Fatalf("expected pod claimed by box-1, got: %+v", p.Labels)
	}

	// 2. Second claim fails (CAS rejection)
	err2 := memKube.ClaimPod(ctx, "cubepod-workloads", "test-standby-pod", "box-2")
	if err2 == nil {
		t.Fatal("expected second ClaimPod to fail on already claimed pod")
	}

	// 3. Trying to claim pod without false label fails
	podUnlabeled := controller.PodManifest{
		Name:      "unlabeled-pod",
		Namespace: "cubepod-workloads",
		Labels:    map[string]string{},
	}
	_, _ = memKube.EnsurePod(ctx, podUnlabeled)
	err3 := memKube.ClaimPod(ctx, "cubepod-workloads", "unlabeled-pod", "box-3")
	if err3 == nil {
		t.Fatal("expected ClaimPod to fail on pod without claimed=false label")
	}
}

func TestReconciler_WarmPool_EnsurePodConflictClaimedNotAdopted(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	// Pre-populate an existing pod that was already created and claimed
	podName := "conflict-pool-warm-1-kata-pod"
	existingPod := controller.PodManifest{
		Name:      podName,
		Namespace: "cubepod-workloads",
		Labels: map[string]string{
			"cubepod.io/warmpool":   "conflict-pool",
			"cubepod.io/template":   "base-tpl",
			"cubepod.io/claimed":    "true",
			"cubepod.io/claimed-by": "prior-sandbox",
		},
	}
	if _, err := memKube.EnsurePod(ctx, existingPod); err != nil {
		t.Fatalf("pre-populating existing pod failed: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Reconcile warm pool with 1 replica: EnsurePod will hit 409 (already exists and claimed).
	// It must skip adopting the claimed pod, and continue iterating to create a replacement standby!
	st, err := r.ReconcileWarmPool(ctx, "conflict-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// Replacement standby was created, reaching 1 available replica
	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica after replacement standby created, got %d", st.AvailableReplicas)
	}

	entries := r.GetWarmPoolEntries("conflict-pool")
	if len(entries) != 1 || entries[0].PodName != "conflict-pool-warm-2-kata-pod" {
		t.Fatalf("expected replacement entry conflict-pool-warm-2-kata-pod, got %+v", entries)
	}
}

type failingUpsertKubeClient struct {
	*controller.MemoryKubeClient
	failUpsert bool
}

func (f *failingUpsertKubeClient) UpsertPodWarmPool(ctx context.Context, pool v1alpha1.PodWarmPool) error {
	if f.failUpsert {
		return fmt.Errorf("injected UpsertPodWarmPool failure")
	}
	return f.MemoryKubeClient.UpsertPodWarmPool(ctx, pool)
}

func TestReconciler_WarmPool_PersistedFlagOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	failingKube := &failingUpsertKubeClient{
		MemoryKubeClient: memKube,
		failUpsert:       true,
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(failingKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// 1. Upsert fails: pool should NOT be marked persisted
	_, err := r.ReconcileWarmPool(ctx, "fail-persist-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// In memory client, pool should NOT be in WarmPools map yet
	if _, err := memKube.GetPodWarmPool(ctx, "cubepod-workloads", "fail-persist-pool"); err == nil {
		t.Fatal("expected pool not to be persisted in KubeClient when upsert failed")
	}

	// 2. Clear failure: next reconcile will retry Upsert and succeed
	failingKube.failUpsert = false
	_, err = r.ReconcileWarmPool(ctx, "fail-persist-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("second ReconcileWarmPool failed: %v", err)
	}

	// Now it should be persisted
	if _, err := memKube.GetPodWarmPool(ctx, "cubepod-workloads", "fail-persist-pool"); err != nil {
		t.Fatalf("expected pool to be persisted after successful upsert: %v", err)
	}
}

func TestReconciler_SyncFromKube_RefreshesExistingPodBoxStatus(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// 1. Create a box in memory with Running state
	initialSpec := v1alpha1.PodBoxSpec{
		TemplateRef:  "ubuntu:24.04",
		DesiredState: v1alpha1.DesiredStateRunning,
	}
	_, err := r.Reconcile(ctx, "refresh-box", initialSpec)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	b, ok := r.GetBox("refresh-box")
	if !ok || b.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected initial box in Running phase, got: %+v", b)
	}

	// 2. Gateway/external agent updates PodBox in K8s to Paused
	pausedBox := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "refresh-box",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			TemplateRef:  "ubuntu:24.04",
			DesiredState: v1alpha1.DesiredStatePaused,
		},
		Status: v1alpha1.PodBoxStatus{
			Phase:   v1alpha1.PodBoxPhasePaused,
			PodName: "refresh-box-kata-pod",
		},
	}
	if err := memKube.UpsertPodBox(ctx, pausedBox); err != nil {
		t.Fatalf("UpsertPodBox failed: %v", err)
	}

	// 3. Controller runs SyncFromKube: MUST refresh the status in memory, not skip it!
	recovered, err := r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered box, got %d", recovered)
	}

	refreshed, ok := r.GetBox("refresh-box")
	if !ok {
		t.Fatal("expected refresh-box to exist in memory")
	}
	if refreshed.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected refreshed phase to be Paused, got: %s", refreshed.Phase)
	}
	refreshedSpec, ok := r.GetBoxSpec("refresh-box")
	if !ok || refreshedSpec.DesiredState != v1alpha1.DesiredStatePaused {
		t.Fatalf("expected refreshed desired state Paused, got: %+v", refreshedSpec)
	}
}

func TestReconciler_ReapIdle_PersistsPauseToKubeAndPreservedOnSync(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	now := time.Now().UTC()
	past := now.Add(-120 * time.Second)

	spec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 60,
		},
	}
	_, err := r.Reconcile(ctx, "idle-persist-box", spec)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	_ = r.TouchActivity("idle-persist-box", past)

	// Ensure initially Running in memKube
	initialKubeBox, err := memKube.GetPodBox(ctx, "cubepod-workloads", "idle-persist-box")
	if err != nil {
		t.Fatalf("GetPodBox failed: %v", err)
	}
	if initialKubeBox.Status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected initial Kube phase Running, got %s", initialKubeBox.Status.Phase)
	}

	// Run ReapIdle: must pause sandbox and persist paused status to K8s
	paused, terminated := r.ReapIdle(ctx, now)
	if len(paused) != 1 || paused[0] != "idle-persist-box" {
		t.Fatalf("expected idle-persist-box to be paused, got: %v (terminated: %v)", paused, terminated)
	}

	// Verify K8s has updated paused status and DesiredStatePaused
	kBox, err := memKube.GetPodBox(ctx, "cubepod-workloads", "idle-persist-box")
	if err != nil {
		t.Fatalf("GetPodBox after ReapIdle failed: %v", err)
	}
	if kBox.Status.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected K8s status to be Paused, got: %s", kBox.Status.Phase)
	}
	if kBox.Spec.DesiredState != v1alpha1.DesiredStatePaused {
		t.Fatalf("expected K8s spec desired state to be Paused, got: %s", kBox.Spec.DesiredState)
	}

	// On next tick, SyncFromKube runs: MUST NOT overwrite paused state back to Running!
	recovered, err := r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered box, got %d", recovered)
	}

	box, ok := r.GetBox("idle-persist-box")
	if !ok {
		t.Fatal("expected idle-persist-box in memory")
	}
	if box.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected box phase to remain Paused after SyncFromKube, got %s", box.Phase)
	}
}

func TestReconciler_SyncFromKube_StaleListDeletionWindow(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	poolSpec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	}
	_, err := r.ReconcileWarmPool(ctx, "grace-pool", poolSpec)
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// Delete from Kubernetes (stale snapshot where pool is missing)
	if err := memKube.DeletePodWarmPool(ctx, "cubepod-workloads", "grace-pool"); err != nil {
		t.Fatalf("DeletePodWarmPool failed: %v", err)
	}

	// 1. Fresh pool (< 10s old): SyncFromKube must NOT delete it
	_, err = r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if _, exists := r.GetWarmPoolSpecs()["grace-pool"]; !exists {
		t.Fatal("expected grace-pool to be protected from deletion within 10s grace window")
	}

	// 2. Set createdAt to > 10s ago: SyncFromKube should now delete it
	r.SetPoolCreatedAt("grace-pool", time.Now().Add(-15*time.Second))
	_, err = r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("second SyncFromKube failed: %v", err)
	}
	if _, exists := r.GetWarmPoolSpecs()["grace-pool"]; exists {
		t.Fatal("expected grace-pool to be deleted after 10s grace window expired")
	}
}

func TestReconciler_ClaimWarmPod_CASFailureMarksOtherReplica(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	failingKube := &failingPatchKubeClient{
		MemoryKubeClient: memKube,
		failPodNames:     make(map[string]bool),
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(failingKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	_, err := r.ReconcileWarmPool(ctx, "cas-test-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	entries := r.GetWarmPoolEntries("cas-test-pool")
	if len(entries) != 2 {
		t.Fatalf("expected 2 warm pool entries, got %d", len(entries))
	}

	pod1 := entries[0].PodName
	pod2 := entries[1].PodName

	// Inject CAS failure on first pod
	failingKube.failPodNames[pod1] = true

	claimed, err := r.ClaimWarmPod(ctx, "cas-test-pool", "claimer-box")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}
	if claimed.PodName != pod2 {
		t.Fatalf("expected pod2 to be claimed, got: %s", claimed.PodName)
	}

	// Available replicas should now be 0 (pod1 marked Claimed by other replica, pod2 claimed by claimer-box)
	st, ok := r.GetWarmPoolStatus("cas-test-pool")
	if !ok || st.AvailableReplicas != 0 {
		t.Fatalf("expected 0 available replicas, got %d", st.AvailableReplicas)
	}
}

func TestReconciler_ClaimWarmPod_TransientErrorLeavesUnclaimedForRetry(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	failingKube := &failingPatchKubeClient{
		MemoryKubeClient: memKube,
		failPodNames:     make(map[string]bool),
		transientError:   true,
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(failingKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	_, err := r.ReconcileWarmPool(ctx, "transient-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	entries := r.GetWarmPoolEntries("transient-pool")
	if len(entries) != 2 {
		t.Fatalf("expected 2 warm pool entries, got %d", len(entries))
	}

	pod1 := entries[0].PodName
	pod2 := entries[1].PodName

	// Inject transient failure on pod1 (NOT ErrCASConflict)
	failingKube.failPodNames[pod1] = true

	claimed, err := r.ClaimWarmPod(ctx, "transient-pool", "claimer-box")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}
	if claimed.PodName != pod2 {
		t.Fatalf("expected pod2 to be claimed, got: %s", claimed.PodName)
	}

	// Transient error on pod1 leaves it unclaimed in memory for future retry!
	st, ok := r.GetWarmPoolStatus("transient-pool")
	if !ok || st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica remaining (pod1 unclaimed for retry), got %d", st.AvailableReplicas)
	}
}

type mockCustomKubeClient struct {
	*controller.MemoryKubeClient
	failListPods  bool
	emptyListPods bool
	failUpsertBox bool
}

func (m *mockCustomKubeClient) ListPods(ctx context.Context, ns, selector string) ([]controller.PodManifest, error) {
	if m.failListPods {
		return nil, fmt.Errorf("injected ListPods failure")
	}
	if m.emptyListPods {
		return []controller.PodManifest{}, nil
	}
	return m.MemoryKubeClient.ListPods(ctx, ns, selector)
}

func (m *mockCustomKubeClient) UpsertPodBox(ctx context.Context, box v1alpha1.PodBox) error {
	if m.failUpsertBox {
		return fmt.Errorf("injected UpsertPodBox failure")
	}
	return m.MemoryKubeClient.UpsertPodBox(ctx, box)
}

func TestReconciler_WarmPool_409AdoptUnclaimed(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	// Pre-populate an existing pod that was created and is unclaimed
	podName := "adopt-pool-warm-1-kata-pod"
	existingPod := controller.PodManifest{
		Name:      podName,
		Namespace: "cubepod-workloads",
		Labels: map[string]string{
			"cubepod.io/warmpool":   "adopt-pool",
			"cubepod.io/template":   "base-tpl",
			"cubepod.io/claimed":    "false",
			"cubepod.io/claimed-by": "",
			"custom.label/key":      "custom-val",
		},
	}
	if _, err := memKube.EnsurePod(ctx, existingPod); err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	st, err := r.ReconcileWarmPool(ctx, "adopt-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica adopted, got %d", st.AvailableReplicas)
	}

	entries := r.GetWarmPoolEntries("adopt-pool")
	if len(entries) != 1 || entries[0].PodName != podName {
		t.Fatalf("expected adopted entry %s, got: %+v", podName, entries)
	}
	if entries[0].PodManifest.Labels["custom.label/key"] != "custom-val" {
		t.Fatalf("expected copied labels, got: %+v", entries[0].PodManifest.Labels)
	}
}

func TestReconciler_WarmPool_409ListPodsError(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	customKube := &mockCustomKubeClient{
		MemoryKubeClient: memKube,
		failListPods:     true,
	}

	// Pre-populate pod so EnsurePod returns 409
	podName := "list-err-pool-warm-1-kata-pod"
	_ = memKube.Pods
	if _, err := memKube.EnsurePod(ctx, controller.PodManifest{
		Name:      podName,
		Namespace: "cubepod-workloads",
	}); err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(customKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// In the creation loop, warm-1 hits 409, ListPods fails -> warning, continue without appending entry.
	// warm-2 does not exist, so EnsurePod succeeds -> 1 available replica.
	st, err := r.ReconcileWarmPool(ctx, "list-err-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica from warm-2, got %d", st.AvailableReplicas)
	}
	entries := r.GetWarmPoolEntries("list-err-pool")
	if len(entries) != 1 || entries[0].PodName != "list-err-pool-warm-2-kata-pod" {
		t.Fatalf("expected entry to be warm-2, got: %+v", entries)
	}
}

func TestReconciler_WarmPool_409PodNotFoundInList(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	customKube := &mockCustomKubeClient{
		MemoryKubeClient: memKube,
		emptyListPods:    true,
	}

	podName := "missing-pod-pool-warm-1-kata-pod"
	if _, err := memKube.EnsurePod(ctx, controller.PodManifest{
		Name:      podName,
		Namespace: "cubepod-workloads",
	}); err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(customKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// warm-1 hits 409, pod not found in list -> continue without appending entry.
	// warm-2 succeeds -> 1 available replica.
	st, err := r.ReconcileWarmPool(ctx, "missing-pod-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if st.AvailableReplicas != 1 {
		t.Fatalf("expected 1 available replica from warm-2, got %d", st.AvailableReplicas)
	}
}

func TestReconciler_ReapIdle_RollbackOnUpsertPodBoxError(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	customKube := &mockCustomKubeClient{
		MemoryKubeClient: memKube,
		failUpsertBox:    false, // start normal
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(customKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	boxSpec := v1alpha1.PodBoxSpec{
		TemplateRef:  "base-tpl",
		DesiredState: v1alpha1.DesiredStateRunning,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 30,
		},
	}
	_, err := r.Reconcile(ctx, "idle-fail-box", boxSpec)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Now make UpsertPodBox fail
	customKube.failUpsertBox = true

	now := time.Now().UTC().Add(60 * time.Second)
	paused, _ := r.ReapIdle(ctx, now)

	// Upsert failed, so paused list must be empty and in-memory box must remain Running!
	if len(paused) != 0 {
		t.Fatalf("expected 0 paused boxes on Upsert failure, got %v", paused)
	}

	box, ok := r.GetBox("idle-fail-box")
	if !ok {
		t.Fatal("box not found")
	}
	if box.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected box to remain Running after rollback, got %s", box.Phase)
	}
}

func TestReconciler_SyncFromKube_UnpersistedPoolCleanupAfter10s(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	failingKube := &failingUpsertKubeClient{
		MemoryKubeClient: memKube,
		failUpsert:       true, // fail persistence to K8s
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(failingKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	_, err := r.ReconcileWarmPool(ctx, "unpersisted-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// 1. Fresh unpersisted pool (<10s old): SyncFromKube must NOT delete it
	_, err = r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if _, exists := r.GetWarmPoolSpecs()["unpersisted-pool"]; !exists {
		t.Fatal("expected unpersisted-pool to be kept within 10s window")
	}

	// 2. Pool older than 10s: SyncFromKube must delete it and clean up resources
	r.SetPoolCreatedAt("unpersisted-pool", time.Now().Add(-15*time.Second))
	_, err = r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if _, exists := r.GetWarmPoolSpecs()["unpersisted-pool"]; exists {
		t.Fatal("expected unpersisted-pool to be deleted after 10s expiration")
	}
}

func TestReconciler_WarmCounter_RecoveryFromPausedPodBoxPVCNames(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	// 1. Pre-populate a paused PodBox in Kubernetes: PodName is cleared, but Rootfs/Workspace PVCs have warm index 12
	pausedBox := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "sb-paused-123",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStatePaused,
			WarmPoolRef:  "test-pool",
		},
		Status: v1alpha1.PodBoxStatus{
			Phase:            v1alpha1.PodBoxPhasePaused,
			PodName:          "", // cleared on pause!
			RootfsPVCName:    "test-pool-warm-12-rootfs-pvc",
			WorkspacePVCName: "test-pool-warm-12-workspace-pvc",
		},
	}
	if err := memKube.UpsertPodBox(ctx, pausedBox); err != nil {
		t.Fatalf("failed to upsert paused PodBox: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// 2. SyncFromKube must recover warmCounter from RootfsPVCName and WorkspacePVCName
	recovered, err := r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered box, got %d", recovered)
	}
	if r.WarmCounter() != 12 {
		t.Fatalf("expected warmCounter to advance to 12 from paused PodBox PVC names, got %d", r.WarmCounter())
	}

	// 3. Scaling up must allocate index 13, avoiding collision with index 12
	_, err = r.ReconcileWarmPool(ctx, "test-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	entries := r.GetWarmPoolEntries("test-pool")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID != "test-pool-warm-13" {
		t.Fatalf("expected entryID test-pool-warm-13, got: %s", entries[0].ID)
	}
}

func TestReconciler_WarmPool_ScaleUpAvoidsBoxPVCConflict(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()

	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Create a warm pool with 1 replica (index 1)
	_, err := r.ReconcileWarmPool(ctx, "test-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// Reconcile sandbox-1 with WarmPoolRef to claim index 1
	boxStatus, err := r.Reconcile(ctx, "sandbox-1", v1alpha1.PodBoxSpec{
		TemplateRef:      "base-tpl",
		WarmPoolRef:      "test-pool",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if boxStatus.RootfsPVCName != "test-pool-warm-1-rootfs-pvc" {
		t.Fatalf("unexpected rootfs PVC: %s", boxStatus.RootfsPVCName)
	}

	// Pause sandbox-1: PodName is cleared
	_, err = r.Pause(ctx, "sandbox-1")
	if err != nil {
		t.Fatalf("Pause failed: %v", err)
	}

	// Now scale up test-pool to 1 replica; it must not collide with index 1
	_, err = r.ReconcileWarmPool(ctx, "test-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    1,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool scale-up failed: %v", err)
	}

	entries := r.GetWarmPoolEntries("test-pool")
	var availEntry *controller.WarmPodEntry
	for _, e := range entries {
		if !e.Claimed {
			availEntry = e
			break
		}
	}
	if availEntry == nil {
		t.Fatal("expected available entry after scale-up")
	}
	if availEntry.ID != "test-pool-warm-2" {
		t.Fatalf("expected entryID test-pool-warm-2, got: %s", availEntry.ID)
	}
}

func envValue(envs []controller.EnvVar, name string) (string, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestReconcile_EgressSecretLifecycle(t *testing.T) {
	ctx := context.Background()
	customCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA failed: %v", err)
	}
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithEgressCA(customCA),
	)

	tpl := sampleTemplate()
	tpl.Egress.Enabled = true
	r.RegisterTemplate("egress-tpl", tpl)

	spec := v1alpha1.PodBoxSpec{
		TemplateRef:      "egress-tpl",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		NetworkPolicy: &v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			AllowHosts:    []string{"api.github.com"},
			UpstreamProxy: "http://upstream-proxy.internal:3128",
			SecretBindings: []v1alpha1.SecretBinding{
				{
					EnvName:      "GITHUB_TOKEN",
					Placeholder:  "cpodref_TOKEN123",
					AllowedHosts: []string{"api.github.com"},
				},
			},
		},
	}

	const boxName = "egress-lifecycle-box"
	if _, err := r.Reconcile(ctx, boxName, spec); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// 1. Verify <box>-egress-mtls Secret contents
	mtlsSec, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-mtls")
	if err != nil {
		t.Fatalf("expected egress-mtls secret: %v", err)
	}
	for _, key := range []string{"tls.crt", "tls.key", "ca.crt", "mitm-ca.crt", "mitm-ca.key", "policy.json"} {
		if len(mtlsSec.Data[key]) == 0 {
			t.Fatalf("expected non-empty %q in %s-egress-mtls", key, boxName)
		}
	}

	verifiedID, err := customCA.VerifySandboxCert(mtlsSec.Data["tls.crt"])
	if err != nil || verifiedID != boxName {
		t.Fatalf("VerifySandboxCert = (%q, %v); want (%q, nil)", verifiedID, err, boxName)
	}
	if string(mtlsSec.Data["ca.crt"]) != string(customCA.CACertPEM()) {
		t.Fatal("ca.crt in egress-mtls does not match configured EgressCA")
	}
	mitmCA, err := egress.LoadCA(mtlsSec.Data["mitm-ca.crt"], mtlsSec.Data["mitm-ca.key"])
	if err != nil {
		t.Fatalf("egress.LoadCA failed for generated MITM CA: %v", err)
	}
	if _, _, err := mitmCA.MintSandboxCert("test-leaf", time.Hour); err != nil {
		t.Fatalf("MITM CA failed to mint cert: %v", err)
	}
	var parsedPolicy v1alpha1.NetworkPolicySpec
	if err := json.Unmarshal(mtlsSec.Data["policy.json"], &parsedPolicy); err != nil {
		t.Fatalf("failed to unmarshal policy.json: %v", err)
	}
	if parsedPolicy.DefaultAction != v1alpha1.NetworkDefaultDeny || parsedPolicy.UpstreamProxy != "http://upstream-proxy.internal:3128" {
		t.Fatalf("unexpected parsed policy.json: %+v", parsedPolicy)
	}

	// 2. Verify <box>-egress-ca Secret contains ONLY mitm-ca.crt (never private keys)
	caSec, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-ca")
	if err != nil {
		t.Fatalf("expected egress-ca secret: %v", err)
	}
	if len(caSec.Data) != 1 || string(caSec.Data["mitm-ca.crt"]) != string(mtlsSec.Data["mitm-ca.crt"]) {
		t.Fatalf("expected egress-ca secret to contain only mitm-ca.crt, got keys=%v", caSec.Data)
	}
	if _, leaked := caSec.Data["mitm-ca.key"]; leaked {
		t.Fatal("mitm-ca.key must NEVER be present in egress-ca secret")
	}
	if _, leaked := caSec.Data["tls.key"]; leaked {
		t.Fatal("tls.key must NEVER be present in egress-ca secret")
	}

	// 3. Verify PodManifest wiring
	pod, ok := memKube.GetPod("cubepod-workloads", boxName+"-kata-pod")
	if !ok {
		t.Fatal("expected Pod manifest in MemoryKubeClient")
	}
	if len(pod.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(pod.Containers))
	}
	mainC, egressC := pod.Containers[0], pod.Containers[1]

	var sandboxMountedCA, sandboxMountedMTLS bool
	for _, vm := range mainC.VolumeMounts {
		if vm.Name == "egress-ca" && vm.MountPath == "/etc/cubepod/ca" && vm.ReadOnly {
			sandboxMountedCA = true
		}
		if vm.Name == "egress-mtls" || vm.MountPath == "/etc/cubepod/certs" {
			sandboxMountedMTLS = true
		}
	}
	if !sandboxMountedCA {
		t.Fatal("expected sandbox container to mount egress-ca read-only at /etc/cubepod/ca")
	}
	if sandboxMountedMTLS {
		t.Fatal("sandbox container must NOT mount egress-mtls")
	}
	for k, want := range map[string]string{
		"NODE_EXTRA_CA_CERTS":   "/etc/cubepod/ca/mitm-ca.crt",
		"CUBEPOD_EXTRA_CA_CERT": "/etc/cubepod/ca/mitm-ca.crt",
		"HTTP_PROXY":            "http://127.0.0.1:18080",
		"HTTPS_PROXY":           "http://127.0.0.1:18080",
		"NO_PROXY":              "127.0.0.1,localhost",
	} {
		if got, exists := envValue(mainC.Env, k); !exists || got != want {
			t.Fatalf("sandbox env %s = (%q, %v); want (%q, true)", k, got, exists, want)
		}
	}

	for k, want := range map[string]string{
		"CUBEPOD_POLICY_FILE":    "/etc/cubepod/certs/policy.json",
		"CUBEPOD_MITM_CA_CERT":   "/etc/cubepod/certs/mitm-ca.crt",
		"CUBEPOD_MITM_CA_KEY":    "/etc/cubepod/certs/mitm-ca.key",
		"CUBEPOD_UPSTREAM_PROXY": "http://upstream-proxy.internal:3128",
	} {
		if got, exists := envValue(egressC.Env, k); !exists || got != want {
			t.Fatalf("egress env %s = (%q, %v); want (%q, true)", k, got, exists, want)
		}
	}

	// 4. Pause garbage-collects both Secrets
	if _, err := r.Pause(ctx, boxName); err != nil {
		t.Fatalf("Pause failed: %v", err)
	}
	if _, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-mtls"); err == nil {
		t.Fatal("expected egress-mtls secret to be deleted on Pause")
	}
	if _, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-ca"); err == nil {
		t.Fatal("expected egress-ca secret to be deleted on Pause")
	}

	// 5. Resume re-creates Secrets with nil NetworkPolicy defaulting to "{}"
	spec.DesiredState = v1alpha1.DesiredStateRunning
	spec.NetworkPolicy = nil
	if _, err := r.Reconcile(ctx, boxName, spec); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	mtlsResumed, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-mtls")
	if err != nil {
		t.Fatalf("expected egress-mtls secret recreated on resume: %v", err)
	}
	if string(mtlsResumed.Data["policy.json"]) != "{}" {
		t.Fatalf("expected default policy.json {}, got %q", string(mtlsResumed.Data["policy.json"]))
	}
	if _, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-ca"); err != nil {
		t.Fatalf("expected egress-ca secret recreated on resume: %v", err)
	}

	// 6. DeleteBox garbage-collects both Secrets even when deleteVolumes is false
	if err := r.DeleteBox(ctx, boxName, false); err != nil {
		t.Fatalf("DeleteBox failed: %v", err)
	}
	if _, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-mtls"); err == nil {
		t.Fatal("expected egress-mtls secret deleted on DeleteBox")
	}
	if _, err := memKube.GetSecret(ctx, "cubepod-workloads", boxName+"-egress-ca"); err == nil {
		t.Fatal("expected egress-ca secret deleted on DeleteBox")
	}
}

func TestForkSandboxWithRootfsGeneration2(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r1 := controller.NewReconciler(controller.WithKubeClient(memKube))

	parentSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "ubuntu-node",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	if _, err := r1.Reconcile(ctx, "parent-gen", parentSpec); err != nil {
		t.Fatalf("initial parent reconcile failed: %v", err)
	}
	parentSpec.RootfsGeneration = 2
	if _, err := r1.Reconcile(ctx, "parent-gen", parentSpec); err != nil {
		t.Fatalf("gen2 parent reconcile failed: %v", err)
	}

	// Also verify resolution works from a second stateless Reconciler instance via GetPodBox.
	r2 := controller.NewReconciler(controller.WithKubeClient(memKube))
	childSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "ubuntu-node",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "parent-gen"},
	}
	if _, err := r2.Reconcile(ctx, "child-gen", childSpec); err != nil {
		t.Fatalf("child fork reconcile failed: %v", err)
	}

	childRootPVC, ok := memKube.GetPVC("cubepod-workloads", "child-gen-rootfs-pvc")
	if !ok {
		t.Fatal("expected child-gen-rootfs-pvc")
	}
	if childRootPVC.DataSourcePVC != "parent-gen-rootfs-gen2-pvc" {
		t.Fatalf("child rootfs DataSourcePVC = %q; want %q", childRootPVC.DataSourcePVC, "parent-gen-rootfs-gen2-pvc")
	}
	childWsPVC, ok := memKube.GetPVC("cubepod-workloads", "child-gen-workspace-pvc")
	if !ok {
		t.Fatal("expected child-gen-workspace-pvc")
	}
	if childWsPVC.DataSourcePVC != "parent-gen-workspace-pvc" {
		t.Fatalf("child workspace DataSourcePVC = %q; want %q", childWsPVC.DataSourcePVC, "parent-gen-workspace-pvc")
	}
}

func TestReconcile_PodBoxStatelessAnnotations(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(controller.WithKubeClient(memKube))

	parentSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "python-312",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 2,
	}
	if _, err := r.Reconcile(ctx, "ann-parent", parentSpec); err != nil {
		t.Fatalf("Reconcile parent failed: %v", err)
	}

	parentBox, err := memKube.GetPodBox(ctx, "cubepod-workloads", "ann-parent")
	if err != nil {
		t.Fatalf("GetPodBox failed: %v", err)
	}
	if got := parentBox.Metadata.Annotations["cubepod.io/template-id"]; got != "python-312" {
		t.Fatalf("template-id = %q; want python-312", got)
	}
	if got := parentBox.Metadata.Annotations["cubepod.io/rootfs-generation"]; got != "2" {
		t.Fatalf("rootfs-generation = %q; want 2", got)
	}
	if _, hasFork := parentBox.Metadata.Annotations["cubepod.io/forked-from"]; hasFork {
		t.Fatalf("non-forked box should not have cubepod.io/forked-from annotation")
	}

	// Simulate Gateway adding custom annotations on the PodBox and verify controller preserves them.
	parentBox.Metadata.Annotations["cubepod.io/timeout-seconds"] = "600"
	parentBox.Metadata.Annotations["cubepod.io/metadata-owner"] = "agent-1"
	if err := memKube.UpsertPodBox(ctx, *parentBox); err != nil {
		t.Fatalf("UpsertPodBox failed: %v", err)
	}
	if _, err := r.Pause(ctx, "ann-parent"); err != nil {
		t.Fatalf("Pause failed: %v", err)
	}
	pausedBox, err := memKube.GetPodBox(ctx, "cubepod-workloads", "ann-parent")
	if err != nil {
		t.Fatalf("GetPodBox after Pause failed: %v", err)
	}
	if pausedBox.Metadata.Annotations["cubepod.io/timeout-seconds"] != "600" ||
		pausedBox.Metadata.Annotations["cubepod.io/metadata-owner"] != "agent-1" {
		t.Fatalf("expected custom Gateway annotations preserved after Pause, got %v", pausedBox.Metadata.Annotations)
	}

	// Verify forked child persists cubepod.io/forked-from.
	childSpec := v1alpha1.PodBoxSpec{
		TemplateRef:      "python-312",
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "ann-parent"},
	}
	if _, err := r.Reconcile(ctx, "ann-child", childSpec); err != nil {
		t.Fatalf("Reconcile child failed: %v", err)
	}
	childBox, err := memKube.GetPodBox(ctx, "cubepod-workloads", "ann-child")
	if err != nil {
		t.Fatalf("GetPodBox child failed: %v", err)
	}
	if got := childBox.Metadata.Annotations["cubepod.io/forked-from"]; got != "ann-parent" {
		t.Fatalf("forked-from = %q; want ann-parent", got)
	}
	if got := childBox.Metadata.Annotations["cubepod.io/template-id"]; got != "python-312" {
		t.Fatalf("child template-id = %q; want python-312", got)
	}
	if got := childBox.Metadata.Annotations["cubepod.io/rootfs-generation"]; got != "1" {
		t.Fatalf("child rootfs-generation = %q; want 1", got)
	}
}

func TestReconciler_VolumeKeyPersistenceAndWarmPoolSkip(t *testing.T) {
	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithVolumeMode("Filesystem"),
	)

	r.RegisterTemplate("python-312", v1alpha1.PodTemplateSpec{
		Image: "python:3.12",
		Storage: v1alpha1.StorageSpec{
			StorageClassName: "local-path",
			RootfsSize:       "5Gi",
			WorkspaceSize:    "10Gi",
		},
	})

	// Pre-populate a warm pool with 1 ready standby entry.
	poolSpec := v1alpha1.PodWarmPoolSpec{
		TemplateRef: "python-312",
		Replicas:    1,
	}
	if _, err := r.ReconcileWarmPool(ctx, "fast-pool", poolSpec); err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}

	// Reconcile box-1 with BOTH WarmPoolRef and VolumeKey: warmpool must be skipped
	// and workspace PVC must be deterministic from VolumeKey.
	volKey := "Workspace_Alpha/Agent-7"
	spec1 := v1alpha1.PodBoxSpec{
		TemplateRef:      "python-312",
		WarmPoolRef:      "fast-pool",
		VolumeKey:        volKey,
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	st1, err := r.Reconcile(ctx, "cpod-vol-1", spec1)
	if err != nil {
		t.Fatalf("Reconcile box-1 failed: %v", err)
	}
	wantWSPVC := controller.WorkspacePVCName("cpod-vol-1", spec1)
	if wantWSPVC != "workspace-alpha-agent-7-workspace-pvc" {
		t.Fatalf("unexpected deterministic PVC name: %q", wantWSPVC)
	}
	if st1.WorkspacePVCName != wantWSPVC {
		t.Fatalf("st1.WorkspacePVCName = %q; want %q", st1.WorkspacePVCName, wantWSPVC)
	}
	if st1.Reason == "WarmPoolClaimed" {
		t.Fatalf("expected WarmPool claim to be skipped when VolumeKey is set, got reason=%q", st1.Reason)
	}
	if poolStatus, err := r.ReconcileWarmPool(ctx, "fast-pool", poolSpec); err != nil || poolStatus.AvailableReplicas != 1 {
		t.Fatalf("expected warm pool standby to remain unclaimed (available=1), got %+v err=%v", poolStatus, err)
	}

	// Delete box-1 with deleteVolumes=false -> rootfs PVC MUST be deleted, workspace PVC MUST stay.
	if err := r.DeleteBox(ctx, "cpod-vol-1", false); err != nil {
		t.Fatalf("DeleteBox(false) box-1 failed: %v", err)
	}
	if _, ok := memKube.GetPVC("cubepod-workloads", "cpod-vol-1-rootfs-pvc"); ok {
		t.Fatal("expected ephemeral cpod-vol-1-rootfs-pvc to be deleted on DeleteBox(..., false) for volumeKey box")
	}
	if _, ok := memKube.GetPVC("cubepod-workloads", wantWSPVC); !ok {
		t.Fatalf("expected deterministic workspace PVC %q to be retained on DeleteBox(..., false)", wantWSPVC)
	}

	// Reconcile box-2 with same VolumeKey -> reattaches the same deterministic workspace PVC.
	spec2 := spec1
	spec2.WarmPoolRef = ""
	st2, err := r.Reconcile(ctx, "cpod-vol-2", spec2)
	if err != nil {
		t.Fatalf("Reconcile box-2 failed: %v", err)
	}
	if st2.WorkspacePVCName != wantWSPVC {
		t.Fatalf("st2.WorkspacePVCName = %q; want %q", st2.WorkspacePVCName, wantWSPVC)
	}
	pod2, ok := memKube.GetPod("cubepod-workloads", st2.PodName)
	if !ok {
		t.Fatalf("expected pod %q for box-2", st2.PodName)
	}
	attachedWS := false
	for _, v := range pod2.Volumes {
		if v.Name == "workspace-volume" && v.PersistentVolumeClaim == wantWSPVC {
			attachedWS = true
		}
	}
	if !attachedWS {
		t.Fatalf("expected box-2 pod to mount retained PVC %q, got %+v", wantWSPVC, pod2.Volumes)
	}

	// Delete box-2 with deleteVolumes=true -> BOTH PVCs deleted.
	if err := r.DeleteBox(ctx, "cpod-vol-2", true); err != nil {
		t.Fatalf("DeleteBox(true) box-2 failed: %v", err)
	}
	if _, ok := memKube.GetPVC("cubepod-workloads", "cpod-vol-2-rootfs-pvc"); ok {
		t.Fatal("expected cpod-vol-2-rootfs-pvc deleted on DeleteBox(..., true)")
	}
	if _, ok := memKube.GetPVC("cubepod-workloads", wantWSPVC); ok {
		t.Fatalf("expected workspace PVC %q deleted on DeleteBox(..., true)", wantWSPVC)
	}
}
