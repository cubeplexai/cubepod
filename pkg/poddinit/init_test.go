package poddinit_test

import (
	"os"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/poddinit"
)

func TestSetupAndPivotGracefulWhenNoBlockDevice(t *testing.T) {
	cfg := poddinit.DefaultConfig([]string{"/usr/local/bin/podd"})
	cfg.RootfsDevice = "/dev/nonexistent-cubepod-rootfs-test"
	if err := poddinit.SetupAndPivot(cfg); err != nil {
		t.Fatalf("expected graceful fallback when block device is absent, got: %v", err)
	}
}

func TestDefaultConfigOverrides(t *testing.T) {
	_ = os.Setenv("CUBEPOD_ROOTFS_DEV", "/dev/vdb")
	defer os.Unsetenv("CUBEPOD_ROOTFS_DEV")

	cfg := poddinit.DefaultConfig(nil)
	if cfg.RootfsDevice != "/dev/vdb" {
		t.Fatalf("expected /dev/vdb, got %s", cfg.RootfsDevice)
	}
	if len(cfg.NextArgv) == 0 || cfg.NextArgv[0] != "/usr/local/bin/podd" {
		t.Fatalf("unexpected default NextArgv: %v", cfg.NextArgv)
	}
}
