package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func main() {
	var namespace = flag.String("namespace", "cubepod-workloads", "Kubernetes namespace for PodBox workloads")
	flag.Parse()

	slog.Info("starting cubepod-controller (Kubernetes Operator)",
		"version", version.Version,
		"apiGroup", v1alpha1.GroupName,
		"apiVersion", v1alpha1.GroupVersion,
		"namespace", *namespace,
	)

	_ = controller.NewReconciler()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down cubepod-controller")
	os.Exit(0)
}
