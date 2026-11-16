package egress_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA failed: %v", err)
	}

	sandboxID := "sbx-direct-01"
	ph, err := vault.BindSecret(sandboxID, "KEY", "secret-xyz", []string{"api.example.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	payload := fmt.Sprintf(`{"sandboxID":"%s","placeholder":"%s","sniHost":"api.example.com","headerName":"Authorization"}`, sandboxID, ph)

	// 1. When ca != nil, non-mTLS requests (even with X-CubePod-Sandbox-ID / req.SandboxID) MUST be rejected with 403
	strictHandler := egress.VaultExchangeHandler(vault, ca)
	reqNoTLS := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", strings.NewReader(payload))
	reqNoTLS.Header.Set("Content-Type", "application/json")
	reqNoTLS.Header.Set("X-CubePod-Sandbox-ID", sandboxID)
	recNoTLS := httptest.NewRecorder()
	strictHandler.ServeHTTP(recNoTLS, reqNoTLS)
	if recNoTLS.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-mTLS when ca != nil, got %d: %s", recNoTLS.Code, recNoTLS.Body.String())
	}

	// 2. When ca != nil, real mTLS with client certificate succeeds
	srvTLS, err := ca.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig failed: %v", err)
	}
	srv := httptest.NewUnstartedServer(strictHandler)
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	certPEM, keyPEM, err := ca.MintSandboxCert(sandboxID, time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	clientTLS, err := ca.ClientTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}
	mtlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
	resp, err := mtlsClient.Post(srv.URL+"/internal/v1/egress/exchange", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("mTLS Post failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "secret-xyz") {
		t.Fatalf("expected 200 OK with secret over mTLS, got %d: %s", resp.StatusCode, string(body))
	}

	// 3. When ca == nil (local/unit test mode), loopback header X-CubePod-Sandbox-ID fallback works
	localHandler := egress.VaultExchangeHandler(vault, nil)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CubePod-Sandbox-ID", sandboxID)
	rec := httptest.NewRecorder()
	localHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret-xyz") {
		t.Fatalf("expected 200 OK with secret when ca == nil, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Test sandbox mismatch when ca == nil
	reqMismatch := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", strings.NewReader(payload))
	reqMismatch.Header.Set("Content-Type", "application/json")
	reqMismatch.Header.Set("X-CubePod-Sandbox-ID", "imposter")
	recMismatch := httptest.NewRecorder()
	localHandler.ServeHTTP(recMismatch, reqMismatch)
	if recMismatch.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on sandbox mismatch, got %d", recMismatch.Code)
	}
}

func TestEgressProxyCubePlexWireContractAndMITM(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "cpod-cbx-mitm-1"
	placeholder := "cbxref_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

	var upstreamAuth string
	var upstreamHits int
	_, originAddr := startTLSOrigin(t, originCA, "api.example.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		upstreamAuth = r.Header.Get("Authorization")
		if upstreamAuth != "Bearer sk-live-cbx-42" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cbx-mitm-ok"))
	}))

	exchangeCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA exchangeCA: %v", err)
	}
	var lastHost string
	exchangeSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Placeholder string `json:"placeholder"`
			Host        string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lastHost = body.Host
		if body.Placeholder != placeholder {
			http.Error(w, "unknown placeholder", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secret":       "sk-live-cbx-42",
			"header_names": []string{"Authorization"},
		})
	}))
	srvTLS, err := exchangeCA.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	exchangeSrv.TLS = srvTLS
	exchangeSrv.StartTLS()
	defer exchangeSrv.Close()

	certPEM, keyPEM, err := exchangeCA.MintSandboxCert(sandboxID, time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert: %v", err)
	}
	clientTLS, err := exchangeCA.ClientTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}

	// Empty SecretBindings with ExchangeURL configured must still trigger MITM over CONNECT.
	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        sandboxID,
		ExchangeURL:      exchangeSrv.URL,
		ClientTLSConfig:  clientTLS,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{"api.example.com": originAddr},
		Policy: v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			AllowHosts:    []string{"api.example.com"},
		},
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				RootCAs: mitmCA.CertPool(),
			},
		},
		Timeout: 5 * time.Second,
	}

	// 1. Allowed header (Authorization) succeeds and sends wire field "host".
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/test", nil)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request via MITM failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(bodyBytes) != "cbx-mitm-ok" {
		t.Fatalf("expected 200 OK cbx-mitm-ok, got %d %q", resp.StatusCode, string(bodyBytes))
	}
	if lastHost != "api.example.com" {
		t.Fatalf("expected exchange host api.example.com, got %q", lastHost)
	}
	if upstreamAuth != "Bearer sk-live-cbx-42" {
		t.Fatalf("expected upstream Authorization swap, got %q", upstreamAuth)
	}

	// 2. Disallowed header (X-Leak) must be blocked 403 before reaching upstream.
	hitsBefore := upstreamHits
	leakReq, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/leak", nil)
	leakReq.Header.Set("X-Leak", placeholder)
	leakResp, err := client.Do(leakReq)
	if err != nil {
		t.Fatalf("leak request failed: %v", err)
	}
	_ = leakResp.Body.Close()
	if leakResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for disallowed header X-Leak, got %d", leakResp.StatusCode)
	}
	if upstreamHits != hitsBefore {
		t.Fatalf("upstream should not have been called on disallowed header (%d -> %d)", hitsBefore, upstreamHits)
	}
}

func TestEgressProxy_RejectsHostAndURLMismatch(t *testing.T) {
	deniedHits := 0
	deniedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deniedHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should-never-reach"))
	}))
	defer deniedUpstream.Close()

	deniedURL, err := url.Parse(deniedUpstream.URL)
	if err != nil {
		t.Fatalf("parse denied URL: %v", err)
	}

	mitmCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA mitm: %v", err)
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:   "cpod-ssrf-test",
		ExchangeURL: "https://exchange.internal:8443",
		MITMCA:      mitmCA,
		Policy: v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			AllowHosts:    []string{"allowed.example"},
		},
	})

	// 1. TransparentHTTPHandler must reject mismatched Host vs URL.Host (400) before dialing.
	mismatchURL, _ := url.Parse(deniedUpstream.URL + "/latest/meta-data")
	reqTransparent := httptest.NewRequest(http.MethodGet, "http://allowed.example/latest/meta-data", nil)
	reqTransparent.Host = "allowed.example"
	reqTransparent.URL = mismatchURL
	recTransparent := httptest.NewRecorder()
	proxy.TransparentHTTPHandler().ServeHTTP(recTransparent, reqTransparent)
	if recTransparent.Code != http.StatusBadRequest {
		t.Fatalf("TransparentHTTPHandler mismatch status = %d; want 400", recTransparent.Code)
	}

	// 2. TransparentHTTPHandler must reject non-http schemes (e.g. https) on plain listener.
	reqScheme := httptest.NewRequest(http.MethodGet, "https://allowed.example/secret", nil)
	reqScheme.Host = "allowed.example"
	recScheme := httptest.NewRecorder()
	proxy.TransparentHTTPHandler().ServeHTTP(recScheme, reqScheme)
	if recScheme.Code != http.StatusBadRequest {
		t.Fatalf("TransparentHTTPHandler non-http scheme status = %d; want 400", recScheme.Code)
	}

	// 3. Explicit ServeHTTP must reject mismatched URL.Host vs Host header on GET and CONNECT.
	reqProxyGet := httptest.NewRequest(http.MethodGet, "http://allowed.example/ok", nil)
	reqProxyGet.Host = deniedURL.Host
	recProxyGet := httptest.NewRecorder()
	proxy.ServeHTTP(recProxyGet, reqProxyGet)
	if recProxyGet.Code != http.StatusBadRequest {
		t.Fatalf("ServeHTTP GET mismatch status = %d; want 400", recProxyGet.Code)
	}

	reqConnect := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: "allowed.example:443"},
		Host:   deniedURL.Host,
		Header: make(http.Header),
	}
	recConnect := httptest.NewRecorder()
	proxy.ServeHTTP(recConnect, reqConnect)
	if recConnect.Code != http.StatusBadRequest {
		t.Fatalf("ServeHTTP CONNECT mismatch status = %d; want 400", recConnect.Code)
	}

	// 4. MITM CONNECT must reject inner HTTP requests whose URL.Host disagrees with SNI/Host.
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()
	proxyAddr := strings.TrimPrefix(proxySrv.URL, "http://")

	rawConn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = rawConn.Close() }()

	_, _ = fmt.Fprintf(rawConn, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
	br := bufio.NewReader(rawConn)
	connectResp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d; want 200", connectResp.StatusCode)
	}

	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: "allowed.example",
		RootCAs:    mitmCA.CertPool(),
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("MITM TLS handshake: %v", err)
	}
	_, _ = fmt.Fprintf(tlsConn, "GET https://%s/steal HTTP/1.1\r\nHost: allowed.example\r\n\r\n", deniedURL.Host)
	mitmResp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	_ = mitmResp.Body.Close()
	if mitmResp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("MITM mismatched inner URL.Host status = %d; want 421", mitmResp.StatusCode)
	}

	if deniedHits != 0 {
		t.Fatalf("denied upstream received %d hits; want 0", deniedHits)
	}
}

func TestVaultManagedSecretRotationRevocationAndMITMTransforms(t *testing.T) {
	var seenAuth string
	var authCount int
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCount = len(r.Header.Values("Authorization"))
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamHostPort := strings.TrimPrefix(upstream.URL, "https://")
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())

	vault := egress.NewVault()
	mitmCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA mitm: %v", err)
	}

	ph, err := vault.BindManagedSecret("sbx-1", "tenant-a", "sec_123", "", "sk-v1", []string{"example.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindManagedSecret: %v", err)
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        "sbx-1",
		MITMCA:           mitmCA,
		UpstreamRootCAs:  upstreamPool,
		UpstreamDialAddr: map[string]string{"example.com:443": upstreamHostPort},
		ExchangeHandler:  egress.VaultExchangeHandler(vault, nil),
		Policy: v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			AllowHosts:    []string{"example.com"},
			HeaderTransforms: []v1alpha1.HeaderTransformRule{
				{
					Host: "example.com",
					Headers: map[string]string{
						"Authorization": "Bearer " + ph,
					},
				},
			},
		},
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: mitmCA.CertPool()},
		},
	}

	// 1. Transform overwrites spoofed Authorization header and exchanges v1
	req1, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/charges", nil)
	req1.Header.Set("Authorization", "Bearer spoofed")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("client.Do 1: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK || seenAuth != "Bearer sk-v1" || authCount != 1 {
		t.Fatalf("step 1: status=%d auth=%q count=%d", resp1.StatusCode, seenAuth, authCount)
	}

	// 2. Rotate secret -> next request receives sk-v2
	if n := vault.UpdateSecretByRef("tenant-a", "sec_123", "sk-v2"); n != 1 {
		t.Fatalf("UpdateSecretByRef updated %d; want 1", n)
	}
	req2, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/charges", nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("client.Do 2: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || seenAuth != "Bearer sk-v2" {
		t.Fatalf("step 2: status=%d auth=%q", resp2.StatusCode, seenAuth)
	}

	// 3. CRLF secret value must fail closed with 403
	if n := vault.UpdateSecretByRef("tenant-a", "sec_123", "sk-bad\r\nX-Injected: 1"); n != 1 {
		t.Fatalf("UpdateSecretByRef bad value count = %d", n)
	}
	req3, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/charges", nil)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("client.Do 3: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("step 3 CRLF status = %d; want 403", resp3.StatusCode)
	}

	// 4. Revoke secret -> fails closed with 403
	if n := vault.RevokeSecretByRef("tenant-a", "sec_123"); n != 1 {
		t.Fatalf("RevokeSecretByRef revoked %d; want 1", n)
	}
	req4, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/charges", nil)
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatalf("client.Do 4: %v", err)
	}
	_ = resp4.Body.Close()
	if resp4.StatusCode != http.StatusForbidden {
		t.Fatalf("step 4 revoked status = %d; want 403", resp4.StatusCode)
	}
}

func TestVaultExchangeHandlerRehydrationAndStrictMTLS(t *testing.T) {
	vault := egress.NewVault()
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	sandboxID := "sbx-rehydrate-1"
	placeholder := "cpodref_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

	rehydrateCalls := 0
	rehydrate := func(_ context.Context, sbxID, ph string) error {
		rehydrateCalls++
		if sbxID != sandboxID || ph != placeholder {
			return egress.ErrUnknownPlaceholder
		}
		return vault.RestoreManagedSecret(ph, sbxID, "tenant-a", "sec_1", "", "sk-rehydrated-999", []string{"api.openai.com"}, []string{"Authorization"})
	}

	srvTLS, err := ca.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(egress.VaultExchangeHandlerWithRehydrator(vault, ca, rehydrate))
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	certPEM, keyPEM, err := ca.MintSandboxCert(sandboxID, time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert: %v", err)
	}
	clientTLS, err := ca.ClientTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}

	payload := fmt.Sprintf(`{"sandboxID":"%s","placeholder":"%s","sniHost":"api.openai.com","headerName":"Authorization"}`, sandboxID, placeholder)

	// First call hits cache miss -> triggers rehydrate -> succeeds 200
	resp1, err := client.Post(srv.URL+"/internal/v1/egress/exchange", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Post 1: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK || !strings.Contains(string(body1), "sk-rehydrated-999") {
		t.Fatalf("expected 200 OK with rehydrated secret, got %d: %s", resp1.StatusCode, string(body1))
	}
	if rehydrateCalls != 1 {
		t.Fatalf("expected 1 rehydrate call, got %d", rehydrateCalls)
	}

	// Second call hits Vault cache directly without calling rehydrate again
	resp2, err := client.Post(srv.URL+"/internal/v1/egress/exchange", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Post 2: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on cached exchange, got %d", resp2.StatusCode)
	}
	if rehydrateCalls != 1 {
		t.Fatalf("expected cached exchange not to call rehydrate again, got %d", rehydrateCalls)
	}

	// Unknown placeholder where rehydrate returns error -> fails closed 403
	badPayload := fmt.Sprintf(`{"sandboxID":"%s","placeholder":"cpodref_UNKNOWN000000000000000000000000","sniHost":"api.openai.com","headerName":"Authorization"}`, sandboxID)
	resp3, err := client.Post(srv.URL+"/internal/v1/egress/exchange", "application/json", strings.NewReader(badPayload))
	if err != nil {
		t.Fatalf("Post 3: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unknown placeholder, got %d", resp3.StatusCode)
	}
}

func TestVault_ConcurrentExchangeAndRotateAndForkedPlaceholderIsolation(t *testing.T) {
	vault := egress.NewVault()
	ph, err := vault.BindManagedSecret("parent-sbx", "tenant-a", "sec_race", "OPENAI_API_KEY", "sk-v0", []string{"api.openai.com"}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindManagedSecret: %v", err)
	}

	// Forked child restores same inherited placeholder without clobbering parent binding.
	if err := vault.RestoreManagedSecret(ph, "child-sbx", "tenant-a", "sec_race", "", "sk-v0", []string{"api.openai.com"}, []string{"Authorization"}); err != nil {
		t.Fatalf("RestoreManagedSecret child: %v", err)
	}
	if _, err := vault.Exchange("attacker-sbx", ph, "api.openai.com", "Authorization"); !errors.Is(err, egress.ErrSandboxMismatch) {
		t.Fatalf("expected ErrSandboxMismatch for unbound sandbox, got %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		sbx := "parent-sbx"
		if g%2 == 1 {
			sbx = "child-sbx"
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				sec, exErr := vault.Exchange(id, ph, "api.openai.com", "Authorization")
				if exErr != nil || sec == "" {
					t.Errorf("Exchange(%s) failed: sec=%q err=%v", id, sec, exErr)
					return
				}
			}
		}(sbx)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 50; i++ {
			n := vault.UpdateSecretByRef("tenant-a", "sec_race", fmt.Sprintf("sk-v%d", i))
			if n != 2 {
				t.Errorf("expected UpdateSecretByRef to update both parent and child bindings, got %d", n)
				return
			}
		}
	}()
	wg.Wait()
}
