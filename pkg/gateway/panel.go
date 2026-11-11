// Package gateway implements the CubePod Unified API Gateway (E2B-compatible superset)
// and the authenticated HTTP/WebSocket/WebRTC panel reverse proxy.
package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
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

type mintEndpointReq struct {
	ExpiresInSeconds int    `json:"expiresInSeconds,omitempty"`
	TargetURL        string `json:"targetUrl,omitempty"`
	UpstreamURL      string `json:"upstreamUrl,omitempty"`
}

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
	if env := os.Getenv("CUBEPOD_PANEL_SECRET"); env != "" {
		return []byte(env)
	}
	return []byte("cubepod-default-hmac-panel-secret-key-32b")
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

// handleCreateEndpoint is an alias for handleMintEndpointToken.
func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	s.handleMintEndpointToken(w, r)
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

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	var req mintEndpointReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	targetURL := req.TargetURL
	if targetURL == "" {
		targetURL = req.UpstreamURL
	}
	if targetURL == "" && rec != nil && rec.PoddURL != "" {
		if boxStatus, ok := s.reconciler.GetBox(sandboxID); ok && boxStatus.PodIP != "" {
			targetURL = fmt.Sprintf("http://%s:%d", boxStatus.PodIP, port)
		} else if u, err := url.Parse(rec.PoddURL); err == nil && u.Hostname() != "" {
			targetURL = fmt.Sprintf("http://%s:%d", u.Hostname(), port)
		}
	}
	if targetURL != "" {
		s.RegisterPort(sandboxID, port, targetURL)
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn == 0 {
		expiresIn = 3600
	}
	expTime := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	expUnix := expTime.Unix()

	payload := fmt.Sprintf("%s:%d:%d", sandboxID, port, expUnix)
	mac := hmac.New(sha256.New, s.getPanelSecret())
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)

	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	token := payloadB64 + "." + sigB64
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

	mac := hmac.New(sha256.New, s.getPanelSecret())
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

	s.mu.RLock()
	rec, ok := s.sandboxes[claims.SandboxID]
	s.mu.RUnlock()
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

	targetURLStr := s.GetRegisteredPort(claims.SandboxID, claims.Port)
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

	proxy.ServeHTTP(w, r)
}
