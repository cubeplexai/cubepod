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
	var addr = flag.String("addr", ":8080", "HTTP listen address for CubePod Gateway")
	flag.Parse()

	slog.Info("starting cubepod-gateway (E2B-Compatible API & Panel Reverse Proxy)",
		"version", version.Version,
		"addr", *addr,
	)

	reconciler := controller.NewReconciler()
	gw := gateway.NewServer(reconciler)
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
