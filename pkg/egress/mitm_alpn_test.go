package egress_test

import (
	"bufio"
	"context"
	"crypto/tls"
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

	"golang.org/x/net/http2"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

const (
	alpnHost       = "api.github.com"
	alpnOtherHost  = "other.example.com"
	alpnCpodSecret = "ghp_alpn_real_secret"
	alpnCbxSecret  = "cbx_alpn_real_secret"
	alpnCbxRef     = "cbxref_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

// startALPNOrigin starts a TLS origin whose certificate covers hosts. When enableH2
// is true the origin advertises h2 via ALPN, otherwise it is HTTP/1.1-only.
func startALPNOrigin(t *testing.T, originCA *egress.CA, enableH2 bool, handler http.Handler, hosts ...string) string {
	t.Helper()
	certPEM, keyPEM, err := originCA.MintServerCert(hosts, 24*time.Hour)
	if err != nil {
		t.Fatalf("MintServerCert failed: %v", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair failed: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = enableH2
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

type alpnFixture struct {
	proxy       *egress.EgressProxy
	mitmCA      *egress.CA
	cpodref     string
	originHits  *atomic.Int32
	originProto *atomic.Value // last r.Proto seen by the origin
	listener    net.Listener
	proxySrv    *httptest.Server
}

// newALPNFixture wires an EgressProxy (CONNECT via proxySrv and transparent TLS via
// listener) in front of an origin served by handler.
func newALPNFixture(t *testing.T, originH2 bool, handler http.Handler) *alpnFixture {
	t.Helper()
	originCA, _ := egress.NewCA()
	mitmCA, _ := egress.NewCA()
	sandboxID := "sbx-alpn"

	fx := &alpnFixture{mitmCA: mitmCA, originHits: &atomic.Int32{}, originProto: &atomic.Value{}}
	originAddr := startALPNOrigin(t, originCA, originH2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.originHits.Add(1)
		fx.originProto.Store(r.Proto)
		handler.ServeHTTP(w, r)
	}), alpnHost, alpnOtherHost)

	vault, clientTLS, exchangeURL := startVaultServer(t, sandboxID)
	cpodref, err := vault.BindSecret(sandboxID, "GITHUB_TOKEN", alpnCpodSecret, []string{alpnHost}, []string{"Authorization"})
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}
	if err := vault.RestoreManagedSecret(alpnCbxRef, sandboxID, "tenant-a", "sec_alpn", "", alpnCbxSecret,
		[]string{alpnHost}, []string{"X-Api-Key", "X-Transform-Token"}); err != nil {
		t.Fatalf("RestoreManagedSecret failed: %v", err)
	}
	fx.cpodref = cpodref

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{alpnHost, alpnOtherHost},
		SecretBindings: []v1alpha1.SecretBinding{{
			EnvName:        "GITHUB_TOKEN",
			Placeholder:    cpodref,
			AllowedHosts:   []string{alpnHost},
			AllowedHeaders: []string{"Authorization"},
		}},
		HeaderTransforms: []v1alpha1.HeaderTransformRule{
			{Host: alpnHost, Headers: map[string]string{
				"X-Injected":        "transformed",
				"X-Transform-Token": "Token " + alpnCbxRef,
			}},
			{Host: alpnOtherHost, Headers: map[string]string{"X-Injected": "transformed"}},
		},
	}

	fx.proxy = egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:        sandboxID,
		Policy:           policy,
		ExchangeURL:      exchangeURL,
		ClientTLSConfig:  clientTLS,
		MITMCA:           mitmCA,
		UpstreamRootCAs:  originCA.CertPool(),
		UpstreamDialAddr: map[string]string{alpnHost: originAddr, alpnOtherHost: originAddr},
	})

	fx.proxySrv = httptest.NewServer(fx.proxy)
	t.Cleanup(fx.proxySrv.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = fx.proxy.ServeTransparentTLSListener(ln) }()
	fx.listener = ln
	return fx
}

// connectClient returns an HTTP client that reaches the proxy with CONNECT and offers h2 + http/1.1.
func (fx *alpnFixture) connectClient() *http.Client {
	proxyURL, _ := url.Parse(fx.proxySrv.URL)
	tr := &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{RootCAs: fx.mitmCA.CertPool()},
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// transparentClient returns an HTTP client dialing the transparent TLS listener directly.
func (fx *alpnFixture) transparentClient(sni string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", fx.listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{ServerName: sni, RootCAs: fx.mitmCA.CertPool()},
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func (fx *alpnFixture) metrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	fx.proxy.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+alpnCpodSecret ||
		r.Header.Get("X-Api-Key") != alpnCbxSecret ||
		r.Header.Get("X-Transform-Token") != "Token "+alpnCbxSecret ||
		r.Header.Get("X-Injected") != "transformed" {
		http.Error(w, fmt.Sprintf("bad headers: %v", r.Header), http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("X-Origin-Proto", r.Proto)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "ok:%s:%s:%s", r.Method, r.URL.Path, body)
}

func assertH2Swap(t *testing.T, fx *alpnFixture, client *http.Client, target string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer "+fx.cpodref)
	req.Header.Set("X-Api-Key", alpnCbxRef)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("h2 request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.ProtoMajor != 2 {
		t.Fatalf("expected client-side HTTP/2 response, got %s", resp.Proto)
	}
	if resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("expected negotiated ALPN h2, got %+v", resp.TLS)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok:POST:/v1/user:payload" {
		t.Fatalf("unexpected response (%d): %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Origin-Proto"); got != "HTTP/2.0" {
		t.Fatalf("expected upstream leg to use HTTP/2.0, origin saw %q", got)
	}
}

func TestMITMH2ALPNOverConnect(t *testing.T) {
	fx := newALPNFixture(t, true, http.HandlerFunc(echoHandler))
	assertH2Swap(t, fx, fx.connectClient(), "https://"+alpnHost+"/v1/user")

	m := fx.metrics(t)
	if !strings.Contains(m, `cubepod_egress_mitm_alpn_total{proto="h2"} 1`) {
		t.Fatalf("missing h2 ALPN metric in:\n%s", m)
	}
	if !strings.Contains(m, `cubepod_egress_secret_exchanges_total{result="ok"} 3`) {
		t.Fatalf("expected 3 ok secret exchanges (cpodref, cbxref header, cbxref transform) in:\n%s", m)
	}
}

func TestMITMH2ALPNOverTransparentTLS(t *testing.T) {
	fx := newALPNFixture(t, true, http.HandlerFunc(echoHandler))
	assertH2Swap(t, fx, fx.transparentClient(alpnHost), "https://"+alpnHost+"/v1/user")

	if m := fx.metrics(t); !strings.Contains(m, `cubepod_egress_mitm_alpn_total{proto="h2"} 1`) {
		t.Fatalf("missing h2 ALPN metric in:\n%s", m)
	}
}

func TestMITMH2ClientFallsBackToHTTP1Upstream(t *testing.T) {
	fx := newALPNFixture(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echoHandler(w, r)
	}))
	client := fx.transparentClient(alpnHost)
	req, _ := http.NewRequest(http.MethodPost, "https://"+alpnHost+"/v1/user", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer "+fx.cpodref)
	req.Header.Set("X-Api-Key", alpnCbxRef)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK || string(body) != "ok:POST:/v1/user:payload" {
		t.Fatalf("unexpected response %s (%d): %s", resp.Proto, resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Origin-Proto"); got != "HTTP/1.1" {
		t.Fatalf("expected h1 origin to be reached over HTTP/1.1, got %q", got)
	}
}

func TestMITMHTTP1OnlyClientNegotiatesHTTP11(t *testing.T) {
	fx := newALPNFixture(t, true, http.HandlerFunc(echoHandler))

	conn, err := tls.Dial("tcp", fx.listener.Addr().String(), &tls.Config{
		ServerName: alpnHost,
		RootCAs:    fx.mitmCA.CertPool(),
		NextProtos: []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("tls.Dial failed: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("expected http/1.1, got %q", got)
	}

	br := bufio.NewReader(conn)
	for i := 0; i < 2; i++ { // keep-alive loop stays intact
		raw := fmt.Sprintf("GET /v1/user HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Api-Key: %s\r\n\r\n",
			alpnHost, fx.cpodref, alpnCbxRef)
		if _, err := io.WriteString(conn, raw); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("ReadResponse failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.ProtoMajor != 1 || resp.StatusCode != http.StatusOK || string(body) != "ok:GET:/v1/user:" {
			t.Fatalf("unexpected response %s (%d): %s", resp.Proto, resp.StatusCode, body)
		}
	}

	if m := fx.metrics(t); !strings.Contains(m, `cubepod_egress_mitm_alpn_total{proto="http/1.1"} 1`) {
		t.Fatalf("missing http/1.1 ALPN metric in:\n%s", m)
	}
}

func TestMITMH2ConcurrentStreamsSingleConnection(t *testing.T) {
	const streams = 20

	var arrived atomic.Int32
	allArrived := make(chan struct{})
	var once sync.Once
	fx := newALPNFixture(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+alpnCpodSecret {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if arrived.Add(1) == streams {
			once.Do(func() { close(allArrived) })
		}
		// Hold every stream until all 20 are in flight to prove real multiplexing.
		select {
		case <-allArrived:
		case <-time.After(8 * time.Second):
			http.Error(w, "streams never ran concurrently", http.StatusGatewayTimeout)
			return
		}
		_, _ = fmt.Fprintf(w, "stream-%s", r.URL.Query().Get("i"))
	}))

	var dials atomic.Int32
	tr := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			dials.Add(1)
			return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fx.listener.Addr().String(), &tls.Config{
				ServerName: alpnHost,
				RootCAs:    fx.mitmCA.CertPool(),
				NextProtos: []string{"h2"},
			})
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/stream?i=%d", alpnHost, i), nil)
			req.Header.Set("Authorization", "Bearer "+fx.cpodref)
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK || string(body) != fmt.Sprintf("stream-%d", i) {
				errs <- fmt.Errorf("stream %d: %s %d %q", i, resp.Proto, resp.StatusCode, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("expected 20 streams to share 1 TLS connection, dialed %d", got)
	}
	if got := fx.originHits.Load(); got != streams {
		t.Fatalf("expected %d origin hits, got %d", streams, got)
	}
	if m := fx.metrics(t); !strings.Contains(m, `cubepod_egress_mitm_alpn_total{proto="h2"} 1`) {
		t.Fatalf("expected exactly one h2 MITM connection in:\n%s", m)
	}
}

func TestMITMH2DomainFrontingAuthorityMismatchRejected(t *testing.T) {
	fx := newALPNFixture(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("origin must not be reached on :authority != SNI")
	}))

	for name, client := range map[string]*http.Client{
		"transparent": fx.transparentClient(alpnHost),
		"connect":     fx.connectClient(),
	} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+alpnHost+"/", nil)
		req.Host = "evil.example.com" // sent as :authority over h2
		req.Header.Set("Authorization", "Bearer "+fx.cpodref)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: request failed: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("%s: expected h2 421, got %s %d", name, resp.Proto, resp.StatusCode)
		}
	}

	if fx.originHits.Load() != 0 {
		t.Fatalf("origin was reached %d times", fx.originHits.Load())
	}
	m := fx.metrics(t)
	if strings.Contains(m, "cubepod_egress_secret_exchanges_total{") {
		t.Fatalf("no secret exchange must occur before :authority validation:\n%s", m)
	}
	if !strings.Contains(m, `cubepod_egress_blocked_hosts_total{host="evil.example.com"} 2`) {
		t.Fatalf("missing blocked host metric in:\n%s", m)
	}
}

func TestMITMH2UnauthorizedPlaceholderRejected(t *testing.T) {
	fx := newALPNFixture(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("origin must not be reached when placeholder exchange is denied")
	}))

	cases := map[string]struct {
		host   string
		header string
	}{
		"header not in allowedHeaders": {host: alpnHost, header: "X-Not-Allowed"},
		"host not in allowedHosts":     {host: alpnOtherHost, header: "Authorization"},
	}
	for name, tc := range cases {
		client := fx.transparentClient(tc.host)
		req, _ := http.NewRequest(http.MethodGet, "https://"+tc.host+"/", nil)
		req.Header.Set(tc.header, "Bearer "+fx.cpodref)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: request failed: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: expected h2 403, got %s %d: %s", name, resp.Proto, resp.StatusCode, body)
		}
		if strings.Contains(string(body), alpnCpodSecret) {
			t.Fatalf("%s: response leaked real secret: %s", name, body)
		}
	}
	if fx.originHits.Load() != 0 {
		t.Fatalf("origin was reached %d times", fx.originHits.Load())
	}
	if m := fx.metrics(t); !strings.Contains(m, `cubepod_egress_secret_exchanges_total{result="denied"} 2`) {
		t.Fatalf("missing denied exchange metric in:\n%s", m)
	}
}

func TestMITMH2StreamingResponseFlushesIncrementally(t *testing.T) {
	for _, mode := range []string{"transparent", "connect"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			fx := newALPNFixture(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: one\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "data: two\n\n")
			}))
			client := fx.transparentClient(alpnHost)
			if mode == "connect" {
				client = fx.connectClient()
			}

			req, _ := http.NewRequest(http.MethodGet, "https://"+alpnHost+"/events", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != 2 {
				t.Fatalf("expected HTTP/2, got %s", resp.Proto)
			}

			first := make(chan string, 1)
			go func() {
				buf := make([]byte, len("data: one\n\n"))
				_, _ = io.ReadFull(resp.Body, buf)
				first <- string(buf)
			}()
			select {
			case got := <-first:
				if got != "data: one\n\n" {
					t.Fatalf("unexpected first event %q", got)
				}
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatalf("first chunk was buffered until upstream closed the body")
			}

			close(release)
			rest, _ := io.ReadAll(resp.Body)
			if string(rest) != "data: two\n\n" {
				t.Fatalf("unexpected remainder %q", rest)
			}
		})
	}
}
