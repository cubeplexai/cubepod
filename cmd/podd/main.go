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

	"github.com/cubeplexai/cubepod/pkg/podd"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func main() {
	var (
		addr    = flag.String("addr", ":49983", "HTTP/SSE listen address for podd")
		workdir = flag.String("workdir", "/workspace", "Default working directory for agent commands")
	)
	flag.Parse()

	slog.Info("starting podd (CubePod in-guest execution & file daemon)",
		"version", version.Version,
		"pid", os.Getpid(),
		"addr", *addr,
		"workdir", *workdir,
	)

	srv := podd.NewServer(*addr, *workdir)
	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("podd HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("received termination signal, syncing filesystems and shutting down podd")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
