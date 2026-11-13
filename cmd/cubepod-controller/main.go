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
		namespace            = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		addr                 = flag.String("addr", ":8081", "HTTP server address for health and metrics")
		syncInterval         = flag.Duration("sync-interval", 5*time.Second, "Reconcile background loop sync interval")
		kubeAPI              = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken            = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass         = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode           = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass         = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage            = flag.String("podd-image", "ghcr.io/cubeplexai/cubepod-podd:v0.4.0", "Image for podd-installer initContainer")
		sandboxImage         = flag.String("sandbox-image", "ubuntu:24.04", "Default sandbox image")
		e2bTemplateAPI       = flag.Bool("e2b-template-api", false, "Enable E2B template API feature gate")
		featureGate          = flag.String("feature-gate", "", "Comma-separated feature gates (e.g. E2BTemplateAPI=true)")
		templateBuild        = flag.Bool("template-build", false, "Enable template image build Beta pipeline")
		buildkitAddr         = flag.String("buildkit-addr", "", "BuildKit daemon gRPC address (e.g. tcp://cubepod-buildkit:1234)")
		stateDir             = flag.String("state-dir", "", "State directory for template contexts and logs")
		templateRegistry     = flag.String("template-registry", "", "Target OCI registry URL for built templates")
		templateRepoPrefix   = flag.String("template-repo-prefix", "cubepod-templates", "Repository prefix under target template registry")
		templatePushSecret   = flag.String("template-push-secret", "", "Kubernetes dockerconfigjson secret used to push built templates")
		templatePullSecret   = flag.String("template-pull-secret", "", "Kubernetes imagePullSecret attached to sandbox and probe pods")
		imagePullSecrets     = flag.String("image-pull-secrets", os.Getenv("CUBEPOD_IMAGE_PULL_SECRETS"), "Comma-separated Kubernetes imagePullSecrets attached to sandbox Pods")
		templateProbeTimeout = flag.Duration("template-probe-timeout", 120*time.Second, "Timeout for template readyCmd probe")
	)
	flag.Parse()

	gates := parseFeatureGates(*featureGate)
	e2bAPIEnabled := *e2bTemplateAPI || gates["E2BTemplateAPI"]

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
		"e2bTemplateAPI", e2bAPIEnabled,
		"templateBuild", *templateBuild,
		"buildkitAddr", *buildkitAddr,
		"stateDir", effectiveStateDir,
		"templateRegistry", *templateRegistry,
		"templateRepoPrefix", *templateRepoPrefix,
		"templatePushSecret", *templatePushSecret,
		"templatePullSecret", *templatePullSecret,
		"imagePullSecrets", *imagePullSecrets,
		"templateProbeTimeout", *templateProbeTimeout,
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
		bkOpts := []controller.BuildKitBuilderOption{
			controller.WithBuildKitKubeClient(kubeClient, *namespace),
		}
		if contextDir != "" {
			bkOpts = append(bkOpts, controller.WithBuildKitStateDir(contextDir))
		}
		if *templatePushSecret != "" {
			bkOpts = append(bkOpts, controller.WithBuildKitPushSecret(*templatePushSecret))
		}
		builder = controller.NewBuildKitBuilder(*buildkitAddr, bkOpts...)
	} else {
		builder = controller.NewLocalDockerBuilder(contextDir)
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
		controller.WithImageBuilder(builder),
		controller.WithTemplateRegistry(*templateRegistry),
		controller.WithTemplateRepoPrefix(*templateRepoPrefix),
		controller.WithTemplatePushSecret(*templatePushSecret),
		controller.WithTemplatePullSecret(*templatePullSecret),
		controller.WithDefaultImagePullSecrets(*imagePullSecrets),
		controller.WithTemplateProbeTimeout(*templateProbeTimeout),
	}
	if effectiveStateDir != "" {
		opts = append(opts,
			controller.WithStateDir(effectiveStateDir),
			controller.WithContextStore(controller.NewMemoryContextStore(contextDir)),
		)
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
