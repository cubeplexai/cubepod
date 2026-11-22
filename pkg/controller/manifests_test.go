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
	status := v1alpha1.PodBoxStatus{PodName: "pull-box"}

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
	reconciledPod, ok := memKube.GetPod("cubepod-workloads", "pull-reconcile-box")
	if !ok {
		t.Fatal("expected pod in MemoryKubeClient")
	}
	if reconciledPod.Name != "pull-reconcile-box" {
		t.Fatalf("expected pod name pull-reconcile-box without -kata-pod suffix, got %s", reconciledPod.Name)
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
	pod := controller.BuildKataPod("box-label-1", tpl, spec, v1alpha1.PodBoxStatus{PodName: "box-label-1"})
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
	pod := controller.BuildKataPod("box-egress-1", tpl, spec, v1alpha1.PodBoxStatus{PodName: "box-egress-1"})

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

func TestBuildKataPod_VolumeMounts(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	pvc := "vol-" + id
	spec := v1alpha1.PodBoxSpec{
		TemplateRef: "base",
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id, PVCName: pvc, Path: "/mnt/cpod-test-vol"},
		},
	}
	pod := controller.BuildKataPod("box-vol", v1alpha1.PodTemplateSpec{Image: "ubuntu:24.04"}, spec, v1alpha1.PodBoxStatus{}, "Filesystem")
	var sawVol, sawWS bool
	for _, v := range pod.Volumes {
		if v.Name == pvc && v.PersistentVolumeClaim == pvc {
			sawVol = true
		}
		if v.Name == "workspace-volume" {
			sawWS = true
		}
	}
	if !sawVol || !sawWS {
		t.Fatalf("expected volume PVC %s and workspace volume, got %+v", pvc, pod.Volumes)
	}
	var main *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			main = &pod.Containers[i]
			break
		}
	}
	if main == nil {
		t.Fatal("missing sandbox container")
	}
	stagingPath := "/run/cubepod/volumes/" + id
	finalPath := "/mnt/cpod-test-vol"
	var sawMount, sawDirect, sawWorkspace bool
	for _, m := range main.VolumeMounts {
		if m.Name == pvc && m.MountPath == stagingPath {
			sawMount = true
		}
		if m.Name == pvc && m.MountPath == finalPath {
			sawDirect = true
		}
		if m.MountPath == "/workspace" {
			sawWorkspace = true
		}
	}
	if !sawMount || !sawDirect || !sawWorkspace {
		t.Fatalf("expected staging mount %s, final path %s, and /workspace, got %+v", stagingPath, finalPath, main.VolumeMounts)
	}
	wantEnv := `[{"volume_id":"` + id + `","staging_path":"` + stagingPath + `","target_path":"/mnt/cpod-test-vol"}]`
	var sawEnv bool
	for _, e := range main.Env {
		if e.Name == "CUBEPOD_VOLUME_MOUNTS" && e.Value == wantEnv {
			sawEnv = true
		}
	}
	if !sawEnv {
		t.Fatalf("expected CUBEPOD_VOLUME_MOUNTS=%s, got env=%+v", wantEnv, main.Env)
	}
}

func TestBuildKataPod_VolumeMountsWithBlockWorkspace(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	pvc := "vol-" + id
	spec := v1alpha1.PodBoxSpec{
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: pvc, Path: "/mnt/data"}},
	}
	pod := controller.BuildKataPod("box-block", v1alpha1.PodTemplateSpec{Image: "ubuntu:24.04", RuntimeClassName: "kata-qemu"}, spec, v1alpha1.PodBoxStatus{}, "Block")
	var main *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			main = &pod.Containers[i]
			break
		}
	}
	if main == nil {
		t.Fatal("missing sandbox container")
	}
	stagingPath := "/run/cubepod/volumes/" + id
	var sawFS bool
	for _, m := range main.VolumeMounts {
		if m.Name == pvc && m.MountPath == stagingPath {
			sawFS = true
		}
	}
	if !sawFS {
		t.Fatalf("expected filesystem staging mount %s alongside block devices, mounts=%+v devices=%+v", stagingPath, main.VolumeMounts, main.VolumeDevices)
	}
	if len(main.VolumeDevices) < 2 {
		t.Fatalf("expected workspace/rootfs block devices to remain, got %+v", main.VolumeDevices)
	}
}

func TestBuildKataPod_SharedRWXPVCDeduplicationAndSubPath(t *testing.T) {
	id1 := "0123456789abcdef0123456789abcdef"
	id2 := "fedcba9876543210fedcba9876543210"
	sharedPVC := "cubepod-volumes"
	spec := v1alpha1.PodBoxSpec{
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id1, PVCName: sharedPVC, SubPath: id1 + "/data", Path: "/mnt/ds1"},
			{VolumeID: id2, PVCName: sharedPVC, SubPath: id2 + "/data", Path: "/mnt/ds2"},
		},
	}
	pod := controller.BuildKataPod("box-shared", v1alpha1.PodTemplateSpec{Image: "ubuntu:24.04", RuntimeClassName: "kata-qemu"}, spec, v1alpha1.PodBoxStatus{}, "Block")
	pvcCount := 0
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim == sharedPVC {
			pvcCount++
		}
	}
	if pvcCount != 1 {
		t.Fatalf("expected shared PVC %s to be added once to pod.Volumes, got %d (%+v)", sharedPVC, pvcCount, pod.Volumes)
	}
	var main *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			main = &pod.Containers[i]
			break
		}
	}
	if main == nil {
		t.Fatal("missing sandbox container")
	}
	var m1, m2 bool
	for _, m := range main.VolumeMounts {
		if m.Name == sharedPVC && m.MountPath == "/run/cubepod/volumes/"+id1 && m.SubPath == id1+"/data" {
			m1 = true
		}
		if m.Name == sharedPVC && m.MountPath == "/run/cubepod/volumes/"+id2 && m.SubPath == id2+"/data" {
			m2 = true
		}
	}
	if !m1 || !m2 {
		t.Fatalf("expected both subPath staging mounts on shared PVC, got %+v", main.VolumeMounts)
	}
	wantEnv := `[{"volume_id":"` + id1 + `","staging_path":"/run/cubepod/volumes/` + id1 + `","target_path":"/mnt/ds1"},{"volume_id":"` + id2 + `","staging_path":"/run/cubepod/volumes/` + id2 + `","target_path":"/mnt/ds2"}]`
	var sawEnv bool
	for _, e := range main.Env {
		if e.Name == "CUBEPOD_VOLUME_MOUNTS" && e.Value == wantEnv {
			sawEnv = true
		}
	}
	if !sawEnv {
		t.Fatalf("expected CUBEPOD_VOLUME_MOUNTS=%s, got env=%+v", wantEnv, main.Env)
	}
}

func TestValidVolumeContainerPath(t *testing.T) {
	allow := []string{"/mnt/cpod-test-vol", "/mnt/data", "/data", "/mnt/a..b"}
	deny := []string{"/", "/workspace", "/workspace/sub", "/rootfs-upper", "/rootfs-upper/x", "/etc/passwd", "/proc", "/sys", "/dev/sda", "/opt/cubepod/bin", "/mnt/cubepod/rootfs", "/usr/bin", "/sbin/init", "/mnt/../x", ""}
	for _, p := range allow {
		if !v1alpha1.ValidVolumeContainerPath(p) {
			t.Fatalf("expected allow %q", p)
		}
	}
	for _, p := range deny {
		if v1alpha1.ValidVolumeContainerPath(p) {
			t.Fatalf("expected deny %q", p)
		}
	}
}

func TestValidateVolumeMounts_RejectsInvalid(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	cases := [][]v1alpha1.VolumeMount{
		{{VolumeID: "bad", PVCName: "vol-bad", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "vol-wrong", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "vol-" + id, Path: "/rootfs-upper"}},
		{
			{VolumeID: id, PVCName: "vol-" + id, Path: "/mnt/dup"},
			{VolumeID: id, PVCName: "vol-" + id, Path: "/mnt/dup"},
		},
		{{VolumeID: id, PVCName: "cubepod-volumes", SubPath: "/abs/path", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "cubepod-volumes", SubPath: "../escape", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "cubepod-volumes", SubPath: "a/../b", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "", SubPath: id + "/data", Path: "/mnt/data"}},
		{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/mnt/data"}},
	}
	for _, tc := range cases {
		if err := controller.ValidateVolumeMounts(tc); err == nil {
			t.Fatalf("expected error for invalid mounts %+v", tc)
		}
	}
}

func TestBuildPodManifest_TransparentEgress(t *testing.T) {
	tpl := sampleTemplate()
	tpl.Egress = v1alpha1.EgressConfig{
		Enabled:     true,
		Image:       "cubepod-egress:v0.6.0",
		ExchangeURL: "https://exchange.internal:8443",
	}
	spec := v1alpha1.PodBoxSpec{RootfsGeneration: 1}
	status := v1alpha1.PodBoxStatus{PodName: "trans-box"}

	findEgress := func(pod controller.PodManifest) controller.ContainerManifest {
		for _, c := range pod.Containers {
			if c.Name == "egress" {
				return c
			}
		}
		t.Fatal("egress container not found")
		return controller.ContainerManifest{}
	}

	// 1. TransparentEgress = true -> CUBEPOD_TRANSPARENT=1 and NET_ADMIN capability
	podOn := controller.BuildPodManifest("trans-box", tpl, spec, status, controller.PodBuildOptions{
		VolumeMode:        "Block",
		TransparentEgress: true,
	})
	egressOn := findEgress(podOn)
	var hasTransparentEnv bool
	for _, ev := range egressOn.Env {
		if ev.Name == "CUBEPOD_TRANSPARENT" && ev.Value == "1" {
			hasTransparentEnv = true
		}
	}
	if !hasTransparentEnv {
		t.Fatalf("expected CUBEPOD_TRANSPARENT=1 in egress env, got %+v", egressOn.Env)
	}
	if egressOn.SecurityContext == nil || egressOn.SecurityContext.Capabilities == nil ||
		!slices.Contains(egressOn.SecurityContext.Capabilities.Add, "NET_ADMIN") {
		t.Fatalf("expected NET_ADMIN capability on egress container, got %+v", egressOn.SecurityContext)
	}

	// 2. TransparentEgress = false -> neither CUBEPOD_TRANSPARENT nor NET_ADMIN
	podOff := controller.BuildPodManifest("trans-box", tpl, spec, status, controller.PodBuildOptions{
		VolumeMode:        "Block",
		TransparentEgress: false,
	})
	egressOff := findEgress(podOff)
	for _, ev := range egressOff.Env {
		if ev.Name == "CUBEPOD_TRANSPARENT" {
			t.Fatalf("did not expect CUBEPOD_TRANSPARENT when TransparentEgress=false, got %+v", ev)
		}
	}
	if egressOff.SecurityContext != nil {
		t.Fatalf("expected nil SecurityContext when TransparentEgress=false, got %+v", egressOff.SecurityContext)
	}
}

func TestBuildBlockPVCsWithOptions_VolumeSnapshotDataSource(t *testing.T) {
	tpl := sampleTemplate()
	spec := v1alpha1.PodBoxSpec{
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "parent-box"},
	}

	rootfsPVC, wsPVC := controller.BuildBlockPVCsWithOptions("child-box", tpl, spec, controller.BlockPVCOptions{
		VolumeMode:            "Block",
		RootfsSnapshotName:    "child-box-rootfs-snap",
		WorkspaceSnapshotName: "child-box-workspace-snap",
	})
	if rootfsPVC.DataSourceSnapshot != "child-box-rootfs-snap" ||
		rootfsPVC.DataSourceKind != "VolumeSnapshot" ||
		rootfsPVC.DataSourceAPIGroup != "snapshot.storage.k8s.io" ||
		rootfsPVC.DataSourcePVC != "" {
		t.Fatalf("unexpected rootfsPVC snapshot dataSource: %+v", rootfsPVC)
	}
	if wsPVC.DataSourceSnapshot != "child-box-workspace-snap" ||
		wsPVC.DataSourceKind != "VolumeSnapshot" ||
		wsPVC.DataSourceAPIGroup != "snapshot.storage.k8s.io" ||
		wsPVC.DataSourcePVC != "" {
		t.Fatalf("unexpected wsPVC snapshot dataSource: %+v", wsPVC)
	}
}

// The container baseline must not need CAP_SYS_ADMIN just to mount an E2B PVC.
func TestBuildKataPod_OfficialMountsWithoutPivot(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	for _, runtimeClass := range []string{"", "none", "-"} {
		t.Run("runtime="+runtimeClass, func(t *testing.T) {
			shared := "cubepod-volumes"
			subPath := id + "/data"
			spec := v1alpha1.PodBoxSpec{VolumeMounts: []v1alpha1.VolumeMount{
				{VolumeID: id, PVCName: shared, SubPath: subPath, Path: "/data"},
			}}
			pod := controller.BuildKataPod("box-container", v1alpha1.PodTemplateSpec{RuntimeClassName: runtimeClass}, spec, v1alpha1.PodBoxStatus{}, "Filesystem")
			main := pod.Containers[0]
			if main.Privileged || main.SecurityContext != nil {
				t.Fatal("standard volume mounting must not grant additional privileges")
			}
			paths := map[string]bool{}
			for _, mount := range main.VolumeMounts {
				if mount.Name == shared && mount.SubPath == subPath {
					paths[mount.MountPath] = true
				}
			}
			if !paths["/data"] || !paths["/run/cubepod/volumes/"+id] {
				t.Fatalf("expected both final and staging paths for identical volume, got %+v", main.VolumeMounts)
			}
		})
	}
}
