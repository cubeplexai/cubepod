// Package egress implements zero-trust egress network policies, mTLS sandbox
// client certificate issuance and verification, and secret placeholder vault exchange.
package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	SandboxID       string
	Policy          v1alpha1.NetworkPolicySpec
	ExchangeURL     string
	ExchangeHandler http.Handler
	ClientTLSConfig *tls.Config
	Transport       http.RoundTripper
}

// EgressProxy is the Zero-Trust Egress sidecar proxy.
type EgressProxy struct {
	sandboxID       string
	policy          v1alpha1.NetworkPolicySpec
	exchangeURL     string
	exchangeHandler http.Handler
	clientTLSConfig *tls.Config
	transport       http.RoundTripper
	httpClient      *http.Client
	mu              sync.RWMutex
}

// NewEgressProxy creates and initializes an EgressProxy instance.
func NewEgressProxy(cfg EgressProxyConfig) *EgressProxy {
	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	if cfg.ClientTLSConfig != nil {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: cfg.ClientTLSConfig,
		}
	}

	return &EgressProxy{
		sandboxID:       cfg.SandboxID,
		policy:          cfg.Policy,
		exchangeURL:     cfg.ExchangeURL,
		exchangeHandler: cfg.ExchangeHandler,
		clientTLSConfig: cfg.ClientTLSConfig,
		transport:       transport,
		httpClient:      httpClient,
	}
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

// ServeHTTP handles outbound requests, enforcing AllowedHosts/DeniedHosts,
// scanning headers for cbxref_ tokens, exchanging them over mTLS, and forwarding upstream.
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Determine destination host
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

	// 1. Enforce destination host policy
	p.mu.RLock()
	policy := p.policy
	p.mu.RUnlock()

	if !CheckEgressAllowed(policy, targetHost) {
		http.Error(w, fmt.Sprintf("egress to host %q is forbidden by network policy", targetHost), http.StatusForbidden)
		return
	}

	// 2. Handle CONNECT tunnels (e.g. HTTPS passthrough)
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}

	// 3. Scan headers for cbxref_ placeholders and exchange them
	newHeaders := make(http.Header)
	for key, values := range r.Header {
		for _, val := range values {
			replacedVal := val
			for {
				idx := strings.Index(replacedVal, "cbxref_")
				if idx == -1 {
					break
				}
				if idx+39 > len(replacedVal) {
					break
				}
				placeholder := replacedVal[idx : idx+39]
				// Exchange placeholder for real secret
				realSecret, err := p.exchangeSecret(r.Context(), placeholder, targetHost, key)
				if err != nil {
					http.Error(w, fmt.Sprintf("egress secret exchange forbidden for header %q: %v", key, err), http.StatusForbidden)
					return
				}
				replacedVal = strings.Replace(replacedVal, placeholder, realSecret, 1)
			}
			newHeaders.Add(key, replacedVal)
		}
	}

	// 4. Build outbound request
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

	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		_ = clientConn.Close()
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
}
