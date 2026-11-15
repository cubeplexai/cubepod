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

func TestSanitizeLabelValue_AndWarmPoolTemplateLabels(t *testing.T) {
	labelRe := regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"simple-pool_1.0", "simple-pool_1.0"},
		{"org/pool:v1.0", "org-pool-v1.0"},
		{"---__abc..def---", "abc..def"},
		{"///:::", ""},
		{strings.Repeat("a", 62) + "-bbbb", strings.Repeat("a", 62)},
	}
	for _, tc := range cases {
		got := controller.SanitizeLabelValue(tc.in)
		if got != tc.want {
			t.Fatalf("SanitizeLabelValue(%q) = %q; want %q", tc.in, got, tc.want)
		}
		if got != "" {
			if len(got) > 63 || !labelRe.MatchString(got) {
				t.Fatalf("SanitizeLabelValue(%q) produced invalid label %q", tc.in, got)
			}
		}
	}

	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		WarmPoolRef:      "org/pool:v1",
		TemplateRef:      "--repo/ubuntu:24.04--",
		RootfsGeneration: 1,
	}
	pod := controller.BuildKataPod("box-label-1", tpl, spec, v1alpha1.PodBoxStatus{PodName: "box-label-1-kata-pod"})
	if pod.Labels["cubepod.io/warmpool"] != "org-pool-v1" {
		t.Fatalf("warmpool label = %q; want org-pool-v1", pod.Labels["cubepod.io/warmpool"])
	}
	if pod.Labels["cubepod.io/template"] != "repo-ubuntu-24.04" {
		t.Fatalf("template label = %q; want repo-ubuntu-24.04", pod.Labels["cubepod.io/template"])
	}
}

func TestBuildKataPod_EgressEnvVars(t *testing.T) {
	tpl := sampleTemplate()
	tpl.Egress = v1alpha1.EgressConfig{
		Enabled:     true,
		Image:       "egress:v1",
		ExchangeURL: "https://192.168.1.215:9443/api/v1/internal/egress/exchange",
	}
	spec := v1alpha1.PodBoxSpec{RootfsGeneration: 1}
	pod := controller.BuildKataPod("box-egress-1", tpl, spec, v1alpha1.PodBoxStatus{PodName: "box-egress-1-kata-pod"})

	var mainC *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			mainC = &pod.Containers[i]
			break
		}
	}
	if mainC == nil {
		t.Fatal("main container not found")
	}
	envMap := make(map[string]string, len(mainC.Env))
	for _, e := range mainC.Env {
		envMap[e.Name] = e.Value
	}

	wantEnvs := map[string]string{
		"NODE_EXTRA_CA_CERTS":   "/etc/cubepod/ca/mitm-ca.crt",
		"CUBEPOD_EXTRA_CA_CERT": "/etc/cubepod/ca/mitm-ca.crt",
		"SSL_CERT_FILE":         "/etc/ssl/certs/ca-certificates.crt",
		"REQUESTS_CA_BUNDLE":    "/etc/ssl/certs/ca-certificates.crt",
		"CURL_CA_BUNDLE":        "/etc/ssl/certs/ca-certificates.crt",
		"HTTP_PROXY":            "http://127.0.0.1:18080",
		"HTTPS_PROXY":           "http://127.0.0.1:18080",
		"NO_PROXY":              "127.0.0.1,localhost",
		"http_proxy":            "http://127.0.0.1:18080",
		"https_proxy":           "http://127.0.0.1:18080",
		"no_proxy":              "127.0.0.1,localhost",
	}
	for k, want := range wantEnvs {
		if got := envMap[k]; got != want {
			t.Errorf("main env[%q] = %q; want %q", k, got, want)
		}
	}
}
