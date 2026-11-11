package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func main() {
	var (
		addr           = flag.String("addr", ":8080", "HTTP listen address for CubePod Gateway")
		namespace      = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
		kubeAPI        = flag.String("kube-api", "", "Kubernetes API Server URL (empty to auto-detect from in-cluster / env)")
		kubeToken      = flag.String("kube-token", "", "Kubernetes API Bearer token")
		storageClass   = flag.String("storage-class", "nfs-storage", "Default StorageClass name")
		volumeMode     = flag.String("volume-mode", "Filesystem", "Storage volume mode (Block or Filesystem)")
		runtimeClass   = flag.String("runtime-class", "", "Container runtime class name (empty for standard runc/containerd)")
		poddImage      = flag.String("podd-image", "hub.sensedeal.vip/library/cubepod-podd:v0.3.0-k8s", "Image for podd-installer initContainer")
		sandboxImage   = flag.String("sandbox-image", "hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917", "Default sandbox image")
		livePodRouting = flag.Bool("live-pod-routing", true, "Route commands and files to live in-cluster Pod IP")
	)
	flag.Parse()

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
	)

	var kubeClient controller.KubeClient
	if *kubeAPI != "" {
		kubeClient = controller.NewRESTKubeClient(*kubeAPI, *namespace, *kubeToken, nil, controller.WithWaitForRunning(true))
	} else if client, err := controller.NewRESTKubeClientFromEnv(controller.WithKubeClientNamespace(*namespace), controller.WithWaitForRunning(true)); err == nil {
		kubeClient = client
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
	}

	reconciler := controller.NewReconciler(opts...)
	gw := gateway.NewServer(reconciler)
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
