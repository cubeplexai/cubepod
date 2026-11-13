package controller_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

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
