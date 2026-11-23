package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/pkg/version"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
)

func main() {
	var (
		volumesDir  = flag.String("volumes-dir", envOrDefault("CUBEPOD_VOLUMES_DIR", "/var/lib/cubepod/volumes"), "Path to the official volumes RWX store")
		nfsAddr     = flag.String("nfs-addr", envOrDefault("CUBEPOD_VOLUME_NFS_ADDR", ":2049"), "TCP address for userspace NFSv3 + MOUNT")
		leaseAddr   = flag.String("lease-addr", envOrDefault("CUBEPOD_VOLUME_NFS_LEASE_ADDR", ":8080"), "TCP address for the internal lease API (must not be the NFS port)")
		unboundTTL  = flag.Duration("unbound-lease-ttl", volumenfs.DefaultUnboundTTL, "TTL for unbound leases, aligned with the EnsurePod timeout")
		handleCache = flag.Int("handle-cache", 1024, "In-memory go-nfs filehandle cache size (Alpha; restarts ESTALE active mounts)")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *volumesDir == "" {
		logger.Error("volumes-dir is required")
		os.Exit(1)
	}

	logger.Info("starting cubepod-volume-nfs",
		"version", version.Version,
		"volumesDir", *volumesDir,
		"nfsAddr", *nfsAddr,
		"leaseAddr", *leaseAddr,
		"unboundLeaseTTL", *unboundTTL,
		"handleCache", *handleCache,
		"alphaLimits", "single-replica; in-memory filehandles; process restart may ESTALE active mounts",
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := volumenfs.Serve(ctx, volumenfs.Config{
		VolumesDir:   *volumesDir,
		NFSAddr:      *nfsAddr,
		LeaseAPIAddr: *leaseAddr,
		UnboundTTL:   *unboundTTL,
		HandleCache:  *handleCache,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("volume nfs server exited", "err", err)
		os.Exit(1)
	}
	// Give in-flight RPCs a moment when the context is cancelled.
	time.Sleep(10 * time.Millisecond)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
