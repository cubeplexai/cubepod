package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

func TestBuildKataPod_BlockMode_PrivilegedAndDevices(t *testing.T) {
	tpl := v1alpha1.PodTemplateSpec{
		Image:     "ubuntu:24.04",
		PoddImage: "cubepod-podd:latest",
	}
	spec := v1alpha1.PodBoxSpec{}
	status := v1alpha1.PodBoxStatus{
		PodName:          "block-box-kata-pod",
		RootfsPVCName:    "block-box-rootfs-pvc",
		WorkspacePVCName: "block-box-workspace-pvc",
	}

	// 1. Explicit "Block" volumeMode
	podBlock := controller.BuildKataPod("block-box", tpl, spec, status, "Block")
	if len(podBlock.Containers) == 0 {
		t.Fatal("expected at least 1 container")
	}
	mainC := podBlock.Containers[0]
	if !mainC.Privileged {
		t.Fatal("expected mainContainer.Privileged to be true in Block volumeMode")
	}
	if len(mainC.VolumeDevices) != 2 {
		t.Fatalf("expected 2 volumeDevices in Block mode, got %d", len(mainC.VolumeDevices))
	}
	if podBlock.RuntimeClassName != "kata-qemu" {
		t.Fatalf("expected runtimeClassName kata-qemu by default in Block mode, got %s", podBlock.RuntimeClassName)
	}

	// 2. Default volumeMode (omitted -> defaults to Block)
	podDefault := controller.BuildKataPod("default-box", tpl, spec, status)
	if len(podDefault.Containers) == 0 {
		t.Fatal("expected at least 1 container")
	}
	mainCDefault := podDefault.Containers[0]
	if !mainCDefault.Privileged {
		t.Fatal("expected mainContainer.Privileged to be true when volumeMode is omitted (default Block)")
	}
	if len(mainCDefault.VolumeDevices) != 2 {
		t.Fatalf("expected 2 volumeDevices in default Block mode, got %d", len(mainCDefault.VolumeDevices))
	}
}

func TestBuildKataPod_FilesystemMode_Unprivileged(t *testing.T) {
	tpl := v1alpha1.PodTemplateSpec{
		Image:     "ubuntu:24.04",
		PoddImage: "cubepod-podd:latest",
	}
	spec := v1alpha1.PodBoxSpec{}
	status := v1alpha1.PodBoxStatus{}

	podFS := controller.BuildKataPod("fs-box", tpl, spec, status, "Filesystem")
	if len(podFS.Containers) == 0 {
		t.Fatal("expected at least 1 container")
	}
	mainC := podFS.Containers[0]
	if mainC.Privileged {
		t.Fatal("expected mainContainer.Privileged to be false in Filesystem volumeMode")
	}
	if len(mainC.VolumeDevices) != 0 {
		t.Fatalf("expected 0 volumeDevices in Filesystem mode, got %d", len(mainC.VolumeDevices))
	}
	if podFS.RuntimeClassName != "" {
		t.Fatalf("expected empty runtimeClassName in Filesystem mode by default, got %s", podFS.RuntimeClassName)
	}
}

func TestBuildKataPod_RuntimeClassName_NoneResolvesToEmpty(t *testing.T) {
	spec := v1alpha1.PodBoxSpec{}
	status := v1alpha1.PodBoxStatus{}

	testCases := []struct {
		name         string
		runtimeClass string
		volumeMode   string
		expectedRC   string
	}{
		{
			name:         "lowercase none in Block mode",
			runtimeClass: "none",
			volumeMode:   "Block",
			expectedRC:   "",
		},
		{
			name:         "uppercase NONE in Block mode",
			runtimeClass: "NONE",
			volumeMode:   "Block",
			expectedRC:   "",
		},
		{
			name:         "hyphen '-' in Block mode",
			runtimeClass: "-",
			volumeMode:   "Block",
			expectedRC:   "",
		},
		{
			name:         "empty string in Block mode defaults to kata-qemu",
			runtimeClass: "",
			volumeMode:   "Block",
			expectedRC:   "kata-qemu",
		},
		{
			name:         "explicit kata-qemu in Block mode",
			runtimeClass: "kata-qemu",
			volumeMode:   "Block",
			expectedRC:   "kata-qemu",
		},
		{
			name:         "explicit custom-runtime in Block mode",
			runtimeClass: "custom-runtime",
			volumeMode:   "Block",
			expectedRC:   "custom-runtime",
		},
		{
			name:         "empty string in Filesystem mode defaults to empty",
			runtimeClass: "",
			volumeMode:   "Filesystem",
			expectedRC:   "",
		},
		{
			name:         "lowercase none in Filesystem mode",
			runtimeClass: "none",
			volumeMode:   "Filesystem",
			expectedRC:   "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tpl := v1alpha1.PodTemplateSpec{
				RuntimeClassName: tc.runtimeClass,
			}
			pod := controller.BuildKataPod("test-box", tpl, spec, status, tc.volumeMode)
			if pod.RuntimeClassName != tc.expectedRC {
				t.Fatalf("expected RuntimeClassName %q, got %q", tc.expectedRC, pod.RuntimeClassName)
			}
		})
	}
}

func TestEnsurePod_SerializesSecurityContextPrivileged(t *testing.T) {
	var capturedPodPayload map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&capturedPodPayload); err != nil {
				t.Errorf("failed to decode captured pod payload: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "test-box-kata-pod"},
				"status":   map[string]any{"podIP": "10.244.1.88"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	tpl := v1alpha1.PodTemplateSpec{
		Image:            "ubuntu:24.04",
		RuntimeClassName: "none", // standard runc/containerd without Kata
	}
	spec := v1alpha1.PodBoxSpec{}
	status := v1alpha1.PodBoxStatus{
		PodName:          "test-box-kata-pod",
		RootfsPVCName:    "test-box-rootfs-pvc",
		WorkspacePVCName: "test-box-workspace-pvc",
	}

	// BuildKataPod in Block mode with RuntimeClassName="none"
	pod := controller.BuildKataPod("test-box", tpl, spec, status, "Block")
	if pod.RuntimeClassName != "" {
		t.Fatalf("expected empty RuntimeClassName, got %q", pod.RuntimeClassName)
	}

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	podIP, err := client.EnsurePod(context.Background(), pod)
	if err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}
	if podIP != "10.244.1.88" {
		t.Fatalf("expected podIP 10.244.1.88, got %s", podIP)
	}

	specMap, ok := capturedPodPayload["spec"].(map[string]any)
	if !ok {
		t.Fatalf("missing or invalid spec in payload: %v", capturedPodPayload)
	}

	// Verify runtimeClassName is omitted because it was resolved to ""
	if _, exists := specMap["runtimeClassName"]; exists {
		t.Fatalf("runtimeClassName should be omitted when empty, got %v", specMap["runtimeClassName"])
	}

	containers, ok := specMap["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("missing or empty containers in spec: %v", specMap)
	}

	// Locate main sandbox container and verify securityContext.privileged = true
	var foundSandbox bool
	for _, c := range containers {
		cObj, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cObj["name"] == "sandbox" {
			foundSandbox = true
			sc, ok := cObj["securityContext"].(map[string]any)
			if !ok {
				t.Fatalf("expected securityContext object in sandbox container, got %v", cObj["securityContext"])
			}
			if sc["privileged"] != true {
				t.Fatalf("expected securityContext.privileged=true, got %v", sc["privileged"])
			}
		}
	}
	if !foundSandbox {
		t.Fatal("sandbox container not found in captured pod payload")
	}
}
