package main

import (
	"context"
	"crypto/tls"
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

func main() {
	var (
		addr         = flag.String("addr", ":18080", "HTTP listen address for CubePod Egress sidecar")
		sandboxID    = flag.String("sandbox-id", "", "Sandbox ID to authenticate as")
		exchangeURL  = flag.String("exchange-url", "", "mTLS exchange URL on the Gateway")
		certFile     = flag.String("cert-file", "", "Path to client certificate PEM")
		keyFile      = flag.String("key-file", "", "Path to client private key PEM")
		caFile       = flag.String("ca-file", "", "Path to root CA certificate PEM")
		allowedHosts = flag.String("allowed-hosts", "", "Comma-separated list of allowed host patterns")
		deniedHosts  = flag.String("denied-hosts", "", "Comma-separated list of denied host patterns")
		defaultAllow = flag.Bool("default-allow", false, "Whether default egress policy action is Allow")
	)
	flag.Parse()

	// Environment variable overrides
	if *sandboxID == "" {
		*sandboxID = os.Getenv("CUBEPOD_SANDBOX_ID")
	}
	if *exchangeURL == "" {
		*exchangeURL = os.Getenv("CUBEPOD_EXCHANGE_URL")
	}
	if *certFile == "" {
		*certFile = os.Getenv("CUBEPOD_CERT_FILE")
	}
	if *keyFile == "" {
		*keyFile = os.Getenv("CUBEPOD_KEY_FILE")
	}
	if *caFile == "" {
		*caFile = os.Getenv("CUBEPOD_CA_FILE")
	}

	slog.Info("starting cubepod-egress sidecar",
		"version", version.Version,
		"addr", *addr,
		"sandboxID", *sandboxID,
		"exchangeURL", *exchangeURL,
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
	}

	policy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
	}
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

	proxy := egress.NewEgressProxy(egress.EgressProxyConfig{
		SandboxID:       *sandboxID,
		Policy:          policy,
		ExchangeURL:     *exchangeURL,
		ClientTLSConfig: tlsConfig,
	})

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: proxy,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("egress proxy failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down cubepod-egress")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
