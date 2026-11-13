package main

import (
	"context"
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
	"github.com/cubeplexai/cubepod/pkg/gateway"
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
		poddImage            = flag.String("podd-image", "ghcr.io/cubeplexai/cubepod-podd:v0.4.0", "Image for podd-installer initContainer")
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
