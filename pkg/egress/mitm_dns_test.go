package egress_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

func startTLSOrigin(t *testing.T, originCA *egress.CA, host string, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	certPEM, keyPEM, err := originCA.MintServerCert([]string{host}, 24*time.Hour)
	if err != nil {
		t.Fatalf("MintServerCert(%q) failed: %v", host, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair failed: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	return srv, u.Host
}

func startVaultServer(t *testing.T, sandboxID string) (*egress.Vault, *tls.Config, string) {
	t.Helper()
	vaultCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA failed: %v", err)
	}
	vault := egress.NewVault()

	srvTLS, err := vaultCA.ServerTLSConfig("127.0.0.1", "localhost")
	if err != nil {
		t.Fatalf("ServerTLSConfig failed: %v", err)
	}
	vaultSrv := httptest.NewUnstartedServer(egress.VaultExchangeHandler(vault, vaultCA))
	vaultSrv.TLS = srvTLS
	vaultSrv.StartTLS()
	t.Cleanup(vaultSrv.Close)

	certPEM, keyPEM, err := vaultCA.MintSandboxCert(sandboxID, time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	clientTLS, err := vaultCA.ClientTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}

	return vault, clientTLS, vaultSrv.URL
}

func TestHTTPSConnectSelectiveMITMSecretSwap(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "sbx-mitm-01"

	var receivedAuth string
	_, originAddr := startTLSOrigin(t, originCA, "api.github.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if receivedAuth != "Bearer ghp_real_secret_123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"login":"cubeplex-agent"}`))
	}))

	vault, clientTLS, exchangeURL := startVaultServer(t, sandboxID)
	placeholder, err := vault.BindSecret(
		sandboxID,
		"GITHUB_TOKEN",
		"ghp_real_secret_123",
		[]string{"api.github.com"},
		[]string{"Authorization"},
	)
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"api.github.com"},
		SecretBindings: []v1alpha1.SecretBinding{
			{
				EnvName:        "GITHUB_TOKEN",
				Placeholder:    placeholder,
				AllowedHosts:   []string{"api.github.com"},
				AllowedHeaders: []string{"Authorization"},
			},
		},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        sandboxID,
		Policy:           policy,
		ExchangeURL:      exchangeURL,
		ClientTLSConfig:  clientTLS,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{"api.github.com": originAddr},
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

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request via proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}
	if receivedAuth != "Bearer ghp_real_secret_123" {
		t.Fatalf("expected upstream to receive real secret, got %q", receivedAuth)
	}
	if !strings.Contains(string(body), "cubeplex-agent") {
		t.Fatalf("unexpected body: %s", string(body))
	}

	// Verify Prometheus metrics
	rec := httptest.NewRecorder()
	proxy.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricsBody := rec.Body.String()
	if !strings.Contains(metricsBody, `cubepod_egress_requests_total{mode="mitm",decision="allow"} 1`) {
		t.Fatalf("missing mitm allow metric in:\n%s", metricsBody)
	}
	if !strings.Contains(metricsBody, `cubepod_egress_secret_exchanges_total{result="ok"} 1`) {
		t.Fatalf("missing secret exchange ok metric in:\n%s", metricsBody)
	}
}

func TestHTTPSConnectPassthroughWithoutMITM(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()

	originSrv, originAddr := startTLSOrigin(t, originCA, "pypi.org", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("simple index"))
	}))

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"pypi.org", "api.github.com"},
		SecretBindings: []v1alpha1.SecretBinding{
			{
				EnvName:      "GITHUB_TOKEN",
				Placeholder:  "cpodref_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				AllowedHosts: []string{"api.github.com"},
			},
		},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        "sbx-passthrough",
		Policy:           policy,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{"pypi.org": originAddr},
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	// Client trusts ONLY originCA (not mitmCA), proving no MITM termination occurs.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				RootCAs: originCA.CertPool(),
			},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("https://pypi.org/simple/")
	if err != nil {
		t.Fatalf("passthrough HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatalf("expected TLS peer certificate")
	}
	if !resp.TLS.PeerCertificates[0].Equal(originSrv.Certificate()) {
		t.Fatalf("expected original upstream leaf cert (passthrough), got different cert")
	}
}

func TestTransparentTLSListenerSNIAndSwap(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "sbx-transparent-tls"

	_, originAddr := startTLSOrigin(t, originCA, "api.github.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_transparent_secret" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("transparent-ok"))
	}))

	vault, clientTLS, exchangeURL := startVaultServer(t, sandboxID)
	placeholder, _ := vault.BindSecret(
		sandboxID,
		"GITHUB_TOKEN",
		"ghp_transparent_secret",
		[]string{"api.github.com"},
		[]string{"Authorization"},
	)

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"api.github.com"},
		SecretBindings: []v1alpha1.SecretBinding{
			{
				EnvName:        "GITHUB_TOKEN",
				Placeholder:    placeholder,
				AllowedHosts:   []string{"api.github.com"},
				AllowedHeaders: []string{"Authorization"},
			},
		},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        sandboxID,
		Policy:           policy,
		ExchangeURL:      exchangeURL,
		ClientTLSConfig:  clientTLS,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{"api.github.com": originAddr},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer ln.Close()
	go func() {
		_ = proxy.ServeTransparentTLSListener(ln)
	}()

	// Dial transparent TLS listener directly without CONNECT.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", ln.Addr().String())
			},
			TLSClientConfig: &tls.Config{
				ServerName: "api.github.com",
				RootCAs:    mitmCA.CertPool(),
			},
		},
		Timeout: 5 * time.Second,
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("transparent TLS request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "transparent-ok" {
		t.Fatalf("unexpected response (%d): %s", resp.StatusCode, string(body))
	}

	// Denied SNI host should be closed immediately.
	deniedConn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName:         "evil.com",
		InsecureSkipVerify: true,
	})
	if err == nil {
		_ = deniedConn.Close()
		t.Fatalf("expected TLS handshake to fail for denied SNI evil.com")
	}
}

func TestDomainFrontingHostMismatchRejected(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "sbx-domain-front"

	_, originAddr := startTLSOrigin(t, originCA, "api.github.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("origin should never be reached on domain fronting mismatch")
		w.WriteHeader(http.StatusOK)
	}))

	var vaultCalls atomic.Int32
	vault := egress.NewVault()
	placeholder, _ := vault.BindSecret(
		sandboxID,
		"GITHUB_TOKEN",
		"ghp_secret",
		[]string{"api.github.com"},
		[]string{"Authorization"},
	)

	countingVaultHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vaultCalls.Add(1)
		egress.VaultExchangeHandler(vault, nil).ServeHTTP(w, r)
	})

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"api.github.com"},
		SecretBindings: []v1alpha1.SecretBinding{
			{
				EnvName:        "GITHUB_TOKEN",
				Placeholder:    placeholder,
				AllowedHosts:   []string{"api.github.com"},
				AllowedHeaders: []string{"Authorization"},
			},
		},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        sandboxID,
		Policy:           policy,
		ExchangeHandler:  countingVaultHandler,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{"api.github.com": originAddr},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer ln.Close()
	go func() {
		_ = proxy.ServeTransparentTLSListener(ln)
	}()

	// Connect with SNI=api.github.com, but send HTTP Host: evil.example.com
	tlsConn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName: "api.github.com",
		RootCAs:    mitmCA.CertPool(),
	})
	if err != nil {
		t.Fatalf("tls.Dial failed: %v", err)
	}
	defer tlsConn.Close()

	rawReq := fmt.Sprintf("GET / HTTP/1.1\r\nHost: evil.example.com\r\nAuthorization: Bearer %s\r\n\r\n", placeholder)
	if _, err := io.WriteString(tlsConn, rawReq); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("expected 421 Misdirected Request, got %d", resp.StatusCode)
	}
	if vaultCalls.Load() != 0 {
		t.Fatalf("expected 0 Vault calls on domain fronting, got %d", vaultCalls.Load())
	}
}

func TestUpstreamProxyChaining(t *testing.T) {
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "sbx-chain"

	_, originAddr := startTLSOrigin(t, originCA, "api.github.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_chained_secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("chained-ok"))
	}))

	// Mock upstream HTTP CONNECT proxy that forwards api.github.com:443 -> originAddr
	var chainedConnectHost string
	var chainMu sync.Mutex
	upstreamProxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chainMu.Lock()
		chainedConnectHost = r.Host
		chainMu.Unlock()

		destConn, err := net.DialTimeout("tcp", originAddr, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = destConn.Close()
			return
		}
		clientConn, _, err := hj.Hijack()
		if err != nil {
			_ = destConn.Close()
			return
		}
		_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			defer clientConn.Close()
			defer destConn.Close()
			_, _ = io.Copy(destConn, clientConn)
		}()
		go func() {
			defer clientConn.Close()
			defer destConn.Close()
			_, _ = io.Copy(clientConn, destConn)
		}()
	}))
	defer upstreamProxySrv.Close()

	vault, clientTLS, exchangeURL := startVaultServer(t, sandboxID)
	placeholder, _ := vault.BindSecret(
		sandboxID,
		"GITHUB_TOKEN",
		"ghp_chained_secret",
		[]string{"api.github.com"},
		[]string{"Authorization"},
	)

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"api.github.com"},
		SecretBindings: []v1alpha1.SecretBinding{
			{
				EnvName:        "GITHUB_TOKEN",
				Placeholder:    placeholder,
				AllowedHosts:   []string{"api.github.com"},
				AllowedHeaders: []string{"Authorization"},
			},
		},
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:       sandboxID,
		Policy:          policy,
		ExchangeURL:     exchangeURL,
		ClientTLSConfig: clientTLS,
		MITMCA:          mitmCA,
		UpstreamRootCAs: originCA.CertPool(),
		UpstreamProxy:   upstreamProxySrv.URL,
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

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/zen", nil)
	req.Header.Set("Authorization", "Bearer "+placeholder)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chained request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "chained-ok" {
		t.Fatalf("unexpected chained response (%d): %s", resp.StatusCode, string(body))
	}

	chainMu.Lock()
	gotHost := chainedConnectHost
	chainMu.Unlock()
	if gotHost != "api.github.com:443" {
		t.Fatalf("expected upstream proxy to receive CONNECT api.github.com:443, got %q", gotHost)
	}
}

func buildTestDNSQuery(id uint16, domain string) []byte {
	var pkt []byte
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], id)
	binary.BigEndian.PutUint16(hdr[2:4], 0x0100) // RD=1
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // QDCOUNT=1
	pkt = append(pkt, hdr...)

	for _, label := range strings.Split(strings.TrimSuffix(domain, "."), ".") {
		pkt = append(pkt, byte(len(label)))
		pkt = append(pkt, []byte(label)...)
	}
	pkt = append(pkt, 0x00)       // root
	pkt = append(pkt, 0x00, 0x01) // QTYPE=A
	pkt = append(pkt, 0x00, 0x01) // QCLASS=IN
	return pkt
}

func TestDNSFilterAllowAndDeny(t *testing.T) {
	// Mock upstream UDP DNS responder
	upstreamPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream ListenPacket failed: %v", err)
	}
	defer upstreamPC.Close()

	var upstreamHits atomic.Int32
	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := upstreamPC.ReadFrom(buf)
			if err != nil {
				return
			}
			upstreamHits.Add(1)
			resp := make([]byte, n)
			copy(resp, buf[:n])
			binary.BigEndian.PutUint16(resp[2:4], 0x8180) // QR=1, RD=1, RA=1, RCODE=0
			_, _ = upstreamPC.WriteTo(resp, addr)
		}
	}()

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"api.github.com"},
		DenyHosts:     []string{"evil.com"},
	}

	metrics := egress.NewMetrics()
	dnsSrv := egress.NewDNSServer(egress.DNSConfig{
		UpstreamAddr: upstreamPC.LocalAddr().String(),
		GetPolicy:    func() v1alpha1.NetworkPolicySpec { return policy },
		Metrics:      metrics,
	})

	dnsPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dns ListenPacket failed: %v", err)
	}
	defer dnsPC.Close()
	go func() {
		_ = dnsSrv.ServePacketConn(dnsPC)
	}()

	// 1. Allowed domain over UDP
	conn, err := net.Dial("udp", dnsPC.LocalAddr().String())
	if err != nil {
		t.Fatalf("udp dial failed: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	_, _ = conn.Write(buildTestDNSQuery(0x1234, "api.github.com"))
	respBuf := make([]byte, 512)
	n, err := conn.Read(respBuf)
	if err != nil {
		t.Fatalf("read allowed DNS response failed: %v", err)
	}
	if n < 12 || binary.BigEndian.Uint16(respBuf[0:2]) != 0x1234 {
		t.Fatalf("unexpected DNS response header")
	}
	rcode := binary.BigEndian.Uint16(respBuf[2:4]) & 0x000F
	if rcode != 0 {
		t.Fatalf("expected RCODE=0 for allowed host, got %d", rcode)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", upstreamHits.Load())
	}

	// 2. Denied domain over UDP -> RCODE=5 (Refused) without hitting upstream
	_, _ = conn.Write(buildTestDNSQuery(0x5678, "evil.com"))
	n, err = conn.Read(respBuf)
	if err != nil {
		t.Fatalf("read denied DNS response failed: %v", err)
	}
	if n < 12 || binary.BigEndian.Uint16(respBuf[0:2]) != 0x5678 {
		t.Fatalf("unexpected DNS response ID")
	}
	rcode = binary.BigEndian.Uint16(respBuf[2:4]) & 0x000F
	if rcode != 5 {
		t.Fatalf("expected RCODE=5 (Refused) for evil.com, got %d", rcode)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected upstream not to be queried for denied host, got %d hits", upstreamHits.Load())
	}

	// 3. Denied domain over TCP -> RCODE=5 (Refused)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen failed: %v", err)
	}
	defer tcpLn.Close()
	go func() {
		_ = dnsSrv.ServeTCPListener(tcpLn)
	}()

	tcpConn, err := net.DialTimeout("tcp", tcpLn.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("tcp dial failed: %v", err)
	}
	defer tcpConn.Close()

	q := buildTestDNSQuery(0x9abc, "evil.com")
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(q)))
	_, _ = tcpConn.Write(lenBuf[:])
	_, _ = tcpConn.Write(q)

	_, _ = io.ReadFull(tcpConn, lenBuf[:])
	tcpResp := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
	_, _ = io.ReadFull(tcpConn, tcpResp)
	if rcode := binary.BigEndian.Uint16(tcpResp[2:4]) & 0x000F; rcode != 5 {
		t.Fatalf("expected TCP RCODE=5 (Refused), got %d", rcode)
	}
}

func TestLoadCAAndTransparentHTTP(t *testing.T) {
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("NewCA failed: %v", err)
	}
	certPEM, keyPEM, err := ca.MintSandboxCert("test", time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	if _, err := egress.LoadCA(certPEM, keyPEM); err != nil {
		t.Fatalf("LoadCA failed: %v", err)
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID: "sbx-thttp",
		Policy: v1alpha1.NetworkPolicySpec{
			DefaultAction: v1alpha1.NetworkDefaultDeny,
			AllowHosts:    []string{"allowed.example.com"},
		},
	})
	srv := httptest.NewServer(proxy.TransparentHTTPHandler())
	defer srv.Close()

	// Blocked host on transparent HTTP
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
	req.Host = "blocked.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d", resp.StatusCode)
	}
}
