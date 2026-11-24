package controller_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

func TestBuildPodManifest_FirecrackerOmitsOfficialVolumeMountsAndEmitsNFS(t *testing.T) {
	id1 := "0123456789abcdef0123456789abcdef"
	id2 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	leaseID := "00112233445566778899aabbccddeeff"
	spec := v1alpha1.PodBoxSpec{
		TemplateRef: "base",
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id1, PVCName: "cubepod-volumes", Path: "/data", SubPath: id1 + "/data"},
			{VolumeID: id2, PVCName: "cubepod-volumes", Path: "/models", SubPath: id2 + "/data"},
		},
	}
	tpl := v1alpha1.PodTemplateSpec{
		Image:            "ubuntu:24.04",
		RuntimeClassName: "kata-fc",
		Egress:           v1alpha1.EgressConfig{Enabled: true, Image: "cubepod-egress:test"},
	}
	pod := controller.BuildPodManifest("box-fc", tpl, spec, v1alpha1.PodBoxStatus{PodName: "box-fc"}, controller.PodBuildOptions{
		VolumeMode:        "Block",
		TransparentEgress: true,
		VolumeNFS: controller.VolumeNFSOptions{
			ClusterIP: "10.96.10.20",
			Port:      2049,
			LeaseID:   leaseID,
		},
	})
	if pod.RuntimeClassName != "kata-fc" {
		t.Fatalf("runtime class: %s", pod.RuntimeClassName)
	}
	var main, egress *controller.ContainerManifest
	for i := range pod.Containers {
		switch pod.Containers[i].Name {
		case "sandbox":
			main = &pod.Containers[i]
		case "egress":
			egress = &pod.Containers[i]
		}
	}
	if main == nil {
		t.Fatal("missing sandbox container")
	}
	for _, m := range main.VolumeMounts {
		if strings.HasPrefix(m.MountPath, "/run/cubepod/volumes/") || m.MountPath == "/data" || m.MountPath == "/models" {
			t.Fatalf("Firecracker pod must not attach official volume as VolumeMount, got %+v", main.VolumeMounts)
		}
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim == "cubepod-volumes" {
			t.Fatalf("Firecracker pod must not attach cubepod-volumes PVC, got %+v", pod.Volumes)
		}
		if v.Name == "workspace-volume" || v.Name == "rootfs-volume" {
			continue
		}
	}
	var envVal string
	for _, e := range main.Env {
		if e.Name == "CUBEPOD_VOLUME_MOUNTS" {
			envVal = e.Value
		}
	}
	if envVal == "" {
		t.Fatal("expected CUBEPOD_VOLUME_MOUNTS")
	}
	var entries []map[string]string
	if err := json.Unmarshal([]byte(envVal), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %s", envVal)
	}
	seen := map[string]string{}
	for _, e := range entries {
		if e["transport"] != "nfs" {
			t.Fatalf("expected transport nfs, got %v", e)
		}
		if e["staging_path"] != "" {
			t.Fatalf("nfs entry must omit staging_path: %v", e)
		}
		if !strings.HasPrefix(e["nfs_target"], "10.96.10.20:/"+leaseID+"/") {
			t.Fatalf("unexpected nfs_target %q", e["nfs_target"])
		}
		if e["nfs_target"] != "10.96.10.20:/"+leaseID+"/"+e["volume_id"] {
			t.Fatalf("nfs_target must share one leaseID: %v", e)
		}
		seen[e["volume_id"]] = e["target_path"]
	}
	if seen[id1] != "/data" || seen[id2] != "/models" {
		t.Fatalf("unexpected targets: %v", seen)
	}
	if egress == nil {
		t.Fatal("expected egress sidecar")
	}
	var nfsAddr string
	for _, e := range egress.Env {
		if e.Name == "CUBEPOD_VOLUME_NFS_ADDR" {
			nfsAddr = e.Value
		}
	}
	if nfsAddr != "10.96.10.20:2049" {
		t.Fatalf("egress NFS exemption addr: %q", nfsAddr)
	}
}

func TestBuildPodManifest_FirecrackerWithoutNFSOmitsVolumeMounts(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	spec := v1alpha1.PodBoxSpec{
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"},
		},
	}
	pod := controller.BuildKataPod("box-fc", v1alpha1.PodTemplateSpec{
		Image:            "ubuntu:24.04",
		RuntimeClassName: "kata-fc-e2e",
	}, spec, v1alpha1.PodBoxStatus{}, "Block")
	var main *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			main = &pod.Containers[i]
		}
	}
	if main == nil {
		t.Fatal("missing sandbox")
	}
	for _, m := range main.VolumeMounts {
		if strings.Contains(m.MountPath, "/run/cubepod/volumes/") || m.MountPath == "/data" {
			t.Fatalf("must not attach official volume copy: %+v", main.VolumeMounts)
		}
	}
	for _, e := range main.Env {
		if e.Name == "CUBEPOD_VOLUME_MOUNTS" {
			t.Fatalf("incomplete NFS config must not emit CUBEPOD_VOLUME_MOUNTS, got %s", e.Value)
		}
	}
}

func TestBuildPodManifest_QemuStillUsesBindTransport(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	pvc := "cubepod-volumes"
	spec := v1alpha1.PodBoxSpec{
		VolumeMounts: []v1alpha1.VolumeMount{
			{VolumeID: id, PVCName: pvc, Path: "/data", SubPath: id + "/data"},
		},
	}
	pod := controller.BuildKataPod("box-qemu", v1alpha1.PodTemplateSpec{
		Image:            "ubuntu:24.04",
		RuntimeClassName: "kata-qemu",
	}, spec, v1alpha1.PodBoxStatus{}, "Block")
	var main *controller.ContainerManifest
	for i := range pod.Containers {
		if pod.Containers[i].Name == "sandbox" {
			main = &pod.Containers[i]
		}
	}
	found := false
	for _, m := range main.VolumeMounts {
		if m.MountPath == "/run/cubepod/volumes/"+id && m.SubPath == id+"/data" {
			found = true
		}
	}
	if !found {
		t.Fatalf("qemu must still stage official volumes, mounts=%+v", main.VolumeMounts)
	}
	for _, e := range main.Env {
		if e.Name == "CUBEPOD_VOLUME_MOUNTS" && strings.Contains(e.Value, `"transport":"nfs"`) {
			t.Fatalf("qemu must not emit nfs transport: %s", e.Value)
		}
	}
}
