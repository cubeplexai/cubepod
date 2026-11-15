package volume

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestStore_SymlinkEscapeBlockedAcrossAllOps(t *testing.T) {
	dir := t.TempDir()
	hostEscapeDir := t.TempDir()
	hostFile := filepath.Join(hostEscapeDir, "secret.txt")
	if err := os.WriteFile(hostFile, []byte("host-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	vol, err := st.Create("cpod_test_api_key", "cpod-test-sym")
	if err != nil {
		t.Fatal(err)
	}

	// Symlink directory inside data/ pointing to hostEscapeDir.
	dirLink := filepath.Join(vol.DataDir, "dirlink")
	if err := os.Symlink(hostEscapeDir, dirLink); err != nil {
		t.Fatal(err)
	}
	// Symlink file inside data/ pointing to hostFile.
	fileLink := filepath.Join(vol.DataDir, "filelink")
	if err := os.Symlink(hostFile, fileLink); err != nil {
		t.Fatal(err)
	}

	// WriteFile through symlink dir or onto symlink file must fail and never touch host.
	if _, err := st.WriteFile(vol.VolumeID, "/dirlink/pwn.txt", strings.NewReader("pwn"), WriteOpts{}); err == nil {
		t.Fatal("WriteFile through symlink dir succeeded")
	}
	if _, err := st.WriteFile(vol.VolumeID, "/filelink", strings.NewReader("pwn"), WriteOpts{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("WriteFile onto symlink file: got %v, want ErrConflict", err)
	}
	if _, err := os.Stat(filepath.Join(hostEscapeDir, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatalf("WriteFile escaped onto host: %v", err)
	}
	got, _ := os.ReadFile(hostFile)
	if string(got) != "host-secret" {
		t.Fatalf("hostFile was modified: %q", got)
	}

	// OpenFile must reject both symlink file and file under symlink dir.
	if f, err := st.OpenFile(vol.VolumeID, "/filelink"); err == nil {
		f.Close()
		t.Fatal("OpenFile followed symlink file")
	}
	if f, err := st.OpenFile(vol.VolumeID, "/dirlink/secret.txt"); err == nil {
		f.Close()
		t.Fatal("OpenFile traversed symlink dir")
	}

	// Patch must reject symlinks and never chmod hostFile.
	m777 := 0o777
	if _, err := st.Patch(vol.VolumeID, "/filelink", WriteOpts{Mode: &m777}); err == nil {
		t.Fatal("Patch followed symlink")
	}
	if _, err := st.Patch(vol.VolumeID, "/dirlink/secret.txt", WriteOpts{Mode: &m777}); err == nil {
		t.Fatal("Patch traversed symlink dir")
	}
	if modeOf(t, hostFile) != 0o600 {
		t.Fatalf("hostFile mode changed to %#o", modeOf(t, hostFile))
	}

	// MakeDir must reject symlink target and symlink parent.
	if _, err := st.MakeDir(vol.VolumeID, "/dirlink", WriteOpts{}); err == nil {
		t.Fatal("MakeDir accepted symlink target")
	}
	if _, err := st.MakeDir(vol.VolumeID, "/dirlink/sub", WriteOpts{Force: true}); err == nil {
		t.Fatal("MakeDir traversed symlink parent")
	}
	if _, err := os.Stat(filepath.Join(hostEscapeDir, "sub")); !os.IsNotExist(err) {
		t.Fatalf("MakeDir escaped onto host: %v", err)
	}

	// ListDir must reject symlink dir.
	if _, err := st.ListDir(vol.VolumeID, "/dirlink", 1); err == nil {
		t.Fatal("ListDir followed symlink dir")
	}

	// Remove on dirlink removes the symlink itself, NOT hostEscapeDir contents.
	if err := st.Remove(vol.VolumeID, "/dirlink"); err != nil {
		t.Fatalf("Remove symlink: %v", err)
	}
	if _, err := os.Stat(hostFile); err != nil {
		t.Fatalf("Remove symlink deleted hostFile: %v", err)
	}
}

func TestStore_ConcurrentSymlinkSwapRace(t *testing.T) {
	dir := t.TempDir()
	hostEscapeDir := t.TempDir()
	hostFile := filepath.Join(hostEscapeDir, "victim.txt")
	if err := os.WriteFile(hostFile, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	vol, err := st.Create("cpod_test_api_key", "cpod-test-race")
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		target := filepath.Join(vol.DataDir, "victim.txt")
		sub := filepath.Join(vol.DataDir, "sub")
		for {
			select {
			case <-stop:
				return
			default:
				_ = os.Remove(target)
				_ = os.Symlink(hostFile, target)
				_ = os.Remove(target)
				_ = os.WriteFile(target, []byte("ok"), 0o644)
				_ = os.RemoveAll(sub)
				_ = os.Symlink(hostEscapeDir, sub)
				_ = os.Remove(sub)
				_ = os.Mkdir(sub, 0o755)
			}
		}
	}()

	m644 := 0o644
	for i := 0; i < 40; i++ {
		_, _ = st.WriteFile(vol.VolumeID, "/victim.txt", strings.NewReader("update"), WriteOpts{Mode: &m644})
		_, _ = st.Patch(vol.VolumeID, "/victim.txt", WriteOpts{Mode: &m644})
		_, _ = st.MakeDir(vol.VolumeID, "/sub/nested", WriteOpts{Force: true})
		_, _ = st.WriteFile(vol.VolumeID, "/sub/victim.txt", strings.NewReader("update"), WriteOpts{Force: true})
	}
	close(stop)
	wg.Wait()

	got, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "intact" {
		t.Fatalf("race modified hostFile: %q", got)
	}
	if modeOf(t, hostFile) != 0o600 {
		t.Fatalf("race modified hostFile mode: %#o", modeOf(t, hostFile))
	}
	if _, err := os.Stat(filepath.Join(hostEscapeDir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("race created nested dir on host: %v", err)
	}
}

func TestStore_StripsSetuidAndPreservesMetadata(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	vol, err := st.Create("cpod_test_api_key", "cpod-test-setuid")
	if err != nil {
		t.Fatal(err)
	}
	uid0 := 0
	setuidMode := 0o4755
	ent, err := st.WriteFile(vol.VolumeID, "/bin.sh", strings.NewReader("#!/bin/sh\n"), WriteOpts{
		UID:  &uid0,
		Mode: &setuidMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ent.Mode != 0o755 {
		t.Fatalf("expected masked mode 0755, got %#o", ent.Mode)
	}
	if ent.UID != 0 {
		t.Fatalf("expected metadata UID 0, got %d", ent.UID)
	}
	fi, err := os.Lstat(filepath.Join(vol.DataDir, "bin.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("on-disk file has special mode bits: %v", fi.Mode())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("on-disk perm %#o, want 0755", fi.Mode().Perm())
	}
}

func TestStore_QuotaDotDotAndReconcileMounts(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{MaxVolumesPerTenant: 2})
	if err != nil {
		t.Fatal(err)
	}
	key := "cpod_test_api_key"
	v1, err := st.Create(key, "cpod-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(key, "cpod-test-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(key, "cpod-test-3"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}

	// /mnt/a..b must be allowed; /mnt/../x must be rejected.
	if _, err := st.WriteFile(v1.VolumeID, "/a..b", strings.NewReader("ok"), WriteOpts{}); err != nil {
		t.Fatalf("legitimate double-dot filename rejected: %v", err)
	}
	if _, err := st.WriteFile(v1.VolumeID, "/../escape", strings.NewReader("no"), WriteOpts{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath for /../escape, got %v", err)
	}

	// Mount v1 on stale-sb, then reconcile with only active-sb -> Delete succeeds.
	planned, err := st.LookupMounts(key, []MountRequest{{Name: v1.Name, Path: "/mnt/data"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMounts("stale-sb", key, planned); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(key, v1.VolumeID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse before reconcile, got %v", err)
	}
	removed := st.ReconcileMounts(map[string]struct{}{"active-sb": {}})
	if removed != 1 {
		t.Fatalf("expected 1 stale mount removed, got %d", removed)
	}
	if _, err := st.Delete(key, v1.VolumeID); err != nil {
		t.Fatalf("Delete after ReconcileMounts failed: %v", err)
	}
}

func TestDockerClient_EnsureBindVolumeNosuidAnd409DeviceCheck(t *testing.T) {
	var createBody map[string]any
	device := "/var/lib/cubepod/e2b/vol1/data"
	inspectDevice := device
	inspectCode := http.StatusOK
	createCode := http.StatusCreated

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/volumes/create"):
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			w.WriteHeader(createCode)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/volumes/"):
			w.WriteHeader(inspectCode)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Name":    "cpod-test-vol-1",
				"Options": map[string]string{"device": inspectDevice},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dc := dockerClient{host: srv.URL, enabled: true}
	if err := dc.ensureBindVolume("cpod-test-vol-1", device); err != nil {
		t.Fatalf("ensureBindVolume create: %v", err)
	}
	opts, _ := createBody["DriverOpts"].(map[string]any)
	if opts["o"] != "bind,nosuid,nodev" {
		t.Fatalf("expected bind,nosuid,nodev driver option, got %#v", opts)
	}

	// 409 with matching device succeeds.
	createCode = http.StatusConflict
	if err := dc.ensureBindVolume("cpod-test-vol-1", device); err != nil {
		t.Fatalf("ensureBindVolume 409 matching device: %v", err)
	}

	// 409 with mismatched device fails.
	inspectDevice = "/other/path"
	if err := dc.ensureBindVolume("cpod-test-vol-1", device); err == nil {
		t.Fatal("expected error on 409 device mismatch")
	}
}

func TestStore_NotePVC_SharedRWX(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	v, err := st.Create("key-1", "shared-vol")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.NotePVC(v.VolumeID, "cubepod-e2b-volumes", "cubepod-system"); err != nil {
		t.Fatalf("NotePVC shared RWX failed: %v", err)
	}
	if err := st.NotePVC(v.VolumeID, "../bad", "cubepod-system"); err == nil {
		t.Fatal("expected error for invalid PVC name")
	}

	reopened, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := reopened.Get("key-1", v.VolumeID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.PVCName != "cubepod-e2b-volumes" || got.PVCNamespace != "cubepod-system" {
		t.Fatalf("unexpected persisted PVC metadata: %+v", got)
	}
}
