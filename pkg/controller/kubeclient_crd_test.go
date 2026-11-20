package controller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

type mockCRDServer struct {
	mu                 sync.RWMutex
	podBoxes           map[string]v1alpha1.PodBox
	podWarmPools       map[string]v1alpha1.PodWarmPool
	volumeSnapshots    map[string]controller.VolumeSnapshot
	getCalls           int
	postCalls          int
	putCalls           int
	putStatus          int
	deleteCalls        int
	listCalls          int
	wpGetCalls         int
	wpPostCalls        int
	wpPutCalls         int
	wpPutStatus        int
	return404CRD       bool
	return404PutStatus bool
}

func newMockCRDServer() (*mockCRDServer, *httptest.Server) {
	m := &mockCRDServer{
		podBoxes:        make(map[string]v1alpha1.PodBox),
		podWarmPools:    make(map[string]v1alpha1.PodWarmPool),
		volumeSnapshots: make(map[string]controller.VolumeSnapshot),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		path := r.URL.Path

		if strings.HasPrefix(path, "/api/v1/namespaces/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if strings.Contains(path, "/pods") {
				if r.Method == http.MethodGet && strings.HasSuffix(path, "/pods") {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"metadata": map[string]any{"name": "mock-pod"},
					"status": map[string]any{
						"phase": "Running",
						"podIP": "10.244.0.10",
						"conditions": []map[string]any{
							{"type": "Ready", "status": "True"},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Success"})
			return
		}

		if m.return404CRD {
			http.Error(w, `{"kind":"Status","status":"Failure","message":"the server could not find the requested resource","reason":"NotFound","code":404}`, http.StatusNotFound)
			return
		}

		// Prefix: /apis/snapshot.storage.k8s.io/v1/namespaces/{ns}/volumesnapshots
		const snapPrefix = "/apis/snapshot.storage.k8s.io/v1/namespaces/"
		if strings.HasPrefix(path, snapPrefix) {
			trimmed := strings.TrimPrefix(path, snapPrefix)
			parts := strings.Split(trimmed, "/")
			if len(parts) < 2 || parts[1] != "volumesnapshots" {
				http.NotFound(w, r)
				return
			}
			ns := parts[0]
			if len(parts) == 2 && r.Method == http.MethodPost {
				m.postCalls++
				var snap controller.VolumeSnapshot
				if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				key := fmt.Sprintf("%s/%s", ns, snap.Metadata.Name)
				if _, exists := m.volumeSnapshots[key]; exists {
					w.WriteHeader(http.StatusConflict)
					return
				}
				snap.Metadata.Namespace = ns
				m.volumeSnapshots[key] = snap
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(snap)
				return
			}
			if len(parts) == 3 {
				name := parts[2]
				key := fmt.Sprintf("%s/%s", ns, name)
				switch r.Method {
				case http.MethodGet:
					m.getCalls++
					snap, ok := m.volumeSnapshots[key]
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(snap)
					return
				case http.MethodDelete:
					m.deleteCalls++
					if _, ok := m.volumeSnapshots[key]; !ok {
						http.NotFound(w, r)
						return
					}
					delete(m.volumeSnapshots, key)
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
					return
				}
			}
			http.NotFound(w, r)
			return
		}

		// Prefix: /apis/cubepod.io/v1alpha1/namespaces/{ns}/{resource}
		const prefix = "/apis/cubepod.io/v1alpha1/namespaces/"
		if !strings.HasPrefix(path, prefix) {
			http.NotFound(w, r)
			return
		}

		trimmed := strings.TrimPrefix(path, prefix)
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}

		ns := parts[0]
		resource := parts[1]

		if resource == "podwarmpools" {
			// List or Create: /apis/cubepod.io/v1alpha1/namespaces/{ns}/podwarmpools
			if len(parts) == 2 {
				switch r.Method {
				case http.MethodGet:
					m.listCalls++
					var items []v1alpha1.PodWarmPool
					for _, p := range m.podWarmPools {
						if p.Metadata.Namespace == ns || ns == "" {
							items = append(items, p)
						}
					}
					list := v1alpha1.PodWarmPoolList{
						APIVersion: "cubepod.io/v1alpha1",
						Kind:       "PodWarmPoolList",
						Items:      items,
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(list)
					return

				case http.MethodPost:
					m.postCalls++
					m.wpPostCalls++
					var pool v1alpha1.PodWarmPool
					if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					key := fmt.Sprintf("%s/%s", ns, pool.Metadata.Name)
					if _, exists := m.podWarmPools[key]; exists {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"kind":    "Status",
							"status":  "Failure",
							"message": fmt.Sprintf("podwarmpools.cubepod.io %q already exists", pool.Metadata.Name),
							"reason":  "AlreadyExists",
							"code":    409,
						})
						return
					}
					if pool.Metadata.ResourceVersion == "" {
						pool.Metadata.ResourceVersion = "1"
					}
					pool.Metadata.Namespace = ns
					m.podWarmPools[key] = pool
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(pool)
					return
				default:
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
			}

			// Specific item: /apis/cubepod.io/v1alpha1/namespaces/{ns}/podwarmpools/{name}
			name := parts[2]
			key := fmt.Sprintf("%s/%s", ns, name)

			if len(parts) == 3 {
				switch r.Method {
				case http.MethodGet:
					m.getCalls++
					m.wpGetCalls++
					pool, ok := m.podWarmPools[key]
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(pool)
					return

				case http.MethodPut:
					m.putCalls++
					m.wpPutCalls++
					var pool v1alpha1.PodWarmPool
					if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					existing, ok := m.podWarmPools[key]
					if !ok {
						http.NotFound(w, r)
						return
					}
					if pool.Metadata.ResourceVersion != "" && pool.Metadata.ResourceVersion != existing.Metadata.ResourceVersion {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"kind":    "Status",
							"status":  "Failure",
							"message": fmt.Sprintf("Operation cannot be fulfilled on podwarmpools.cubepod.io %q: conflicting resourceVersion", name),
							"reason":  "Conflict",
							"code":    409,
						})
						return
					}
					pool.Metadata.Namespace = ns
					pool.Metadata.ResourceVersion = "2"
					m.podWarmPools[key] = pool
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(pool)
					return

				case http.MethodDelete:
					m.deleteCalls++
					if _, ok := m.podWarmPools[key]; !ok {
						http.NotFound(w, r)
						return
					}
					delete(m.podWarmPools, key)
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
					return

				default:
					http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
					return
				}
			}

			// Status subresource: /apis/cubepod.io/v1alpha1/namespaces/{ns}/podwarmpools/{name}/status
			if len(parts) == 4 && parts[3] == "status" {
				if r.Method == http.MethodPut {
					m.putStatus++
					m.wpPutStatus++
					var pool v1alpha1.PodWarmPool
					if err := json.NewDecoder(r.Body).Decode(&pool); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					existing, ok := m.podWarmPools[key]
					if !ok {
						http.NotFound(w, r)
						return
					}
					if pool.Metadata.ResourceVersion != existing.Metadata.ResourceVersion {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"kind":    "Status",
							"status":  "Failure",
							"message": fmt.Sprintf("Operation cannot be fulfilled on podwarmpools.cubepod.io %q: the object has been modified; please apply your changes to the latest version and try again", name),
							"reason":  "Conflict",
							"code":    409,
						})
						return
					}
					existing.Status = pool.Status
					existing.Metadata.ResourceVersion = "3"
					m.podWarmPools[key] = existing
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(existing)
					return
				}
			}

			http.NotFound(w, r)
			return
		}

		if resource != "podboxes" {
			http.NotFound(w, r)
			return
		}

		// List or Create: /apis/cubepod.io/v1alpha1/namespaces/{ns}/podboxes
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				m.listCalls++
				var items []v1alpha1.PodBox
				for _, b := range m.podBoxes {
					if b.Metadata.Namespace == ns || ns == "" {
						items = append(items, b)
					}
				}
				list := v1alpha1.PodBoxList{
					APIVersion: "cubepod.io/v1alpha1",
					Kind:       "PodBoxList",
					Items:      items,
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(list)
				return

			case http.MethodPost:
				m.postCalls++
				var box v1alpha1.PodBox
				if err := json.NewDecoder(r.Body).Decode(&box); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				key := fmt.Sprintf("%s/%s", ns, box.Metadata.Name)
				if _, exists := m.podBoxes[key]; exists {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"kind":    "Status",
						"status":  "Failure",
						"message": fmt.Sprintf("podboxes.cubepod.io %q already exists", box.Metadata.Name),
						"reason":  "AlreadyExists",
						"code":    409,
					})
					return
				}
				if box.Metadata.ResourceVersion == "" {
					box.Metadata.ResourceVersion = "1"
				}
				box.Metadata.Namespace = ns
				// Real K8s API server strips .status on POST when subresources.status is enabled.
				box.Status = v1alpha1.PodBoxStatus{}
				m.podBoxes[key] = box
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(box)
				return
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
		}

		// Specific item: /apis/cubepod.io/v1alpha1/namespaces/{ns}/podboxes/{name} or .../status
		name := parts[2]
		key := fmt.Sprintf("%s/%s", ns, name)

		if len(parts) == 3 {
			switch r.Method {
			case http.MethodGet:
				m.getCalls++
				box, ok := m.podBoxes[key]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(box)
				return

			case http.MethodPut:
				m.putCalls++
				var box v1alpha1.PodBox
				if err := json.NewDecoder(r.Body).Decode(&box); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				box.Metadata.Namespace = ns
				box.Metadata.ResourceVersion = "2"
				m.podBoxes[key] = box
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(box)
				return

			case http.MethodDelete:
				m.deleteCalls++
				if _, ok := m.podBoxes[key]; !ok {
					http.NotFound(w, r)
					return
				}
				delete(m.podBoxes, key)
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "Success"})
				return

			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
		}

		if len(parts) == 4 && parts[3] == "status" {
			if m.return404PutStatus {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodPut {
				m.putStatus++
				var box v1alpha1.PodBox
				if err := json.NewDecoder(r.Body).Decode(&box); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				existing, ok := m.podBoxes[key]
				if !ok {
					http.NotFound(w, r)
					return
				}
				existing.Status = box.Status
				existing.Metadata.ResourceVersion = "3"
				m.podBoxes[key] = existing
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(existing)
				return
			}
		}

		http.NotFound(w, r)
	})

	ts := httptest.NewServer(handler)
	return m, ts
}

func TestRESTKubeClient_PodBox_CRUD(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	now := time.Now().UTC()
	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "test-box-1",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: 1,
		},
		Status: v1alpha1.PodBoxStatus{
			Phase:          v1alpha1.PodBoxPhaseRunning,
			PodName:        "test-box-1",
			PodIP:          "10.244.1.20",
			LastActivityAt: &now,
		},
	}

	// 1. Create (POST)
	if err := client.UpsertPodBox(ctx, box); err != nil {
		t.Fatalf("UpsertPodBox (create) failed: %v", err)
	}

	mock.mu.RLock()
	if mock.postCalls != 1 {
		t.Errorf("expected 1 POST call, got %d", mock.postCalls)
	}
	if mock.putStatus != 1 {
		t.Errorf("expected 1 PUT status call on create, got %d", mock.putStatus)
	}
	mock.mu.RUnlock()

	// 2. Get (GET)
	fetched, err := client.GetPodBox(ctx, "cubepod-workloads", "test-box-1")
	if err != nil {
		t.Fatalf("GetPodBox failed: %v", err)
	}
	if fetched.Metadata.Name != "test-box-1" {
		t.Errorf("expected name 'test-box-1', got %s", fetched.Metadata.Name)
	}
	if fetched.Status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Errorf("expected phase Running, got %s", fetched.Status.Phase)
	}
	if fetched.Status.PodIP != "10.244.1.20" {
		t.Errorf("expected PodIP '10.244.1.20', got %s", fetched.Status.PodIP)
	}

	// 3. List (GET)
	boxes, err := client.ListPodBoxes(ctx, "cubepod-workloads")
	if err != nil {
		t.Fatalf("ListPodBoxes failed: %v", err)
	}
	if len(boxes) != 1 {
		t.Fatalf("expected 1 PodBox in list, got %d", len(boxes))
	}
	if boxes[0].Metadata.Name != "test-box-1" {
		t.Errorf("expected item 'test-box-1', got %s", boxes[0].Metadata.Name)
	}

	// 4. Update on 409 Conflict:
	// Calling UpsertPodBox again when item exists triggers POST -> 409 -> GET rv -> PUT obj -> PUT status
	box.Spec.DesiredState = v1alpha1.DesiredStatePaused
	box.Status.Phase = v1alpha1.PodBoxPhasePaused
	box.Status.PodName = ""
	box.Status.PodIP = ""
	box.Status.PausedAt = &now

	if err := client.UpsertPodBox(ctx, box); err != nil {
		t.Fatalf("UpsertPodBox (update on 409) failed: %v", err)
	}

	mock.mu.RLock()
	if mock.putCalls < 1 {
		t.Errorf("expected at least 1 PUT call on conflict, got %d", mock.putCalls)
	}
	if mock.putStatus < 1 {
		t.Errorf("expected at least 1 PUT status call on conflict, got %d", mock.putStatus)
	}
	mock.mu.RUnlock()

	updated, err := client.GetPodBox(ctx, "cubepod-workloads", "test-box-1")
	if err != nil {
		t.Fatalf("GetPodBox after update failed: %v", err)
	}
	if updated.Spec.DesiredState != v1alpha1.DesiredStatePaused {
		t.Errorf("expected DesiredState Paused, got %s", updated.Spec.DesiredState)
	}
	if updated.Status.Phase != v1alpha1.PodBoxPhasePaused {
		t.Errorf("expected Phase Paused, got %s", updated.Status.Phase)
	}

	// 5. Delete (DELETE)
	if err := client.DeletePodBox(ctx, "cubepod-workloads", "test-box-1"); err != nil {
		t.Fatalf("DeletePodBox failed: %v", err)
	}

	_, err = client.GetPodBox(ctx, "cubepod-workloads", "test-box-1")
	if err == nil {
		t.Fatal("expected GetPodBox to fail after deletion, got nil error")
	}

	// 6. Delete non-existent object (returns 404) should be ignored gracefully
	if err := client.DeletePodBox(ctx, "cubepod-workloads", "non-existent-box"); err != nil {
		t.Fatalf("DeletePodBox on non-existent object should ignore 404, got: %v", err)
	}

	// 7. CRD not installed (404 NotFound on POST) returns nil gracefully
	mock.mu.Lock()
	mock.return404CRD = true
	mock.mu.Unlock()

	crdMissingBox := v1alpha1.PodBox{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "crd-missing-box",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStateRunning,
		},
	}
	if err := client.UpsertPodBox(ctx, crdMissingBox); err != nil {
		t.Fatalf("UpsertPodBox when CRD is missing (404) should return nil, got: %v", err)
	}
}

func TestRESTKubeClient_UpsertPodBox_Status404ReturnsError(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	box := v1alpha1.PodBox{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "box-404-status",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStateRunning,
		},
	}
	// Initial create succeeds
	if err := client.UpsertPodBox(ctx, box); err != nil {
		t.Fatalf("initial create failed: %v", err)
	}

	// Make PUT .../status return 404
	mock.return404PutStatus = true

	// Upsert again (triggering conflict update -> PUT status returns 404)
	err := client.UpsertPodBox(ctx, box)
	if err == nil {
		t.Fatal("expected error when status update returns 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected error message to mention 404, got: %v", err)
	}
}

func TestReconciler_SyncFromKube_Recovery(t *testing.T) {
	_, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	// Initial Reconciler: spawns two PodBoxes
	r1 := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)

	spec1 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	st1, err := r1.Reconcile(ctx, "box-alpha", spec1)
	if err != nil {
		t.Fatalf("Reconcile box-alpha failed: %v", err)
	}
	if st1.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected box-alpha phase Running, got %s", st1.Phase)
	}

	spec2 := v1alpha1.PodBoxSpec{
		DesiredState:     v1alpha1.DesiredStateRunning,
		RootfsGeneration: 1,
	}
	_, err = r1.Reconcile(ctx, "box-beta", spec2)
	if err != nil {
		t.Fatalf("Reconcile box-beta failed: %v", err)
	}

	// Pause box-beta via Pause method
	st2, err := r1.Pause(ctx, "box-beta")
	if err != nil {
		t.Fatalf("Pause box-beta failed: %v", err)
	}
	if st2.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected box-beta phase Paused, got %s", st2.Phase)
	}

	// Verify both were persisted to the CRD API
	storedAlpha, err := client.GetPodBox(ctx, "cubepod-workloads", "box-alpha")
	if err != nil {
		t.Fatalf("GetPodBox box-alpha failed: %v", err)
	}
	if storedAlpha.Status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Fatalf("expected stored box-alpha phase Running, got %s", storedAlpha.Status.Phase)
	}

	storedBeta, err := client.GetPodBox(ctx, "cubepod-workloads", "box-beta")
	if err != nil {
		t.Fatalf("GetPodBox box-beta failed: %v", err)
	}
	if storedBeta.Status.Phase != v1alpha1.PodBoxPhasePaused {
		t.Fatalf("expected stored box-beta phase Paused, got %s", storedBeta.Status.Phase)
	}

	// Create a completely fresh Reconciler instance (simulating controller restart)
	r2 := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Memory state in r2 should be empty initially
	if _, ok := r2.GetBox("box-alpha"); ok {
		t.Fatal("fresh reconciler should not have box-alpha before SyncFromKube")
	}
	if _, ok := r2.GetBox("box-beta"); ok {
		t.Fatal("fresh reconciler should not have box-beta before SyncFromKube")
	}

	// Recover state via SyncFromKube
	recoveredCount, err := r2.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if recoveredCount != 2 {
		t.Fatalf("expected 2 recovered PodBoxes, got %d", recoveredCount)
	}

	// Verify recovered boxes in r2
	recAlpha, ok := r2.GetBox("box-alpha")
	if !ok {
		t.Fatal("expected box-alpha to be present in r2 after sync")
	}
	if recAlpha.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Errorf("expected box-alpha recovered phase Running, got %s", recAlpha.Phase)
	}
	if recAlpha.PodName != "box-alpha" {
		t.Errorf("expected box-alpha podName box-alpha, got %s", recAlpha.PodName)
	}

	recBeta, ok := r2.GetBox("box-beta")
	if !ok {
		t.Fatal("expected box-beta to be present in r2 after sync")
	}
	if recBeta.Phase != v1alpha1.PodBoxPhasePaused {
		t.Errorf("expected box-beta recovered phase Paused, got %s", recBeta.Phase)
	}

	specBeta, ok := r2.GetBoxSpec("box-beta")
	if !ok {
		t.Fatal("expected box-beta spec to be present in r2 after sync")
	}
	if specBeta.DesiredState != v1alpha1.DesiredStatePaused {
		t.Errorf("expected box-beta spec desiredState Paused, got %s", specBeta.DesiredState)
	}

	// DeleteBox removes from memory and from Kubernetes API
	if err := r2.DeleteBox(ctx, "box-alpha", false); err != nil {
		t.Fatalf("DeleteBox failed: %v", err)
	}
	if _, ok := r2.GetBox("box-alpha"); ok {
		t.Fatal("box-alpha should be removed from r2 memory")
	}

	// Reconciler 3 should only see 1 box after alpha was deleted
	r3 := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)
	count3, err := r3.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube in r3 failed: %v", err)
	}
	if count3 != 1 {
		t.Fatalf("expected 1 box in r3, got %d", count3)
	}
	if _, ok := r3.GetBox("box-beta"); !ok {
		t.Fatal("expected box-beta to survive in r3")
	}
}

func TestMemoryKubeClient_PodBox_CRUD_And_Sync(t *testing.T) {
	memClient := controller.NewMemoryKubeClient()
	ctx := context.Background()

	box1 := v1alpha1.PodBox{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "mem-box-1",
			Namespace: "default",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStateRunning,
		},
		Status: v1alpha1.PodBoxStatus{
			Phase: v1alpha1.PodBoxPhaseRunning,
		},
	}
	box2 := v1alpha1.PodBox{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "mem-box-2",
			Namespace: "default",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStatePaused,
		},
		Status: v1alpha1.PodBoxStatus{
			Phase: v1alpha1.PodBoxPhasePaused,
		},
	}

	// Upsert
	if err := memClient.UpsertPodBox(ctx, box1); err != nil {
		t.Fatalf("UpsertPodBox box1 failed: %v", err)
	}
	if err := memClient.UpsertPodBox(ctx, box2); err != nil {
		t.Fatalf("UpsertPodBox box2 failed: %v", err)
	}

	// Get
	got1, err := memClient.GetPodBox(ctx, "default", "mem-box-1")
	if err != nil {
		t.Fatalf("GetPodBox failed: %v", err)
	}
	if got1.Status.Phase != v1alpha1.PodBoxPhaseRunning {
		t.Errorf("expected phase Running, got %s", got1.Status.Phase)
	}

	// List
	list, err := memClient.ListPodBoxes(ctx, "default")
	if err != nil {
		t.Fatalf("ListPodBoxes failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 boxes, got %d", len(list))
	}

	// Reconciler SyncFromKube
	r := controller.NewReconciler(
		controller.WithKubeClient(memClient),
		controller.WithNamespace("default"),
	)
	count, err := r.SyncFromKube(ctx)
	if err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 recovered boxes, got %d", count)
	}
	if _, ok := r.GetBox("mem-box-1"); !ok {
		t.Fatal("expected mem-box-1 recovered")
	}
	if _, ok := r.GetBox("mem-box-2"); !ok {
		t.Fatal("expected mem-box-2 recovered")
	}

	// Delete
	if err := memClient.DeletePodBox(ctx, "default", "mem-box-1"); err != nil {
		t.Fatalf("DeletePodBox failed: %v", err)
	}
	_, err = memClient.GetPodBox(ctx, "default", "mem-box-1")
	if err == nil {
		t.Fatal("expected GetPodBox to return error after deletion")
	}
}

func TestRESTKubeClient_PodWarmPool_StatusPersistenceAndConflict(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	pool := v1alpha1.PodWarmPool{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodWarmPool",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "test-pool-1",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodWarmPoolSpec{
			TemplateRef: "ubuntu:24.04",
			Replicas:    3,
		},
		Status: v1alpha1.PodWarmPoolStatus{
			ReadyReplicas:     3,
			AvailableReplicas: 3,
		},
	}

	// 1. Initial Upsert (POST -> 201 Created -> PUT /status with rv=1 -> updated to rv=3)
	if err := client.UpsertPodWarmPool(ctx, pool); err != nil {
		t.Fatalf("UpsertPodWarmPool failed: %v", err)
	}

	mock.mu.RLock()
	if mock.wpPostCalls != 1 {
		t.Errorf("expected 1 POST call, got %d", mock.wpPostCalls)
	}
	if mock.wpPutStatus != 1 {
		t.Errorf("expected 1 PUT status call, got %d", mock.wpPutStatus)
	}
	mock.mu.RUnlock()

	// 2. Verify status persistence via GetPodWarmPool
	got, err := client.GetPodWarmPool(ctx, "cubepod-workloads", "test-pool-1")
	if err != nil {
		t.Fatalf("GetPodWarmPool failed: %v", err)
	}
	if got.Status.AvailableReplicas != 3 || got.Status.ReadyReplicas != 3 {
		t.Fatalf("expected 3 available/ready replicas, got %+v", got.Status)
	}
	if got.Metadata.ResourceVersion != "3" {
		t.Fatalf("expected resourceVersion 3 after status update, got %s", got.Metadata.ResourceVersion)
	}

	// 3. Update status directly with matching resourceVersion (via UpdatePodWarmPoolStatus)
	newStatus := v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     2,
		AvailableReplicas: 2,
	}
	if err := client.UpdatePodWarmPoolStatus(ctx, "cubepod-workloads", "test-pool-1", newStatus); err != nil {
		t.Fatalf("UpdatePodWarmPoolStatus failed: %v", err)
	}

	gotAfterUpdate, err := client.GetPodWarmPool(ctx, "cubepod-workloads", "test-pool-1")
	if err != nil {
		t.Fatalf("GetPodWarmPool failed: %v", err)
	}
	if gotAfterUpdate.Status.AvailableReplicas != 2 {
		t.Fatalf("expected 2 available replicas, got %d", gotAfterUpdate.Status.AvailableReplicas)
	}

	// 4. Test 409 Conflict on mismatched resourceVersion on /status:
	// Manually attempt to PUT /status with a stale resourceVersion (e.g. "stale-version-99")
	stalePool := *gotAfterUpdate
	stalePool.Metadata.ResourceVersion = "stale-version-99"
	stalePayload, _ := json.Marshal(stalePool)
	statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s/status", ts.URL, "cubepod-workloads", "test-pool-1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, statusURL, bytes.NewReader(stalePayload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("failed to send PUT status request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on mismatched resourceVersion, got status %d", resp.StatusCode)
	}

	// 5. UpsertPodWarmPool on 409 Conflict:
	// Calling UpsertPodWarmPool again triggers POST (409) -> GET rv -> PUT obj -> PUT status
	pool.Spec.Replicas = 5
	pool.Status.AvailableReplicas = 5
	pool.Status.ReadyReplicas = 5
	if err := client.UpsertPodWarmPool(ctx, pool); err != nil {
		t.Fatalf("UpsertPodWarmPool on existing pool failed: %v", err)
	}

	gotFinal, err := client.GetPodWarmPool(ctx, "cubepod-workloads", "test-pool-1")
	if err != nil {
		t.Fatalf("GetPodWarmPool final failed: %v", err)
	}
	if gotFinal.Spec.Replicas != 5 {
		t.Errorf("expected spec replicas 5, got %d", gotFinal.Spec.Replicas)
	}
	if gotFinal.Status.AvailableReplicas != 5 {
		t.Errorf("expected status available replicas 5, got %d", gotFinal.Status.AvailableReplicas)
	}
}

func TestReconciler_SyncFromKube_WarmPoolSpecAndCounter(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	// Initial pool in Kube with spec replicas = 2
	pool := v1alpha1.PodWarmPool{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodWarmPool",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "sync-pool",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodWarmPoolSpec{
			TemplateRef: "ubuntu:24.04",
			Replicas:    2,
		},
	}
	if err := client.UpsertPodWarmPool(ctx, pool); err != nil {
		t.Fatalf("UpsertPodWarmPool failed: %v", err)
	}

	// Create a PodBox whose PodName reflects an existing warm pod index (e.g. 7)
	box := v1alpha1.PodBox{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "existing-box-7",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodBoxSpec{
			DesiredState: v1alpha1.DesiredStateRunning,
		},
		Status: v1alpha1.PodBoxStatus{
			Phase:   v1alpha1.PodBoxPhaseRunning,
			PodName: "sync-pool-warm-7",
		},
	}
	if err := client.UpsertPodBox(ctx, box); err != nil {
		t.Fatalf("UpsertPodBox failed: %v", err)
	}

	r := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)

	// Initial sync recovers pool and box, and updates warmCounter to 7
	if _, err := r.SyncFromKube(ctx); err != nil {
		t.Fatalf("initial SyncFromKube failed: %v", err)
	}

	specs := r.GetWarmPoolSpecs()
	if specs["sync-pool"].Replicas != 2 {
		t.Fatalf("expected replicas 2 initially, got %d", specs["sync-pool"].Replicas)
	}

	// Now simulate external update on APIServer (e.g. kubectl scale to replicas = 5)
	pool.Spec.Replicas = 5
	if err := client.UpsertPodWarmPool(ctx, pool); err != nil {
		t.Fatalf("scale update UpsertPodWarmPool failed: %v", err)
	}

	// Reconciler sync adopts new spec
	if _, err := r.SyncFromKube(ctx); err != nil {
		t.Fatalf("second SyncFromKube failed: %v", err)
	}

	specsAfter := r.GetWarmPoolSpecs()
	if specsAfter["sync-pool"].Replicas != 5 {
		t.Fatalf("expected reconciler to adopt updated replicas 5 from APIServer, got %d", specsAfter["sync-pool"].Replicas)
	}

	// Now reconcile warm pool to add replicas; next created pod must be index > 7 (i.e. 8)
	st, err := r.ReconcileWarmPool(ctx, "sync-pool", specsAfter["sync-pool"])
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if st.AvailableReplicas != 5 {
		t.Fatalf("expected 5 available replicas, got %d", st.AvailableReplicas)
	}

	claimed, err := r.ClaimWarmPod(ctx, "sync-pool", "new-box")
	if err != nil {
		t.Fatalf("ClaimWarmPod failed: %v", err)
	}
	if !strings.Contains(claimed.ID, "-warm-8") {
		t.Fatalf("expected newly created entry to have index 8 avoiding collision with 7, got %s", claimed.ID)
	}
	_ = mock
}

func TestRESTKubeClient_PodWarmPool_MissingCRD(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	mock.mu.Lock()
	mock.return404CRD = true
	mock.mu.Unlock()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	pool := v1alpha1.PodWarmPool{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "cubepod-workloads",
		},
		Spec: v1alpha1.PodWarmPoolSpec{
			TemplateRef: "base-tpl",
			Replicas:    2,
		},
	}

	// 1. UpsertPodWarmPool should return ErrCRDNotInstalled when APIServer returns 404
	err := client.UpsertPodWarmPool(ctx, pool)
	if !errors.Is(err, controller.ErrCRDNotInstalled) {
		t.Fatalf("expected ErrCRDNotInstalled from UpsertPodWarmPool, got: %v", err)
	}

	// 2. ListPodWarmPools should return ErrCRDNotInstalled when APIServer returns 404
	pools, err := client.ListPodWarmPools(ctx, "cubepod-workloads")
	if !errors.Is(err, controller.ErrCRDNotInstalled) {
		t.Fatalf("expected ErrCRDNotInstalled from ListPodWarmPools, got: %v (pools=%v)", err, pools)
	}

	// 3. Reconciler with missing CRD: pool is created in memory, but not persisted
	r := controller.NewReconciler(
		controller.WithKubeClient(client),
		controller.WithNamespace("cubepod-workloads"),
	)
	st, err := r.ReconcileWarmPool(ctx, "missing-crd-pool", v1alpha1.PodWarmPoolSpec{
		TemplateRef: "base-tpl",
		Replicas:    2,
	})
	if err != nil {
		t.Fatalf("ReconcileWarmPool failed: %v", err)
	}
	if st.AvailableReplicas != 2 {
		t.Fatalf("expected 2 available replicas, got %d", st.AvailableReplicas)
	}

	// 4. SyncFromKube with missing CRD should NOT wipe in-memory pools
	if _, err := r.SyncFromKube(ctx); err != nil {
		t.Fatalf("SyncFromKube failed: %v", err)
	}

	specs := r.GetWarmPoolSpecs()
	if _, exists := specs["missing-crd-pool"]; !exists {
		t.Fatal("expected missing-crd-pool to NOT be deleted when CRD is not installed")
	}
}

func TestRESTKubeClient_VolumeSnapshot_CRUDAndWaitReady(t *testing.T) {
	mock, ts := newMockCRDServer()
	defer ts.Close()

	client := controller.NewRESTKubeClient(
		ts.URL,
		"cubepod-workloads",
		"test-token",
		ts.Client(),
		controller.WithPollInterval(10*time.Millisecond),
		controller.WithPollTimeout(2*time.Second),
	)
	ctx := context.Background()

	snapClass := "longhorn-snapclass"
	notReady := false
	snap := controller.VolumeSnapshot{
		Metadata: v1alpha1.ObjectMeta{
			Name:      "snap-1",
			Namespace: "cubepod-workloads",
		},
		Spec: controller.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapClass,
			Source: controller.VolumeSnapshotSource{
				PersistentVolumeClaimName: "parent-rootfs-pvc",
			},
		},
		Status: controller.VolumeSnapshotStatus{
			ReadyToUse: &notReady,
		},
	}

	if err := client.EnsureVolumeSnapshot(ctx, snap); err != nil {
		t.Fatalf("EnsureVolumeSnapshot failed: %v", err)
	}
	// Idempotent on 409 Conflict
	if err := client.EnsureVolumeSnapshot(ctx, snap); err != nil {
		t.Fatalf("EnsureVolumeSnapshot idempotent 409 failed: %v", err)
	}

	got, err := client.GetVolumeSnapshot(ctx, "cubepod-workloads", "snap-1")
	if err != nil {
		t.Fatalf("GetVolumeSnapshot failed: %v", err)
	}
	if got.Spec.Source.PersistentVolumeClaimName != "parent-rootfs-pvc" {
		t.Fatalf("unexpected snapshot source: %+v", got.Spec.Source)
	}
	if got.Spec.VolumeSnapshotClassName == nil || *got.Spec.VolumeSnapshotClassName != snapClass {
		t.Fatalf("unexpected volumeSnapshotClassName: %v", got.Spec.VolumeSnapshotClassName)
	}

	// Transition readyToUse -> true asynchronously and verify WaitVolumeSnapshotReady succeeds
	go func() {
		time.Sleep(30 * time.Millisecond)
		mock.mu.Lock()
		defer mock.mu.Unlock()
		s := mock.volumeSnapshots["cubepod-workloads/snap-1"]
		ready := true
		s.Status.ReadyToUse = &ready
		mock.volumeSnapshots["cubepod-workloads/snap-1"] = s
	}()
	if err := client.WaitVolumeSnapshotReady(ctx, "cubepod-workloads", "snap-1"); err != nil {
		t.Fatalf("WaitVolumeSnapshotReady failed: %v", err)
	}

	// Error status causes WaitVolumeSnapshotReady to fail immediately
	mock.mu.Lock()
	errSnap := mock.volumeSnapshots["cubepod-workloads/snap-1"]
	errSnap.Status.ReadyToUse = &notReady
	errSnap.Status.Error = &controller.VolumeSnapshotError{Message: "csi snapshot failed"}
	mock.volumeSnapshots["cubepod-workloads/snap-1"] = errSnap
	mock.mu.Unlock()
	if err := client.WaitVolumeSnapshotReady(ctx, "cubepod-workloads", "snap-1"); err == nil || !strings.Contains(err.Error(), "csi snapshot failed") {
		t.Fatalf("expected WaitVolumeSnapshotReady error containing 'csi snapshot failed', got %v", err)
	}

	// DeleteVolumeSnapshot + idempotent 404
	if err := client.DeleteVolumeSnapshot(ctx, "cubepod-workloads", "snap-1"); err != nil {
		t.Fatalf("DeleteVolumeSnapshot failed: %v", err)
	}
	if err := client.DeleteVolumeSnapshot(ctx, "cubepod-workloads", "snap-1"); err != nil {
		t.Fatalf("DeleteVolumeSnapshot idempotent 404 failed: %v", err)
	}
	if _, err := client.GetVolumeSnapshot(ctx, "cubepod-workloads", "snap-1"); err == nil {
		t.Fatal("expected GetVolumeSnapshot after delete to fail")
	}
}

func TestMemoryKubeClient_VolumeSnapshot_CRUDAndWaitReady(t *testing.T) {
	mk := controller.NewMemoryKubeClient()
	ctx := context.Background()

	if err := mk.EnsureVolumeSnapshot(ctx, controller.VolumeSnapshot{
		Metadata: v1alpha1.ObjectMeta{Name: "mem-snap", Namespace: "ns1"},
		Spec: controller.VolumeSnapshotSpec{
			Source: controller.VolumeSnapshotSource{PersistentVolumeClaimName: "pvc-1"},
		},
	}); err != nil {
		t.Fatalf("EnsureVolumeSnapshot failed: %v", err)
	}
	if err := mk.WaitVolumeSnapshotReady(ctx, "ns1", "mem-snap"); err != nil {
		t.Fatalf("WaitVolumeSnapshotReady failed: %v", err)
	}
	got, err := mk.GetVolumeSnapshot(ctx, "ns1", "mem-snap")
	if err != nil {
		t.Fatalf("GetVolumeSnapshot failed: %v", err)
	}
	if got.APIVersion != "snapshot.storage.k8s.io/v1" || got.Kind != "VolumeSnapshot" {
		t.Fatalf("unexpected apiVersion/kind: %s/%s", got.APIVersion, got.Kind)
	}
	if err := mk.DeleteVolumeSnapshot(ctx, "ns1", "mem-snap"); err != nil {
		t.Fatalf("DeleteVolumeSnapshot failed: %v", err)
	}
	if _, err := mk.GetVolumeSnapshot(ctx, "ns1", "mem-snap"); err == nil {
		t.Fatal("expected error getting deleted snapshot")
	}
}

// mockWebhookEventServer emulates the APIServer for sandboxwebhooks.cubepod.io and core/v1 Events.
type mockWebhookEventServer struct {
	mu          sync.Mutex
	webhooks    map[string]v1alpha1.SandboxWebhook
	events      map[string]controller.KubeEvent
	rv          int
	statusPuts  int
	specPuts    int
	lastListSel string
}

func newMockWebhookEventServer(t *testing.T) (*mockWebhookEventServer, *httptest.Server) {
	t.Helper()
	m := &mockWebhookEventServer{
		webhooks: make(map[string]v1alpha1.SandboxWebhook),
		events:   make(map[string]controller.KubeEvent),
	}
	matches := func(labels map[string]string, sel string) bool {
		if sel == "" {
			return true
		}
		for _, part := range strings.Split(sel, ",") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 || labels[kv[0]] != kv[1] {
				return false
			}
		}
		return true
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		const whPrefix = "/apis/cubepod.io/v1alpha1/namespaces/cubepod-workloads/sandboxwebhooks"
		const evPrefix = "/api/v1/namespaces/cubepod-workloads/events"
		switch {
		case strings.HasPrefix(path, whPrefix):
			rest := strings.Trim(strings.TrimPrefix(path, whPrefix), "/")
			parts := strings.Split(rest, "/")
			switch {
			case rest == "" && r.Method == http.MethodGet:
				m.lastListSel = r.URL.Query().Get("labelSelector")
				items := []v1alpha1.SandboxWebhook{}
				for _, wh := range m.webhooks {
					if matches(wh.Metadata.Labels, m.lastListSel) {
						items = append(items, wh)
					}
				}
				_ = json.NewEncoder(w).Encode(v1alpha1.SandboxWebhookList{Items: items})
			case rest == "" && r.Method == http.MethodPost:
				var wh v1alpha1.SandboxWebhook
				_ = json.NewDecoder(r.Body).Decode(&wh)
				if _, ok := m.webhooks[wh.Metadata.Name]; ok {
					w.WriteHeader(http.StatusConflict)
					return
				}
				m.rv++
				wh.Metadata.ResourceVersion = fmt.Sprint(m.rv)
				wh.Status = v1alpha1.SandboxWebhookStatus{} // status ignored on create
				m.webhooks[wh.Metadata.Name] = wh
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(wh)
			case len(parts) >= 1:
				name := parts[0]
				existing, ok := m.webhooks[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var in v1alpha1.SandboxWebhook
				if r.Method == http.MethodPut {
					_ = json.NewDecoder(r.Body).Decode(&in)
					if in.Metadata.ResourceVersion != existing.Metadata.ResourceVersion {
						w.WriteHeader(http.StatusConflict)
						return
					}
					m.rv++
				}
				switch {
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(existing)
				case r.Method == http.MethodDelete:
					delete(m.webhooks, name)
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "status":
					m.statusPuts++
					existing.Status = in.Status
					existing.Metadata.ResourceVersion = fmt.Sprint(m.rv)
					m.webhooks[name] = existing
					_ = json.NewEncoder(w).Encode(existing)
				case r.Method == http.MethodPut:
					m.specPuts++
					in.Status = existing.Status
					in.Metadata.ResourceVersion = fmt.Sprint(m.rv)
					m.webhooks[name] = in
					_ = json.NewEncoder(w).Encode(in)
				}
			}
		case path == evPrefix && r.Method == http.MethodPost:
			var ev controller.KubeEvent
			_ = json.NewDecoder(r.Body).Decode(&ev)
			if _, ok := m.events[ev.Metadata.Name]; ok {
				w.WriteHeader(http.StatusConflict)
				return
			}
			m.events[ev.Metadata.Name] = ev
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ev)
		case path == evPrefix && r.Method == http.MethodGet:
			sel := r.URL.Query().Get("labelSelector")
			items := []controller.KubeEvent{}
			for _, ev := range m.events {
				if matches(ev.Metadata.Labels, sel) {
					items = append(items, ev)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return m, ts
}

func sampleSandboxWebhook(name string) v1alpha1.SandboxWebhook {
	return v1alpha1.SandboxWebhook{
		Metadata: v1alpha1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"cubepod.io/resource": "sandbox-webhook", "cubepod.io/tenant": "abcd"},
		},
		Spec: v1alpha1.SandboxWebhookSpec{
			TenantHash: "abcd",
			Name:       "billing",
			URL:        "https://example.com/hook",
			Events:     []string{"sandbox.lifecycle.created"},
			Enabled:    true,
			SecretName: "cubepod-whk-sec-1",
		},
	}
}

func TestRESTKubeClient_SandboxWebhook_CRUDAndStatus(t *testing.T) {
	mock, ts := newMockWebhookEventServer(t)
	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	wh := sampleSandboxWebhook("cubepod-whk-1")
	now := time.Now().UTC().Truncate(time.Second)
	wh.Status = v1alpha1.SandboxWebhookStatus{LastDeliveryTime: &now, ConsecutiveFailures: 2}
	if err := client.UpsertSandboxWebhook(ctx, wh); err != nil {
		t.Fatalf("UpsertSandboxWebhook create: %v", err)
	}
	got, err := client.GetSandboxWebhook(ctx, "", "cubepod-whk-1")
	if err != nil {
		t.Fatalf("GetSandboxWebhook: %v", err)
	}
	if got.Status.ConsecutiveFailures != 2 || got.Status.LastDeliveryTime == nil || !got.Status.LastDeliveryTime.Equal(now) {
		t.Fatalf("status not persisted via /status on create: %+v", got.Status)
	}
	if got.Spec.SecretName != "cubepod-whk-sec-1" || got.Spec.URL != "https://example.com/hook" {
		t.Fatalf("unexpected spec: %+v", got.Spec)
	}

	// Conflict path: second upsert replaces metadata/spec via PUT without overwriting /status.
	wh.Spec.URL = "https://example.com/v2"
	wh.Status.ConsecutiveFailures = 0
	if err := client.UpsertSandboxWebhook(ctx, wh); err != nil {
		t.Fatalf("UpsertSandboxWebhook update: %v", err)
	}
	got, _ = client.GetSandboxWebhook(ctx, "", "cubepod-whk-1")
	if got.Spec.URL != "https://example.com/v2" || got.Status.ConsecutiveFailures != 2 {
		t.Fatalf("update spec or preserved status wrong: spec=%+v status=%+v", got.Spec, got.Status)
	}
	if mock.specPuts != 1 || mock.statusPuts != 1 {
		t.Fatalf("specPuts=%d statusPuts=%d; want 1 and 1", mock.specPuts, mock.statusPuts)
	}

	// Status-only update with stale resourceVersion yields ErrCASConflict.
	stale := *got
	stale.Metadata.ResourceVersion = "1"
	stale.Status.ConsecutiveFailures = 9
	if err := client.UpdateSandboxWebhookStatus(ctx, stale); !errors.Is(err, controller.ErrCASConflict) {
		t.Fatalf("stale status update err = %v; want ErrCASConflict", err)
	}
	fresh := *got
	fresh.Status.DeadLetters = []v1alpha1.SandboxWebhookDeadLetter{{ID: "dlq_1", EventID: "e1", EventType: "sandbox.lifecycle.created", Reason: "queue_full", Event: json.RawMessage(`{"id":"e1"}`)}}
	if err := client.UpdateSandboxWebhookStatus(ctx, fresh); err != nil {
		t.Fatalf("fresh status update: %v", err)
	}
	got, _ = client.GetSandboxWebhook(ctx, "", "cubepod-whk-1")
	if len(got.Status.DeadLetters) != 1 || got.Status.DeadLetters[0].ID != "dlq_1" {
		t.Fatalf("dead letters not persisted: %+v", got.Status.DeadLetters)
	}

	list, err := client.ListSandboxWebhooks(ctx, "", "cubepod.io/resource=sandbox-webhook,cubepod.io/tenant=abcd")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSandboxWebhooks = %d, %v; want 1", len(list), err)
	}
	if mock.lastListSel != "cubepod.io/resource=sandbox-webhook,cubepod.io/tenant=abcd" {
		t.Fatalf("labelSelector not forwarded: %q", mock.lastListSel)
	}
	if list, _ := client.ListSandboxWebhooks(ctx, "", "cubepod.io/tenant=other"); len(list) != 0 {
		t.Fatalf("expected tenant selector isolation, got %d", len(list))
	}

	if err := client.DeleteSandboxWebhook(ctx, "", "cubepod-whk-1"); err != nil {
		t.Fatalf("DeleteSandboxWebhook: %v", err)
	}
	if err := client.DeleteSandboxWebhook(ctx, "", "cubepod-whk-1"); err != nil {
		t.Fatalf("DeleteSandboxWebhook idempotent: %v", err)
	}
	if _, err := client.GetSandboxWebhook(ctx, "", "cubepod-whk-1"); !errors.Is(err, controller.ErrKubeNotFound) {
		t.Fatalf("Get after delete err = %v; want ErrKubeNotFound", err)
	}
}

func TestRESTKubeClient_KubeEvent_CreateDedupAndList(t *testing.T) {
	_, ts := newMockWebhookEventServer(t)
	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()
	now := time.Now().UTC()
	ev := controller.KubeEvent{
		Metadata: v1alpha1.ObjectMeta{
			Name:   "sbx-1.abc123",
			Labels: map[string]string{"cubepod.io/resource": "sandbox-event", "cubepod.io/sandbox": "sbx-1"},
		},
		InvolvedObject: controller.KubeObjectReference{APIVersion: "cubepod.io/v1alpha1", Kind: "PodBox", Name: "sbx-1"},
		Reason:         "PodBoxPaused",
		Type:           controller.KubeEventTypeNormal,
		FirstTimestamp: &now,
		LastTimestamp:  &now,
		Count:          1,
	}
	if err := client.CreateKubeEvent(ctx, ev); err != nil {
		t.Fatalf("CreateKubeEvent: %v", err)
	}
	if err := client.CreateKubeEvent(ctx, ev); !errors.Is(err, controller.ErrKubeAlreadyExists) {
		t.Fatalf("duplicate CreateKubeEvent err = %v; want ErrKubeAlreadyExists", err)
	}
	list, err := client.ListKubeEvents(ctx, "", "cubepod.io/resource=sandbox-event,cubepod.io/sandbox=sbx-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListKubeEvents = %d, %v; want 1", len(list), err)
	}
	if list[0].InvolvedObject.Namespace != "cubepod-workloads" || list[0].InvolvedObject.Kind != "PodBox" {
		t.Fatalf("involvedObject not defaulted: %+v", list[0].InvolvedObject)
	}
	if list, _ := client.ListKubeEvents(ctx, "", "cubepod.io/sandbox=other"); len(list) != 0 {
		t.Fatalf("expected label filtering, got %d", len(list))
	}
}

func TestMemoryKubeClient_SandboxWebhookAndEvents(t *testing.T) {
	client := controller.NewMemoryKubeClient()
	ctx := context.Background()
	wh := sampleSandboxWebhook("cubepod-whk-2")
	wh.Metadata.Namespace = "cubepod-workloads"
	if err := client.UpsertSandboxWebhook(ctx, wh); err != nil {
		t.Fatalf("UpsertSandboxWebhook: %v", err)
	}
	got, err := client.GetSandboxWebhook(ctx, "cubepod-workloads", "cubepod-whk-2")
	if err != nil {
		t.Fatalf("GetSandboxWebhook: %v", err)
	}
	stale := *got
	got.Status.ConsecutiveFailures = 3
	if err := client.UpdateSandboxWebhookStatus(ctx, *got); err != nil {
		t.Fatalf("UpdateSandboxWebhookStatus: %v", err)
	}
	if err := client.UpdateSandboxWebhookStatus(ctx, stale); !errors.Is(err, controller.ErrCASConflict) {
		t.Fatalf("stale status update err = %v; want ErrCASConflict", err)
	}
	// Upserting spec on existing webhook must preserve ConsecutiveFailures=3.
	wh.Spec.URL = "https://example.com/updated"
	wh.Status.ConsecutiveFailures = 0
	if err := client.UpsertSandboxWebhook(ctx, wh); err != nil {
		t.Fatalf("UpsertSandboxWebhook spec update: %v", err)
	}
	afterUpsert, _ := client.GetSandboxWebhook(ctx, "cubepod-workloads", "cubepod-whk-2")
	if afterUpsert.Spec.URL != "https://example.com/updated" || afterUpsert.Status.ConsecutiveFailures != 3 {
		t.Fatalf("UpsertSandboxWebhook overwrote status: spec=%+v status=%+v", afterUpsert.Spec, afterUpsert.Status)
	}
	if list, _ := client.ListSandboxWebhooks(ctx, "cubepod-workloads", "cubepod.io/tenant=abcd"); len(list) != 1 {
		t.Fatalf("ListSandboxWebhooks = %d; want 1", len(list))
	}
	if err := client.DeleteSandboxWebhook(ctx, "cubepod-workloads", "cubepod-whk-2"); err != nil {
		t.Fatalf("DeleteSandboxWebhook: %v", err)
	}
	if _, err := client.GetSandboxWebhook(ctx, "cubepod-workloads", "cubepod-whk-2"); !errors.Is(err, controller.ErrKubeNotFound) {
		t.Fatalf("Get after delete err = %v; want ErrKubeNotFound", err)
	}

	for i := 0; i < controller.MaxMemoryKubeEvents+5; i++ {
		ev := controller.KubeEvent{Metadata: v1alpha1.ObjectMeta{Name: fmt.Sprintf("ev-%d", i), Namespace: "cubepod-workloads", Labels: map[string]string{"cubepod.io/resource": "sandbox-event"}}}
		if err := client.CreateKubeEvent(ctx, ev); err != nil {
			t.Fatalf("CreateKubeEvent %d: %v", i, err)
		}
	}
	dup := controller.KubeEvent{Metadata: v1alpha1.ObjectMeta{Name: fmt.Sprintf("ev-%d", controller.MaxMemoryKubeEvents), Namespace: "cubepod-workloads"}}
	if err := client.CreateKubeEvent(ctx, dup); !errors.Is(err, controller.ErrKubeAlreadyExists) {
		t.Fatalf("duplicate err = %v; want ErrKubeAlreadyExists", err)
	}
	list, _ := client.ListKubeEvents(ctx, "cubepod-workloads", "cubepod.io/resource=sandbox-event")
	if len(list) != controller.MaxMemoryKubeEvents {
		t.Fatalf("memory events = %d; want bounded at %d", len(list), controller.MaxMemoryKubeEvents)
	}
	if list[0].Metadata.Name != "ev-5" {
		t.Fatalf("oldest retained = %s; want ev-5", list[0].Metadata.Name)
	}
}

func TestMemoryKubeClient_UpdatePodTemplateStatusCAS(t *testing.T) {
	mk := controller.NewMemoryKubeClient()
	ctx := context.Background()

	if err := mk.CreatePodTemplate(ctx, v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-cas", Namespace: "ns1"},
		Status: v1alpha1.PodTemplateStatus{
			TemplateID:    "tpl-cas",
			LatestBuildID: "bld-1",
			BuildStatus:   "waiting",
		},
	}); err != nil {
		t.Fatalf("CreatePodTemplate failed: %v", err)
	}

	gotA, err := mk.GetPodTemplate(ctx, "ns1", "tpl-cas")
	if err != nil {
		t.Fatalf("GetPodTemplate A failed: %v", err)
	}
	gotB, err := mk.GetPodTemplate(ctx, "ns1", "tpl-cas")
	if err != nil {
		t.Fatalf("GetPodTemplate B failed: %v", err)
	}
	if gotA.Metadata.ResourceVersion == "" || gotA.Metadata.ResourceVersion != gotB.Metadata.ResourceVersion {
		t.Fatalf("expected matching non-empty ResourceVersion, got A=%q B=%q", gotA.Metadata.ResourceVersion, gotB.Metadata.ResourceVersion)
	}

	gotA.Status.BuildStatus = "building"
	updated, err := mk.UpdatePodTemplateStatusCAS(ctx, *gotA)
	if err != nil {
		t.Fatalf("first CAS update failed: %v", err)
	}
	if updated.Metadata.ResourceVersion == gotA.Metadata.ResourceVersion {
		t.Fatalf("expected bumped ResourceVersion after CAS, still %q", updated.Metadata.ResourceVersion)
	}

	// Second replica holding stale ResourceVersion must fail with ErrCASConflict.
	gotB.Status.BuildStatus = "building"
	if _, err := mk.UpdatePodTemplateStatusCAS(ctx, *gotB); !errors.Is(err, controller.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict on stale ResourceVersion, got %v", err)
	}
}

func TestRESTKubeClient_UpdatePodTemplateStatusCAS(t *testing.T) {
	var (
		mu         sync.Mutex
		rv         = 1
		storedTpl  v1alpha1.PodTemplate
		hasStored  bool
		statusPath = "/apis/cubepod.io/v1alpha1/namespaces/cubepod-workloads/podtemplates/tpl-rest/status"
		objPath    = "/apis/cubepod.io/v1alpha1/namespaces/cubepod-workloads/podtemplates"
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == objPath:
			var incoming v1alpha1.PodTemplate
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			rv++
			incoming.Metadata.ResourceVersion = fmt.Sprintf("%d", rv)
			storedTpl = incoming
			hasStored = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(storedTpl)
		case r.Method == http.MethodGet && r.URL.Path == objPath+"/tpl-rest":
			if !hasStored {
				http.Error(w, "NotFound", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(storedTpl)
		case r.Method == http.MethodPut && r.URL.Path == statusPath:
			var incoming v1alpha1.PodTemplate
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !hasStored {
				http.Error(w, "NotFound", http.StatusNotFound)
				return
			}
			if incoming.Metadata.ResourceVersion != storedTpl.Metadata.ResourceVersion {
				http.Error(w, "Conflict: resourceVersion mismatch", http.StatusConflict)
				return
			}
			rv++
			storedTpl.Status = incoming.Status
			storedTpl.Metadata.ResourceVersion = fmt.Sprintf("%d", rv)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(storedTpl)
		default:
			http.Error(w, "unexpected route: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer ts.Close()

	client := controller.NewRESTKubeClient(ts.URL, "cubepod-workloads", "test-token", ts.Client())
	ctx := context.Background()

	if err := client.CreatePodTemplate(ctx, v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-rest", Namespace: "cubepod-workloads"},
		Status: v1alpha1.PodTemplateStatus{
			TemplateID:    "tpl-rest",
			LatestBuildID: "bld-1",
			BuildStatus:   "waiting",
		},
	}); err != nil {
		t.Fatalf("CreatePodTemplate failed: %v", err)
	}

	snap1, err := client.GetPodTemplate(ctx, "cubepod-workloads", "tpl-rest")
	if err != nil {
		t.Fatalf("GetPodTemplate 1 failed: %v", err)
	}
	snap2, err := client.GetPodTemplate(ctx, "cubepod-workloads", "tpl-rest")
	if err != nil {
		t.Fatalf("GetPodTemplate 2 failed: %v", err)
	}

	// Empty ResourceVersion rejected before HTTP request.
	noRV := *snap1
	noRV.Metadata.ResourceVersion = ""
	if _, err := client.UpdatePodTemplateStatusCAS(ctx, noRV); !errors.Is(err, controller.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict for empty ResourceVersion, got %v", err)
	}

	snap1.Status.BuildStatus = "building"
	res, err := client.UpdatePodTemplateStatusCAS(ctx, *snap1)
	if err != nil {
		t.Fatalf("UpdatePodTemplateStatusCAS winner failed: %v", err)
	}
	if res.Metadata.ResourceVersion == snap1.Metadata.ResourceVersion {
		t.Fatalf("expected bumped ResourceVersion, got %q", res.Metadata.ResourceVersion)
	}

	snap2.Status.BuildStatus = "building"
	if _, err := client.UpdatePodTemplateStatusCAS(ctx, *snap2); !errors.Is(err, controller.ErrCASConflict) {
		t.Fatalf("expected ErrCASConflict for stale REST CAS, got %v", err)
	}
}
