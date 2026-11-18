package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestResolveEventsConfig(t *testing.T) {
	kc := controller.NewMemoryKubeClient()

	// 1. Unset backend + live hasKube + empty CUBEPOD_EVENTS_DIR -> defaults to k8s (HA)
	enabled, isK8s, cfg, err := resolveEventsConfig("", "", "/var/lib/cubepod/state", "", "", "", true, kc, "cubepod-workloads")
	if err != nil || !enabled || !isK8s || cfg.Backend != "k8s" || cfg.Namespace != "cubepod-workloads" || cfg.KubeClient == nil {
		t.Fatalf("unexpected auto k8s result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 2. Local (no kube) mode falls back to file under <state-dir>/events
	rawKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	enabled, isK8s, cfg, err = resolveEventsConfig("", "", "/var/lib/cubepod/state", "", rawKey, "1", false, nil, "")
	if err != nil || !enabled || isK8s || cfg.Backend != "file" || cfg.Dir != "/var/lib/cubepod/state/events" || len(cfg.EncryptionKey) != 32 || !cfg.AllowPrivateIPs {
		t.Fatalf("unexpected local file result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 3. Explicit CUBEPOD_EVENTS_DIR keeps file backend even with hasKube
	enabled, isK8s, cfg, err = resolveEventsConfig("", "/tmp/events", "", "", "", "", true, kc, "ns1")
	if err != nil || !enabled || isK8s || cfg.Backend != "file" || cfg.Dir != "/tmp/events" {
		t.Fatalf("unexpected explicit dir result: enabled=%v isK8s=%v cfg=%+v err=%v", enabled, isK8s, cfg, err)
	}

	// 4. Volume-dir sibling fallback when no state dir is configured
	enabled, _, cfg, err = resolveEventsConfig("file", "", "", "/var/lib/cubepod/e2b-volumes", "", "", false, nil, "")
	if err != nil || !enabled || cfg.Dir != "/var/lib/cubepod/events" {
		t.Fatalf("unexpected volume sibling result: enabled=%v cfg=%+v err=%v", enabled, cfg, err)
	}

	// 5. Explicit k8s without KubeClient errors; disabled wins over everything
	if _, _, _, err := resolveEventsConfig("k8s", "", "", "", "", "", false, nil, ""); err == nil {
		t.Fatal("expected error for k8s backend without KubeClient")
	}
	for _, tc := range [][2]string{{"disabled", ""}, {"", "disabled"}} {
		enabled, isK8s, _, err = resolveEventsConfig(tc[0], tc[1], "/state", "", "", "", true, kc, "ns1")
		if err != nil || enabled || isK8s {
			t.Fatalf("expected disabled events for %v, got enabled=%v isK8s=%v err=%v", tc, enabled, isK8s, err)
		}
	}

	// 6. No directory known in file mode -> disabled; invalid backend / key -> error
	if enabled, _, _, err := resolveEventsConfig("", "", "", "", "", "", false, nil, ""); err != nil || enabled {
		t.Fatalf("expected events disabled without any dir, got enabled=%v err=%v", enabled, err)
	}
	if _, _, _, err := resolveEventsConfig("postgres", "", "", "", "", "", false, nil, ""); err == nil {
		t.Fatal("expected error for unsupported backend")
	}
	if _, _, _, err := resolveEventsConfig("file", "/tmp/events", "", "", "short", "", false, nil, ""); err == nil {
		t.Fatal("expected error for invalid CUBEPOD_EVENTS_KEY")
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

func TestResolveE2BTemplateAPIFlag(t *testing.T) {
	if !resolveE2BTemplateAPIFlag(false, true, nil) {
		t.Fatalf("expected default true")
	}
	if resolveE2BTemplateAPIFlag(true, false, nil) {
		t.Fatalf("expected explicit --e2b-template-api=false to disable")
	}
	if resolveE2BTemplateAPIFlag(false, true, map[string]bool{"E2BTemplateAPI": false}) {
		t.Fatalf("expected --feature-gate=E2BTemplateAPI=false to disable")
	}
	if resolveE2BTemplateAPIFlag(true, true, map[string]bool{"E2BTemplateAPI": false}) {
		t.Fatalf("expected explicit flag + gate=false to disable")
	}
}

func TestResolveE2BVolumeConfig(t *testing.T) {
	hexKEK := hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32))
	envMap := map[string]string{
		"CUBEPOD_E2B_VOLUME_DIR":             "/tmp/e2b-vols",
		"CUBEPOD_E2B_VOLUMES_KEK":            hexKEK,
		"CUBEPOD_E2B_VOLUMES_KEK_ID":         "kek-prod-1",
		"CUBEPOD_E2B_MAX_TENANT_BYTES":       "10485760",
		"CUBEPOD_E2B_MAX_VOLUMES_PER_TENANT": "12",
	}
	enabled, vcfg, err := resolveE2BVolumeConfig(func(k string) string { return envMap[k] }, "")
	if err != nil || !enabled {
		t.Fatalf("resolveE2BVolumeConfig local KEK: enabled=%v err=%v", enabled, err)
	}
	if vcfg.KMS == nil || vcfg.KMS.KeyID() != "kek-prod-1" || vcfg.MaxBytesPerTenant != 10485760 || vcfg.MaxVolumesPerTenant != 12 {
		t.Fatalf("unexpected vcfg: %+v", vcfg)
	}

	// External HTTP KMS URL takes precedence and validates HTTPS for remote hosts.
	envHTTP := map[string]string{
		"CUBEPOD_E2B_VOLUME_DIR":         "/tmp/e2b-vols",
		"CUBEPOD_E2B_VOLUMES_KMS_URL":    "https://kms.internal.example:8443/v1",
		"CUBEPOD_E2B_VOLUMES_KMS_TOKEN":  "secret-kms-bearer",
		"CUBEPOD_E2B_VOLUMES_KMS_KEY_ID": "vault-vol-kek",
	}
	enabled, vcfg, err = resolveE2BVolumeConfig(func(k string) string { return envHTTP[k] }, "")
	if err != nil || !enabled || vcfg.KMS == nil || vcfg.KMS.KeyID() != "vault-vol-kek" {
		t.Fatalf("resolveE2BVolumeConfig HTTP KMS: enabled=%v vcfg=%+v err=%v", enabled, vcfg, err)
	}

	// Malformed KEK must return an error without echoing the raw secret string.
	badEnv := map[string]string{
		"CUBEPOD_E2B_VOLUME_DIR":  "/tmp/e2b-vols",
		"CUBEPOD_E2B_VOLUMES_KEK": "super-sensitive-invalid-kek",
	}
	if _, _, err := resolveE2BVolumeConfig(func(k string) string { return badEnv[k] }, ""); err == nil || strings.Contains(err.Error(), "super-sensitive-invalid-kek") {
		t.Fatalf("expected redacted error on malformed KEK, got %v", err)
	}

	// Invalid CUBEPOD_E2B_MAX_TENANT_BYTES must fail fast.
	badQuota := map[string]string{
		"CUBEPOD_E2B_VOLUME_DIR":       "/tmp/e2b-vols",
		"CUBEPOD_E2B_MAX_TENANT_BYTES": "-5",
	}
	if _, _, err := resolveE2BVolumeConfig(func(k string) string { return badQuota[k] }, ""); err == nil {
		t.Fatal("expected error for negative CUBEPOD_E2B_MAX_TENANT_BYTES")
	}
}
