package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func envOrDefault(flagVal, envKey, fallback string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return fallback
}

func envBool(flagVal bool, envKey string) bool {
	if flagVal {
		return true
	}
	v := strings.TrimSpace(strings.ToLower(os.Getenv(envKey)))
	return v == "1" || v == "true" || v == "yes"
}

func loadPolicyFile(path string) (v1alpha1.NetworkPolicySpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return v1alpha1.NetworkPolicySpec{}, err
	}
	var policy v1alpha1.NetworkPolicySpec
	if err := json.Unmarshal(data, &policy); err != nil {
		return v1alpha1.NetworkPolicySpec{}, err
	}
	return policy, nil
}

func main() {
	var (
		addr                = flag.String("addr", ":18080", "HTTP/CONNECT listen address for CubePod Egress sidecar")
		transparentHTTPAddr = flag.String("transparent-http-addr", "", "Transparent HTTP listen address (e.g. :18081)")
		transparentTLSAddr  = flag.String("transparent-tls-addr", "", "Transparent TLS SNI listen address (e.g. :18443)")
		dnsAddr             = flag.String("dns-addr", "", "DNS allowlist proxy listen address (e.g. :18053)")
		upstreamDNS         = flag.String("upstream-dns", "", "Upstream DNS resolver address (e.g. 10.96.0.10:53)")
		metricsAddr         = flag.String("metrics-addr", ":19090", "Prometheus /metrics listen address (empty to disable)")
		sandboxID           = flag.String("sandbox-id", "", "Sandbox ID to authenticate as")
		exchangeURL         = flag.String("exchange-url", "", "mTLS exchange URL on the Gateway")
		certFile            = flag.String("cert-file", "", "Path to client certificate PEM")
		keyFile             = flag.String("key-file", "", "Path to client private key PEM")
		caFile              = flag.String("ca-file", "", "Path to root CA certificate PEM for Vault mTLS")
		mitmCACert          = flag.String("mitm-ca-cert", "", "Path to MITM CA certificate PEM")
		mitmCAKey           = flag.String("mitm-ca-key", "", "Path to MITM CA private key PEM")
		enableMITM          = flag.Bool("enable-mitm", false, "Enable selective HTTPS MITM for SecretBindings")
		policyFile          = flag.String("policy-file", "", "Path to JSON NetworkPolicySpec file (reloaded on SIGHUP)")
		upstreamProxy       = flag.String("upstream-proxy", "", "Optional upstream HTTP CONNECT proxy URL")
		allowedHosts        = flag.String("allowed-hosts", "", "Comma-separated list of allowed host patterns")
		deniedHosts         = flag.String("denied-hosts", "", "Comma-separated list of denied host patterns")
		defaultAllow        = flag.Bool("default-allow", false, "Whether default egress policy action is Allow")
	)
	flag.Parse()

	// Environment variable overrides
	*sandboxID = envOrDefault(*sandboxID, "CUBEPOD_SANDBOX_ID", "")
	*exchangeURL = envOrDefault(*exchangeURL, "CUBEPOD_EXCHANGE_URL", "")
	*certFile = envOrDefault(*certFile, "CUBEPOD_CERT_FILE", "")
	*keyFile = envOrDefault(*keyFile, "CUBEPOD_KEY_FILE", "")
	*caFile = envOrDefault(*caFile, "CUBEPOD_CA_FILE", "")
	*mitmCACert = envOrDefault(*mitmCACert, "CUBEPOD_MITM_CA_CERT", "")
	*mitmCAKey = envOrDefault(*mitmCAKey, "CUBEPOD_MITM_CA_KEY", "")
	*policyFile = envOrDefault(*policyFile, "CUBEPOD_POLICY_FILE", "")
	*upstreamProxy = envOrDefault(*upstreamProxy, "CUBEPOD_UPSTREAM_PROXY", "")
	*upstreamDNS = envOrDefault(*upstreamDNS, "CUBEPOD_UPSTREAM_DNS", "")
	*transparentHTTPAddr = envOrDefault(*transparentHTTPAddr, "CUBEPOD_TRANSPARENT_HTTP_ADDR", "")
	*transparentTLSAddr = envOrDefault(*transparentTLSAddr, "CUBEPOD_TRANSPARENT_TLS_ADDR", "")
	*dnsAddr = envOrDefault(*dnsAddr, "CUBEPOD_DNS_ADDR", "")
	if v := os.Getenv("CUBEPOD_METRICS_ADDR"); v != "" && *metricsAddr == ":19090" {
		*metricsAddr = v
	}
	mitmEnabled := envBool(*enableMITM, "CUBEPOD_ENABLE_MITM")
	transparentMode := envBool(false, "CUBEPOD_TRANSPARENT")
	if transparentMode {
		if *transparentHTTPAddr == "" {
			*transparentHTTPAddr = ":18081"
		}
		if *transparentTLSAddr == "" {
			*transparentTLSAddr = ":18443"
		}
		if *dnsAddr == "" {
			*dnsAddr = ":18053"
		}
	}

	slog.Info("starting cubepod-egress sidecar",
		"version", version.Version,
		"addr", *addr,
		"transparentHTTPAddr", *transparentHTTPAddr,
		"transparentTLSAddr", *transparentTLSAddr,
		"dnsAddr", *dnsAddr,
		"metricsAddr", *metricsAddr,
		"sandboxID", *sandboxID,
		"exchangeURL", *exchangeURL,
		"mitmEnabled", mitmEnabled,
	)

	var tlsConfig *tls.Config
	if *certFile != "" && *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			slog.Error("failed to load client keypair", "err", err)
			os.Exit(1)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		if *caFile != "" {
			caPEM, err := os.ReadFile(*caFile)
			if err != nil {
				slog.Error("failed to load Vault CA file", "err", err)
				os.Exit(1)
			}
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caPEM) {
				tlsConfig.RootCAs = pool
			}
		}
	}

	var mitmCA *egress.CA
	if mitmEnabled {
		var err error
		if *mitmCACert != "" && *mitmCAKey != "" {
			mitmCA, err = egress.LoadCAFromFiles(*mitmCACert, *mitmCAKey)
		} else {
			mitmCA, err = egress.NewCA()
		}
		if err != nil {
			slog.Error("failed to initialize MITM CA", "err", err)
			os.Exit(1)
		}
	}

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
	}
	if *policyFile != "" {
		loaded, err := loadPolicyFile(*policyFile)
		if err != nil {
			slog.Error("failed to load policy file", "path", *policyFile, "err", err)
			os.Exit(1)
		}
		policy = loaded
	} else {
		if *defaultAllow {
			policy.DefaultAction = v1alpha1.NetworkDefaultAllow
		}
		if *allowedHosts != "" {
			for _, h := range strings.Split(*allowedHosts, ",") {
				if trimmed := strings.TrimSpace(h); trimmed != "" {
					policy.AllowHosts = append(policy.AllowHosts, trimmed)
				}
			}
		}
		if *deniedHosts != "" {
			for _, h := range strings.Split(*deniedHosts, ",") {
				if trimmed := strings.TrimSpace(h); trimmed != "" {
					policy.DenyHosts = append(policy.DenyHosts, trimmed)
				}
			}
		}
	}

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:       *sandboxID,
		Policy:          policy,
		ExchangeURL:     *exchangeURL,
		ClientTLSConfig: tlsConfig,
		MITMCA:          mitmCA,
		UpstreamProxy:   *upstreamProxy,
	})

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: proxy,
	}

	var servers []*http.Server
	servers = append(servers, httpSrv)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP policy reload
	if *policyFile != "" {
		sighupCh := make(chan os.Signal, 1)
		signal.Notify(sighupCh, syscall.SIGHUP)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-sighupCh:
					reloaded, err := loadPolicyFile(*policyFile)
					if err != nil {
						slog.Error("failed to reload policy file on SIGHUP", "path", *policyFile, "err", err)
						continue
					}
					proxy.SetPolicy(reloaded)
					slog.Info("reloaded egress policy from file", "path", *policyFile)
				}
			}
		}()
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("egress proxy failed", "err", err)
			os.Exit(1)
		}
	}()

	if *transparentHTTPAddr != "" {
		tHTTPSrv := &http.Server{
			Addr:    *transparentHTTPAddr,
			Handler: proxy.TransparentHTTPHandler(),
		}
		servers = append(servers, tHTTPSrv)
		go func() {
			if err := tHTTPSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("transparent HTTP proxy failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	if *transparentTLSAddr != "" {
		go func() {
			if err := proxy.ListenAndServeTransparentTLS(*transparentTLSAddr); err != nil {
				slog.Error("transparent TLS proxy failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	if *dnsAddr != "" {
		dnsSrv := egress.NewDNSServer(egress.DNSConfig{
			UpstreamAddr: *upstreamDNS,
			GetPolicy:    proxy.GetPolicy,
			Metrics:      proxy.Metrics(),
		})
		go func() {
			if err := dnsSrv.ListenAndServe(*dnsAddr); err != nil {
				slog.Error("dns proxy failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", proxy.MetricsHandler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		metricsSrv := &http.Server{
			Addr:    *metricsAddr,
			Handler: mux,
		}
		servers = append(servers, metricsSrv)
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("shutting down cubepod-egress")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
}
