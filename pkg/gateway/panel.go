// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// PanelTokenClaims holds the verified claims from an HMAC-SHA256 signed panel token.
type PanelTokenClaims struct {
	SandboxID string
	Port      int
	ExpUnix   int64
}

// mintEndpointReq is the public POST /sandboxes/{id}/endpoints/{port} body.
// Client-supplied targetUrl / upstreamUrl fields are ignored; the panel
// proxy always resolves the sandbox's own host (Changed in v0.7.0).
type mintEndpointReq struct {
	ExpiresInSeconds int `json:"expiresInSeconds,omitempty"`
}

// ErrPanelSecretMissing is returned when the gateway has no HMAC secret
// for signing and verifying /panel/{token}/ URLs (Changed in v0.7.0).
var ErrPanelSecretMissing = errors.New("CUBEPOD_PANEL_SECRET is required; refusing to start with an empty panel HMAC secret")

// SetPanelSecret sets the HMAC secret used to sign and verify panel tokens.
func (s *Server) SetPanelSecret(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panelSecret = []byte(secret)
}

func (s *Server) getPanelSecret() []byte {
	s.mu.RLock()
	sec := s.panelSecret
	s.mu.RUnlock()
	if len(sec) > 0 {
		return sec
	}
	if env := strings.TrimSpace(os.Getenv("CUBEPOD_PANEL_SECRET")); env != "" {
		return []byte(env)
	}
	return nil
}

// RequirePanelSecret resolves the HMAC secret from SetPanelSecret or
// CUBEPOD_PANEL_SECRET and returns ErrPanelSecretMissing if neither is set.
// On success the resolved secret is pinned on the server. There is no
// compiled-in default (Changed in v0.7.0).
func (s *Server) RequirePanelSecret() error {
	secret := strings.TrimSpace(string(s.getPanelSecret()))
	if secret == "" {
		return ErrPanelSecretMissing
	}
	s.SetPanelSecret(secret)
	return nil
}

// RegisterPort registers the upstream target URL for a sandbox port.
func (s *Server) RegisterPort(sandboxID string, port int, targetURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endpoints[sandboxID] == nil {
		s.endpoints[sandboxID] = make(map[int]string)
	}
	s.endpoints[sandboxID][port] = targetURL
}

// RegisterEndpoint is an alias for RegisterPort.
func (s *Server) RegisterEndpoint(sandboxID string, port int, targetURL string) {
	s.RegisterPort(sandboxID, port, targetURL)
}

// GetRegisteredPort returns the registered upstream target URL for a sandbox port, if any.
func (s *Server) GetRegisteredPort(sandboxID string, port int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ports, ok := s.endpoints[sandboxID]; ok {
		return ports[port]
	}
	return ""
}

// panelUpstreamURL returns the gateway-side upstream for a sandbox port.
// The sandbox's own host (PodIP or podd URL hostname) always wins so a
// client cannot steer /panel/{token}/ at an arbitrary address.
// RegisterPort is only used when no sandbox host is known, which covers
// in-process test injection (not the public mint API).
func (s *Server) panelUpstreamURL(rec *SandboxRecord, port int) string {
	if rec == nil {
		return ""
	}
	if host := s.resolveSandboxHost(rec); host != "" {
		return fmt.Sprintf("http://%s:%d", host, port)
	}
	return s.GetRegisteredPort(rec.SandboxID, port)
}

// handleCreateEndpoint is an alias for handleMintEndpointToken.
func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	s.handleMintEndpointToken(w, r)
}

func (s *Server) mintPanelToken(sandboxID string, port int, expiresInSeconds int) (string, time.Time) {
	if expiresInSeconds == 0 {
		expiresInSeconds = 3600
	}
	expTime := time.Now().UTC().Add(time.Duration(expiresInSeconds) * time.Second)
	expUnix := expTime.Unix()

	payload := fmt.Sprintf("%s:%d:%d", sandboxID, port, expUnix)
	mac := hmac.New(sha256.New, s.getPanelSecret())
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)

	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return payloadB64 + "." + sigB64, expTime
}

func (s *Server) resolveSandboxHost(rec *SandboxRecord) string {
	if rec == nil {
		return ""
	}
	if s.reconciler != nil {
		if boxStatus, ok := s.reconciler.GetBox(rec.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			if host, _, err := net.SplitHostPort(boxStatus.PodIP); err == nil && host != "" {
				return host
			}
			return boxStatus.PodIP
		}
	}
	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()
	if poddURL != "" {
		if u, err := url.Parse(poddURL); err == nil && u.Hostname() != "" && u.Hostname() != "10.244.0.42" {
			return u.Hostname()
		}
	}
	return ""
}

// handleMintEndpointToken mints an HMAC-SHA256 signed panel token for (sandboxID, port).
// POST /sandboxes/{sandboxID}/endpoints/{port}
func (s *Server) handleMintEndpointToken(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	portStr := r.PathValue("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port parameter", http.StatusBadRequest)
		return
	}

	if _, ok := s.resolveSandbox(r.Context(), sandboxID); !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	var req mintEndpointReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	// Do not honor client targetUrl / upstreamUrl and do not pin a
	// caller-supplied upstream into the in-memory endpoint map.
	if len(s.getPanelSecret()) == 0 {
		http.Error(w, "panel HMAC secret is not configured", http.StatusInternalServerError)
		return
	}
	token, expTime := s.mintPanelToken(sandboxID, port, req.ExpiresInSeconds)
	urlPath := fmt.Sprintf("/panel/%s/", token)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"url":       urlPath,
		"token":     token,
		"expiresAt": expTime.Format(time.RFC3339),
	})
}

// parseAndVerifyPanelToken verifies token HMAC-SHA256 signature and expiration time.
func (s *Server) parseAndVerifyPanelToken(token string) (*PanelTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		slog.Warn("panel token rejected: invalid format", "token", token)
		return nil, fmt.Errorf("invalid token format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		slog.Warn("panel token rejected: base64 payload decode error", "err", err)
		return nil, fmt.Errorf("invalid token payload encoding")
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		slog.Warn("panel token rejected: base64 signature decode error", "err", err)
		return nil, fmt.Errorf("invalid token signature encoding")
	}

	sec := s.getPanelSecret()
	if len(sec) == 0 {
		slog.Error("panel token rejected: panel HMAC secret is not configured")
		return nil, fmt.Errorf("panel HMAC secret is not configured")
	}
	mac := hmac.New(sha256.New, sec)
	mac.Write(payloadBytes)
	expectedSig := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expectedSig) {
		slog.Warn("panel token rejected: invalid signature", "token", token)
		return nil, fmt.Errorf("invalid signature")
	}

	payloadStr := string(payloadBytes)
	fields := strings.Split(payloadStr, ":")
	if len(fields) < 3 {
		slog.Warn("panel token rejected: malformed payload fields", "payload", payloadStr)
		return nil, fmt.Errorf("malformed token payload")
	}

	expUnix, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		slog.Warn("panel token rejected: invalid expiration", "err", err)
		return nil, fmt.Errorf("invalid expiration timestamp")
	}

	port, err := strconv.Atoi(fields[len(fields)-2])
	if err != nil {
		slog.Warn("panel token rejected: invalid port", "err", err)
		return nil, fmt.Errorf("invalid port")
	}

	sandboxID := strings.Join(fields[:len(fields)-2], ":")

	if time.Now().Unix() > expUnix {
		slog.Warn("panel token rejected: token expired", "expUnix", expUnix, "now", time.Now().Unix())
		return nil, fmt.Errorf("token expired")
	}

	return &PanelTokenClaims{
		SandboxID: sandboxID,
		Port:      port,
		ExpUnix:   expUnix,
	}, nil
}

// handlePanelProxy verifies token, auto-resumes the sandbox if Paused,
// and reverse-proxies HTTP/WebSocket traffic to the sandbox port upstream.
// /panel/{token}/{rest...} and /panel/{token}
func (s *Server) handlePanelProxy(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	// Canonicalize bare /panel/{token} to /panel/{token}/ so relative links work in iframes
	if r.PathValue("rest") == "" && !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, "/panel/"+token+"/", http.StatusMovedPermanently)
		return
	}

	claims, err := s.parseAndVerifyPanelToken(token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	rec, ok := s.resolveSandbox(r.Context(), claims.SandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	// Auto-resume sandbox if currently Paused
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		slog.Error("failed to auto-resume sandbox for panel proxy", "sandboxID", claims.SandboxID, "err", err)
		http.Error(w, "failed to auto-resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	targetURLStr := s.panelUpstreamURL(rec, claims.Port)
	if targetURLStr == "" {
		http.Error(w, fmt.Sprintf("no upstream registered for sandbox %s port %d", claims.SandboxID, claims.Port), http.StatusBadGateway)
		return
	}

	targetURL, err := url.Parse(targetURLStr)
	if err != nil {
		http.Error(w, "invalid target URL: "+err.Error(), http.StatusBadGateway)
		return
	}

	rest := r.PathValue("rest")

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host

			targetPath := strings.TrimSuffix(targetURL.Path, "/")
			cleanRest := strings.TrimPrefix(rest, "/")
			if cleanRest == "" {
				req.URL.Path = targetPath + "/"
			} else {
				req.URL.Path = targetPath + "/" + cleanRest
			}

			if targetURL.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = targetURL.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("panel reverse proxy connection failed", "target", targetURLStr, "err", err)
			http.Error(w, fmt.Sprintf("reverse proxy error: %v", err), http.StatusBadGateway)
		},
	}

	m := s.Metrics()
	m.PanelActiveSessions.Add(1)
	defer m.PanelActiveSessions.Add(-1)

	proxy.ServeHTTP(w, r)
}
