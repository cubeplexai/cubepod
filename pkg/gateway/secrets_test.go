package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func newSecretsTestServer(t *testing.T) (*gateway.Server, *controller.MemoryKubeClient) {
	t.Helper()
	kube := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(
		controller.WithKubeClient(kube),
		controller.WithNamespace("default"),
	)

	srv := gateway.NewServer(rec)
	srv.SetWorkspaceBaseDir(t.TempDir())
	if err := srv.ConfigureE2BSecrets(gateway.E2BSecretConfig{
		Dir:                 t.TempDir(),
		MaxSecretsPerTenant: 10,
	}); err != nil {
		t.Fatalf("ConfigureE2BSecrets: %v", err)
	}
	return srv, kube
}

func doJSONReq(t *testing.T, h http.Handler, method, path, apiKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestE2BSecretsCRUDAndPagination(t *testing.T) {
	srv, _ := newSecretsTestServer(t)
	h := srv.Handler()

	// Unauthorized without API key
	if rr := doJSONReq(t, h, http.MethodGet, "/secrets", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /secrets without key = %d; want 401", rr.Code)
	}

	// Create 3 secrets
	var createdID string
	for i, name := range []string{"alpha_key", "beta_key", "gamma_key"} {
		rr := doJSONReq(t, h, http.MethodPost, "/secrets", "tenant-1", map[string]string{
			"name":  name,
			"value": "super-secret-" + name,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("POST /secrets %d = %d (%s); want 201", i, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "super-secret-") {
			t.Fatalf("POST /secrets response leaked plaintext value!")
		}
		var info map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
			t.Fatalf("unmarshal create: %v", err)
		}
		if i == 0 {
			createdID, _ = info["id"].(string)
		}
	}

	// Duplicate name returns 409 Conflict
	if rr := doJSONReq(t, h, http.MethodPost, "/secrets", "tenant-1", map[string]string{
		"name":  "alpha_key",
		"value": "other",
	}); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate POST /secrets = %d; want 409", rr.Code)
	}

	// Tenant isolation: tenant-2 cannot see tenant-1's secret
	if rr := doJSONReq(t, h, http.MethodGet, "/secrets/"+createdID, "tenant-2", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET /secrets/{id} = %d; want 404", rr.Code)
	}

	// List pagination
	rrPage1 := doJSONReq(t, h, http.MethodGet, "/secrets?limit=2", "tenant-1", nil)
	if rrPage1.Code != http.StatusOK {
		t.Fatalf("GET /secrets page 1 = %d", rrPage1.Code)
	}
	if strings.Contains(rrPage1.Body.String(), "super-secret-") {
		t.Fatalf("GET /secrets leaked plaintext value!")
	}
	nextTok := rrPage1.Header().Get("X-Next-Token")
	if nextTok == "" {
		t.Fatalf("expected X-Next-Token header")
	}
	var page1 []map[string]any
	_ = json.Unmarshal(rrPage1.Body.Bytes(), &page1)
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d; want 2", len(page1))
	}

	rrPage2 := doJSONReq(t, h, http.MethodGet, "/secrets?limit=2&nextToken="+nextTok, "tenant-1", nil)
	if rrPage2.Code != http.StatusOK || rrPage2.Header().Get("X-Next-Token") != "" {
		t.Fatalf("page2 unexpected: code=%d next=%q", rrPage2.Code, rrPage2.Header().Get("X-Next-Token"))
	}
	var page2 []map[string]any
	_ = json.Unmarshal(rrPage2.Body.Bytes(), &page2)
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d; want 1", len(page2))
	}

	// Rotate secret
	rrUpd := doJSONReq(t, h, http.MethodPost, "/secrets/"+createdID, "tenant-1", map[string]string{
		"value": "rotated-secret-value",
	})
	if rrUpd.Code != http.StatusOK {
		t.Fatalf("POST /secrets/{id} = %d (%s)", rrUpd.Code, rrUpd.Body.String())
	}
	var updInfo map[string]any
	_ = json.Unmarshal(rrUpd.Body.Bytes(), &updInfo)
	if updInfo["version"] != float64(2) {
		t.Fatalf("expected version=2, got %v", updInfo["version"])
	}

	// Delete secret
	if rrDel := doJSONReq(t, h, http.MethodDelete, "/secrets/"+createdID, "tenant-1", nil); rrDel.Code != http.StatusNoContent {
		t.Fatalf("DELETE /secrets/{id} = %d; want 204", rrDel.Code)
	}
	if rrGet := doJSONReq(t, h, http.MethodGet, "/secrets/"+createdID, "tenant-1", nil); rrGet.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d; want 404", rrGet.Code)
	}
}

func TestSandboxCreateWithE2BNetworkSecretsAndRotation(t *testing.T) {
	srv, kube := newSecretsTestServer(t)
	h := srv.Handler()

	// Create secret
	rrSec := doJSONReq(t, h, http.MethodPost, "/secrets", "tenant-1", map[string]string{
		"name":  "stripe_api_key",
		"value": "sk_live_secret_v1",
	})
	if rrSec.Code != http.StatusCreated {
		t.Fatalf("create secret = %d", rrSec.Code)
	}
	var secInfo struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rrSec.Body.Bytes(), &secInfo)

	// Forbidden header name rejected
	rrBadHdr := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-1", map[string]any{
		"templateID": "base",
		"network": map[string]any{
			"rules": map[string]any{
				"api.stripe.com": []map[string]any{
					{"transform": map[string]any{"headers": map[string]string{"Host": "evil.com"}}},
				},
			},
		},
	})
	if rrBadHdr.Code != http.StatusBadRequest {
		t.Fatalf("forbidden header status = %d; want 400", rrBadHdr.Code)
	}

	// Unknown secret rejected with 404 before creating PodBox
	rrMissing := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-1", map[string]any{
		"templateID": "base",
		"network": map[string]any{
			"rules": map[string]any{
				"api.stripe.com": []map[string]any{
					{"transform": map[string]any{"headers": map[string]string{"Authorization": "Bearer ${e2b.secrets.does_not_exist}"}}},
				},
			},
		},
	})
	if rrMissing.Code != http.StatusNotFound {
		t.Fatalf("missing secret status = %d; want 404 (%s)", rrMissing.Code, rrMissing.Body.String())
	}

	// Create sandbox with E2B network rules referencing ${e2b.secrets.stripe_api_key}
	rrCreate := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-1", map[string]any{
		"templateID": "base",
		"network": map[string]any{
			"allowOut": []string{"api.stripe.com"},
			"denyOut":  []string{"0.0.0.0/0"},
			"rules": map[string]any{
				"api.stripe.com": []map[string]any{
					{"transform": map[string]any{"headers": map[string]string{"Authorization": "Bearer ${e2b.secrets.stripe_api_key}"}}},
				},
			},
		},
	})
	if rrCreate.Code != http.StatusCreated {
		t.Fatalf("POST /sandboxes = %d (%s); want 201", rrCreate.Code, rrCreate.Body.String())
	}
	var sbx struct {
		SandboxID string `json:"sandboxID"`
	}
	_ = json.Unmarshal(rrCreate.Body.Bytes(), &sbx)

	// Verify PodBox contains ONLY cpodref_, never raw secret, and skipped WarmPool claim
	box, err := kube.GetPodBox(context.Background(), "default", sbx.SandboxID)
	if err != nil {
		t.Fatalf("GetPodBox: %v", err)
	}
	if box.Spec.WarmPoolRef != "" {
		t.Fatalf("expected WarmPoolRef to be skipped when network policy is set, got %q", box.Spec.WarmPoolRef)
	}
	if box.Spec.NetworkPolicy == nil || box.Spec.NetworkPolicy.DefaultAction != v1alpha1.NetworkDefaultDeny {
		t.Fatalf("expected DefaultAction=Deny, got %+v", box.Spec.NetworkPolicy)
	}
	if len(box.Spec.NetworkPolicy.HeaderTransforms) != 1 {
		t.Fatalf("expected 1 HeaderTransform, got %+v", box.Spec.NetworkPolicy.HeaderTransforms)
	}
	authHdr := box.Spec.NetworkPolicy.HeaderTransforms[0].Headers["Authorization"]
	if !strings.HasPrefix(authHdr, "Bearer cpodref_") || strings.Contains(authHdr, "sk_live_secret_v1") {
		t.Fatalf("unexpected HeaderTransform Authorization = %q", authHdr)
	}
	placeholder := strings.TrimPrefix(authHdr, "Bearer ")

	// Exchange over /internal/v1/egress/exchange returns v1
	rrEx1 := doJSONReq(t, h, http.MethodPost, "/internal/v1/egress/exchange", "", map[string]string{
		"sandboxID":   sbx.SandboxID,
		"placeholder": placeholder,
		"sniHost":     "api.stripe.com",
		"headerName":  "Authorization",
	})
	if rrEx1.Code != http.StatusOK || !strings.Contains(rrEx1.Body.String(), "sk_live_secret_v1") {
		t.Fatalf("exchange v1 failed: code=%d body=%s", rrEx1.Code, rrEx1.Body.String())
	}

	// Rotate secret -> exchange returns v2
	if rrUpd := doJSONReq(t, h, http.MethodPost, "/secrets/"+secInfo.ID, "tenant-1", map[string]string{
		"value": "sk_live_secret_v2",
	}); rrUpd.Code != http.StatusOK {
		t.Fatalf("rotate secret = %d", rrUpd.Code)
	}
	rrEx2 := doJSONReq(t, h, http.MethodPost, "/internal/v1/egress/exchange", "", map[string]string{
		"sandboxID":   sbx.SandboxID,
		"placeholder": placeholder,
		"sniHost":     "api.stripe.com",
		"headerName":  "Authorization",
	})
	if rrEx2.Code != http.StatusOK || !strings.Contains(rrEx2.Body.String(), "sk_live_secret_v2") {
		t.Fatalf("exchange v2 failed: code=%d body=%s", rrEx2.Code, rrEx2.Body.String())
	}

	// Delete secret -> exchange fails closed with 403
	if rrDel := doJSONReq(t, h, http.MethodDelete, "/secrets/"+secInfo.ID, "tenant-1", nil); rrDel.Code != http.StatusNoContent {
		t.Fatalf("delete secret = %d", rrDel.Code)
	}
	rrEx3 := doJSONReq(t, h, http.MethodPost, "/internal/v1/egress/exchange", "", map[string]string{
		"sandboxID":   sbx.SandboxID,
		"placeholder": placeholder,
		"sniHost":     "api.stripe.com",
		"headerName":  "Authorization",
	})
	if rrEx3.Code != http.StatusForbidden {
		t.Fatalf("exchange after delete = %d; want 403", rrEx3.Code)
	}
}

func TestE2BSecretsQuotaControlCharsAndVolumeFailureRollback(t *testing.T) {
	srv, _ := newSecretsTestServer(t)
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{Dir: t.TempDir()}); err != nil {
		t.Fatalf("ConfigureE2BVolumes: %v", err)
	}
	h := srv.Handler()

	// 1. Create 10 secrets (MaxSecretsPerTenant=10) -> 11th returns 429
	for i := 0; i < 10; i++ {
		rr := doJSONReq(t, h, http.MethodPost, "/secrets", "tenant-quota", map[string]string{
			"name":  "k_" + string(rune('a'+i)),
			"value": "val",
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %d = %d", i, rr.Code)
		}
	}
	rrOver := doJSONReq(t, h, http.MethodPost, "/secrets", "tenant-quota", map[string]string{
		"name":  "k_overflow",
		"value": "val",
	})
	if rrOver.Code != http.StatusTooManyRequests {
		t.Fatalf("11th secret status = %d; want 429", rrOver.Code)
	}

	// 2. Control characters in header template rejected with 400
	rrCtl := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-quota", map[string]any{
		"templateID": "base",
		"network": map[string]any{
			"rules": map[string]any{
				"api.example.com": []map[string]any{
					{"transform": map[string]any{"headers": map[string]string{"X-Test": "bad\x07val"}}},
				},
			},
		},
	})
	if rrCtl.Code != http.StatusBadRequest {
		t.Fatalf("CTL header template status = %d; want 400", rrCtl.Code)
	}

	// 3. Volume mount failure must revoke vault bindings bound in resolveSandboxSecretsAndNetwork
	rrVolFail := doJSONReq(t, h, http.MethodPost, "/sandboxes", "tenant-quota", map[string]any{
		"templateID": "base",
		"volumeMounts": []map[string]string{
			{"name": "missing-vol", "path": "/data"},
		},
		"network": map[string]any{
			"rules": map[string]any{
				"api.example.com": []map[string]any{
					{"transform": map[string]any{"headers": map[string]string{"Authorization": "Bearer ${e2b.secrets.k_a}"}}},
				},
			},
		},
	})
	if rrVolFail.Code != http.StatusNotFound {
		t.Fatalf("missing volume status = %d; want 404 (%s)", rrVolFail.Code, rrVolFail.Body.String())
	}
	if got := srv.Vault().Count(); got != 0 {
		t.Fatalf("expected 0 vault bindings after rollback, got %d", got)
	}
}
