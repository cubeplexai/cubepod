package egress_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

func TestEgressProxyZeroTrustExchange(t *testing.T) {
	// 1. Mock upstream LLM server that asserts the real secret is received
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		auth := r.Header.Get("Authorization")
		if auth != "Bearer sk-real-secret-999" {
			http.Error(w, fmt.Sprintf("unauthorized, got header %q", auth), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock upstream llm response"))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream URL: %v", err)
	}

	// 2. Real mTLS Vault Exchange server using CA and Vault
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("failed to create CA: %v", err)
	}

	vault := egress.NewVault()
	sandboxID := "sbx-test-01"
	rawSecret := "sk-real-secret-999"

	// Bind secret: allow upstream host and only "Authorization" header
	placeholder, err := vault.BindSecret(
		sandboxID,
		"OPENAI_API_KEY",
		rawSecret,
		[]string{upstreamURL.Hostname(), "api.openai.com"},
		[]string{"Authorization"},
	)
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	// Start mTLS Vault Exchange server
	serverTLS, err := ca.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig failed: %v", err)
	}

	exchangeSrv := httptest.NewUnstartedServer(egress.VaultExchangeHandler(vault, ca))
	exchangeSrv.TLS = serverTLS
	exchangeSrv.StartTLS()
	defer exchangeSrv.Close()

	// 3. Mint sandbox mTLS client cert
	certPEM, keyPEM, err := ca.MintSandboxCert(sandboxID, 1*time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	clientTLS, err := ca.ClientTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}

	// 4. Start EgressProxy
	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{upstreamURL.Hostname(), "api.openai.com"},
		DenyHosts:     []string{"evil.com", "*.malicious.org"},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:       sandboxID,
		Policy:          policy,
		ExchangeURL:     exchangeSrv.URL,
		ClientTLSConfig: clientTLS,
	})

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 5 * time.Second,
	}

	// 5. Test successful request with cpodref_ placeholder
	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+placeholder)

	resp, err := proxyClient.Do(req)
	if err != nil {
		t.Fatalf("proxyClient.Do failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from upstream, got %d: %s", resp.StatusCode, string(body))
	}
	if string(body) != "mock upstream llm response" {
		t.Fatalf("unexpected body: %s", string(body))
	}
	if !upstreamCalled {
		t.Fatalf("upstream server was never reached")
	}

	// 6. Test unauthorized destination host (evil.com) -> 403 Forbidden
	evilReq, _ := http.NewRequest(http.MethodGet, "http://evil.com/leak", nil)
	evilResp, err := proxyClient.Do(evilReq)
	if err != nil {
		t.Fatalf("request to evil.com failed: %v", err)
	}
	defer evilResp.Body.Close()
	if evilResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for evil.com, got %d", evilResp.StatusCode)
	}

	// 7. Test wrong header (X-Leak) -> Vault rejects -> 403 Forbidden
	leakReq, _ := http.NewRequest(http.MethodGet, upstream.URL+"/v1/leak", nil)
	leakReq.Header.Set("X-Leak", placeholder)
	leakResp, err := proxyClient.Do(leakReq)
	if err != nil {
		t.Fatalf("leak request failed: %v", err)
	}
	defer leakResp.Body.Close()
	if leakResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for disallowed header X-Leak, got %d", leakResp.StatusCode)
	}
}

func TestEgressProxyPolicyEnforcement(t *testing.T) {
	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"*.github.com", "pypi.org"},
		DenyHosts:     []string{"evil.github.com"},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID: "sbx-policy-test",
		Policy:    policy,
	})

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 2 * time.Second,
	}

	// Blocked by explicit deny list
	req1, _ := http.NewRequest(http.MethodGet, "http://evil.github.com/test", nil)
	resp1, err := client.Do(req1)
	if err == nil {
		defer resp1.Body.Close()
		if resp1.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d", resp1.StatusCode)
		}
	}

	// Blocked by default deny
	req2, _ := http.NewRequest(http.MethodGet, "http://unallowed.com/test", nil)
	resp2, err := client.Do(req2)
	if err == nil {
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d", resp2.StatusCode)
		}
	}
}

func TestVaultExchangeHandlerDirect(t *testing.T) {
	vault := egress.NewVault()
	ca, _ := egress.NewCA()

	sandboxID := "sbx-direct-01"
	ph, err := vault.BindSecret(sandboxID, "KEY", "secret-xyz", []string{"api.example.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	handler := egress.VaultExchangeHandler(vault, ca)

	// Test with loopback header X-CubePod-Sandbox-ID
	payload := fmt.Sprintf(`{"sandboxID":"%s","placeholder":"%s","sniHost":"api.example.com","headerName":"Authorization"}`, sandboxID, ph)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CubePod-Sandbox-ID", sandboxID)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret-xyz") {
		t.Fatalf("expected secret in response: %s", rec.Body.String())
	}

	// Test sandbox mismatch
	reqMismatch := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", strings.NewReader(payload))
	reqMismatch.Header.Set("Content-Type", "application/json")
	reqMismatch.Header.Set("X-CubePod-Sandbox-ID", "imposter")
	recMismatch := httptest.NewRecorder()

	handler.ServeHTTP(recMismatch, reqMismatch)
	if recMismatch.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on sandbox mismatch, got %d", recMismatch.Code)
	}
}
