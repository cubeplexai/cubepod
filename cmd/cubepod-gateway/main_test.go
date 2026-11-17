package main

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

func TestResolveSecretsConfig(t *testing.T) {
	kc := controller.NewMemoryKubeClient()

	// 1. Unset backend + live hasKube + empty CUBEPOD_SECRETS_DIR -> defaults to k8s
	enabled, isK8s, cfg, err := resolveSecretsConfig("", "", "/var/lib/cubepod/e2b-volumes", "", "", true, kc, "cubepod-workloads")
	if err != nil || !enabled || !isK8s || cfg.Backend != "k8s" || cfg.Namespace != "cubepod-workloads" {
		t.Fatalf("unexpected auto k8s result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 2. Unset backend + live hasKube + explicit CUBEPOD_SECRETS_DIR -> uses file
	rawKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	enabled, isK8s, cfg, err = resolveSecretsConfig("", "/tmp/secrets", "", rawKey, "50", true, kc, "cubepod-workloads")
	if err != nil || !enabled || isK8s || cfg.Backend != "file" || cfg.Dir != "/tmp/secrets" || cfg.MaxSecretsPerTenant != 50 || len(cfg.EncryptionKey) != 32 {
		t.Fatalf("unexpected explicit dir file result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 3. Explicit CUBEPOD_SECRETS_BACKEND=k8s even without hasKube
	enabled, isK8s, cfg, err = resolveSecretsConfig("k8s", "/tmp/ignored", "", "", "", false, kc, "ns1")
	if err != nil || !enabled || !isK8s || cfg.Backend != "k8s" || cfg.Namespace != "ns1" {
		t.Fatalf("unexpected explicit k8s result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 4. Explicit CUBEPOD_SECRETS_BACKEND=disabled
	enabled, isK8s, _, err = resolveSecretsConfig("disabled", "/tmp/secrets", "/var/lib/cubepod/e2b-volumes", "", "", true, kc, "ns1")
	if err != nil || enabled || isK8s {
		t.Fatalf("expected disabled secrets, got enabled=%v isK8s=%v err=%v", enabled, isK8s, err)
	}

	// 5. Invalid CUBEPOD_SECRETS_BACKEND returns error
	if _, _, _, err := resolveSecretsConfig("postgres", "", "", "", "", false, kc, "ns1"); err == nil {
		t.Fatalf("expected error for unsupported backend")
	}
}

func TestBuildEgressMTLSServerEnforcesClientCerts(t *testing.T) {
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	otherCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("otherCA: %v", err)
	}

	// Requires non-nil EgressCA
	if _, err := buildEgressMTLSServer(":0", "", "", "default", nil, func(w http.ResponseWriter, r *http.Request) {}); err == nil {
		t.Fatalf("expected error when EgressCA is nil")
	}

	vault := egress.NewVault()
	ph, err := vault.BindSecret("sbx-mtls-1", "", "sk-mtls-secret", []string{"api.openai.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindSecret: %v", err)
	}

	srvSpec, err := buildEgressMTLSServer(":0", "", "", "default", ca, egress.VaultExchangeHandler(vault, ca))
	if err != nil {
		t.Fatalf("buildEgressMTLSServer: %v", err)
	}
	if srvSpec.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert || srvSpec.TLSConfig.ClientCAs == nil {
		t.Fatalf("expected RequireAndVerifyClientCert with ClientCAs")
	}

	ts := httptest.NewUnstartedServer(srvSpec.Handler)
	ts.TLS = srvSpec.TLSConfig
	ts.StartTLS()
	defer ts.Close()

	// Unauthenticated client (no client cert) fails TLS handshake
	noCertClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: ca.CertPool()},
		},
	}
	if _, err := noCertClient.Post(ts.URL+"/internal/v1/egress/exchange", "application/json", nil); err == nil {
		t.Fatalf("expected TLS error for client without cert")
	}

	// Untrusted CA client cert fails TLS handshake
	badCert, badKey, _ := otherCA.MintSandboxCert("sbx-mtls-1", time.Hour)
	badPair, _ := tls.X509KeyPair(badCert, badKey)
	badClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      ca.CertPool(),
				Certificates: []tls.Certificate{badPair},
			},
		},
	}
	if _, err := badClient.Post(ts.URL+"/internal/v1/egress/exchange", "application/json", nil); err == nil {
		t.Fatalf("expected TLS error for client cert signed by untrusted CA")
	}

	// Valid sandbox cert minted by EgressCA succeeds
	goodCert, goodKey, err := ca.MintSandboxCert("sbx-mtls-1", time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert: %v", err)
	}
	clientTLS, err := ca.ClientTLSConfig(goodCert, goodKey)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	goodClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	resp, err := goodClient.Post(ts.URL+"/internal/v1/egress/exchange", "application/json",
		http.NoBody)
	if err != nil {
		t.Fatalf("expected TLS connection to succeed with valid client cert: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest { // empty body -> 400 after passing mTLS
		t.Fatalf("expected 400 BadRequest on empty body after mTLS auth, got %d", resp.StatusCode)
	}
	_ = ph
}
