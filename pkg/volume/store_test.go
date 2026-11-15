package volume

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("CUBE_VOLUME_DROP") == "1" {
		uid, _ := strconv.Atoi(os.Getenv("CUBE_VOLUME_UID"))
		gid, _ := strconv.Atoi(os.Getenv("CUBE_VOLUME_GID"))
		target := os.Getenv("CUBE_VOLUME_PATH")
		if err := syscall.Setgroups([]int{gid}); err != nil {
			fmt.Fprintf(os.Stderr, "setgroups: %v\n", err)
			os.Exit(10)
		}
		if err := syscall.Setgid(gid); err != nil {
			fmt.Fprintf(os.Stderr, "setgid: %v\n", err)
			os.Exit(11)
		}
		if err := syscall.Setuid(uid); err != nil {
			fmt.Fprintf(os.Stderr, "setuid: %v\n", err)
			os.Exit(12)
		}
		f, err := os.Open(target)
		if err == nil {
			_ = f.Close()
			os.Exit(0)
		}
		if os.IsPermission(err) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "open %s: %v\n", target, err)
		os.Exit(13)
	}
	os.Exit(m.Run())
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestStorePermissionsAndMountPaths(t *testing.T) {
	dir := t.TempDir()
	// A previous process may have left the root group-readable. Open tightens it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if modeOf(t, st.Dir())&0o077 != 0 {
		t.Fatalf("store root mode %#o", modeOf(t, st.Dir()))
	}

	vol, err := st.Create("cpod_test_api_key", "cpod-test-secure")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Token == "" {
		t.Fatal("create omitted token")
	}
	parent := filepath.Join(st.Dir(), vol.VolumeID)
	meta := filepath.Join(parent, "meta.json")
	data := filepath.Join(parent, "data")
	if modeOf(t, parent)&0o077 != 0 {
		t.Fatalf("volume parent mode %#o", modeOf(t, parent))
	}
	if modeOf(t, meta)&0o077 != 0 {
		t.Fatalf("meta mode %#o", modeOf(t, meta))
	}
	dataMode := modeOf(t, data)
	if dataMode&0o002 != 0 {
		t.Fatalf("data dir is world-writable: %#o", dataMode)
	}
	if filepath.Base(data) != "data" {
		t.Fatalf("data dir %s", data)
	}

	listed, err := st.List("cpod_test_api_key")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Token != "" {
		t.Fatalf("list leaked token: %+v", listed)
	}
	if _, err := st.Authenticate(vol.VolumeID, "not-the-token"); err != ErrUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := st.Authenticate(vol.VolumeID, vol.Token); err != nil {
		t.Fatal(err)
	}

	ent, err := st.WriteFile(vol.VolumeID, "/hello.txt", strings.NewReader("hi"), WriteOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if ent.Mode != 0o644 {
		t.Fatalf("default file mode %#o", ent.Mode)
	}
	fileMode := modeOf(t, filepath.Join(data, "hello.txt"))
	if fileMode&0o002 != 0 || fileMode != 0o644 {
		t.Fatalf("file mode %#o", fileMode)
	}
	if modeOf(t, filepath.Join(parent, "attrs.json"))&0o077 != 0 {
		t.Fatalf("attrs mode %#o", modeOf(t, filepath.Join(parent, "attrs.json")))
	}
	if os.Getuid() == 0 {
		fi, err := os.Stat(filepath.Join(data, "hello.txt"))
		if err != nil {
			t.Fatal(err)
		}
		stt := fi.Sys().(*syscall.Stat_t)
		if stt.Uid != defaultDataUID || stt.Gid != defaultDataGID {
			t.Fatalf("file owner %d:%d", stt.Uid, stt.Gid)
		}
	}

	uid, gid := 10, 20
	mode := 0o640
	ent, err = st.WriteFile(vol.VolumeID, "/owned.txt", strings.NewReader("x"), WriteOpts{UID: &uid, GID: &gid, Mode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if ent.UID != uid || ent.GID != gid || ent.Mode != mode {
		t.Fatalf("explicit owner: %+v", ent)
	}

	other, err := st.Create("cpod_test_other_key", "cpod-test-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupMounts("cpod_test_api_key", []MountRequest{{Name: "cpod-test-secure", Path: "/workspace"}}); err != ErrInvalidPath {
		t.Fatalf("reserved /workspace: %v", err)
	}
	if _, err := st.LookupMounts("cpod_test_api_key", []MountRequest{{Name: "cpod-test-secure", Path: "/mnt/cubepod/data"}}); err != ErrInvalidPath {
		t.Fatalf("reserved /mnt/cubepod: %v", err)
	}
	planned, err := st.LookupMounts("cpod_test_api_key", []MountRequest{{Name: "cpod-test-secure", Path: "/mnt/cpod-test-vol"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMounts("sb-1", "cpod_test_api_key", planned); err != nil {
		t.Fatal(err)
	}
	if modeOf(t, filepath.Join(st.Dir(), "mounts.json"))&0o077 != 0 {
		t.Fatalf("mounts mode %#o", modeOf(t, filepath.Join(st.Dir(), "mounts.json")))
	}
	specs := st.MountsForSandbox("sb-1")
	if len(specs) != 1 || filepath.Base(specs[0].HostPath) != "data" || !strings.Contains(specs[0].HostPath, vol.VolumeID) {
		t.Fatalf("mount spec %+v", specs)
	}
	if specs[0].HostPath == st.Dir() || specs[0].HostPath == parent || specs[0].LocalPath == parent {
		t.Fatalf("mount exposed metadata: %+v", specs[0])
	}
	_, err = st.LookupMounts("cpod_test_other_key", []MountRequest{{Name: "cpod-test-secure", Path: "/mnt/data"}})
	if err != ErrNotFound {
		t.Fatalf("cross tenant mount: %v", err)
	}

	// Re-open must keep the root private even if something loosened it.
	if err := os.Chmod(st.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Config{}); err != nil {
		t.Fatal(err)
	}
	if modeOf(t, dir)&0o077 != 0 {
		t.Fatalf("reopen root mode %#o", modeOf(t, dir))
	}

	if os.Getuid() != 0 {
		t.Log("skipping non-owner traversal check: not root")
		return
	}
	sibling := filepath.Join(st.Dir(), other.VolumeID, "data")
	for _, target := range []string{st.Dir(), sibling, filepath.Join(st.Dir(), other.VolumeID)} {
		assertNotTraversable(t, target)
	}
}

func assertNotTraversable(t *testing.T, target string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(),
		"CUBE_VOLUME_DROP=1",
		"CUBE_VOLUME_UID=1000",
		"CUBE_VOLUME_GID=1000",
		"CUBE_VOLUME_PATH="+target,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("uid 1000 opened %s: %s", target, out)
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("drop uid: %v %s", err, out)
	}
	if exit.ExitCode() != 2 {
		t.Fatalf("uid 1000 open %s exit %d: %s", target, exit.ExitCode(), out)
	}
}
