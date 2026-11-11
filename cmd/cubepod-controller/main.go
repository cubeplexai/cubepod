package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func main() {
	var (
		namespace    = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		addr         = flag.String("addr", ":8081", "HTTP server address for health and metrics")
		syncInterval = flag.Duration("sync-interval", 5*time.Second, "Reconcile background loop sync interval")
		kubeAPI      = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken    = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode   = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage    = flag.String("podd-image", "hub.sensedeal.vip/library/cubepod-podd:v0.3.0-k8s", "Image for podd-installer initContainer")
		sandboxImage = flag.String("sandbox-image", "hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917", "Default sandbox image")
	)
	flag.Parse()

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
	)

	var kubeClient controller.KubeClient
	if *kubeAPI != "" {
		kubeClient = controller.NewRESTKubeClient(*kubeAPI, *namespace, *kubeToken, nil, controller.WithWaitForRunning(true))
	} else if client, err := controller.NewRESTKubeClientFromEnv(controller.WithKubeClientNamespace(*namespace), controller.WithWaitForRunning(true)); err == nil {
		kubeClient = client
	} else {
		kubeClient = controller.NewMemoryKubeClient()
	}

	reconciler := controller.NewReconciler(
		controller.WithKubeClient(kubeClient),
		controller.WithNamespace(*namespace),
		controller.WithVolumeMode(*volumeMode),
		controller.WithDefaultStorageClass(*storageClass),
		controller.WithDefaultRuntimeClass(*runtimeClass),
		controller.WithPoddInjectorImage(*poddImage),
		controller.WithDefaultSandboxImage(*sandboxImage),
	)

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
