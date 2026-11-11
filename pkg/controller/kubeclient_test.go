package controller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

type mockK8sServer struct {
	mu           sync.RWMutex
	pvcs         map[string]map[string]any
	pods         map[string]map[string]any
	conflictNext map[string]bool
	deletedPVCs  []string
	deletedPods  []string
}

func newMockK8sServer() (*mockK8sServer, *httptest.Server) {
	m := &mockK8sServer{
		pvcs:         make(map[string]map[string]any),
		pods:         make(map[string]map[string]any),
		conflictNext: make(map[string]bool),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		path := r.URL.Path

		// PVC endpoints: /api/v1/namespaces/{ns}/persistentvolumeclaims[/{name}]
		if strings.HasPrefix(path, "/api/v1/namespaces/cubepod-workloads/persistentvolumeclaims") {
			sub := strings.TrimPrefix(path, "/api/v1/namespaces/cubepod-workloads/persistentvolumeclaims")
			if sub == "" || sub == "/" {
				if r.Method == http.MethodPost {
					if m.conflictNext["pvc"] {
						m.conflictNext["pvc"] = false
						http.Error(w, `{"kind":"Status","status":"Failure","reason":"AlreadyExists","code":409}`, http.StatusConflict)
						return
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					meta := body["metadata"].(map[string]any)
					name := meta["name"].(string)
					m.pvcs[name] = body
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(body)
					return
				}
			} else {
				pvcName := strings.TrimPrefix(sub, "/")
				if r.Method == http.MethodDelete {
					m.deletedPVCs = append(m.deletedPVCs, pvcName)
					delete(m.pvcs, pvcName)
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
					return
				}
			}
		}

		// Pod endpoints: /api/v1/namespaces/{ns}/pods[/{name}]
		if strings.HasPrefix(path, "/api/v1/namespaces/cubepod-workloads/pods") {
			sub := strings.TrimPrefix(path, "/api/v1/namespaces/cubepod-workloads/pods")
			if sub == "" || sub == "/" {
				if r.Method == http.MethodPost {
					if m.conflictNext["pod"] {
						m.conflictNext["pod"] = false
						http.Error(w, `{"kind":"Status","status":"Failure","reason":"AlreadyExists","code":409}`, http.StatusConflict)
						return
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					meta := body["metadata"].(map[string]any)
					name := meta["name"].(string)
					m.pods[name] = body
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"metadata": body["metadata"],
						"status": map[string]any{
							"podIP": "10.244.2.55",
						},
					})
					return
				}
			} else {
				podName := strings.TrimPrefix(sub, "/")
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"metadata": map[string]any{"name": podName},
						"status":   map[string]any{"podIP": "10.244.2.55"},
					})
					return
				}
				if r.Method == http.MethodDelete {
					m.deletedPods = append(m.deletedPods, podName)
					delete(m.pods, podName)
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
					return
				}
			}
		}

		http.NotFound(w, r)
	})

	server := httptest.NewServer(handler)
	return m, server
}

func TestRESTKubeClient_Direct(t *testing.T) {
	mock, ts := newMockK8sServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-bearer-token", ts.Client())

	ctx := context.Background()

	// 1. EnsurePVC
	pvc := controller.BlockPVCManifest{
		Name:             "test-pvc",
		StorageClassName: "standard",
		VolumeMode:       "Block",
		Size:             "10Gi",
		AccessModes:      []string{"ReadWriteOnce"},
	}
	if err := client.EnsurePVC(ctx, pvc); err != nil {
		t.Fatalf("EnsurePVC failed: %v", err)
	}

	mock.mu.RLock()
	if _, ok := mock.pvcs["test-pvc"]; !ok {
		t.Fatal("expected test-pvc in mock APIServer")
	}
	mock.mu.RUnlock()

	// 2. EnsurePVC with 409 Conflict -> should succeed idempotently
	mock.mu.Lock()
	mock.conflictNext["pvc"] = true
	mock.mu.Unlock()

	if err := client.EnsurePVC(ctx, pvc); err != nil {
		t.Fatalf("EnsurePVC on 409 Conflict should succeed, got: %v", err)
	}

	// 3. EnsurePod
	pod := controller.PodManifest{
		Name:             "test-pod",
		RuntimeClassName: "kata-qemu",
		Containers: []controller.ContainerManifest{
			{Name: "sandbox", Image: "ubuntu:24.04"},
		},
	}
	ip, err := client.EnsurePod(ctx, pod)
	if err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}
	if ip != "10.244.2.55" {
		t.Fatalf("expected podIP 10.244.2.55, got %s", ip)
	}

	// 4. EnsurePod with 409 Conflict -> should succeed idempotently
	mock.mu.Lock()
	mock.conflictNext["pod"] = true
	mock.mu.Unlock()

	ip2, err := client.EnsurePod(ctx, pod)
	if err != nil {
		t.Fatalf("EnsurePod on 409 Conflict should succeed, got: %v", err)
	}
	if ip2 != "10.244.2.55" {
		t.Fatalf("expected podIP 10.244.2.55 from GET, got %s", ip2)
	}

	// 5. DeletePod
	if err := client.DeletePod(ctx, "cubepod-workloads", "test-pod"); err != nil {
		t.Fatalf("DeletePod failed: %v", err)
	}
	mock.mu.RLock()
	if len(mock.deletedPods) == 0 || mock.deletedPods[0] != "test-pod" {
		t.Fatalf("expected test-pod deleted, got %v", mock.deletedPods)
	}
	mock.mu.RUnlock()

	// 6. DeletePVC
	if err := client.DeletePVC(ctx, "cubepod-workloads", "test-pvc"); err != nil {
		t.Fatalf("DeletePVC failed: %v", err)
	}
	mock.mu.RLock()
	if len(mock.deletedPVCs) == 0 || mock.deletedPVCs[0] != "test-pvc" {
		t.Fatalf("expected test-pvc deleted, got %v", mock.deletedPVCs)
	}
	mock.mu.RUnlock()
}

func TestReconciler_WithRESTKubeClient_FullLifecycle(t *testing.T) {
	mock, ts := newMockK8sServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	r := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)

	ctx := context.Background()

	// 1. Initial Reconcile Running -> creates dual block PVCs and Kata Pod
	spec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	status, err := r.Reconcile(ctx, "sandbox-app", spec)
	if err != nil {
		t.Fatalf("reconcile Running failed: %v", err)
	}
	if status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected Running phase, got %s", status.Phase)
	}
	if status.PodIP != "10.244.2.55" {
		t.Fatalf("expected PodIP 10.244.2.55, got %s", status.PodIP)
	}

	mock.mu.RLock()
	if _, ok := mock.pvcs["sandbox-app-rootfs-pvc"]; !ok {
		t.Fatal("expected sandbox-app-rootfs-pvc created in APIServer")
	}
	if _, ok := mock.pvcs["sandbox-app-workspace-pvc"]; !ok {
		t.Fatal("expected sandbox-app-workspace-pvc created in APIServer")
	}
	if _, ok := mock.pods["sandbox-app-kata-pod"]; !ok {
		t.Fatal("expected sandbox-app-kata-pod created in APIServer")
	}
	mock.mu.RUnlock()

	// 2. Pause sandbox -> deletes Pod, keeps both PVCs
	spec.DesiredState = v1alpha1.DesiredStatePaused
	statusPaused, err := r.Reconcile(ctx, "sandbox-app", spec)
	if err != nil {
		t.Fatalf("reconcile Paused failed: %v", err)
	}
	if statusPaused.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected Paused phase, got %s", statusPaused.Phase)
	}

	mock.mu.RLock()
	var podDeleted bool
	for _, p := range mock.deletedPods {
		if p == "sandbox-app-kata-pod" {
			podDeleted = true
			break
		}
	}
	if !podDeleted {
		t.Fatal("expected sandbox-app-kata-pod deleted on pause")
	}
	if _, ok := mock.pvcs["sandbox-app-rootfs-pvc"]; !ok {
		t.Fatal("rootfs PVC must remain intact on pause")
	}
	if _, ok := mock.pvcs["sandbox-app-workspace-pvc"]; !ok {
		t.Fatal("workspace PVC must remain intact on pause")
	}
	mock.mu.RUnlock()

	// 3. Resume sandbox -> re-creates Pod
	spec.DesiredState = v1alpha1.DesiredStateRunning
	statusResumed, err := r.Reconcile(ctx, "sandbox-app", spec)
	if err != nil {
		t.Fatalf("reconcile Resume failed: %v", err)
	}
	if statusResumed.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected Running phase, got %s", statusResumed.Phase)
	}

	// 4. Reset RootFS to gen 2 -> deletes gen1 rootfs PVC, creates gen2 rootfs PVC, preserves workspace PVC
	spec.RootfsGeneration = 2
	statusGen2, err := r.Reconcile(ctx, "sandbox-app", spec)
	if err != nil {
		t.Fatalf("reconcile Gen2 failed: %v", err)
	}
	if statusGen2.RootfsPVCName != "sandbox-app-rootfs-gen2-pvc" {
		t.Fatalf("expected gen2 rootfs PVC name, got %s", statusGen2.RootfsPVCName)
	}

	mock.mu.RLock()
	var oldPVCDeleted bool
	for _, p := range mock.deletedPVCs {
		if p == "sandbox-app-rootfs-pvc" {
			oldPVCDeleted = true
			break
		}
	}
	if !oldPVCDeleted {
		t.Fatal("expected old rootfs PVC deleted on generation reset")
	}
	if _, ok := mock.pvcs["sandbox-app-rootfs-gen2-pvc"]; !ok {
		t.Fatal("expected new gen2 rootfs PVC created in APIServer")
	}
	if _, ok := mock.pvcs["sandbox-app-workspace-pvc"]; !ok {
		t.Fatal("workspace PVC must remain intact across RootFS reset")
	}
	mock.mu.RUnlock()

	// 5. CoW Fork -> creates child PVCs with dataSource
	forkSpec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "sandbox-app"},
	}
	_, err = r.Reconcile(ctx, "sandbox-fork", forkSpec)
	if err != nil {
		t.Fatalf("reconcile Fork failed: %v", err)
	}

	mock.mu.RLock()
	forkRootfsPVC, ok := mock.pvcs["sandbox-fork-rootfs-pvc"]
	if !ok {
		t.Fatal("expected sandbox-fork-rootfs-pvc created in APIServer")
	}
	forkSpecMap := forkRootfsPVC["spec"].(map[string]any)
	ds, ok := forkSpecMap["dataSource"].(map[string]any)
	if !ok || ds["name"] != "sandbox-app-rootfs-pvc" {
		t.Fatalf("expected dataSource pointing to parent rootfs PVC, got %v", ds)
	}
	mock.mu.RUnlock()

	// 6. Delete sandbox -> removes Pod and both PVCs
	if err := r.Delete(ctx, "sandbox-app"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	mock.mu.RLock()
	var foundRootfsDel, foundWsDel bool
	for _, p := range mock.deletedPVCs {
		if p == "sandbox-app-rootfs-gen2-pvc" {
			foundRootfsDel = true
		}
		if p == "sandbox-app-workspace-pvc" {
			foundWsDel = true
		}
	}
	mock.mu.RUnlock()

	if !foundRootfsDel || !foundWsDel {
		t.Fatalf("expected delete of both PVCs on sandbox delete, got deletedPVCs: %v", mock.deletedPVCs)
	}
}

func TestControllerManager_HealthzAndMetrics(t *testing.T) {
	r := controller.NewReconciler()
	ctx := context.Background()

	// Setup: 1 Running, 1 Paused, 1 WarmPool with 2 replicas
	_, _ = r.Reconcile(ctx, "running-box", v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	})
	_, _ = r.Reconcile(ctx, "paused-box", v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStatePaused,
		RootfsGeneration: 1,
	})
	_, _ = r.ReconcileWarmPool(ctx, "pool-metric", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "ubuntu:24.04",
		Replicas:    2,
	})

	mgr := controller.NewControllerManager(r, ":8081", 100*time.Millisecond)
	ts := httptest.NewServer(mgr.Handler())
	defer ts.Close()

	// 1. GET /healthz
	respHealth, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer respHealth.Body.Close()
	if respHealth.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /healthz, got %d", respHealth.StatusCode)
	}
	var healthBody map[string]string
	_ = json.NewDecoder(respHealth.Body).Decode(&healthBody)
	if healthBody["status"] != "ok" || healthBody["component"] != "cubepod-controller" {
		t.Fatalf("unexpected healthz response: %v", healthBody)
	}

	// 2. GET /metrics (Prometheus text)
	respMetrics, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer respMetrics.Body.Close()
	if respMetrics.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /metrics, got %d", respMetrics.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(respMetrics.Body)
	bodyStr := string(bodyBytes)

	if !strings.Contains(bodyStr, "cubepod_active_sandboxes 1") {
		t.Fatalf("expected cubepod_active_sandboxes 1 in metrics, got:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "cubepod_paused_sandboxes 1") {
		t.Fatalf("expected cubepod_paused_sandboxes 1 in metrics, got:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "cubepod_warmpool_ready 2") {
		t.Fatalf("expected cubepod_warmpool_ready 2 in metrics, got:\n%s", bodyStr)
	}

	// 3. GET /metrics (JSON format with Accept header)
	reqJSON, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	reqJSON.Header.Set("Accept", "application/json")
	respJSON, err := http.DefaultClient.Do(reqJSON)
	if err != nil {
		t.Fatalf("GET /metrics JSON failed: %v", err)
	}
	defer respJSON.Body.Close()
	var metricsJSON map[string]int
	_ = json.NewDecoder(respJSON.Body).Decode(&metricsJSON)
	if metricsJSON["cubepod_active_sandboxes"] != 1 ||
		metricsJSON["cubepod_paused_sandboxes"] != 1 ||
		metricsJSON["cubepod_warmpool_ready"] != 2 {
		t.Fatalf("unexpected json metrics: %v", metricsJSON)
	}
}

func TestControllerManager_BackgroundLoop(t *testing.T) {
	r := controller.NewReconciler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create an idle box with 1s timeout
	_, _ = r.Reconcile(ctx, "auto-reap-box", v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		Lifecycle: v1alpha1.LifecyclePolicy{
			IdleTimeoutSeconds: 1,
		},
	})
	// Backdate activity
	past := time.Now().UTC().Add(-5 * time.Second)
	_ = r.TouchActivity("auto-reap-box", past)

	mgr := controller.NewControllerManager(r, "127.0.0.1:0", 50*time.Millisecond)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()

	// Wait for background ticker to reap
	time.Sleep(150 * time.Millisecond)
	cancel()

	<-errCh

	status, ok := r.GetBox("auto-reap-box")
	if !ok {
		t.Fatal("box not found")
	}
	if status.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected auto-reap-box to be Paused by background loop, got %s", status.Phase)
	}
	if status.Reason != "IdlePaused" {
		t.Fatalf("expected reason IdlePaused, got %s", status.Reason)
	}
}

func TestFilesystemVolumeMode_PVCAndPodGeneration(t *testing.T) {
	tpl := v1alpha1.PodTemplateSpec{
		Image: "ubuntu:24.04",
		Storage: v1alpha1.StorageSpec{
			StorageClassName: "nfs-storage",
			RootfsSize:       "20Gi",
			WorkspaceSize:    "50Gi",
		},
		RuntimeClassName: "",
	}
	spec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}

	// 1. BuildBlockPVCs with Filesystem mode
	rootPVC, wsPVC := controller.BuildBlockPVCs("fs-box", tpl, spec, "Filesystem")
	if rootPVC.VolumeMode != "Filesystem" {
		t.Fatalf("expected rootPVC VolumeMode 'Filesystem', got %s", rootPVC.VolumeMode)
	}
	if wsPVC.VolumeMode != "Filesystem" {
		t.Fatalf("expected wsPVC VolumeMode 'Filesystem', got %s", wsPVC.VolumeMode)
	}

	// 2. BuildBlockPVCs with ForkFrom in Filesystem mode -> DataSourcePVC suppressed
	forkSpec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
		ForkFrom:         &v1alpha1.ForkSource{PodBoxName: "parent-box"},
	}
	rootForkPVC, wsForkPVC := controller.BuildBlockPVCs("fork-box", tpl, forkSpec, "Filesystem")
	if rootForkPVC.DataSourcePVC != "" || wsForkPVC.DataSourcePVC != "" {
		t.Fatalf("expected empty DataSourcePVC in Filesystem mode, got root=%q ws=%q", rootForkPVC.DataSourcePVC, wsForkPVC.DataSourcePVC)
	}

	// 3. BuildKataPod with Filesystem mode and empty RuntimeClassName
	status := v1alpha1.PodBoxStatus{
		PodName:          "fs-box-kata-pod",
		RootfsPVCName:    rootPVC.Name,
		WorkspacePVCName: wsPVC.Name,
	}
	pod := controller.BuildKataPod("fs-box", tpl, spec, status, "Filesystem")
	if pod.RuntimeClassName != "" {
		t.Fatalf("expected empty RuntimeClassName, got %q", pod.RuntimeClassName)
	}

	// Main sandbox container should have VolumeMounts for rootfs and workspace, and no VolumeDevices
	if len(pod.Containers) == 0 {
		t.Fatal("expected at least one container in pod")
	}
	mainC := pod.Containers[0]
	if len(mainC.VolumeDevices) != 0 {
		t.Fatalf("expected 0 volume devices in Filesystem mode, got %d", len(mainC.VolumeDevices))
	}
	var foundRootfsMount, foundWorkspaceMount, foundSkipPivot bool
	for _, vm := range mainC.VolumeMounts {
		if vm.MountPath == "/mnt/cubepod/rootfs" {
			foundRootfsMount = true
		}
		if vm.MountPath == "/workspace" {
			foundWorkspaceMount = true
		}
	}
	if !foundRootfsMount || !foundWorkspaceMount {
		t.Fatalf("expected /mnt/cubepod/rootfs and /workspace in VolumeMounts, got %+v", mainC.VolumeMounts)
	}
	for _, env := range mainC.Env {
		if env.Name == "CUBEPOD_SKIP_PIVOT" && env.Value == "1" {
			foundSkipPivot = true
		}
	}
	if !foundSkipPivot {
		t.Fatal("expected CUBEPOD_SKIP_PIVOT=1 when RuntimeClassName is empty")
	}

	// 4. Test EnsurePod omits runtimeClassName in JSON payload when empty
	var capturedPodPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&capturedPodPayload)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "fs-box-kata-pod"},
				"status":   map[string]any{"podIP": "10.244.10.15"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ip, err := client.EnsurePod(context.Background(), pod)
	if err != nil {
		t.Fatalf("EnsurePod failed: %v", err)
	}
	if ip != "10.244.10.15" {
		t.Fatalf("expected podIP 10.244.10.15, got %s", ip)
	}

	specMap, ok := capturedPodPayload["spec"].(map[string]any)
	if !ok {
		t.Fatalf("invalid spec in captured payload: %v", capturedPodPayload)
	}
	if _, exists := specMap["runtimeClassName"]; exists {
		t.Fatalf("runtimeClassName must be omitted when empty, got %v", specMap["runtimeClassName"])
	}
}

func TestReconciler_DeleteBox(t *testing.T) {
	mock, ts := newMockK8sServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	r := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)
	ctx := context.Background()

	// 1. Reconcile running box
	spec := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	_, err := r.Reconcile(ctx, "box-keep-pvc", spec)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// 2. DeleteBox with deleteVolumes=false
	if err := r.DeleteBox(ctx, "box-keep-pvc", false); err != nil {
		t.Fatalf("DeleteBox(deleteVolumes=false) failed: %v", err)
	}

	// Box should be removed from reconciler
	if _, exists := r.GetBox("box-keep-pvc"); exists {
		t.Fatal("expected box-keep-pvc to be removed from reconciler")
	}

	// Pod should be deleted
	mock.mu.RLock()
	var podDeleted bool
	for _, p := range mock.deletedPods {
		if p == "box-keep-pvc-kata-pod" {
			podDeleted = true
			break
		}
	}
	if !podDeleted {
		t.Fatal("expected box-keep-pvc-kata-pod to be deleted")
	}
	// PVCs should NOT be deleted
	for _, p := range mock.deletedPVCs {
		if p == "box-keep-pvc-rootfs-pvc" || p == "box-keep-pvc-workspace-pvc" {
			t.Fatalf("PVC %s should not have been deleted when deleteVolumes=false", p)
		}
	}
	mock.mu.RUnlock()

	// 3. Reconcile another box and DeleteBox with deleteVolumes=true
	_, err = r.Reconcile(ctx, "box-delete-pvc", spec)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if err := r.DeleteBox(ctx, "box-delete-pvc", true); err != nil {
		t.Fatalf("DeleteBox(deleteVolumes=true) failed: %v", err)
	}

	mock.mu.RLock()
	var rootPVCDeleted, wsPVCDeleted bool
	for _, p := range mock.deletedPVCs {
		if p == "box-delete-pvc-rootfs-pvc" {
			rootPVCDeleted = true
		}
		if p == "box-delete-pvc-workspace-pvc" {
			wsPVCDeleted = true
		}
	}
	mock.mu.RUnlock()

	if !rootPVCDeleted || !wsPVCDeleted {
		t.Fatalf("expected both PVCs deleted when deleteVolumes=true, got deletedPVCs: %v", mock.deletedPVCs)
	}
}

func TestKubeconfigParsing(t *testing.T) {
	dummyCA := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nCA-DATA\n-----END CERTIFICATE-----"))
	dummyCert := base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nCLIENT-CERT\n-----END CERTIFICATE-----"))
	dummyKey := base64.StdEncoding.EncodeToString([]byte("-----BEGIN RSA PRIVATE KEY-----\nCLIENT-KEY\n-----END RSA PRIVATE KEY-----"))

	kubeconfigYAML := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    insecure-skip-tls-verify: true
    server: "https://192.168.1.207:6443"
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: test-workloads
    user: test-user
  name: test-context
current-context: test-context
kind: Config
preferences: {}
users:
- name: test-user
  user:
    client-certificate-data: %s
    client-key-data: %s
    token: "test-secret-token"
`, dummyCA, dummyCert, dummyKey)

	info, err := controller.ParseKubeconfig([]byte(kubeconfigYAML))
	if err != nil {
		t.Fatalf("ParseKubeconfig failed: %v", err)
	}

	if info.Server != "https://192.168.1.207:6443" {
		t.Fatalf("expected server https://192.168.1.207:6443, got %s", info.Server)
	}
	if info.Namespace != "test-workloads" {
		t.Fatalf("expected namespace test-workloads, got %s", info.Namespace)
	}
	if info.Token != "test-secret-token" {
		t.Fatalf("expected token test-secret-token, got %s", info.Token)
	}
	if !info.InsecureSkipTLSVerify {
		t.Fatal("expected InsecureSkipTLSVerify to be true")
	}
	if !strings.Contains(string(info.CACertificateData), "CA-DATA") {
		t.Fatalf("unexpected CACertificateData: %s", string(info.CACertificateData))
	}
	if !strings.Contains(string(info.ClientCertificateData), "CLIENT-CERT") {
		t.Fatalf("unexpected ClientCertificateData: %s", string(info.ClientCertificateData))
	}
	if !strings.Contains(string(info.ClientKeyData), "CLIENT-KEY") {
		t.Fatalf("unexpected ClientKeyData: %s", string(info.ClientKeyData))
	}

	// Verify NewRESTKubeClientWithConfig creates client
	cfg := controller.RESTKubeClientConfig{
		APIServerURL:   info.Server,
		Namespace:      info.Namespace,
		BearerToken:    info.Token,
		InsecureTLS:    info.InsecureSkipTLSVerify,
		WaitForRunning: true,
		PollInterval:   100 * time.Millisecond,
		PollTimeout:    5 * time.Second,
	}
	client, err := controller.NewRESTKubeClientWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewRESTKubeClientWithConfig failed: %v", err)
	}
	if !client.WaitForRunning() {
		t.Fatal("expected WaitForRunning to be true")
	}

	// Verify NewRESTKubeClientFromKubeconfig with token-based kubeconfig
	tokenInfo := &controller.KubeconfigInfo{
		Server:    info.Server,
		Namespace: info.Namespace,
		Token:     info.Token,
	}
	clientFromKubeconfig, err := controller.NewRESTKubeClientFromKubeconfig(tokenInfo)
	if err != nil {
		t.Fatalf("NewRESTKubeClientFromKubeconfig failed: %v", err)
	}
	if clientFromKubeconfig == nil {
		t.Fatal("expected non-nil client from NewRESTKubeClientFromKubeconfig")
	}
}

func TestWaitForRunning_Polling(t *testing.T) {
	var pollCount int
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "poll-pod"},
				"status":   map[string]any{"phase": "Pending"},
			})
			return
		}

		if r.Method == http.MethodGet {
			pollCount++
			if pollCount < 3 {
				// Still Pending or ContainerCreating without IP
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"metadata": map[string]any{"name": "poll-pod"},
					"status": map[string]any{
						"phase": "Pending",
						"podIP": "",
					},
				})
				return
			}

			// Pod is Running and has an IP!
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "poll-pod"},
				"status": map[string]any{
					"phase": "Running",
					"podIP": "10.244.5.99",
				},
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	client := controller.NewRESTKubeClient(
		ts.URL,
		"cubepod-workloads",
		"poll-token",
		ts.Client(),
		controller.WithWaitForRunning(true),
		controller.WithPollInterval(20*time.Millisecond),
		controller.WithPollTimeout(2*time.Second),
	)

	pod := controller.PodManifest{
		Name:             "poll-pod",
		RuntimeClassName: "kata-qemu",
		Containers: []controller.ContainerManifest{
			{Name: "sandbox", Image: "ubuntu:24.04"},
		},
	}

	ip, err := client.EnsurePod(context.Background(), pod)
	if err != nil {
		t.Fatalf("EnsurePod with WaitForRunning failed: %v", err)
	}

	if ip != "10.244.5.99" {
		t.Fatalf("expected podIP 10.244.5.99, got %s", ip)
	}

	mu.Lock()
	count := pollCount
	mu.Unlock()

	if count < 3 {
		t.Fatalf("expected at least 3 polls before reaching Running phase, got %d", count)
	}
}
