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
		kubeAPI      = flag.String("kube-api", "", "Kubernetes API Server URL (empty for in-memory)")
		kubeToken    = flag.String("kube-token", "", "Kubernetes API Bearer token")
	)
	flag.Parse()

	slog.Info("starting cubepod-controller (Kubernetes Operator)",
		"version", version.Version,
		"apiGroup", v1alpha1.GroupName,
		"apiVersion", v1alpha1.GroupVersion,
		"namespace", *namespace,
		"addr", *addr,
	)

	var kubeClient controller.KubeClient
	if *kubeAPI != "" {
		kubeClient = controller.NewRESTKubeClient(*kubeAPI, *namespace, *kubeToken, nil)
	} else {
		kubeClient = controller.NewMemoryKubeClient()
	}

	reconciler := controller.NewReconciler(
		controller.WithKubeClient(kubeClient),
		controller.WithNamespace(*namespace),
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
