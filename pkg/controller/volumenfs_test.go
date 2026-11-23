package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
)

func TestReconcileFirecrackerVolumesFailClosedWhenGateOff(t *testing.T) {
	r := controller.NewReconciler()
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{
		RuntimeClassName: "kata-fc",
		Image:            "ubuntu:24.04",
	})
	id := "0123456789abcdef0123456789abcdef"
	_, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef: "fc",
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "FirecrackerVolumeNFS") {
		t.Fatalf("expected fail-closed gate error, got %v", err)
	}
}

func TestReconcileFirecrackerVolumesPreRegistersAndBindsLease(t *testing.T) {
	nfs := volumenfs.NewServer(volumenfs.Config{VolumesDir: t.TempDir()})
	ts := httptest.NewServer(nfs.LeaseHTTPHandler())
	defer ts.Close()

	id := "0123456789abcdef0123456789abcdef"
	r := controller.NewReconciler(
		controller.WithVolumeNFS(controller.VolumeNFSSettings{
			Enabled:     true,
			ServiceName: "cubepod-volume-nfs",
			ServiceNS:   "cubepod-system",
			NFSPort:     2049,
			LeaseClient: volumenfs.NewClient(ts.URL, ts.Client()),
			Lookup: controller.FakeVolumeNFSLookup{
				ClusterIP: "10.96.10.20",
				Ready:     true,
			},
		}),
	)
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{
		RuntimeClassName: "kata-fc",
		Image:            "ubuntu:24.04",
	})
	status, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef: "fc",
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if status.PodIP == "" {
		t.Fatal("expected pod IP")
	}
	mem, ok := r.KubeClient().(*controller.MemoryKubeClient)
	if !ok {
		t.Fatal("expected MemoryKubeClient")
	}
	var pod controller.PodManifest
	found := false
	for _, p := range mem.Pods {
		if p.Name == status.PodName || strings.Contains(p.Name, "box-fc") {
			pod = p
			found = true
		}
	}
	// Memory stores by key; list pods
	if !found {
		for _, p := range mem.Pods {
			pod = p
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pod not stored: %+v", mem.Pods)
	}
	var env string
	for _, c := range pod.Containers {
		if c.Name != "sandbox" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "CUBEPOD_VOLUME_MOUNTS" {
				env = e.Value
			}
		}
		for _, m := range c.VolumeMounts {
			if strings.HasPrefix(m.MountPath, "/run/cubepod/volumes/") {
				t.Fatalf("pod attached official VolumeMount: %+v", c.VolumeMounts)
			}
		}
	}
	if !strings.Contains(env, `"transport":"nfs"`) {
		t.Fatalf("expected nfs transport in env, got %s", env)
	}
	var entries []map[string]string
	if err := json.Unmarshal([]byte(env), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0]["nfs_target"] == "" {
		t.Fatalf("entries=%v", entries)
	}
	leaseID := strings.Split(strings.TrimPrefix(entries[0]["nfs_target"], "10.96.10.20:/"), "/")[0]
	lease, ok := nfs.Store.Get(leaseID)
	if !ok {
		t.Fatalf("lease %s not stored", leaseID)
	}
	if !lease.Bound() || lease.PodIP != status.PodIP {
		t.Fatalf("lease should be bound to pod IP, got %+v status IP %s", lease, status.PodIP)
	}
	if len(lease.VolumeIDs) != 1 || lease.VolumeIDs[0] != id {
		t.Fatalf("volumeIDs=%v", lease.VolumeIDs)
	}

	if err := r.Delete(context.Background(), "box-fc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := nfs.Store.Get(leaseID); ok {
		t.Fatal("lease should be revoked on delete")
	}
}

func TestReconcileFirecrackerVolumesFailClosedWhenServiceNotReady(t *testing.T) {
	nfs := volumenfs.NewServer(volumenfs.Config{VolumesDir: t.TempDir()})
	ts := httptest.NewServer(nfs.LeaseHTTPHandler())
	defer ts.Close()
	id := "0123456789abcdef0123456789abcdef"
	r := controller.NewReconciler(
		controller.WithVolumeNFS(controller.VolumeNFSSettings{
			Enabled:     true,
			LeaseClient: volumenfs.NewClient(ts.URL, ts.Client()),
			Lookup:      controller.FakeVolumeNFSLookup{ClusterIP: "10.96.1.2", Ready: false},
		}),
	)
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	_, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ready endpoints") {
		t.Fatalf("expected service-not-ready error, got %v", err)
	}
}

func TestRegisterTemplateKeepsQemuVolumesWithoutNFS(t *testing.T) {
	r := controller.NewReconciler(controller.WithFirecrackerVolumeNFS(true))
	r.RegisterTemplate("qemu", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-qemu", Image: "ubuntu:24.04"})
	id := "0123456789abcdef0123456789abcdef"
	_, err := r.Reconcile(context.Background(), "box-qemu", v1alpha1.PodBoxSpec{
		TemplateRef:  "qemu",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	})
	if err != nil {
		t.Fatalf("qemu volumes should not require NFS: %v", err)
	}
}

func nfsTestSettings(t *testing.T, lookup controller.FakeVolumeNFSLookup) (*volumenfs.Server, *httptest.Server, controller.VolumeNFSSettings) {
	t.Helper()
	nfs := volumenfs.NewServer(volumenfs.Config{VolumesDir: t.TempDir()})
	ts := httptest.NewServer(nfs.LeaseHTTPHandler())
	t.Cleanup(ts.Close)
	if lookup.ClusterIP == "" {
		lookup.ClusterIP = "10.96.10.20"
	}
	if !lookup.Ready && lookup.Err == nil && lookup.ClusterIP != "" {
		// leave Ready as provided
	}
	return nfs, ts, controller.VolumeNFSSettings{
		Enabled:     true,
		ServiceName: "cubepod-volume-nfs",
		ServiceNS:   "cubepod-system",
		NFSPort:     2049,
		LeaseClient: volumenfs.NewClient(ts.URL, ts.Client()),
		Lookup:      lookup,
	}
}

func TestReconcileFirecrackerMultiVolumeSingleLease(t *testing.T) {
	id1 := "0123456789abcdef0123456789abcdef"
	id2 := "fedcba9876543210fedcba9876543210"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	status, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef: "fc",
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id1, PVCName: "cubepod-volumes", Path: "/data1", SubPath: id1 + "/data"},
			{VolumeID: id2, PVCName: "cubepod-volumes", Path: "/data2", SubPath: id2 + "/data"},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("expected one lease, got %d", nfs.Store.Active())
	}
	_ = status
	// Bind both volumeIDs on the single lease.
	var found bool
	nfs.Store.Active()
	// Walk via Get after reading env
	mem := r.KubeClient().(*controller.MemoryKubeClient)
	var env string
	for _, p := range mem.Pods {
		for _, c := range p.Containers {
			for _, e := range c.Env {
				if e.Name == "CUBEPOD_VOLUME_MOUNTS" {
					env = e.Value
				}
			}
		}
	}
	var entries []map[string]string
	if err := json.Unmarshal([]byte(env), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%v", entries)
	}
	leaseA := strings.Split(strings.TrimPrefix(entries[0]["nfs_target"], "10.96.10.20:/"), "/")[0]
	leaseB := strings.Split(strings.TrimPrefix(entries[1]["nfs_target"], "10.96.10.20:/"), "/")[0]
	if leaseA != leaseB || leaseA == "" {
		t.Fatalf("expected one shared lease, got %q %q", leaseA, leaseB)
	}
	lease, ok := nfs.Store.Get(leaseA)
	if !ok || len(lease.VolumeIDs) != 2 {
		t.Fatalf("lease=%+v ok=%v", lease, ok)
	}
	found = true
	if !found {
		t.Fatal("missing mounts env")
	}
}

func TestReconcileFirecrackerEnsurePodFailureRevokesLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	kube := &failEnsurePod{MemoryKubeClient: controller.NewMemoryKubeClient(), err: context.DeadlineExceeded}
	r := controller.NewReconciler(controller.WithKubeClient(kube), controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	_, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	})
	if err == nil {
		t.Fatal("expected EnsurePod failure")
	}
	if nfs.Store.Active() != 0 {
		t.Fatalf("lease should be revoked after EnsurePod failure, active=%d", nfs.Store.Active())
	}
}

func TestReconcileFirecrackerTimeoutRevokesBoundLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	kube := &hookThenTimeout{MemoryKubeClient: controller.NewMemoryKubeClient()}
	r := controller.NewReconciler(controller.WithKubeClient(kube), controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	_, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if nfs.Store.Active() != 0 {
		t.Fatalf("bound lease should be revoked after IP timeout, active=%d", nfs.Store.Active())
	}
}

func TestReconcileFirecrackerPauseResumeIssuesNewLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	status, err := r.Reconcile(context.Background(), "box-fc", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("active=%d", nfs.Store.Active())
	}
	first := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	spec.DesiredState = v1alpha1.DesiredStatePaused
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if nfs.Store.Active() != 0 {
		t.Fatalf("lease should be revoked on pause, active=%d", nfs.Store.Active())
	}
	spec.DesiredState = v1alpha1.DesiredStateRunning
	status, err = r.Reconcile(context.Background(), "box-fc", spec)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("phase=%s", status.Phase)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("expected new lease after resume, active=%d", nfs.Store.Active())
	}
	second := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if second == "" || second == first {
		t.Fatalf("resume must issue a new lease, first=%s second=%s", first, second)
	}
}

func TestReconcileAlreadyRunningSandboxDoesNotMintNewLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("reconnect must not mint a second lease, active=%d", nfs.Store.Active())
	}
	second := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if second != first {
		t.Fatalf("stored lease changed: first=%s second=%s", first, second)
	}
}

func TestReconcileCustomFirecrackerRuntimeClassUsesNFS(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{
		ClusterIP: "10.96.10.20",
		Ready:     true,
		Handler:   "kata-fc",
		Labels:    map[string]string{controller.LabelHypervisor: controller.HypervisorFirecracker},
	})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("custom", v1alpha1.PodTemplateSpec{RuntimeClassName: "custom-fc", Image: "ubuntu:24.04"})
	if _, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef:  "custom",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("custom Firecracker RuntimeClass should use NFS, active=%d", nfs.Store.Active())
	}
}

func TestReconcileRuntimeClassLookupErrorFailsClosed(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	_, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{
		ClusterIP:       "10.96.10.20",
		Ready:           true,
		RuntimeClassErr: context.DeadlineExceeded,
	})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("custom", v1alpha1.PodTemplateSpec{RuntimeClassName: "custom-fc", Image: "ubuntu:24.04"})
	_, err := r.Reconcile(context.Background(), "box-fc", v1alpha1.PodBoxSpec{
		TemplateRef:  "custom",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	})
	if err == nil || !strings.Contains(err.Error(), "RuntimeClass") {
		t.Fatalf("expected fail-closed RuntimeClass lookup error, got %v", err)
	}
}

type failEnsurePod struct {
	*controller.MemoryKubeClient
	err error
}

func (f *failEnsurePod) EnsurePod(context.Context, controller.PodManifest) (string, error) {
	return "", f.err
}

func (f *failEnsurePod) EnsurePodWithHook(context.Context, controller.PodManifest, controller.PodIPHook) (string, error) {
	return "", f.err
}

type hookThenTimeout struct {
	*controller.MemoryKubeClient
}

func (h *hookThenTimeout) EnsurePod(ctx context.Context, pod controller.PodManifest) (string, error) {
	return "", context.DeadlineExceeded
}

func (h *hookThenTimeout) EnsurePodWithHook(ctx context.Context, pod controller.PodManifest, hook controller.PodIPHook) (string, error) {
	if hook != nil {
		if err := hook(ctx, pod.Namespace, pod.Name, "10.244.8.8"); err != nil {
			return "10.244.8.8", err
		}
	}
	return "10.244.8.8", context.DeadlineExceeded
}

func leaseIDFromReconciler(t *testing.T, r *controller.Reconciler, prefix string) string {
	t.Helper()
	mem := r.KubeClient().(*controller.MemoryKubeClient)
	for _, p := range mem.Pods {
		for _, c := range p.Containers {
			for _, e := range c.Env {
				if e.Name != "CUBEPOD_VOLUME_MOUNTS" || e.Value == "" {
					continue
				}
				var entries []map[string]string
				if err := json.Unmarshal([]byte(e.Value), &entries); err != nil {
					t.Fatal(err)
				}
				if len(entries) == 0 {
					continue
				}
				return strings.Split(strings.TrimPrefix(entries[0]["nfs_target"], prefix), "/")[0]
			}
		}
	}
	t.Fatal("CUBEPOD_VOLUME_MOUNTS not found")
	return ""
}

func TestReconcileEvictedFirecrackerPodMintsNewLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	mem := r.KubeClient().(*controller.MemoryKubeClient)
	for key := range mem.Pods {
		delete(mem.Pods, key)
		delete(mem.IPs, key)
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("reconcile after eviction: %v", err)
	}
	if nfs.Store.Active() != 1 {
		t.Fatalf("expected a single replacement lease, active=%d", nfs.Store.Active())
	}
	second := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if second == "" || second == first {
		t.Fatalf("evicted pod must get a new lease, first=%s second=%s", first, second)
	}
	var env string
	for _, p := range mem.Pods {
		for _, c := range p.Containers {
			for _, e := range c.Env {
				if e.Name == "CUBEPOD_VOLUME_MOUNTS" {
					env = e.Value
				}
			}
			for _, m := range c.VolumeMounts {
				if strings.HasPrefix(m.MountPath, "/run/cubepod/volumes/") {
					t.Fatalf("replacement Firecracker pod attached Kata VolumeMount: %+v", c.VolumeMounts)
				}
			}
		}
	}
	if !strings.Contains(env, `"transport":"nfs"`) {
		t.Fatalf("replacement pod missing nfs transport: %s", env)
	}
}

func TestReconcileResumePrepareErrorKeepsPausedPhase(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, ts, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = nfs
	spec.DesiredState = v1alpha1.DesiredStatePaused
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("pause: %v", err)
	}
	st, ok := r.GetBox("box-fc")
	if !ok || st.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected paused, got %+v ok=%v", st, ok)
	}
	ts.Close()
	spec.DesiredState = v1alpha1.DesiredStateRunning
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err == nil {
		t.Fatal("expected resume to fail with lease API down")
	}
	st, ok = r.GetBox("box-fc")
	if !ok || st.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("failed resume must leave phase Paused, got %+v ok=%v", st, ok)
	}
}

func TestReconcileFailedPodMintsNewLease(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	status, err := r.Reconcile(context.Background(), "box-fc", spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	mem := r.KubeClient().(*controller.MemoryKubeClient)
	if mem.PodPhases == nil {
		mem.PodPhases = map[string]string{}
	}
	// Mark the existing Pod Failed so Reconcile must replace it.
	for key := range mem.Pods {
		mem.PodPhases[key] = "Failed"
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("reconcile failed pod: %v", err)
	}
	_ = status
	if nfs.Store.Active() != 1 {
		t.Fatalf("expected one live lease, active=%d", nfs.Store.Active())
	}
	second := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if second == "" || second == first {
		t.Fatalf("Failed pod must get a new lease, first=%s second=%s", first, second)
	}
}

func TestReconcileTransientGetPodPhaseErrorKeepsLeaseAndPod(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	nfs, _, settings := nfsTestSettings(t, controller.FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true})
	r := controller.NewReconciler(controller.WithVolumeNFS(settings))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	first := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	mem := r.KubeClient().(*controller.MemoryKubeClient)
	podCount := len(mem.Pods)
	mem.GetPodPhaseErr = errors.New("apiserver temporarily unavailable")
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err == nil {
		t.Fatal("expected transient GetPodPhase error")
	}
	mem.GetPodPhaseErr = nil
	if nfs.Store.Active() != 1 {
		t.Fatalf("transient GetPodPhase error must not revoke the lease, active=%d", nfs.Store.Active())
	}
	second := leaseIDFromReconciler(t, r, "10.96.10.20:/")
	if second != first {
		t.Fatalf("transient error must not mint a new lease, first=%s second=%s", first, second)
	}
	if len(mem.Pods) != podCount {
		t.Fatalf("transient error must not replace the live pod, before=%d after=%d", podCount, len(mem.Pods))
	}
	st, ok := r.GetBox("box-fc")
	if !ok || st.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("box must stay Running, got %+v ok=%v", st, ok)
	}
}
