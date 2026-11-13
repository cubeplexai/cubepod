package controller_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
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
