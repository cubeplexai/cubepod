package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/gateway"
	"github.com/cubeplexai/cubepod/pkg/runtime"
	"github.com/cubeplexai/cubepod/pkg/version"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBoolOrDefault(key string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func resolveE2BTemplateAPIFlag(explicit bool, flagVal bool, gates map[string]bool) bool {
	if explicit {
		if gateVal, ok := gates[gateway.FeatureGateE2BTemplateAPI]; ok {
			return flagVal && gateVal
		}
		return flagVal
	}
	if gateVal, ok := gates[gateway.FeatureGateE2BTemplateAPI]; ok {
		return gateVal
	}
	return flagVal
}

func defaultEgressImage() string {
	if env := strings.TrimSpace(os.Getenv("CUBEPOD_EGRESS_IMAGE")); env != "" {
		return env
	}
	return "ghcr.io/cubeplexai/cubepod-egress:v0.7.1"
}

func main() {
	var (
		addr                  = flag.String("addr", ":8080", "HTTP listen address for CubePod Gateway")
		namespace             = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		kubeAPI               = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken             = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass          = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode            = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass          = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage             = flag.String("podd-image", "ghcr.io/cubeplexai/cubepod-podd:v0.7.1", "Image for podd-installer initContainer")
		sandboxImage          = flag.String("sandbox-image", "ubuntu:24.04", "Default sandbox image")
		egressImage           = flag.String("egress-image", defaultEgressImage(), "Default cubepod-egress sidecar image when template egress.image is empty")
		livePodRouting        = flag.Bool("live-pod-routing", true, "Route commands and files to live in-cluster Pod IP")
		e2bTemplateAPI        = flag.Bool("e2b-template-api", true, "Enable E2B template API feature gate")
		featureGate           = flag.String("feature-gate", "", "Comma-separated feature gates (e.g. E2BTemplateAPI=true)")
		templateBuild         = flag.Bool("template-build", false, "Enable template image build Beta pipeline")
		controllerURL         = flag.String("controller-url", "", "Base URL of cubepod-controller for forwarding template build operations")
		contextStoreBackend   = flag.String("context-store-backend", envOrDefault("CUBEPOD_CONTEXT_STORE_BACKEND", "local"), "Template build context store backend (local or s3)")
		s3Endpoint            = flag.String("s3-endpoint", os.Getenv("CUBEPOD_S3_ENDPOINT"), "S3-compatible endpoint URL for context store backend=s3")
		s3Bucket              = flag.String("s3-bucket", os.Getenv("CUBEPOD_S3_BUCKET"), "S3 bucket name for context store backend=s3")
		s3Region              = flag.String("s3-region", envOrDefault("CUBEPOD_S3_REGION", "us-east-1"), "S3 region for SigV4 signing")
		s3Prefix              = flag.String("s3-prefix", os.Getenv("CUBEPOD_S3_PREFIX"), "Optional key prefix inside the S3 context store bucket")
		s3ForcePathStyle      = flag.Bool("s3-force-path-style", envBoolOrDefault("CUBEPOD_S3_FORCE_PATH_STYLE", false), "Use path-style S3 URLs (required for MinIO/SeaweedFS/R2)")
		stateDir              = flag.String("state-dir", "", "State directory for local template catalog, contexts, and logs")
		templateProbeTimeout  = flag.Duration("template-probe-timeout", 120*time.Second, "Timeout for template readyCmd probe")
		templateRegistry      = flag.String("template-registry", "", "Target OCI registry URL for template builds (local single-process mode)")
		templateRepoPrefix    = flag.String("template-repo-prefix", "cubepod-templates", "Repository prefix under target template registry")
		templatePullSecret    = flag.String("template-pull-secret", "", "Kubernetes imagePullSecret attached to sandbox pods")
		imagePullSecrets      = flag.String("image-pull-secrets", os.Getenv("CUBEPOD_IMAGE_PULL_SECRETS"), "Comma-separated Kubernetes imagePullSecrets attached to sandbox Pods")
		egressCACert          = flag.String("egress-ca-cert", os.Getenv("CUBEPOD_EGRESS_CA_CERT"), "Path to egress mTLS root CA certificate PEM")
		egressCAKey           = flag.String("egress-ca-key", os.Getenv("CUBEPOD_EGRESS_CA_KEY"), "Path to egress mTLS root CA private key PEM")
		egressUpstreamCAFile  = flag.String("egress-upstream-ca-file", os.Getenv("CUBEPOD_EGRESS_UPSTREAM_CA_FILE"), "Optional path to upstream CA PEM mounted into egress sidecars")
		egressMTLSAddr        = flag.String("egress-mtls-addr", os.Getenv("CUBEPOD_EGRESS_MTLS_ADDR"), "Optional mTLS listen address for /internal/v1/egress/exchange")
		egressTLSCert         = flag.String("egress-tls-cert", os.Getenv("CUBEPOD_EGRESS_TLS_CERT"), "Optional TLS server certificate PEM path for --egress-mtls-addr")
		egressTLSKey          = flag.String("egress-tls-key", os.Getenv("CUBEPOD_EGRESS_TLS_KEY"), "Optional TLS server private key PEM path for --egress-mtls-addr")
		metricsSampleInterval = flag.Duration("metrics-sample-interval", envDuration("CUBEPOD_METRICS_SAMPLE_INTERVAL", 10*time.Second), "Background sandbox metrics sampling interval (0 disables)")
	)
	flag.Parse()

	gates := gateway.ParseFeatureGates(*featureGate)
	e2bFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "e2b-template-api" {
			e2bFlagSet = true
		}
	})
	e2bAPIEnabled := resolveE2BTemplateAPIFlag(e2bFlagSet, *e2bTemplateAPI, gates)

	effectiveStateDir := *stateDir
	if effectiveStateDir == "" {
		effectiveStateDir = os.Getenv("CUBEPOD_STATE")
	}

	slog.Info("starting cubepod-gateway (E2B-Compatible API & Panel Reverse Proxy)",
		"version", version.Version,
		"addr", *addr,
		"namespace", *namespace,
		"volumeMode", *volumeMode,
		"storageClass", *storageClass,
		"runtimeClass", *runtimeClass,
		"poddImage", *poddImage,
		"sandboxImage", *sandboxImage,
		"egressImage", *egressImage,
		"livePodRouting", *livePodRouting,
		"e2bTemplateAPI", e2bAPIEnabled,
		"templateBuild", *templateBuild,
		"controllerURL", *controllerURL,
		"contextStoreBackend", *contextStoreBackend,
		"s3Bucket", *s3Bucket,
		"s3Region", *s3Region,
		"stateDir", effectiveStateDir,
		"templateProbeTimeout", *templateProbeTimeout,
	)

	var kubeClient controller.KubeClient
	hasKube := false
	if *kubeAPI != "" {
		kubeClient = controller.NewRESTKubeClient(*kubeAPI, *namespace, *kubeToken, nil, controller.WithWaitForRunning(true))
		hasKube = true
	} else if client, err := controller.NewRESTKubeClientFromEnv(controller.WithKubeClientNamespace(*namespace), controller.WithWaitForRunning(true)); err == nil {
		kubeClient = client
		hasKube = true
	} else {
		kubeClient = controller.NewMemoryKubeClient()
	}

	opts := []controller.ReconcilerOption{
		controller.WithKubeClient(kubeClient),
		controller.WithNamespace(*namespace),
		controller.WithVolumeMode(*volumeMode),
		controller.WithDefaultStorageClass(*storageClass),
		controller.WithDefaultRuntimeClass(*runtimeClass),
		controller.WithPoddInjectorImage(*poddImage),
		controller.WithDefaultSandboxImage(*sandboxImage),
		controller.WithDefaultEgressImage(*egressImage),
		controller.WithE2BTemplateAPI(e2bAPIEnabled),
		controller.WithTemplateBuildEnabled(*templateBuild),
		controller.WithTemplateProbeTimeout(*templateProbeTimeout),
		controller.WithTemplateRegistry(*templateRegistry),
		controller.WithTemplateRepoPrefix(*templateRepoPrefix),
		controller.WithTemplatePullSecret(*templatePullSecret),
		controller.WithDefaultImagePullSecrets(*imagePullSecrets),
	}

	var contextDir string
	if effectiveStateDir != "" {
		contextDir = filepath.Join(effectiveStateDir, "template-context")
		opts = append(opts, controller.WithStateDir(effectiveStateDir))
	}
	ctxStore, err := controller.NewContextStoreFromConfig(*contextStoreBackend, effectiveStateDir, controller.S3ContextStoreConfig{
		Endpoint:       *s3Endpoint,
		Bucket:         *s3Bucket,
		Region:         *s3Region,
		Prefix:         *s3Prefix,
		ForcePathStyle: *s3ForcePathStyle,
	})
	if err != nil {
		slog.Error("failed to configure template context store", "backend", *contextStoreBackend, "err", err)
		os.Exit(1)
	}
	opts = append(opts, controller.WithContextStore(ctxStore))
	if *controllerURL == "" && !hasKube && *templateBuild {
		opts = append(opts, controller.WithImageBuilder(controller.NewLocalDockerBuilder(contextDir)))
	}
	if *egressCACert != "" || *egressCAKey != "" {
		if *egressCACert == "" || *egressCAKey == "" {
			slog.Error("both egress-ca-cert and egress-ca-key must be set together")
			os.Exit(1)
		}
		ca, err := egress.LoadCAFromFiles(*egressCACert, *egressCAKey)
		if err != nil {
			slog.Error("failed to load egress CA", "cert", *egressCACert, "err", err)
			os.Exit(1)
		}
		opts = append(opts, controller.WithEgressCA(ca))
	}
	if *egressUpstreamCAFile != "" {
		pemBytes, err := os.ReadFile(*egressUpstreamCAFile)
		if err != nil {
			slog.Error("failed to load egress upstream CA file", "path", *egressUpstreamCAFile, "err", err)
			os.Exit(1)
		}
		opts = append(opts, controller.WithEgressUpstreamCA(pemBytes))
	}

	reconciler := controller.NewReconciler(opts...)
	if n, err := reconciler.SyncFromKube(context.Background()); err != nil {
		slog.Warn("gateway initial SyncFromKube failed", "err", err)
	} else {
		slog.Info("gateway initial SyncFromKube completed", "recoveredBoxes", n)
	}
	if e2bAPIEnabled {
		if err := reconciler.EnsureSeededTemplates(context.Background()); err != nil {
			slog.Warn("gateway EnsureSeededTemplates failed", "err", err)
		}
	}
	if *controllerURL == "" && !hasKube && *templateBuild {
		reconciler.RecoverInFlightBuilds(context.Background())
	}

	gw := gateway.NewServer(reconciler)
	if *egressCACert != "" || hasKube || strings.TrimSpace(*egressMTLSAddr) != "" {
		gw.SetStrictEgressMTLS(true)
	}
	gateway.ApplyTemplateConfig(gw, e2bAPIEnabled, *templateBuild, *controllerURL, *templateProbeTimeout)
	if *livePodRouting {
		gw.SetLivePodRouting(true)
	}

	// Persistent volume store. Routes are always registered; this opens the
	// durable directory so management and content APIs work.
	// CUBEPOD_VOLUMES_DIR wins; otherwise stateDir/volumes is used
	// when a state directory is set. Docker named volumes stay off unless
	// CUBEPOD_VOLUMES_DOCKER is explicitly enabled (compose/runc tests).
	volEnabled, vcfg, volErr := resolveVolumeConfig(os.Getenv, effectiveStateDir)
	if volErr != nil {
		slog.Error("invalid volume store configuration", "err", volErr)
		os.Exit(1)
	}
	volDir := vcfg.Dir
	if volEnabled {
		if err := gw.ConfigureVolumes(vcfg); err != nil {
			slog.Error("failed to configure volume store", "dir", volDir, "err", err)
			os.Exit(1)
		}
		kmsKeyID := ""
		if vcfg.KMS != nil {
			kmsKeyID = vcfg.KMS.KeyID()
		}
		slog.Info("volume store enabled",
			"dir", volDir,
			"dockerVolumes", vcfg.DockerVolumes,
			"maxTenantBytes", vcfg.MaxBytesPerTenant,
			"kmsEnabled", vcfg.KMS != nil,
			"kmsKeyID", kmsKeyID,
		)
	}
	gw.ApplyFeatureGates(gates)
	gw.SetFeatureGate(gateway.FeatureGateE2BTemplateAPI, e2bAPIEnabled)

	secretsEnabled, secretsK8s, scfg, err := resolveSecretsConfig(
		os.Getenv("CUBEPOD_SECRETS_BACKEND"),
		os.Getenv("CUBEPOD_SECRETS_DIR"),
		volDir,
		os.Getenv("CUBEPOD_SECRETS_KEY"),
		os.Getenv("CUBEPOD_MAX_SECRETS_PER_TENANT"),
		hasKube,
		reconciler.KubeClient(),
		*namespace,
	)
	if err != nil {
		slog.Error("invalid E2B secret store configuration", "err", err)
		os.Exit(1)
	}
	if secretsEnabled {
		if err := gw.ConfigureE2BSecrets(scfg); err != nil {
			slog.Error("failed to configure E2B secret store", "backend", scfg.Backend, "dir", scfg.Dir, "err", err)
			os.Exit(1)
		}
		slog.Info("E2B secret store enabled", "backend", scfg.Backend, "dir", scfg.Dir, "namespace", scfg.Namespace)
	}

	eventsEnabled, eventsK8s, ecfg, err := resolveEventsConfig(
		os.Getenv("CUBEPOD_EVENTS_BACKEND"),
		os.Getenv("CUBEPOD_EVENTS_DIR"),
		effectiveStateDir,
		volDir,
		os.Getenv("CUBEPOD_EVENTS_KEY"),
		os.Getenv("CUBEPOD_WEBHOOK_ALLOW_PRIVATE"),
		hasKube,
		reconciler.KubeClient(),
		*namespace,
	)
	if err != nil {
		slog.Error("invalid E2B events and webhooks configuration", "err", err)
		os.Exit(1)
	}
	if eventsEnabled {
		if err := gw.ConfigureEvents(ecfg); err != nil {
			slog.Error("failed to configure E2B events and webhooks", "backend", ecfg.Backend, "dir", ecfg.Dir, "err", err)
			os.Exit(1)
		}
		defer gw.CloseEvents()
		slog.Info("E2B events and webhooks enabled", "backend", ecfg.Backend, "dir", ecfg.Dir, "namespace", ecfg.Namespace, "allowPrivateIPs", ecfg.AllowPrivateIPs)
	}

	if strings.EqualFold(strings.TrimSpace(os.Getenv("CUBEPOD_RUNTIME")), "docker") {
		cfg := runtime.DockerConfig{
			DockerHost:          os.Getenv("DOCKER_HOST"),
			BinDir:              os.Getenv("CUBEPOD_DOCKER_BIN_DIR"),
			BaseDir:             os.Getenv("CUBEPOD_DOCKER_BASE_DIR"),
			HostBaseDir:         os.Getenv("CUBEPOD_DOCKER_HOST_BASE_DIR"),
			Network:             os.Getenv("CUBEPOD_DOCKER_NETWORK"),
			DefaultImage:        os.Getenv("CUBEPOD_DOCKER_IMAGE"),
			ContainerNamePrefix: os.Getenv("CUBEPOD_DOCKER_CONTAINER_PREFIX"),
		}
		// Test-only overrides for local compose harnesses; production deployments must
		// keep runtime binary injection (Invariant 2) and dual-volume binding (Invariant 1) enabled.
		if v := strings.ToLower(strings.TrimSpace(os.Getenv("CUBEPOD_DOCKER_INJECT_BIN"))); v == "0" || v == "false" || v == "no" {
			f := false
			cfg.InjectPoddBin = &f
			slog.Warn("test-only override: CUBEPOD_DOCKER_INJECT_BIN=0 disables runtime binary injection")
		}
		if v := strings.ToLower(strings.TrimSpace(os.Getenv("CUBEPOD_DOCKER_BIND_STORAGE"))); v == "0" || v == "false" || v == "no" {
			f := false
			cfg.BindSandboxStorage = &f
			slog.Warn("test-only override: CUBEPOD_DOCKER_BIND_STORAGE=0 disables workspace/rootfs bind storage")
		}
		gw.SetRuntime(runtime.NewDockerBackend(cfg))
		slog.Info("docker runtime enabled", "network", cfg.Network, "image", cfg.DefaultImage)
	}

	var mtlsSrv *http.Server
	if strings.TrimSpace(*egressMTLSAddr) != "" {
		var buildErr error
		mtlsSrv, buildErr = buildEgressMTLSServer(*egressMTLSAddr, *egressTLSCert, *egressTLSKey, *namespace, reconciler.EgressCA(), gw.EgressExchangeHandler())
		if buildErr != nil {
			slog.Error("failed to configure egress mTLS listener", "addr", *egressMTLSAddr, "err", buildErr)
			os.Exit(1)
		}
	}

	if err := gw.RequirePanelSecret(); err != nil {
		slog.Error("panel HMAC secret is not configured", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: gw.Handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *metricsSampleInterval > 0 {
		gw.StartMetricsSampler(ctx, *metricsSampleInterval)
		defer gw.StopMetricsSampler()
		slog.Info("started background sandbox metrics sampler", "interval", metricsSampleInterval.String())
	}

	if secretsK8s {
		gw.StartSecretsSync(ctx, 5*time.Second)
		slog.Info("started background E2B Kubernetes secret sync loop", "interval", "5s")
	}
	if eventsK8s {
		gw.StartEventsSync(ctx, 5*time.Second)
		slog.Info("started background SandboxWebhook Kubernetes sync loop", "interval", "5s")
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("gateway HTTP server failed", "err", err)
			os.Exit(1)
		}
	}()

	if mtlsSrv != nil {
		go func() {
			slog.Info("starting egress mTLS exchange listener", "addr", mtlsSrv.Addr)
			if err := mtlsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				slog.Error("egress mTLS server failed", "err", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("shutting down cubepod-gateway")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if mtlsSrv != nil {
		_ = mtlsSrv.Shutdown(shutdownCtx)
	}
}

func resolveVolumeConfig(getenv func(string) string, effectiveStateDir string) (enabled bool, vcfg gateway.VolumeConfig, err error) {
	volDir := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_DIR"))
	if volDir == "" && effectiveStateDir != "" {
		volDir = filepath.Join(effectiveStateDir, "volumes")
	}
	if volDir == "" {
		return false, gateway.VolumeConfig{}, nil
	}

	vcfg = gateway.VolumeConfig{
		Dir:           volDir,
		HostDir:       strings.TrimSpace(getenv("CUBEPOD_VOLUMES_HOST_DIR")),
		SharedPVCName: strings.TrimSpace(getenv("CUBEPOD_VOLUMES_SHARED_PVC")),
	}
	switch strings.ToLower(strings.TrimSpace(getenv("CUBEPOD_K8S_VOLUME_MOUNTS"))) {
	case "1", "true", "yes":
		vcfg.K8sVolumeMounts = true
	}
	if raw := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_MAX_FILE_BYTES")); raw != "" {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil && n > 0 {
			vcfg.MaxFileSizeBytes = n
		}
	}
	if raw := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_MAX_PER_TENANT")); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 {
			return false, gateway.VolumeConfig{}, fmt.Errorf("CUBEPOD_VOLUMES_MAX_PER_TENANT must be a positive integer")
		}
		vcfg.MaxVolumesPerTenant = n
	}
	if raw := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_MAX_TENANT_BYTES")); raw != "" {
		n, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil || n <= 0 {
			return false, gateway.VolumeConfig{}, fmt.Errorf("CUBEPOD_VOLUMES_MAX_TENANT_BYTES must be a positive integer")
		}
		vcfg.MaxBytesPerTenant = n
	}

	kmsURL := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KMS_URL"))
	rawKEK := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KEK"))
	if kmsURL != "" {
		httpCfg := volume.HTTPKMSConfig{
			URL:         kmsURL,
			BearerToken: strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KMS_TOKEN")),
			KeyID:       strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KMS_KEY_ID")),
		}
		if rawTimeout := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KMS_TIMEOUT")); rawTimeout != "" {
			dur, durErr := time.ParseDuration(rawTimeout)
			if durErr != nil || dur <= 0 {
				return false, gateway.VolumeConfig{}, fmt.Errorf("invalid CUBEPOD_VOLUMES_KMS_TIMEOUT")
			}
			httpCfg.Timeout = dur
		}
		if caFile := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_KMS_CA_FILE")); caFile != "" {
			pemBytes, readErr := os.ReadFile(caFile)
			if readErr != nil {
				return false, gateway.VolumeConfig{}, fmt.Errorf("read CUBEPOD_VOLUMES_KMS_CA_FILE: %w", readErr)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return false, gateway.VolumeConfig{}, fmt.Errorf("CUBEPOD_VOLUMES_KMS_CA_FILE contains no valid PEM certificates")
			}
			httpCfg.TLSConfig = &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			}
		}
		kmsProv, kmsErr := volume.NewHTTPKMSProvider(httpCfg)
		if kmsErr != nil {
			return false, gateway.VolumeConfig{}, kmsErr
		}
		vcfg.KMS = kmsProv
	} else if rawKEK != "" {
		kekProv, kekErr := volume.NewLocalKEKProviderFromString(getenv("CUBEPOD_VOLUMES_KEK_ID"), rawKEK)
		if kekErr != nil {
			return false, gateway.VolumeConfig{}, kekErr
		}
		vcfg.KMS = kekProv
	}

	switch strings.ToLower(strings.TrimSpace(getenv("CUBEPOD_VOLUMES_DOCKER"))) {
	case "1", "true", "yes":
		vcfg.DockerVolumes = true
		vcfg.DockerPrefix = "cubepod-vol-"
		if p := strings.TrimSpace(getenv("CUBEPOD_VOLUMES_DOCKER_PREFIX")); p != "" {
			vcfg.DockerPrefix = p
		}
		vcfg.DockerHost = getenv("DOCKER_HOST")
	}
	return true, vcfg, nil
}

func resolveSecretsConfig(backendEnv, secretsDirEnv, volDir, keyEnv, maxPerTenantEnv string, hasKube bool, kc controller.KubeClient, namespace string) (enabled bool, isK8s bool, cfg gateway.E2BSecretConfig, err error) {
	backend := strings.ToLower(strings.TrimSpace(backendEnv))
	explicitDir := strings.TrimSpace(secretsDirEnv)
	if backend == "disabled" || strings.EqualFold(explicitDir, "disabled") {
		return false, false, gateway.E2BSecretConfig{}, nil
	}
	if rawMax := strings.TrimSpace(maxPerTenantEnv); rawMax != "" {
		if n, convErr := strconv.Atoi(rawMax); convErr == nil && n > 0 {
			cfg.MaxSecretsPerTenant = n
		}
	}

	if backend == "k8s" || (backend == "" && hasKube && explicitDir == "") {
		if kc == nil {
			return false, false, gateway.E2BSecretConfig{}, fmt.Errorf("k8s secrets backend requires KubeClient")
		}
		cfg.Backend = "k8s"
		cfg.KubeClient = kc
		cfg.Namespace = namespace
		return true, true, cfg, nil
	}

	if backend != "" && backend != "file" {
		return false, false, gateway.E2BSecretConfig{}, fmt.Errorf("unsupported CUBEPOD_SECRETS_BACKEND %q (expected file, k8s, or disabled)", backendEnv)
	}

	secretsDir := explicitDir
	if secretsDir == "" && volDir != "" {
		secretsDir = filepath.Join(filepath.Dir(volDir), "secrets")
	}
	if secretsDir == "" {
		return false, false, gateway.E2BSecretConfig{}, nil
	}

	cfg.Backend = "file"
	cfg.Dir = secretsDir
	if rawKey := strings.TrimSpace(keyEnv); rawKey != "" {
		if decoded, decErr := base64.StdEncoding.DecodeString(rawKey); decErr == nil && len(decoded) == 32 {
			cfg.EncryptionKey = decoded
		} else if decoded, decErr := hex.DecodeString(rawKey); decErr == nil && len(decoded) == 32 {
			cfg.EncryptionKey = decoded
		} else {
			return false, false, gateway.E2BSecretConfig{}, fmt.Errorf("CUBEPOD_SECRETS_KEY must be a 32-byte base64 or 64-char hex key")
		}
	}
	return true, false, cfg, nil
}

// resolveEventsConfig mirrors resolveSecretsConfig for CUBEPOD_EVENTS_BACKEND
// ("k8s" | "file" | "disabled"). When unset, the Kubernetes backend is used if the gateway
// has a KubeClient and CUBEPOD_EVENTS_DIR is unset; otherwise the file backend under
// <state-dir>/events (or next to the E2B volume dir) is used when a directory is known.
func resolveEventsConfig(backendEnv, eventsDirEnv, stateDir, volDir, keyEnv, allowPrivateEnv string, hasKube bool, kc controller.KubeClient, namespace string) (enabled bool, isK8s bool, cfg gateway.E2BEventsConfig, err error) {
	backend := strings.ToLower(strings.TrimSpace(backendEnv))
	explicitDir := strings.TrimSpace(eventsDirEnv)
	if backend == "disabled" || strings.EqualFold(explicitDir, "disabled") {
		return false, false, gateway.E2BEventsConfig{}, nil
	}
	switch strings.ToLower(strings.TrimSpace(allowPrivateEnv)) {
	case "1", "true", "yes":
		cfg.AllowPrivateIPs = true
	}

	if backend == "k8s" || (backend == "" && hasKube && explicitDir == "") {
		if kc == nil {
			return false, false, gateway.E2BEventsConfig{}, fmt.Errorf("k8s events backend requires KubeClient")
		}
		cfg.Backend = "k8s"
		cfg.KubeClient = kc
		cfg.Namespace = namespace
		return true, true, cfg, nil
	}

	if backend != "" && backend != "file" {
		return false, false, gateway.E2BEventsConfig{}, fmt.Errorf("unsupported CUBEPOD_EVENTS_BACKEND %q (expected file, k8s, or disabled)", backendEnv)
	}

	eventsDir := explicitDir
	if eventsDir == "" && strings.TrimSpace(stateDir) != "" {
		eventsDir = filepath.Join(stateDir, "events")
	} else if eventsDir == "" && volDir != "" {
		eventsDir = filepath.Join(filepath.Dir(volDir), "events")
	}
	if eventsDir == "" {
		return false, false, gateway.E2BEventsConfig{}, nil
	}

	cfg.Backend = "file"
	cfg.Dir = eventsDir
	if rawKey := strings.TrimSpace(keyEnv); rawKey != "" {
		if decoded, decErr := base64.StdEncoding.DecodeString(rawKey); decErr == nil && len(decoded) == 32 {
			cfg.EncryptionKey = decoded
		} else if decoded, decErr := hex.DecodeString(rawKey); decErr == nil && len(decoded) == 32 {
			cfg.EncryptionKey = decoded
		} else {
			return false, false, gateway.E2BEventsConfig{}, fmt.Errorf("CUBEPOD_EVENTS_KEY must be a 32-byte base64 or 64-char hex key")
		}
	}
	return true, false, cfg, nil
}

func buildEgressMTLSServer(addr, certPath, keyPath, namespace string, clientCA *egress.CA, exchangeHandler http.HandlerFunc) (*http.Server, error) {
	if clientCA == nil {
		return nil, fmt.Errorf("--egress-mtls-addr requires --egress-ca-cert and --egress-ca-key (reconciler EgressCA)")
	}
	certPath = strings.TrimSpace(certPath)
	keyPath = strings.TrimSpace(keyPath)
	if (certPath != "") != (keyPath != "") {
		return nil, fmt.Errorf("both --egress-tls-cert and --egress-tls-key must be set together")
	}
	var srvCert tls.Certificate
	var err error
	if certPath != "" {
		srvCert, err = tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load egress TLS keypair: %w", err)
		}
	} else {
		hosts := []string{"localhost", "127.0.0.1", "cubepod-gateway"}
		if namespace != "" {
			hosts = append(hosts, "cubepod-gateway."+namespace+".svc", "cubepod-gateway."+namespace+".svc.cluster.local")
		}
		certPEM, keyPEM, mintErr := clientCA.MintServerCert(hosts, 365*24*time.Hour)
		if mintErr != nil {
			return nil, fmt.Errorf("mint egress TLS server cert: %w", mintErr)
		}
		srvCert, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse minted egress TLS server cert: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/egress/exchange", exchangeHandler)
	return &http.Server{
		Addr:    addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{srvCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCA.CertPool(),
			MinVersion:   tls.VersionTLS12,
		},
	}, nil
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		slog.Warn("invalid duration env, using default", "key", key, "value", raw)
		return def
	}
	return d
}
