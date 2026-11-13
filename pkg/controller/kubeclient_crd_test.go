package controller_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu           sync.RWMutex
	podBoxes     map[string]v1alpha1.PodBox
	podWarmPools map[string]v1alpha1.PodWarmPool
	getCalls     int
	postCalls    int
	putCalls     int
	putStatus    int
	deleteCalls  int
	listCalls    int
	wpGetCalls   int
	wpPostCalls  int
	wpPutCalls   int
	wpPutStatus  int
	return404CRD bool
}

func newMockCRDServer() (*mockCRDServer, *httptest.Server) {
	m := &mockCRDServer{
		podBoxes:     make(map[string]v1alpha1.PodBox),
		podWarmPools: make(map[string]v1alpha1.PodWarmPool),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		path := r.URL.Path

		if m.return404CRD {
			http.Error(w, `{"kind":"Status","status":"Failure","message":"the server could not find the requested resource","reason":"NotFound","code":404}`, http.StatusNotFound)
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
			PodName:        "test-box-1-kata-pod",
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
	if recAlpha.PodName != "box-alpha-kata-pod" {
		t.Errorf("expected box-alpha podName box-alpha-kata-pod, got %s", recAlpha.PodName)
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
			PodName: "sync-pool-warm-7-kata-pod",
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
