package controller_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

func TestBuildKataPod_NoHardcodedImagePullSecrets(t *testing.T) {
	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{RootfsGeneration: 1}
	status := v1alpha1.PodBoxStatus{PodName: "pull-box-kata-pod"}

	podDefault := controller.BuildKataPod("pull-box", tpl, spec, status)
	if len(podDefault.ImagePullSecrets) != 0 {
		t.Fatalf("expected no hardcoded imagePullSecrets, got %v", podDefault.ImagePullSecrets)
	}

	podCustom := controller.BuildKataPod("pull-box", tpl, spec, status, "Block", "cubepod-template-src-123", "my-reg-secret,second-secret", "my-reg-secret")
	want := []string{"my-reg-secret", "second-secret"}
	if !slices.Equal(podCustom.ImagePullSecrets, want) {
		t.Fatalf("ImagePullSecrets = %v; want %v", podCustom.ImagePullSecrets, want)
	}

	ctx := context.Background()
	memKube := controller.NewMemoryKubeClient()
	r := controller.NewReconciler(
		controller.WithKubeClient(memKube),
		controller.WithTemplatePullSecret("tpl-pull"),
		controller.WithDefaultImagePullSecrets("cluster-pull-1,cluster-pull-2", "tpl-pull"),
	)
	if _, err := r.Reconcile(ctx, "pull-reconcile-box", v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	reconciledPod, ok := memKube.GetPod("cubepod-workloads", "pull-reconcile-box-kata-pod")
	if !ok {
		t.Fatal("expected pod in MemoryKubeClient")
	}
	wantReconciled := []string{"tpl-pull", "cluster-pull-1", "cluster-pull-2"}
	if !slices.Equal(reconciledPod.ImagePullSecrets, wantReconciled) {
		t.Fatalf("reconciled ImagePullSecrets = %v; want %v", reconciledPod.ImagePullSecrets, wantReconciled)
	}
}

func TestWorkspacePVCName_NormalizationAndLengthCap(t *testing.T) {
	dns1123 := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	cases := []struct {
		name      string
		boxName   string
		volumeKey string
		wantExact string
	}{
		{
			name:      "empty volumeKey falls back to boxName",
			boxName:   "cpod-abc",
			volumeKey: "",
			wantExact: "cpod-abc-workspace-pvc",
		},
		{
			name:      "normalizes uppercase and invalid chars",
			boxName:   "cpod-1",
			volumeKey: "--WS_User/Alpha@123--",
			wantExact: "ws-user-alpha-123-workspace-pvc",
		},
		{
			name:      "all non-alphanumeric still valid DNS-1123",
			boxName:   "cpod-2",
			volumeKey: "///---///",
		},
		{
			name:      "exact 49 char prefix does not truncate",
			boxName:   "cpod-3",
			volumeKey: strings.Repeat("a", 49),
			wantExact: strings.Repeat("a", 49) + "-workspace-pvc",
		},
		{
			name:      "over 49 char prefix truncates and hashes within 63 chars",
			boxName:   "cpod-4",
			volumeKey: "org-123-ws-456-" + strings.Repeat("z", 50),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := controller.WorkspacePVCName(tc.boxName, v1alpha1.PodBoxSpec{VolumeKey: tc.volumeKey})
			if len(got) > 63 {
				t.Fatalf("WorkspacePVCName length = %d (> 63): %q", len(got), got)
			}
			if !dns1123.MatchString(got) {
				t.Fatalf("WorkspacePVCName %q is not valid DNS-1123", got)
			}
			if tc.wantExact != "" && got != tc.wantExact {
				t.Fatalf("WorkspacePVCName = %q, want %q", got, tc.wantExact)
			}
		})
	}

	longA := controller.WorkspacePVCName("b1", v1alpha1.PodBoxSpec{VolumeKey: strings.Repeat("x", 60) + "1"})
	longB := controller.WorkspacePVCName("b1", v1alpha1.PodBoxSpec{VolumeKey: strings.Repeat("x", 60) + "2"})
	if longA == longB {
		t.Fatalf("expected distinct hashed names for long volumeKeys, both got %q", longA)
	}

	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
		VolumeKey:        "Tenant_1:Agent-Main",
	}
	rootPVC, wsPVC := controller.BuildBlockPVCs("cpod-9", tpl, spec, "Filesystem")
	if rootPVC.Name != "cpod-9-rootfs-pvc" {
		t.Fatalf("rootfs PVC = %q, want cpod-9-rootfs-pvc", rootPVC.Name)
	}
	wantWS := "tenant-1-agent-main-workspace-pvc"
	if wsPVC.Name != wantWS {
		t.Fatalf("workspace PVC = %q, want %q", wsPVC.Name, wantWS)
	}
	pod := controller.BuildKataPod("cpod-9", tpl, spec, v1alpha1.PodBoxStatus{}, "Filesystem")
	foundWS := false
	for _, v := range pod.Volumes {
		if v.Name == "workspace-volume" && v.PersistentVolumeClaim == wantWS {
			foundWS = true
		}
	}
	if !foundWS {
		t.Fatalf("expected pod workspace-volume claim %q, got %+v", wantWS, pod.Volumes)
	}
}
