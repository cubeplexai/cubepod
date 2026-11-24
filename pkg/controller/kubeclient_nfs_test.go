package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

func TestEnsurePodWithHook_BindsIPBeforeReady(t *testing.T) {
	var mu sync.Mutex
	var hookIPs []string
	var hookSawReady bool
	polls := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "nfs-pod"}})
			return
		}
		polls++
		ready := "False"
		phase := "Pending"
		ip := ""
		if polls >= 2 {
			ip = "10.244.9.9"
			phase = "Running"
		}
		if polls >= 4 {
			ready = "True"
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{
				"phase": phase,
				"podIP": ip,
				"conditions": []map[string]any{
					{"type": "Ready", "status": ready},
				},
			},
		})
	}))
	defer ts.Close()

	client := controller.NewRESTKubeClient(
		ts.URL, "cubepod-workloads", "t", ts.Client(),
		controller.WithWaitForRunning(true),
		controller.WithPollInterval(15*time.Millisecond),
		controller.WithPollTimeout(2*time.Second),
	)
	hook := func(_ context.Context, _, _, ip string) error {
		mu.Lock()
		defer mu.Unlock()
		hookIPs = append(hookIPs, ip)
		// Ready must still be false at bind time: inspect last encoded status via polls.
		if polls >= 4 {
			hookSawReady = true
		}
		return nil
	}
	ip, err := client.EnsurePodWithHook(context.Background(), controller.PodManifest{Name: "nfs-pod"}, hook)
	if err != nil {
		t.Fatalf("EnsurePodWithHook: %v", err)
	}
	if ip != "10.244.9.9" {
		t.Fatalf("ip=%s", ip)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hookIPs) != 1 || hookIPs[0] != "10.244.9.9" {
		t.Fatalf("hook IPs=%v", hookIPs)
	}
	if hookSawReady {
		t.Fatal("hook must run before Ready")
	}
}

func TestEnsurePodWithHook_TimeoutWithoutReadyIsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{
				"phase": "Running",
				"podIP": "10.244.1.1",
				"conditions": []map[string]any{
					{"type": "Ready", "status": "False"},
				},
			},
		})
	}))
	defer ts.Close()
	client := controller.NewRESTKubeClient(
		ts.URL, "ns", "", ts.Client(),
		controller.WithWaitForRunning(true),
		controller.WithPollInterval(10*time.Millisecond),
		controller.WithPollTimeout(80*time.Millisecond),
	)
	hooked := false
	_, err := client.EnsurePodWithHook(context.Background(), controller.PodManifest{Name: "p"}, func(context.Context, string, string, string) error {
		hooked = true
		return nil
	})
	if err == nil {
		t.Fatal("timeout without Ready must be an error")
	}
	if !hooked {
		t.Fatal("hook should have run when IP was observed")
	}
}
