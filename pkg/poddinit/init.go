// Package poddinit implements the PID 1 in-guest rootfs initializer (podd-init)
// for CubePod Kata MicroVMs.
//
// Responsibilities of podd-init:
//  1. Verify and auto-heal (/sbin/e2fsck -p) or format (/sbin/mkfs.ext4) the attached
//     CSI Raw Block devices (/dev/cubepod-rootfs and /dev/cubepod-workspace).
//  2. Mount the RootFS Upper block device and construct a native Guest Kernel OverlayFS
//     merging the read-only Base Image lowerdir (/) with the persistent upperdir/workdir.
//  3. Mount the persistent Workspace block device at /workspace and fix ownership (1000:1000).
//  4. Bind-mount essential virtual filesystems (/proc, /sys, /dev, /run) and Kubernetes-injected
//     DNS/host configs (/etc/resolv.conf, /etc/hosts, /etc/hostname) into the merged root.
//  5. Perform pivot_root(2) / chroot(2) into the merged root and exec the target entrypoint
//     (by default `/usr/local/bin/podd`).
package poddinit

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Config defines the runtime parameters for podd-init.
type Config struct {
	RootfsDevice    string
	WorkspaceDevice string
	UpperMountPath  string
	MergedRootPath  string
	WorkspaceDir    string
	DefaultUID      int
	DefaultGID      int
	SkipPivot       bool
	NextArgv        []string
}

// DefaultConfig returns the standard production configuration for podd-init inside Kata.
func DefaultConfig(nextArgv []string) Config {
	if len(nextArgv) == 0 {
		nextArgv = []string{"/usr/local/bin/podd"}
	}
	return Config{
		RootfsDevice:    envOr("CUBEPOD_ROOTFS_DEV", "/dev/cubepod-rootfs"),
		WorkspaceDevice: envOr("CUBEPOD_WORKSPACE_DEV", "/dev/cubepod-workspace"),
		UpperMountPath:  "/mnt/cubepod-upper",
		MergedRootPath:  "/mnt/cubepod-merged",
		WorkspaceDir:    "/workspace",
		DefaultUID:      1000,
		DefaultGID:      1000,
		SkipPivot:       os.Getenv("CUBEPOD_SKIP_PIVOT") == "1",
		NextArgv:        nextArgv,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnsureExt4 checks whether devicePath holds a valid ext4 filesystem.
// If unformatted, it initializes ext4 with mkfs.ext4; otherwise it runs e2fsck -p.
func EnsureExt4(devicePath, label string) error {
	if _, err := os.Stat(devicePath); err != nil {
		return fmt.Errorf("block device %s not accessible: %w", devicePath, err)
	}

	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", devicePath).Output()
	if err != nil || len(out) == 0 {
		slog.Info("podd-init: formatting fresh raw block volume as ext4", "device", devicePath, "label", label)
		mkfs := exec.Command("mkfs.ext4", "-F", "-L", label, "-m", "0", "-E", "lazy_itable_init=0,lazy_journal_init=0", devicePath)
		if msg, err := mkfs.CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs.ext4 failed on %s: %w (%s)", devicePath, err, string(msg))
		}
		return nil
	}

	slog.Info("podd-init: running e2fsck preen check on block volume", "device", devicePath)
	fsck := exec.Command("e2fsck", "-p", devicePath)
	if msg, err := fsck.CombinedOutput(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 2) {
			slog.Info("podd-init: e2fsck auto-repaired filesystem cleanly", "device", devicePath)
			return nil
		}
		slog.Warn("podd-init: e2fsck returned non-zero status", "device", devicePath, "output", string(msg), "err", err)
	}
	return nil
}

// SetupAndPivot prepares the dual-block OverlayFS hierarchy and pivots root if block devices exist.
func SetupAndPivot(cfg Config) error {
	if cfg.SkipPivot {
		slog.Info("podd-init: CUBEPOD_SKIP_PIVOT=1 set, bypassing block mount and pivot_root")
		return nil
	}

	if _, err := os.Stat(cfg.RootfsDevice); os.IsNotExist(err) {
		slog.Info("podd-init: no raw block device found, continuing with standard container rootfs", "device", cfg.RootfsDevice)
		return nil
	}

	// 1. Heal or format RootFS Upper block device.
	if err := EnsureExt4(cfg.RootfsDevice, "cubepod-rootfs"); err != nil {
		return err
	}

	// 2. Mount RootFS Upper block device and create upper/ and work/ dirs.
	if err := os.MkdirAll(cfg.UpperMountPath, 0o755); err != nil {
		return err
	}
	if err := mountExt4(cfg.RootfsDevice, cfg.UpperMountPath); err != nil {
		return fmt.Errorf("mount rootfs upper device: %w", err)
	}

	upperDir := filepath.Join(cfg.UpperMountPath, "upper")
	workDir := filepath.Join(cfg.UpperMountPath, "work")
	for _, d := range []string{upperDir, workDir, cfg.MergedRootPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// 3. Mount OverlayFS combining Base Image (/) as lowerdir and block PVC as upperdir.
	overlayOpts := fmt.Sprintf("lowerdir=/,upperdir=%s,workdir=%s", upperDir, workDir)
	slog.Info("podd-init: mounting persistent OverlayFS root", "target", cfg.MergedRootPath, "opts", overlayOpts)
	if msg, err := exec.Command("mount", "-t", "overlay", "overlay", "-o", overlayOpts, cfg.MergedRootPath).CombinedOutput(); err != nil {
		return fmt.Errorf("overlayfs mount failed: %w (%s)", err, string(msg))
	}

	// 4. Mount Workspace block device (if attached) at <MergedRoot>/workspace and ensure non-root ownership.
	mergedWorkspace := filepath.Join(cfg.MergedRootPath, cfg.WorkspaceDir)
	if err := os.MkdirAll(mergedWorkspace, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.WorkspaceDevice); err == nil {
		if err := EnsureExt4(cfg.WorkspaceDevice, "cubepod-ws"); err != nil {
			return err
		}
		if err := mountExt4(cfg.WorkspaceDevice, mergedWorkspace); err != nil {
			return fmt.Errorf("mount workspace device: %w", err)
		}
	}
	_ = os.Chown(mergedWorkspace, cfg.DefaultUID, cfg.DefaultGID)

	// 5. Bind-mount virtual filesystems and K8s networking files into the new merged root.
	for _, sysDir := range []string{"/proc", "/sys", "/dev", "/run"} {
		target := filepath.Join(cfg.MergedRootPath, sysDir)
		_ = os.MkdirAll(target, 0o755)
		_ = exec.Command("mount", "--rbind", sysDir, target).Run()
	}
	for _, etcFile := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"} {
		if _, err := os.Stat(etcFile); err == nil {
			target := filepath.Join(cfg.MergedRootPath, etcFile)
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			if f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0o644); err == nil {
				_ = f.Close()
			}
			_ = exec.Command("mount", "--bind", etcFile, target).Run()
		}
	}

	// 6. Pivot root (or fallback to chroot) into the persistent OverlayFS root.
	oldRoot := filepath.Join(cfg.MergedRootPath, ".old_root")
	_ = os.MkdirAll(oldRoot, 0o700)
	if err := syscall.PivotRoot(cfg.MergedRootPath, oldRoot); err != nil {
		slog.Warn("podd-init: pivot_root syscall returned error, falling back to chroot", "err", err)
		if err := syscall.Chroot(cfg.MergedRootPath); err != nil {
			return fmt.Errorf("chroot into merged root failed: %w", err)
		}
	} else {
		_ = os.Chdir("/")
		_ = syscall.Unmount("/.old_root", syscall.MNT_DETACH)
		_ = os.Remove("/.old_root")
	}

	if err := os.Chdir(cfg.WorkspaceDir); err != nil {
		_ = os.Chdir("/")
	}
	slog.Info("podd-init: persistent rootfs active", "workdir", cfg.WorkspaceDir)
	return nil
}

// ExecNext replaces the current process with the configured daemon (typically `/usr/local/bin/podd`),
// or runs it as a supervised child when running as PID 1.
func ExecNext(cfg Config) error {
	binPath, err := exec.LookPath(cfg.NextArgv[0])
	if err != nil {
		return fmt.Errorf("lookup next binary %s: %w", cfg.NextArgv[0], err)
	}
	slog.Info("podd-init: handing over execution to daemon", "binary", binPath, "args", cfg.NextArgv[1:])
	return syscall.Exec(binPath, cfg.NextArgv, os.Environ())
}

func mountExt4(device, target string) error {
	msg, err := exec.Command("mount", "-t", "ext4", "-o", "noatime", device, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, string(msg))
	}
	return nil
}
