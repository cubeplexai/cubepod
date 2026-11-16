package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"flag"
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
)

func parseFeatureGates(raw string) map[string]bool {
	gates := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		key := strings.TrimSpace(parts[0])
		if len(parts) == 1 {
			gates[key] = true
			continue
		}
		val, err := strconv.ParseBool(strings.TrimSpace(parts[1]))
		if err == nil {
			gates[key] = val
		}
	}
	return gates
}

func main() {
	var (
		addr                 = flag.String("addr", ":8080", "HTTP listen address for CubePod Gateway")
		namespace            = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		kubeAPI              = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken            = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass         = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode           = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass         = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage            = flag.String("podd-image", "ghcr.io/cubeplexai/cubepod-podd:v0.5.1", "Image for podd-installer initContainer")
		sandboxImage         = flag.String("sandbox-image", "ubuntu:24.04", "Default sandbox image")
		livePodRouting       = flag.Bool("live-pod-routing", true, "Route commands and files to live in-cluster Pod IP")
		e2bTemplateAPI       = flag.Bool("e2b-template-api", false, "Enable E2B template API feature gate")
		featureGate          = flag.String("feature-gate", "", "Comma-separated feature gates (e.g. E2BTemplateAPI=true)")
		templateBuild        = flag.Bool("template-build", false, "Enable template image build Beta pipeline")
		controllerURL        = flag.String("controller-url", "", "Base URL of cubepod-controller for forwarding template build operations")
		stateDir             = flag.String("state-dir", "", "State directory for local template catalog, contexts, and logs")
		templateProbeTimeout = flag.Duration("template-probe-timeout", 120*time.Second, "Timeout for template readyCmd probe")
		templateRegistry     = flag.String("template-registry", "", "Target OCI registry URL for template builds (local single-process mode)")
		templateRepoPrefix   = flag.String("template-repo-prefix", "cubepod-templates", "Repository prefix under target template registry")
		templatePullSecret   = flag.String("template-pull-secret", "", "Kubernetes imagePullSecret attached to sandbox pods")
		egressCACert         = flag.String("egress-ca-cert", os.Getenv("CUBEPOD_EGRESS_CA_CERT"), "Path to egress mTLS root CA certificate PEM")
		egressCAKey          = flag.String("egress-ca-key", os.Getenv("CUBEPOD_EGRESS_CA_KEY"), "Path to egress mTLS root CA private key PEM")
		egressUpstreamCAFile = flag.String("egress-upstream-ca-file", os.Getenv("CUBEPOD_EGRESS_UPSTREAM_CA_FILE"), "Optional path to upstream CA PEM mounted into egress sidecars")
	)
	flag.Parse()

	gates := parseFeatureGates(*featureGate)
	e2bAPIEnabled := *e2bTemplateAPI || gates["E2BTemplateAPI"]

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
		"livePodRouting", *livePodRouting,
		"e2bTemplateAPI", e2bAPIEnabled,
		"templateBuild", *templateBuild,
		"controllerURL", *controllerURL,
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
		controller.WithE2BTemplateAPI(e2bAPIEnabled),
		controller.WithTemplateBuildEnabled(*templateBuild),
		controller.WithTemplateProbeTimeout(*templateProbeTimeout),
		controller.WithTemplateRegistry(*templateRegistry),
		controller.WithTemplateRepoPrefix(*templateRepoPrefix),
		controller.WithTemplatePullSecret(*templatePullSecret),
	}

	var contextDir string
	if effectiveStateDir != "" {
		contextDir = filepath.Join(effectiveStateDir, "template-context")
		opts = append(opts,
			controller.WithStateDir(effectiveStateDir),
			controller.WithContextStore(controller.NewMemoryContextStore(contextDir)),
		)
	}
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
	gateway.ApplyTemplateConfig(gw, e2bAPIEnabled, *templateBuild, *controllerURL, *templateProbeTimeout)
	if *livePodRouting {
		gw.SetLivePodRouting(true)
	}

	// Official E2B 2.64.0 Volume store. Routes are always registered; this
	// opens the durable directory so management and content APIs work.
	// CUBEPOD_E2B_VOLUME_DIR wins; otherwise stateDir/e2b-volumes is used
	// when a state directory is set. Docker named volumes stay off unless
	// CUBEPOD_E2B_DOCKER_VOLUMES is explicitly enabled (compose/runc tests).
	volDir := strings.TrimSpace(os.Getenv("CUBEPOD_E2B_VOLUME_DIR"))
	if volDir == "" && effectiveStateDir != "" {
		volDir = filepath.Join(effectiveStateDir, "e2b-volumes")
	}
	if volDir != "" {
		vcfg := gateway.E2BVolumeConfig{
			Dir:           volDir,
			HostDir:       strings.TrimSpace(os.Getenv("CUBEPOD_E2B_VOLUME_HOST_DIR")),
			SharedPVCName: strings.TrimSpace(os.Getenv("CUBEPOD_E2B_SHARED_PVC")),
		}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("CUBEPOD_K8S_E2B_VOLUME_MOUNTS"))) {
		case "1", "true", "yes":
			vcfg.K8sE2BVolumeMounts = true
		}
		if raw := strings.TrimSpace(os.Getenv("CUBEPOD_E2B_MAX_FILE_BYTES")); raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
				vcfg.MaxFileSizeBytes = n
			}
		}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("CUBEPOD_E2B_DOCKER_VOLUMES"))) {
		case "1", "true", "yes":
			vcfg.DockerVolumes = true
			vcfg.DockerPrefix = "cubepod-e2bvol-"
			if p := strings.TrimSpace(os.Getenv("CUBEPOD_E2B_DOCKER_PREFIX")); p != "" {
				vcfg.DockerPrefix = p
			}
			vcfg.DockerHost = os.Getenv("DOCKER_HOST")
		}
		if err := gw.ConfigureE2BVolumes(vcfg); err != nil {
			slog.Error("failed to configure E2B volume store", "dir", volDir, "err", err)
			os.Exit(1)
		}
		slog.Info("E2B volume store enabled", "dir", volDir, "dockerVolumes", vcfg.DockerVolumes)
	}

	secretsDir := strings.TrimSpace(os.Getenv("CUBEPOD_SECRETS_DIR"))
	if secretsDir == "" && volDir != "" {
		secretsDir = filepath.Join(filepath.Dir(volDir), "secrets")
	}
	if secretsDir != "" && !strings.EqualFold(secretsDir, "disabled") {
		scfg := gateway.E2BSecretConfig{Dir: secretsDir}
		if rawKey := strings.TrimSpace(os.Getenv("CUBEPOD_SECRETS_KEY")); rawKey != "" {
			if decoded, err := base64.StdEncoding.DecodeString(rawKey); err == nil && len(decoded) == 32 {
				scfg.EncryptionKey = decoded
			} else if decoded, err := hex.DecodeString(rawKey); err == nil && len(decoded) == 32 {
				scfg.EncryptionKey = decoded
			} else {
				slog.Error("CUBEPOD_SECRETS_KEY must be a 32-byte base64 or 64-char hex key")
				os.Exit(1)
			}
		}
		if rawMax := strings.TrimSpace(os.Getenv("CUBEPOD_MAX_SECRETS_PER_TENANT")); rawMax != "" {
			if n, err := strconv.Atoi(rawMax); err == nil && n > 0 {
				scfg.MaxSecretsPerTenant = n
			}
		}
		if err := gw.ConfigureE2BSecrets(scfg); err != nil {
			slog.Error("failed to configure E2B secret store", "dir", secretsDir, "err", err)
			os.Exit(1)
		}
		slog.Info("E2B secret store enabled", "dir", secretsDir)
	}

	eventsDir := strings.TrimSpace(os.Getenv("CUBEPOD_EVENTS_DIR"))
	if eventsDir == "" && *stateDir != "" {
		eventsDir = filepath.Join(*stateDir, "events")
	} else if eventsDir == "" && volDir != "" {
		eventsDir = filepath.Join(filepath.Dir(volDir), "events")
	}
	if eventsDir != "" && !strings.EqualFold(eventsDir, "disabled") {
		ecfg := gateway.E2BEventsConfig{Dir: eventsDir}
		switch strings.ToLower(strings.TrimSpace(os.Getenv("CUBEPOD_WEBHOOK_ALLOW_PRIVATE"))) {
		case "1", "true", "yes":
			ecfg.AllowPrivateIPs = true
		}
		if rawKey := strings.TrimSpace(os.Getenv("CUBEPOD_EVENTS_KEY")); rawKey != "" {
			if decoded, err := base64.StdEncoding.DecodeString(rawKey); err == nil && len(decoded) == 32 {
				ecfg.EncryptionKey = decoded
			} else if decoded, err := hex.DecodeString(rawKey); err == nil && len(decoded) == 32 {
				ecfg.EncryptionKey = decoded
			} else {
				slog.Error("CUBEPOD_EVENTS_KEY must be a 32-byte base64 or 64-char hex key")
				os.Exit(1)
			}
		}
		if err := gw.ConfigureEvents(ecfg); err != nil {
			slog.Error("failed to configure E2B events and webhooks", "dir", eventsDir, "err", err)
			os.Exit(1)
		}
		defer gw.CloseEvents()
		slog.Info("E2B events and webhooks enabled", "dir", eventsDir, "allowPrivateIPs", ecfg.AllowPrivateIPs)
	}

	if strings.EqualFold(strings.TrimSpace(os.Getenv("CUBEPOD_RUNTIME")), "docker") {
		cfg := runtime.DockerConfig{
			DockerHost:          os.Getenv("DOCKER_HOST"),
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

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: gw.Handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("gateway HTTP server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down cubepod-gateway")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
