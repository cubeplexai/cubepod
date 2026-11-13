// Package egress implements zero-trust egress network policies, mTLS sandbox
// client certificate issuance and verification, and secret placeholder vault exchange.
package egress

import (
	"bufio"
	"bytes"
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
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// ExchangeRequest represents the wire request to POST /internal/v1/egress/exchange.
type ExchangeRequest struct {
	SandboxID   string `json:"sandboxID"`
	Placeholder string `json:"placeholder"`
	SNIHost     string `json:"sniHost"`
	HeaderName  string `json:"headerName"`
}

// ExchangeResponse represents the wire response from POST /internal/v1/egress/exchange.
type ExchangeResponse struct {
	Secret string `json:"secret,omitempty"`
	Error  string `json:"error,omitempty"`
}

// VaultExchangeHandler returns an http.HandlerFunc implementing the mTLS secret exchange endpoint.
// It verifies the requesting sandbox identity via the mTLS peer certificate CommonName (or
// X-CubePod-Sandbox-ID when running over local loopback trust in tests), validates
// (sandboxID, placeholder, sniHost, headerName) via vault.Exchange(...), and returns the real secret.
func VaultExchangeHandler(vault *Vault, ca *CA) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ExchangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		// 1. Extract sandboxID from mTLS peer certificate CommonName
		var clientSandboxID string
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			clientSandboxID = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		if clientSandboxID == "" {
			clientSandboxID = r.Header.Get("X-CubePod-Sandbox-ID")
		}
		if clientSandboxID == "" {
			clientSandboxID = req.SandboxID
		}

		if clientSandboxID == "" {
			http.Error(w, "unauthenticated sandbox identity", http.StatusForbidden)
			return
		}

		// If sandboxID was also sent in the request body, it must match client certificate
		if req.SandboxID != "" && req.SandboxID != clientSandboxID {
			http.Error(w, "sandbox identity mismatch", http.StatusForbidden)
			return
		}

		// 2. Perform zero-trust exchange via Vault
		secret, err := vault.Exchange(clientSandboxID, req.Placeholder, req.SNIHost, req.HeaderName)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(ExchangeResponse{Error: err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ExchangeResponse{Secret: secret})
	}
}

// EgressProxyConfig defines configuration options for EgressProxy.
type EgressProxyConfig struct {
	SandboxID        string
	Policy           v1alpha1.NetworkPolicySpec
	ExchangeURL      string
	ExchangeHandler  http.Handler
	ClientTLSConfig  *tls.Config
	MITMCA           *CA
	UpstreamRootCAs  *x509.CertPool
	UpstreamProxy    string
	UpstreamDialAddr map[string]string
	Transport        http.RoundTripper
	Metrics          *Metrics
}

// EgressProxy is the Zero-Trust Egress sidecar proxy.
type EgressProxy struct {
	sandboxID        string
	policy           v1alpha1.NetworkPolicySpec
	exchangeURL      string
	exchangeHandler  http.Handler
	clientTLSConfig  *tls.Config
	mitmCA           *CA
	upstreamRootCAs  *x509.CertPool
	upstreamProxy    string
	upstreamDialAddr map[string]string
	transport        http.RoundTripper
	httpClient       *http.Client
	metrics          *Metrics
	mu               sync.RWMutex

	certMu    sync.RWMutex
	certCache map[string]tls.Certificate
}

// NewEgressProxy creates and initializes an EgressProxy instance.
func NewEgressProxy(cfg EgressProxyConfig) *EgressProxy {
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NewMetrics()
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	if cfg.ClientTLSConfig != nil {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: cfg.ClientTLSConfig,
		}
	}

	dialOverrides := make(map[string]string, len(cfg.UpstreamDialAddr))
	for k, v := range cfg.UpstreamDialAddr {
		dialOverrides[strings.ToLower(strings.TrimSpace(k))] = v
	}

	p := &EgressProxy{
		sandboxID:        cfg.SandboxID,
		policy:           cfg.Policy,
		exchangeURL:      cfg.ExchangeURL,
		exchangeHandler:  cfg.ExchangeHandler,
		clientTLSConfig:  cfg.ClientTLSConfig,
		mitmCA:           cfg.MITMCA,
		upstreamRootCAs:  cfg.UpstreamRootCAs,
		upstreamProxy:    strings.TrimSpace(cfg.UpstreamProxy),
		upstreamDialAddr: dialOverrides,
		httpClient:       httpClient,
		metrics:          metrics,
		certCache:        make(map[string]tls.Certificate),
	}

	if cfg.Transport != nil {
		p.transport = cfg.Transport
	} else {
		p.transport = &http.Transport{
			DialContext: p.dialUpstreamAddr,
		}
	}

	return p
}

// Metrics returns the Prometheus metrics registry for this proxy.
func (p *EgressProxy) Metrics() *Metrics {
	return p.metrics
}

// MetricsHandler returns an http.Handler serving Prometheus /metrics exposition.
func (p *EgressProxy) MetricsHandler() http.Handler {
	return p.metrics
}

// SetPolicy updates the network policy at runtime.
func (p *EgressProxy) SetPolicy(policy v1alpha1.NetworkPolicySpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policy = policy
}

// GetPolicy returns the current network policy.
func (p *EgressProxy) GetPolicy() v1alpha1.NetworkPolicySpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.policy
}

// exchangeSecret invokes the Vault exchange endpoint over mTLS (or in-memory handler).
func (p *EgressProxy) exchangeSecret(ctx context.Context, placeholder, host, headerName string) (string, error) {
	p.mu.RLock()
	sandboxID := p.sandboxID
	exchangeURL := p.exchangeURL
	handler := p.exchangeHandler
	client := p.httpClient
	p.mu.RUnlock()

	reqPayload := ExchangeRequest{
		SandboxID:   sandboxID,
		Placeholder: placeholder,
		SNIHost:     host,
		HeaderName:  headerName,
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return "", err
	}

	// 1. In-memory handler execution if provided without ExchangeURL
	if handler != nil && exchangeURL == "" {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/egress/exchange", bytes.NewReader(bodyBytes))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CubePod-Sandbox-ID", sandboxID)
		handler.ServeHTTP(rec, r)

		if rec.Code != http.StatusOK {
			return "", fmt.Errorf("exchange returned status %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		var resp ExchangeResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			return "", err
		}
		if resp.Secret == "" {
			return "", errors.New("empty secret returned from exchange")
		}
		return resp.Secret, nil
	}

	if exchangeURL == "" {
		return "", errors.New("no exchange URL or handler configured")
	}

	// 2. Out-of-process HTTP/mTLS exchange
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, exchangeURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CubePod-Sandbox-ID", sandboxID)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("exchange rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var exResp ExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&exResp); err != nil {
		return "", fmt.Errorf("failed to decode exchange response: %w", err)
	}
	if exResp.Secret == "" {
		return "", errors.New("empty secret returned from exchange")
	}
	return exResp.Secret, nil
}

// replaceHeaderPlaceholders clones src headers and replaces all cpodref_ placeholders via Vault exchange.
func (p *EgressProxy) replaceHeaderPlaceholders(ctx context.Context, src http.Header, targetHost string) (http.Header, error) {
	newHeaders := make(http.Header, len(src))
	for key, values := range src {
		for _, val := range values {
			replacedVal := val
			for {
				idx := strings.Index(replacedVal, DefaultPlaceholderPrefix)
				if idx == -1 || idx+40 > len(replacedVal) {
					break
				}
				placeholder := replacedVal[idx : idx+40]
				realSecret, err := p.exchangeSecret(ctx, placeholder, targetHost, key)
				if err != nil {
					if strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "status 403") {
						p.metrics.IncSecretExchange("denied")
					} else {
						p.metrics.IncSecretExchange("error")
					}
					return nil, fmt.Errorf("header %q: %w", key, err)
				}
				p.metrics.IncSecretExchange("ok")
				replacedVal = strings.Replace(replacedVal, placeholder, realSecret, 1)
			}
			newHeaders.Add(key, replacedVal)
		}
	}
	return newHeaders, nil
}

// ServeHTTP handles outbound requests, enforcing AllowedHosts/DeniedHosts,
// scanning headers for cpodref_ tokens, exchanging them over mTLS, and forwarding upstream.
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawHost := r.URL.Host
	if rawHost == "" {
		rawHost = r.Host
	}
	if rawHost == "" {
		rawHost = r.Header.Get("Host")
	}
	if rawHost == "" {
		http.Error(w, "missing destination host", http.StatusBadRequest)
		return
	}

	targetHost := normalizeHost(rawHost)
	policy := p.GetPolicy()

	if !CheckEgressAllowed(policy, targetHost) {
		mode := "http"
		if r.Method == http.MethodConnect {
			mode = "connect"
		}
		p.metrics.IncRequest(mode, "deny")
		p.metrics.IncBlockedHost(targetHost)
		http.Error(w, fmt.Sprintf("egress to host %q is forbidden by network policy", targetHost), http.StatusForbidden)
		return
	}

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}

	p.metrics.IncRequest("http", "allow")
	p.forwardPlainHTTP(w, r, rawHost, targetHost)
}

// TransparentHTTPHandler returns an http.Handler for transparent HTTP (:18081) interception.
func (p *EgressProxy) TransparentHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "CONNECT not allowed on transparent HTTP listener", http.StatusMethodNotAllowed)
			return
		}
		rawHost := r.Host
		if rawHost == "" {
			rawHost = r.URL.Host
		}
		if rawHost == "" {
			http.Error(w, "missing Host header", http.StatusBadRequest)
			return
		}

		targetHost := normalizeHost(rawHost)
		policy := p.GetPolicy()
		if !CheckEgressAllowed(policy, targetHost) {
			p.metrics.IncRequest("http", "deny")
			p.metrics.IncBlockedHost(targetHost)
			http.Error(w, fmt.Sprintf("egress to host %q is forbidden by network policy", targetHost), http.StatusForbidden)
			return
		}

		p.metrics.IncRequest("http", "allow")
		p.forwardPlainHTTP(w, r, rawHost, targetHost)
	})
}

func (p *EgressProxy) forwardPlainHTTP(w http.ResponseWriter, r *http.Request, rawHost, targetHost string) {
	newHeaders, err := p.replaceHeaderPlaceholders(r.Context(), r.Header, targetHost)
	if err != nil {
		http.Error(w, fmt.Sprintf("egress secret exchange forbidden: %v", err), http.StatusForbidden)
		return
	}

	outURL := *r.URL
	if outURL.Scheme == "" {
		outURL.Scheme = "http"
	}
	if outURL.Host == "" {
		outURL.Host = rawHost
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), r.Body)
	if err != nil {
		http.Error(w, "failed to build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.Header = newHeaders
	outReq.Host = rawHost

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream connection error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *EgressProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	targetHost, targetPort := splitHostPortDefault(r.Host, "443")

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Send 200 Connection Established so the client begins its TLS ClientHello.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		return
	}

	sniHost, replayedConn, err := peekClientHelloSNI(clientConn)
	if err != nil || sniHost == "" {
		p.metrics.IncRequest("connect", "deny")
		p.metrics.IncBlockedHost(targetHost)
		_ = replayedConn.Close()
		return
	}

	// Fail closed if ClientHello SNI does not match the CONNECT authority host.
	if sniHost != targetHost {
		p.metrics.IncRequest("connect", "deny")
		p.metrics.IncBlockedHost(sniHost)
		_ = replayedConn.Close()
		return
	}

	policy := p.GetPolicy()
	if !CheckEgressAllowed(policy, sniHost) {
		p.metrics.IncRequest("connect", "deny")
		p.metrics.IncBlockedHost(sniHost)
		_ = replayedConn.Close()
		return
	}

	if hostMatchesSecretBinding(policy, sniHost) && p.mitmCA != nil {
		p.metrics.IncRequest("mitm", "allow")
		p.serveMITMTLS(replayedConn, sniHost, targetPort)
		return
	}

	p.metrics.IncRequest("passthrough", "allow")
	p.spliceUpstream(replayedConn, sniHost, targetPort)
}

// ListenAndServeTransparentTLS listens on addr and dispatches incoming TLS connections to ServeTransparentTLS.
func (p *EgressProxy) ListenAndServeTransparentTLS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return p.ServeTransparentTLSListener(ln)
}

// ServeTransparentTLSListener accepts TLS connections from ln and handles them via ServeTransparentTLS.
func (p *EgressProxy) ServeTransparentTLSListener(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.ServeTransparentTLS(conn)
	}
}

// ServeTransparentTLS inspects the TLS ClientHello SNI of an intercepted connection,
// enforces NetworkPolicySpec, and routes to selective MITM or zero-copy passthrough.
func (p *EgressProxy) ServeTransparentTLS(conn net.Conn) {
	sniHost, replayedConn, err := peekClientHelloSNI(conn)
	if err != nil || sniHost == "" {
		p.metrics.IncRequest("transparent_tls", "deny")
		p.metrics.IncBlockedHost("unknown")
		_ = replayedConn.Close()
		return
	}

	policy := p.GetPolicy()
	if !CheckEgressAllowed(policy, sniHost) {
		p.metrics.IncRequest("transparent_tls", "deny")
		p.metrics.IncBlockedHost(sniHost)
		_ = replayedConn.Close()
		return
	}

	if hostMatchesSecretBinding(policy, sniHost) && p.mitmCA != nil {
		p.metrics.IncRequest("mitm", "allow")
		p.serveMITMTLS(replayedConn, sniHost, "443")
		return
	}

	p.metrics.IncRequest("passthrough", "allow")
	p.spliceUpstream(replayedConn, sniHost, "443")
}

func (p *EgressProxy) spliceUpstream(clientConn net.Conn, host, port string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	destConn, err := p.dialUpstream(ctx, host, port)
	cancel()
	if err != nil {
		_ = clientConn.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer destConn.Close()
		defer clientConn.Close()
		_, _ = io.Copy(destConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		defer clientConn.Close()
		defer destConn.Close()
		_, _ = io.Copy(clientConn, destConn)
	}()
	wg.Wait()
}

func (p *EgressProxy) serveMITMTLS(clientConn net.Conn, sniHost, port string) {
	leafCert, err := p.getMITMCert(sniHost)
	if err != nil {
		_ = clientConn.Close()
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{leafCert},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	defer tlsConn.Close()

	_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})

	upstreamTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName: sniHost,
			RootCAs:    p.upstreamRootCAs,
			MinVersion: tls.VersionTLS12,
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return p.dialUpstream(ctx, sniHost, port)
		},
	}
	defer upstreamTransport.CloseIdleConnections()

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}

		// Enforce HTTP Host == TLS SNI to prevent domain fronting.
		if req.Host == "" || normalizeHost(req.Host) != sniHost {
			p.metrics.IncBlockedHost(req.Host)
			_ = req.Body.Close()
			_ = writeMITMErrorResponse(tlsConn, http.StatusMisdirectedRequest, "421 Misdirected Request: Host header does not match TLS SNI\n")
			return
		}

		newHeaders, err := p.replaceHeaderPlaceholders(req.Context(), req.Header, sniHost)
		if err != nil {
			_ = req.Body.Close()
			_ = writeMITMErrorResponse(tlsConn, http.StatusForbidden, fmt.Sprintf("403 Forbidden: %v\n", err))
			return
		}

		outURL := *req.URL
		outURL.Scheme = "https"
		if outURL.Host == "" {
			outURL.Host = req.Host
		}

		outReq, err := http.NewRequestWithContext(req.Context(), req.Method, outURL.String(), req.Body)
		if err != nil {
			_ = req.Body.Close()
			_ = writeMITMErrorResponse(tlsConn, http.StatusInternalServerError, "500 Internal Server Error\n")
			return
		}
		outReq.Header = newHeaders
		outReq.Host = req.Host
		outReq.ContentLength = req.ContentLength

		resp, err := upstreamTransport.RoundTrip(outReq)
		_ = req.Body.Close()
		if err != nil {
			_ = writeMITMErrorResponse(tlsConn, http.StatusBadGateway, fmt.Sprintf("502 Bad Gateway: %v\n", err))
			return
		}

		writeErr := resp.Write(tlsConn)
		_ = resp.Body.Close()
		if writeErr != nil || req.Close || resp.Close || !req.ProtoAtLeast(1, 1) {
			return
		}
	}
}

func writeMITMErrorResponse(w io.Writer, statusCode int, body string) error {
	resp := &http.Response{
		StatusCode:    statusCode,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Close:         true,
	}
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp.Header.Set("Connection", "close")
	return resp.Write(w)
}

func (p *EgressProxy) getMITMCert(host string) (tls.Certificate, error) {
	h := normalizeHost(host)
	p.certMu.RLock()
	cached, ok := p.certCache[h]
	p.certMu.RUnlock()
	if ok && cached.Leaf != nil && time.Until(cached.Leaf.NotAfter) > time.Hour {
		return cached, nil
	}

	p.certMu.Lock()
	defer p.certMu.Unlock()
	if cached, ok := p.certCache[h]; ok && cached.Leaf != nil && time.Until(cached.Leaf.NotAfter) > time.Hour {
		return cached, nil
	}

	certPEM, keyPEM, err := p.mitmCA.MintServerCert([]string{h}, 24*time.Hour)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	if len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	p.certCache[h] = cert
	return cert, nil
}

func (p *EgressProxy) dialUpstreamAddr(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port := splitHostPortDefault(addr, "80")
	return p.dialUpstream(ctx, host, port)
}

func (p *EgressProxy) dialUpstream(ctx context.Context, host, port string) (net.Conn, error) {
	h := normalizeHost(host)
	if port == "" {
		port = "443"
	}

	hostPort := net.JoinHostPort(h, port)
	if override, ok := p.upstreamDialAddr[hostPort]; ok && override != "" {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", override)
	}
	if override, ok := p.upstreamDialAddr[h]; ok && override != "" {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", override)
	}

	proxyStr := p.upstreamProxy
	if proxyStr == "" {
		proxyStr = strings.TrimSpace(p.GetPolicy().UpstreamProxy)
	}
	if proxyStr != "" {
		return dialViaHTTPProxy(ctx, proxyStr, hostPort)
	}

	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", hostPort)
}

func dialViaHTTPProxy(ctx context.Context, proxyURLStr, targetHostPort string) (net.Conn, error) {
	if !strings.Contains(proxyURLStr, "://") {
		proxyURLStr = "http://" + proxyURLStr
	}
	u, err := url.Parse(proxyURLStr)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream proxy URL %q: %w", proxyURLStr, err)
	}
	proxyAddr := u.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr = net.JoinHostPort(proxyAddr, "8080")
	}

	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream proxy %q: %w", proxyAddr, err)
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetHostPort, targetHostPort)
	if _, err := io.WriteString(conn, connectReq); err != nil {
		_ = conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT response error: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT rejected with status %d", resp.StatusCode)
	}

	if buffered := br.Buffered(); buffered > 0 {
		peeked, _ := br.Peek(buffered)
		return newPrefixConn(conn, peeked), nil
	}
	return conn, nil
}

func hostMatchesSecretBinding(policy v1alpha1.NetworkPolicySpec, host string) bool {
	for _, sb := range policy.SecretBindings {
		for _, pattern := range sb.AllowedHosts {
			if MatchHost(host, pattern) {
				return true
			}
		}
	}
	return false
}

func splitHostPortDefault(raw, defaultPort string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if host, port, err := net.SplitHostPort(trimmed); err == nil {
		return normalizeHost(host), port
	}
	return normalizeHost(trimmed), defaultPort
}
