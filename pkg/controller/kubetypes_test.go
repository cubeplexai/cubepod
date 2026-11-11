package controller

import (
	"encoding/json"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

func TestToCoreV1PVC_AllFields(t *testing.T) {
	manifest := BlockPVCManifest{
		Name:             "my-pvc",
		Namespace:        "my-ns",
		StorageClassName: "fast-nvme",
		VolumeMode:       "Block",
		Size:             "20Gi",
		AccessModes:      []string{"ReadWriteOnce"},
		DataSourcePVC:    "parent-pvc",
		Labels: map[string]string{
			"app": "cubepod",
		},
	}

	pvc := toCoreV1PVC(manifest, "my-ns")

	if pvc.APIVersion != "v1" {
		t.Fatalf("expected apiVersion v1, got %s", pvc.APIVersion)
	}
	if pvc.Kind != "PersistentVolumeClaim" {
		t.Fatalf("expected kind PersistentVolumeClaim, got %s", pvc.Kind)
	}
	if pvc.Metadata.Name != "my-pvc" || pvc.Metadata.Namespace != "my-ns" {
		t.Fatalf("metadata mismatch: %+v", pvc.Metadata)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-nvme" {
		t.Fatalf("expected storageClassName fast-nvme, got %v", pvc.Spec.StorageClassName)
	}
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Name != "parent-pvc" {
		t.Fatalf("expected dataSource parent-pvc, got %v", pvc.Spec.DataSource)
	}
	if pvc.Spec.Resources.Requests["storage"] != "20Gi" {
		t.Fatalf("expected storage request 20Gi, got %v", pvc.Spec.Resources.Requests)
	}

	data, err := json.Marshal(pvc)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if parsed["apiVersion"] != "v1" || parsed["kind"] != "PersistentVolumeClaim" {
		t.Fatalf("unexpected json payload: %s", string(data))
	}
}

func TestToCoreV1Pod_AllFields(t *testing.T) {
	manifest := PodManifest{
		Name:             "my-pod",
		Namespace:        "test-ns",
		RuntimeClassName: "kata-qemu",
		Labels: map[string]string{
			"cubepod.io/box": "box1",
		},
		InitContainers: []ContainerManifest{
			{
				Name:    "init-installer",
				Image:   "installer:latest",
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "cp -a /bin/* /target"},
				VolumeMounts: []VolumeMount{
					{Name: "bin-dir", MountPath: "/target", ReadOnly: false},
				},
			},
		},
		Containers: []ContainerManifest{
			{
				Name:    "sandbox",
				Image:   "ubuntu:24.04",
				Command: []string{"/opt/cubepod/bin/podd-init"},
				Args:    []string{"/opt/cubepod/bin/podd"},
				VolumeMounts: []VolumeMount{
					{Name: "bin-dir", MountPath: "/opt/cubepod/bin", ReadOnly: true},
				},
				VolumeDevices: []VolumeDevice{
					{Name: "rootfs", DevicePath: "/dev/cubepod-rootfs"},
				},
				Resources: v1alpha1.ResourceRequirements{
					CPURequest:    "500m",
					CPULimit:      "2",
					MemoryRequest: "1Gi",
					MemoryLimit:   "4Gi",
				},
				Env: []EnvVar{
					{Name: "CUBEPOD_DEBUG", Value: "1"},
				},
			},
		},
		Volumes: []VolumeManifest{
			{Name: "bin-dir", EmptyDir: true},
			{Name: "rootfs", PersistentVolumeClaim: "rootfs-pvc"},
			{Name: "certs", SecretName: "tls-secret"},
		},
	}

	pod := toCoreV1Pod(manifest, "test-ns")
	if pod.APIVersion != "v1" || pod.Kind != "Pod" {
		t.Fatalf("unexpected apiVersion or kind: %s/%s", pod.APIVersion, pod.Kind)
	}
	if pod.Metadata.Name != "my-pod" || pod.Metadata.Namespace != "test-ns" {
		t.Fatalf("metadata mismatch: %+v", pod.Metadata)
	}
	if pod.Spec.RuntimeClassName != "kata-qemu" {
		t.Fatalf("expected kata-qemu, got %s", pod.Spec.RuntimeClassName)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(pod.Spec.InitContainers))
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	if len(pod.Spec.Volumes) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(pod.Spec.Volumes))
	}

	c := pod.Spec.Containers[0]
	if c.Resources == nil || c.Resources.Requests["cpu"] != "500m" || c.Resources.Limits["memory"] != "4Gi" {
		t.Fatalf("unexpected container resources: %+v", c.Resources)
	}
	if len(c.VolumeDevices) != 1 || c.VolumeDevices[0].DevicePath != "/dev/cubepod-rootfs" {
		t.Fatalf("unexpected volume devices: %+v", c.VolumeDevices)
	}

	data, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	spec := parsed["spec"].(map[string]any)
	if spec["runtimeClassName"] != "kata-qemu" {
		t.Fatalf("expected runtimeClassName kata-qemu in json, got %v", spec["runtimeClassName"])
	}
}

func TestToCoreV1Container_PrivilegedSecurityContext(t *testing.T) {
	// 1. Privileged = true
	cPriv := ContainerManifest{
		Name:       "sandbox",
		Image:      "ubuntu:24.04",
		Privileged: true,
	}
	v1Priv := toCoreV1Container(cPriv)
	if v1Priv.SecurityContext == nil {
		t.Fatal("expected SecurityContext to be non-nil when Privileged=true")
	}
	if v1Priv.SecurityContext.Privileged == nil || !*v1Priv.SecurityContext.Privileged {
		t.Fatalf("expected SecurityContext.Privileged to be true, got %v", v1Priv.SecurityContext.Privileged)
	}

	dataPriv, err := json.Marshal(v1Priv)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var parsedPriv map[string]any
	if err := json.Unmarshal(dataPriv, &parsedPriv); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	sc, ok := parsedPriv["securityContext"].(map[string]any)
	if !ok {
		t.Fatalf("expected securityContext map in json: %s", string(dataPriv))
	}
	if sc["privileged"] != true {
		t.Fatalf("expected securityContext.privileged=true, got %v", sc["privileged"])
	}

	// 2. Privileged = false (default)
	cUnpriv := ContainerManifest{
		Name:       "sandbox",
		Image:      "ubuntu:24.04",
		Privileged: false,
	}
	v1Unpriv := toCoreV1Container(cUnpriv)
	if v1Unpriv.SecurityContext != nil {
		t.Fatalf("expected SecurityContext to be nil when Privileged=false, got %+v", v1Unpriv.SecurityContext)
	}

	dataUnpriv, err := json.Marshal(v1Unpriv)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var parsedUnpriv map[string]any
	if err := json.Unmarshal(dataUnpriv, &parsedUnpriv); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if _, exists := parsedUnpriv["securityContext"]; exists {
		t.Fatalf("expected securityContext to be omitted in json: %s", string(dataUnpriv))
	}
}

func TestToCoreV1Pod_PrivilegedContainerSerialization(t *testing.T) {
	manifest := PodManifest{
		Name:             "block-pod",
		RuntimeClassName: "",
		Containers: []ContainerManifest{
			{
				Name:       "sandbox",
				Image:      "ubuntu:24.04",
				Privileged: true,
			},
		},
	}

	pod := toCoreV1Pod(manifest, "test-ns")
	data, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	spec := parsed["spec"].(map[string]any)
	containers := spec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	cObj := containers[0].(map[string]any)
	sc, ok := cObj["securityContext"].(map[string]any)
	if !ok || sc["privileged"] != true {
		t.Fatalf("expected containers[0].securityContext.privileged=true, got %v", cObj["securityContext"])
	}
}
