package secret_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/secret"
)

func TestKubeSecretBackendCRUDAndMetadata(t *testing.T) {
	ctx := context.Background()
	kube := controller.NewMemoryKubeClient()

	backend, err := secret.NewKubeSecretBackend(kube, "cubepod-test")
	if err != nil {
		t.Fatalf("NewKubeSecretBackend: %v", err)
	}
	st, err := secret.OpenWithBackend(backend, secret.Config{MaxSecretsPerTenant: 5})
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	info, err := st.Create("api-key-a", "openai_key", "sk-kube-v1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if st.Count() != 1 {
		t.Fatalf("Count = %d; want 1", st.Count())
	}

	// Verify Kubernetes Secret object shape
	kubeName := secret.KubeSecretName(info.ID)
	if !strings.HasPrefix(kubeName, "cubepod-e2b-sec-") {
		t.Fatalf("unexpected KubeSecretName: %q", kubeName)
	}
	ksec, err := kube.GetSecret(ctx, "cubepod-test", kubeName)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if ksec.Metadata.Labels[secret.SecretResourceLabel] != secret.SecretResourceValue {
		t.Fatalf("missing resource label: %+v", ksec.Metadata.Labels)
	}
	tenLabel := ksec.Metadata.Labels[secret.SecretTenantLabel]
	if tenLabel == "" || !strings.HasPrefix(secret.TenantHash("api-key-a"), tenLabel) {
		t.Fatalf("unexpected tenant label %q for hash %q", tenLabel, secret.TenantHash("api-key-a"))
	}
	if string(ksec.Data["value"]) != "sk-kube-v1" {
		t.Fatalf("unexpected Data[value]: %q", string(ksec.Data["value"]))
	}
	var meta map[string]any
	if err := json.Unmarshal(ksec.Data["meta"], &meta); err != nil {
		t.Fatalf("unmarshal Data[meta]: %v", err)
	}
	for _, field := range []string{"tenantHash", "id", "name", "version", "createdAt"} {
		if _, ok := meta[field]; !ok {
			t.Fatalf("Data[meta] missing field %q: %+v", field, meta)
		}
	}

	// Update / Rotate
	upd, err := st.Update("api-key-a", info.ID, "sk-kube-v2")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Version != 2 {
		t.Fatalf("expected version 2, got %d", upd.Version)
	}
	ksec2, err := kube.GetSecret(ctx, "cubepod-test", kubeName)
	if err != nil || string(ksec2.Data["value"]) != "sk-kube-v2" {
		t.Fatalf("rotated K8s secret value = %q err=%v", string(ksec2.Data["value"]), err)
	}

	// Operations snapshot tracking
	_, _, err = st.ResolveByName("api-key-a", "openai_key")
	if err != nil {
		t.Fatalf("ResolveByName: %v", err)
	}
	_, _, _ = st.ResolveByName("api-key-a", "missing")
	_, _ = st.Get("api-key-a", info.ID)
	_, _, _ = st.List("api-key-a", 10, "")

	snap := st.OperationsSnapshot()
	if snap["create"]["ok"] != 1 || snap["update"]["ok"] != 1 || snap["get"]["ok"] != 1 || snap["list"]["ok"] != 1 {
		t.Fatalf("unexpected operations snapshot: %+v", snap)
	}
	if snap["resolve"]["ok"] != 1 || snap["resolve"]["error"] != 1 {
		t.Fatalf("unexpected resolve metrics: %+v", snap["resolve"])
	}

	// Delete
	if _, err := st.Delete("api-key-a", info.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if st.Count() != 0 {
		t.Fatalf("Count after delete = %d; want 0", st.Count())
	}
	if _, err := kube.GetSecret(ctx, "cubepod-test", kubeName); err == nil {
		t.Fatalf("expected K8s secret to be deleted")
	}
}

func TestKubeSecretBackendCrossReplicaSyncAndVaultCallbacks(t *testing.T) {
	ctx := context.Background()
	sharedKube := controller.NewMemoryKubeClient()

	b1, _ := secret.NewKubeSecretBackend(sharedKube, "cubepod-workloads")
	b2, _ := secret.NewKubeSecretBackend(sharedKube, "cubepod-workloads")

	vault2 := egress.NewVault()
	st1, err := secret.OpenWithBackend(b1, secret.Config{})
	if err != nil {
		t.Fatalf("st1 OpenWithBackend: %v", err)
	}
	st2, err := secret.OpenWithBackend(b2, secret.Config{
		OnUpdate: func(tenantHash, secretID string, val secret.SecretValue) {
			vault2.UpdateSecretByRef(tenantHash, secretID, val.Raw())
		},
		OnRevoke: func(tenantHash, secretID string) {
			vault2.RevokeSecretByRef(tenantHash, secretID)
		},
	})
	if err != nil {
		t.Fatalf("st2 OpenWithBackend: %v", err)
	}

	// Replica 1 creates secret
	info, err := st1.Create("tenant-key", "stripe_key", "sk_v1")
	if err != nil {
		t.Fatalf("st1.Create: %v", err)
	}

	// Replica 2 does not know it yet until Sync
	if _, _, err := st2.ResolveByName("tenant-key", "stripe_key"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before sync, got %v", err)
	}
	if err := st2.Sync(ctx); err != nil {
		t.Fatalf("st2.Sync (create): %v", err)
	}
	_, val, err := st2.ResolveByTenantID(secret.TenantHash("tenant-key"), info.ID)
	if err != nil || val.Raw() != "sk_v1" {
		t.Fatalf("st2 after sync: val=%q err=%v", val.Raw(), err)
	}

	// Bind placeholder in replica 2's vault
	ph, err := vault2.BindManagedSecret("sbx-1", secret.TenantHash("tenant-key"), info.ID, "", val.Raw(), []string{"api.stripe.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("vault2.BindManagedSecret: %v", err)
	}

	// Replica 1 rotates secret -> Replica 2 Sync updates vault2
	if _, err := st1.Update("tenant-key", info.ID, "sk_v2"); err != nil {
		t.Fatalf("st1.Update: %v", err)
	}
	if err := st2.Sync(ctx); err != nil {
		t.Fatalf("st2.Sync (rotate): %v", err)
	}
	exchanged, err := vault2.Exchange("sbx-1", ph, "api.stripe.com", "Authorization")
	if err != nil || exchanged != "sk_v2" {
		t.Fatalf("vault2 after rotation sync: val=%q err=%v", exchanged, err)
	}

	// Replica 1 deletes secret -> Replica 2 Sync revokes vault2
	if _, err := st1.Delete("tenant-key", info.ID); err != nil {
		t.Fatalf("st1.Delete: %v", err)
	}
	if err := st2.Sync(ctx); err != nil {
		t.Fatalf("st2.Sync (delete): %v", err)
	}
	if _, err := vault2.Exchange("sbx-1", ph, "api.stripe.com", "Authorization"); !errors.Is(err, egress.ErrUnknownPlaceholder) {
		t.Fatalf("expected ErrUnknownPlaceholder after delete sync, got %v", err)
	}
	if st2.Count() != 0 {
		t.Fatalf("expected st2.Count=0 after delete sync, got %d", st2.Count())
	}
}

func TestKubeSecretBackendWithRESTKubeClient(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	secrets := make(map[string]controller.KubeSecret)
	var lastListSelector string

	kubeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const prefix = "/api/v1/namespaces/cubepod-workloads/secrets"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == prefix:
			var sec controller.KubeSecret
			if err := json.NewDecoder(r.Body).Decode(&sec); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, exists := secrets[sec.Metadata.Name]; exists {
				w.WriteHeader(http.StatusConflict)
				return
			}
			sec.Metadata.ResourceVersion = "1"
			secrets[sec.Metadata.Name] = sec
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sec)

		case r.Method == http.MethodGet && r.URL.Path == prefix:
			lastListSelector = r.URL.Query().Get("labelSelector")
			var items []controller.KubeSecret
			for _, sec := range secrets {
				if lastListSelector == "cubepod.io/resource=e2b-secret" &&
					sec.Metadata.Labels["cubepod.io/resource"] != "e2b-secret" {
					continue
				}
				items = append(items, sec)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/"):
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			sec, ok := secrets[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(sec)

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, prefix+"/"):
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			var sec controller.KubeSecret
			if err := json.NewDecoder(r.Body).Decode(&sec); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sec.Metadata.Name = name
			sec.Metadata.ResourceVersion = "2"
			secrets[name] = sec
			_ = json.NewEncoder(w).Encode(sec)

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/"):
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			delete(secrets, name)
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer kubeSrv.Close()

	restKC := controller.NewRESTKubeClient(kubeSrv.URL, "cubepod-workloads", "tok", kubeSrv.Client())
	b, err := secret.NewKubeSecretBackend(restKC, "cubepod-workloads")
	if err != nil {
		t.Fatalf("NewKubeSecretBackend: %v", err)
	}
	st, err := secret.OpenWithBackend(b, secret.Config{})
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	info, err := st.Create("k-rest", "rest_secret", "val-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := st.Update("k-rest", info.ID, "val-2"); err != nil {
		t.Fatalf("Update (EnsureSecret PUT on 409): %v", err)
	}

	// Re-open fresh Store from REST API to exercise ListSecrets
	b2, _ := secret.NewKubeSecretBackend(restKC, "cubepod-workloads")
	st2, err := secret.OpenWithBackend(b2, secret.Config{})
	if err != nil {
		t.Fatalf("st2 OpenWithBackend: %v", err)
	}
	if lastListSelector != "cubepod.io/resource=e2b-secret" {
		t.Fatalf("expected labelSelector=cubepod.io/resource=e2b-secret, got %q", lastListSelector)
	}
	_, val, err := st2.ResolveByName("k-rest", "rest_secret")
	if err != nil || val.Raw() != "val-2" {
		t.Fatalf("st2.ResolveByName = %q err=%v", val.Raw(), err)
	}
	if _, err := st2.Delete("k-rest", info.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatalf("st.Sync: %v", err)
	}
	if st.Count() != 0 {
		t.Fatalf("expected Count=0 after sync delete, got %d", st.Count())
	}
}

func TestKubeSecretBackendConcurrentSyncRace(t *testing.T) {
	ctx := context.Background()
	kube := controller.NewMemoryKubeClient()
	b, _ := secret.NewKubeSecretBackend(kube, "default")
	st, err := secret.OpenWithBackend(b, secret.Config{})
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := "tenant"
			name := "s_" + string(rune('a'+idx))
			info, err := st.Create(key, name, "v1")
			if err != nil {
				t.Errorf("Create %d: %v", idx, err)
				return
			}
			_, _ = st.Update(key, info.ID, "v2")
			_ = st.Sync(ctx)
			_, _, _ = st.ResolveByID(key, info.ID)
			_ = st.OperationsSnapshot()
			_ = st.Count()
		}(i)
	}
	wg.Wait()
	if st.Count() != 6 {
		t.Fatalf("expected 6 secrets, got %d", st.Count())
	}
}
