package controller_test

import (
	"context"
	"slices"
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
