package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

const leakedDefaultPanelSecret = "cubepod-default-hmac-panel-secret-key-32b"

func TestRequirePanelSecretMissing(t *testing.T) {
	t.Setenv("CUBEPOD_PANEL_SECRET", "")
	srv := NewServer(controller.NewReconciler())
	if err := srv.RequirePanelSecret(); !errors.Is(err, ErrPanelSecretMissing) {
		t.Fatalf("expected ErrPanelSecretMissing, got %v", err)
	}
	got := string(srv.getPanelSecret())
	if got == leakedDefaultPanelSecret {
		t.Fatal("getPanelSecret still returns the compiled-in default")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty panel secret when unset, got %d bytes", len(got))
	}
}

func TestRequirePanelSecretFromEnv(t *testing.T) {
	const secret = "env-panel-hmac-secret-for-tests-32b"
	t.Setenv("CUBEPOD_PANEL_SECRET", secret)
	srv := NewServer(controller.NewReconciler())
	if err := srv.RequirePanelSecret(); err != nil {
		t.Fatalf("RequirePanelSecret from env: %v", err)
	}
	if got := string(srv.getPanelSecret()); got != secret {
		t.Fatalf("pinned secret mismatch")
	}
}

func TestRequirePanelSecretFromSetter(t *testing.T) {
	t.Setenv("CUBEPOD_PANEL_SECRET", "")
	const secret = "setter-panel-hmac-secret-for-tests-32b"
	srv := NewServer(controller.NewReconciler())
	srv.SetPanelSecret(secret)
	if err := srv.RequirePanelSecret(); err != nil {
		t.Fatalf("RequirePanelSecret from setter: %v", err)
	}
	if got := string(srv.getPanelSecret()); got != secret {
		t.Fatalf("pinned secret mismatch")
	}
}

func TestMintEndpointRequiresPanelSecret(t *testing.T) {
	t.Setenv("CUBEPOD_PANEL_SECRET", "")
	srv := NewServer(controller.NewReconciler())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	createResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("create sandbox failed: %v", err)
	}
	var rec SandboxRecord
	_ = json.NewDecoder(createResp.Body).Decode(&rec)
	createResp.Body.Close()
	if rec.SandboxID == "" {
		t.Fatalf("expected sandboxID, status=%d", createResp.StatusCode)
	}

	mintResp, err := http.Post(ts.URL+"/sandboxes/"+rec.SandboxID+"/endpoints/8080", "application/json", bytes.NewReader([]byte(`{"expiresInSeconds":3600}`)))
	if err != nil {
		t.Fatalf("mint request failed: %v", err)
	}
	defer mintResp.Body.Close()
	if mintResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 when panel secret is unset, got %d", mintResp.StatusCode)
	}
}
