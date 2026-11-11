// Command podd-init is the PID 1 entrypoint inside a CubePod Kata MicroVM.
// It initializes and mounts the persistent RootFS Upper and Workspace raw block devices,
// constructs the Guest Kernel OverlayFS, performs pivot_root, and execs podd.
package main

import (
	"log/slog"
	"os"

	"github.com/cubeplexai/cubepod/pkg/poddinit"
	"github.com/cubeplexai/cubepod/pkg/version"
)

func main() {
	nextArgs := os.Args[1:]
	cfg := poddinit.DefaultConfig(nextArgs)

	slog.Info("starting podd-init (CubePod PID 1 In-Guest RootFS Mounter)",
		"version", version.Version,
		"pid", os.Getpid(),
		"rootfsDevice", cfg.RootfsDevice,
		"workspaceDevice", cfg.WorkspaceDevice,
		"next", cfg.NextArgv,
	)

	if err := poddinit.SetupAndPivot(cfg); err != nil {
		slog.Error("podd-init failed to prepare persistent rootfs", "err", err)
		os.Exit(1)
	}

	if err := poddinit.ExecNext(cfg); err != nil {
		slog.Error("podd-init failed to exec target daemon", "err", err)
		os.Exit(1)
	}
}
