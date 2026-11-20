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

func TestPanelSecretValidationAllEntryPoints(t *testing.T) {
	for _, secret := range []string{"", "   ", " replace-with-random-hmac-secret ", " cubepod-default-hmac-panel-secret-key-32b "} {
		for _, source := range []string{"env", "setter"} {
			t.Run(source+"/"+secret, func(t *testing.T) {
				t.Setenv("CUBEPOD_PANEL_SECRET", "")
				srv := NewServer(controller.NewReconciler())
				if source == "env" {
					t.Setenv("CUBEPOD_PANEL_SECRET", secret)
				} else {
					srv.SetPanelSecret(secret)
				}
				if err := srv.RequirePanelSecret(); err == nil {
					t.Fatal("invalid secret accepted at startup")
				}
				tok, exp, err := srv.mintPanelToken("sandbox", 8080, 60)
				if err == nil || tok != "" || !exp.IsZero() {
					t.Fatal("invalid secret minted a token")
				}
				if strings.TrimSpace(secret) == "" && !errors.Is(err, ErrPanelSecretMissing) {
					t.Fatalf("missing error: %v", err)
				}
				// In-process Handler users need not call RequirePanelSecret. Desktop
				// must fail before running the sandbox command or returning any URL.
				req := httptest.NewRequest(http.MethodPost, "/sandboxes/any/desktop/start", strings.NewReader(`{}`))
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "/panel/") {
					t.Fatalf("desktop failed open: %d %s", w.Code, w.Body)
				}
			})
		}
	}
}

func TestPanelSecretWhitespaceAndCrossReplicaVerification(t *testing.T) {
	t.Setenv("CUBEPOD_PANEL_SECRET", "  operator-secret  ")
	envServer := NewServer(controller.NewReconciler())
	envServer.SetPanelSecret(" \t\n") // Whitespace setter must fall through to a valid env secret.
	setterServer := NewServer(controller.NewReconciler())
	setterServer.SetPanelSecret(" \toperator-secret\n")
	if string(envServer.getPanelSecret()) != "operator-secret" || string(setterServer.getPanelSecret()) != "operator-secret" {
		t.Fatal("sources trim differently")
	}
	for _, srv := range []*Server{envServer, setterServer} {
		if err := srv.RequirePanelSecret(); err != nil {
			t.Fatal(err)
		}
	}
	token, _, err := envServer.mintPanelToken("sandbox", 8080, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setterServer.parseAndVerifyPanelToken(token); err != nil {
		t.Fatal(err)
	}
	// No arbitrary minimum-length compatibility change in this patch.
	setterServer.SetPanelSecret("x")
	if err := setterServer.RequirePanelSecret(); err != nil {
		t.Fatal(err)
	}
}
