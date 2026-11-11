// Package podd implements the in-sandbox PID 1 rootfs pivot initializer (podd-init)
// and the HTTP/SSE command & file execution server (podd).
package podd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// PivotConfig holds device paths and target mount points for the persistent OverlayFS setup.
type PivotConfig struct {
	RootfsDevice    string
	WorkspaceDevice string
	DefaultUID      int
	DefaultGID      int
	SkipPivot       bool
}

// DefaultPivotConfig returns the standard Kata MicroVM block device configuration.
func DefaultPivotConfig() PivotConfig {
	return PivotConfig{
		RootfsDevice:    "/dev/cubepod-rootfs",
		WorkspaceDevice: "/dev/cubepod-workspace",
		DefaultUID:      1000,
		DefaultGID:      1000,
		SkipPivot:       os.Getenv("CUBEPOD_SKIP_PIVOT") == "1",
	}
}

// EnsureBlockFilesystem checks whether a block device already contains an ext4 filesystem;
// if not, it formats it with mkfs.ext4, or runs e2fsck -p to auto-heal after unclean shutdowns.
func EnsureBlockFilesystem(devicePath string, label string) error {
	if _, err := os.Stat(devicePath); err != nil {
		return fmt.Errorf("block device %s not found: %w", devicePath, err)
	}

	// Check if filesystem signature exists via blkid.
	checkCmd := exec.Command("blkid", "-o", "value", "-s", "TYPE", devicePath)
	out, err := checkCmd.Output()
	if err != nil || len(out) == 0 {
		slog.Info("formatting fresh raw block device as ext4", "device", devicePath, "label", label)
		mkfs := exec.Command("mkfs.ext4", "-F", "-L", label, "-m", "0", devicePath)
		if out, err := mkfs.CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs.ext4 failed on %s: %w (%s)", devicePath, err, string(out))
		}
		return nil
	}

	slog.Info("running fast e2fsck preen check on existing block device", "device", devicePath)
	fsck := exec.Command("e2fsck", "-p", devicePath)
	if out, err := fsck.CombinedOutput(); err != nil {
		// Exit code 1 means filesystem errors were corrected.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			slog.Info("e2fsck auto-healed filesystem errors", "device", devicePath)
			return nil
		}
		slog.Warn("e2fsck reported non-zero status", "device", devicePath, "output", string(out), "err", err)
	}
	return nil
}

// PreparePersistentRoot mounts the RootFS Upper block PVC and Workspace block PVC
// when running as PID 1 inside a Kata MicroVM.
func PreparePersistentRoot(cfg PivotConfig) error {
	if cfg.SkipPivot {
		slog.Info("skipping in-guest rootfs pivot (CUBEPOD_SKIP_PIVOT=1)")
		return nil
	}

	// If no block devices are attached (e.g., plain local container mode), continue gracefully.
	if _, err := os.Stat(cfg.RootfsDevice); os.IsNotExist(err) {
		slog.Info("no raw block device detected at RootfsDevice; running on standard rootfs", "device", cfg.RootfsDevice)
		return nil
	}

	if err := EnsureBlockFilesystem(cfg.RootfsDevice, "cubepod-rootfs"); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.WorkspaceDevice); err == nil {
		if err := EnsureBlockFilesystem(cfg.WorkspaceDevice, "cubepod-ws"); err != nil {
			return err
		}
	}

	upperMount := "/mnt/cubepod-upper"
	if err := os.MkdirAll(upperMount, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("mount", "-t", "ext4", "-o", "noatime", cfg.RootfsDevice, upperMount).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to mount rootfs upper device: %w (%s)", err, string(out))
	}

	for _, dir := range []string{"upper", "work"} {
		if err := os.MkdirAll(filepath.Join(upperMount, dir), 0o755); err != nil {
			return err
		}
	}

	slog.Info("persistent block layers verified and mounted", "upperMount", upperMount)
	return nil
}
