package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/version"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
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
		if gateVal, ok := gates["E2BTemplateAPI"]; ok {
			return flagVal && gateVal
		}
		return flagVal
	}
	if gateVal, ok := gates["E2BTemplateAPI"]; ok {
		return gateVal
	}
	return flagVal
}

func defaultEgressImage() string {
	if env := strings.TrimSpace(os.Getenv("CUBEPOD_EGRESS_IMAGE")); env != "" {
		return env
	}
	return "ghcr.io/cubeplexai/cubepod-egress:v0.8.0"
}

func main() {
	var (
		namespace            = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		addr                 = flag.String("addr", ":8081", "HTTP server address for health and metrics")
		syncInterval         = flag.Duration("sync-interval", 5*time.Second, "Reconcile background loop sync interval")
		kubeAPI              = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken            = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass         = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode           = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass         = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage            = flag.String("podd-image", "ghcr.io/cubeplexai/cubepod-podd:v0.8.0", "Image for podd-installer initContainer")
		sandboxImage         = flag.String("sandbox-image", "ubuntu:24.04", "Default sandbox image")
		egressImage          = flag.String("egress-image", defaultEgressImage(), "Default cubepod-egress sidecar image when template egress.image is empty")
		egressExchangeURL    = flag.String("egress-exchange-url", os.Getenv("CUBEPOD_EGRESS_EXCHANGE_URL"), "Default egress secret exchange URL when template egress.exchangeUrl is empty")
		defaultEgressEnabled = flag.Bool("default-egress-enabled", true, "Enable zero-trust egress sidecar by default on seeded and fallback templates")
		transparentEgress    = flag.Bool("transparent-egress", true, "Enable transparent nftables/DNS egress interception in sidecars")
		forkMode             = flag.String("fork-mode", "pvc-clone", "Sandbox fork volume cloning mode (pvc-clone or snapshot)")
		volumeSnapshotClass  = flag.String("volume-snapshot-class", "", "VolumeSnapshotClassName used when --fork-mode=snapshot")
		e2bTemplateAPI       = flag.Bool("e2b-template-api", true, "Enable E2B template API feature gate")
		featureGate          = flag.String("feature-gate", "", "Comma-separated feature gates (e.g. E2BTemplateAPI=true,TransparentEgress=true)")
		templateBuild        = flag.Bool("template-build", false, "Enable template image build Beta pipeline")
		buildkitAddr         = flag.String("buildkit-addr", "", "BuildKit daemon gRPC address(es) (comma-separated supported, e.g. tcp://cubepod-buildkit:1234)")
		buildkitCACert       = flag.String("buildkit-ca-cert", envOrDefault("CUBEPOD_BUILDKIT_TLS_CA_CERT", os.Getenv("CUBEPOD_BUILDKIT_TLS_CA")), "Path to CA certificate PEM verifying buildkitd TLS server")
		buildkitTLSCert      = flag.String("buildkit-tls-cert", os.Getenv("CUBEPOD_BUILDKIT_TLS_CERT"), "Path to client TLS certificate PEM for buildkitd mTLS")
		buildkitTLSKey       = flag.String("buildkit-tls-key", os.Getenv("CUBEPOD_BUILDKIT_TLS_KEY"), "Path to client TLS private key PEM for buildkitd mTLS")
		buildkitTLSServer    = flag.String("buildkit-tls-server-name", os.Getenv("CUBEPOD_BUILDKIT_TLS_SERVER_NAME"), "Optional TLS server name override for buildkitd verification")
		contextStoreBackend  = flag.String("context-store-backend", envOrDefault("CUBEPOD_CONTEXT_STORE_BACKEND", "local"), "Template build context store backend (local or s3)")
		s3Endpoint           = flag.String("s3-endpoint", os.Getenv("CUBEPOD_S3_ENDPOINT"), "S3-compatible endpoint URL for context store backend=s3")
		s3Bucket             = flag.String("s3-bucket", os.Getenv("CUBEPOD_S3_BUCKET"), "S3 bucket name for context store backend=s3")
		s3Region             = flag.String("s3-region", envOrDefault("CUBEPOD_S3_REGION", "us-east-1"), "S3 region for SigV4 signing")
		s3Prefix             = flag.String("s3-prefix", os.Getenv("CUBEPOD_S3_PREFIX"), "Optional key prefix inside the S3 context store bucket")
		s3ForcePathStyle     = flag.Bool("s3-force-path-style", envBoolOrDefault("CUBEPOD_S3_FORCE_PATH_STYLE", false), "Use path-style S3 URLs (required for MinIO/SeaweedFS/R2)")
		stateDir             = flag.String("state-dir", "", "State directory for template contexts and logs")
		templateRegistry     = flag.String("template-registry", "", "Target OCI registry URL for built templates")
		templateRepoPrefix   = flag.String("template-repo-prefix", "cubepod-templates", "Repository prefix under target template registry")
		templatePushSecret   = flag.String("template-push-secret", "", "Kubernetes dockerconfigjson secret used to push built templates")
		templatePullSecret   = flag.String("template-pull-secret", "", "Kubernetes imagePullSecret attached to sandbox and probe pods")
		imagePullSecrets     = flag.String("image-pull-secrets", os.Getenv("CUBEPOD_IMAGE_PULL_SECRETS"), "Comma-separated Kubernetes imagePullSecrets attached to sandbox Pods")
		templateProbeTimeout = flag.Duration("template-probe-timeout", 120*time.Second, "Timeout for template readyCmd probe")
		egressCACert         = flag.String("egress-ca-cert", os.Getenv("CUBEPOD_EGRESS_CA_CERT"), "Path to egress mTLS root CA certificate PEM")
		egressCAKey          = flag.String("egress-ca-key", os.Getenv("CUBEPOD_EGRESS_CA_KEY"), "Path to egress mTLS root CA private key PEM")
		egressUpstreamCAFile = flag.String("egress-upstream-ca-file", os.Getenv("CUBEPOD_EGRESS_UPSTREAM_CA_FILE"), "Optional path to upstream CA PEM mounted into egress sidecars")
		volumeNFSService     = flag.String("volume-nfs-service", envOrDefault("CUBEPOD_VOLUME_NFS_SERVICE", "cubepod-volume-nfs"), "Kubernetes Service name for cubepod-volume-nfs")
		volumeNFSNamespace   = flag.String("volume-nfs-namespace", os.Getenv("CUBEPOD_VOLUME_NFS_NAMESPACE"), "Namespace of the cubepod-volume-nfs Service (defaults to the controller namespace)")
		volumeNFSLeaseURL    = flag.String("volume-nfs-lease-url", os.Getenv("CUBEPOD_VOLUME_NFS_LEASE_URL"), "Internal lease API URL (not the NFS port)")
		volumeNFSPort        = flag.Int("volume-nfs-port", 2049, "NFS TCP port advertised to guests and egress exemptions")
	)
	flag.Parse()

	gates := parseFeatureGates(*featureGate)
	e2bFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "e2b-template-api" {
			e2bFlagSet = true
		}
	})
	e2bAPIEnabled := resolveE2BTemplateAPIFlag(e2bFlagSet, *e2bTemplateAPI, gates)
	transparentEgressEnabled := *transparentEgress
	if v, ok := gates["TransparentEgress"]; ok {
		transparentEgressEnabled = v
	}
	// CEP-0031 FirecrackerVolumeNFS is default on; --feature-gate=FirecrackerVolumeNFS=false opts out.
	firecrackerVolumeNFS := true
	if v, ok := gates[controller.FeatureGateFirecrackerVolumeNFS]; ok {
		firecrackerVolumeNFS = v
	}
	normalizedForkMode := strings.ToLower(strings.TrimSpace(*forkMode))
	switch normalizedForkMode {
	case "pvc-clone", "pvc_clone", "":
		normalizedForkMode = "pvc-clone"
	case "snapshot":
		normalizedForkMode = "snapshot"
	default:
		slog.Error("invalid --fork-mode (expected pvc-clone or snapshot)", "forkMode", *forkMode)
		os.Exit(1)
	}

	effectiveStateDir := *stateDir
	if effectiveStateDir == "" {
		effectiveStateDir = os.Getenv("CUBEPOD_STATE")
	}

	slog.Info("starting cubepod-controller (Kubernetes Operator)",
		"version", version.Version,
		"apiGroup", v1alpha1.GroupName,
		"apiVersion", v1alpha1.GroupVersion,
		"namespace", *namespace,
		"addr", *addr,
		"volumeMode", *volumeMode,
		"storageClass", *storageClass,
		"runtimeClass", *runtimeClass,
		"poddImage", *poddImage,
		"sandboxImage", *sandboxImage,
		"egressImage", *egressImage,
		"egressExchangeURL", *egressExchangeURL,
		"defaultEgressEnabled", *defaultEgressEnabled,
		"transparentEgress", transparentEgressEnabled,
		"forkMode", normalizedForkMode,
		"volumeSnapshotClass", *volumeSnapshotClass,
		"e2bTemplateAPI", e2bAPIEnabled,
		"templateBuild", *templateBuild,
		"buildkitAddr", *buildkitAddr,
		"buildkitTLSEnabled", *buildkitCACert != "" || *buildkitTLSCert != "",
		"contextStoreBackend", *contextStoreBackend,
		"s3Bucket", *s3Bucket,
		"s3Region", *s3Region,
		"stateDir", effectiveStateDir,
		"templateRegistry", *templateRegistry,
		"templateRepoPrefix", *templateRepoPrefix,
		"templatePushSecret", *templatePushSecret,
		"templatePullSecret", *templatePullSecret,
		"imagePullSecrets", *imagePullSecrets,
		"templateProbeTimeout", *templateProbeTimeout,
		"firecrackerVolumeNFS", firecrackerVolumeNFS,
		"volumeNFSService", *volumeNFSService,
		"volumeNFSLeaseURL", *volumeNFSLeaseURL,
	)

	var kubeClient controller.KubeClient
	if *kubeAPI != "" {
		kubeClient = controller.NewRESTKubeClient(*kubeAPI, *namespace, *kubeToken, nil, controller.WithWaitForRunning(true))
	} else if client, err := controller.NewRESTKubeClientFromEnv(controller.WithKubeClientNamespace(*namespace), controller.WithWaitForRunning(true)); err == nil {
		kubeClient = client
	} else {
		kubeClient = controller.NewMemoryKubeClient()
	}

	var contextDir string
	if effectiveStateDir != "" {
		contextDir = filepath.Join(effectiveStateDir, "template-context")
	}

	var builder controller.ImageBuilder
	if *buildkitAddr != "" {
		if (*buildkitTLSCert != "") != (*buildkitTLSKey != "") {
			slog.Error("both --buildkit-tls-cert and --buildkit-tls-key must be set together")
			os.Exit(1)
		}
		bkOpts := []controller.BuildKitBuilderOption{
			controller.WithBuildKitKubeClient(kubeClient, *namespace),
		}
		if contextDir != "" {
			bkOpts = append(bkOpts, controller.WithBuildKitStateDir(contextDir))
		}
		if *templatePushSecret != "" {
			bkOpts = append(bkOpts, controller.WithBuildKitPushSecret(*templatePushSecret))
		}
		if *buildkitCACert != "" || *buildkitTLSCert != "" || *buildkitTLSKey != "" || *buildkitTLSServer != "" {
			bkOpts = append(bkOpts, controller.WithBuildKitTLS(*buildkitCACert, *buildkitTLSCert, *buildkitTLSKey, *buildkitTLSServer))
		}
		builder = controller.NewBuildKitBuilder(*buildkitAddr, bkOpts...)
	} else {
		builder = controller.NewLocalDockerBuilder(contextDir)
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

	opts := []controller.ReconcilerOption{
		controller.WithKubeClient(kubeClient),
		controller.WithNamespace(*namespace),
		controller.WithVolumeMode(*volumeMode),
		controller.WithDefaultStorageClass(*storageClass),
		controller.WithDefaultRuntimeClass(*runtimeClass),
		controller.WithPoddInjectorImage(*poddImage),
		controller.WithDefaultSandboxImage(*sandboxImage),
		controller.WithDefaultEgressImage(*egressImage),
		controller.WithDefaultEgressExchangeURL(*egressExchangeURL),
		controller.WithDefaultEgressEnabled(*defaultEgressEnabled),
		controller.WithTransparentEgress(transparentEgressEnabled),
		controller.WithForkMode(normalizedForkMode, *volumeSnapshotClass),
		controller.WithE2BTemplateAPI(e2bAPIEnabled),
		controller.WithTemplateBuildEnabled(*templateBuild),
		controller.WithImageBuilder(builder),
		controller.WithContextStore(ctxStore),
		controller.WithTemplateRegistry(*templateRegistry),
		controller.WithTemplateRepoPrefix(*templateRepoPrefix),
		controller.WithTemplatePushSecret(*templatePushSecret),
		controller.WithTemplatePullSecret(*templatePullSecret),
		controller.WithDefaultImagePullSecrets(*imagePullSecrets),
		controller.WithTemplateProbeTimeout(*templateProbeTimeout),
	}
	nfsSettings := controller.VolumeNFSSettings{
		Enabled:     firecrackerVolumeNFS,
		ServiceName: *volumeNFSService,
		ServiceNS:   *volumeNFSNamespace,
		NFSPort:     *volumeNFSPort,
	}
	if *volumeNFSLeaseURL != "" {
		nfsSettings.LeaseClient = volumenfs.NewClient(*volumeNFSLeaseURL, nil)
	}
	if rest, ok := kubeClient.(*controller.RESTKubeClient); ok {
		nfsSettings.Lookup = controller.RESTVolumeNFSLookup{Client: rest}
	}
	opts = append(opts, controller.WithVolumeNFS(nfsSettings))
	if effectiveStateDir != "" {
		opts = append(opts, controller.WithStateDir(effectiveStateDir))
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

	mgr := controller.NewControllerManager(reconciler, *addr, *syncInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := mgr.Start(ctx); err != nil {
		slog.Error("controller manager terminated with error", "err", err)
		os.Exit(1)
	}

	slog.Info("shutting down cubepod-controller")
	os.Exit(0)
}
