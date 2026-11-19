package volume

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"time"
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

func newMockKMSServer(t *testing.T, expectedBearer, expectedKeyID string, useTLS bool) *httptest.Server {
	t.Helper()
	kek := bytes.Repeat([]byte{0x42}, 32)
	local, err := NewLocalKEKProvider(expectedKeyID, kek)
	if err != nil {
		t.Fatalf("NewLocalKEKProvider: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectedBearer != "" && r.Header.Get("Authorization") != "Bearer "+expectedBearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/wrap":
			var req kmsWrapRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.KeyID != expectedKeyID {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			pt, err := base64.StdEncoding.DecodeString(req.Plaintext)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			wrapped, err := local.Wrap(r.Context(), pt)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(kmsWrapResponse{
				KeyID:      expectedKeyID,
				Ciphertext: base64.StdEncoding.EncodeToString(wrapped),
			})
		case "/unwrap":
			var req kmsUnwrapRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.KeyID != expectedKeyID {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ct, err := base64.StdEncoding.DecodeString(req.Ciphertext)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			pt, err := local.Unwrap(r.Context(), ct)
			if err != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(kmsUnwrapResponse{
				Plaintext: base64.StdEncoding.EncodeToString(pt),
			})
		case "/redirect/wrap":
			http.Redirect(w, r, "https://attacker.invalid/leak", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(handler)
	} else {
		srv = httptest.NewServer(handler)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestKMS_LocalKEKAndHTTPKMSWrapUnwrapAndRedaction(t *testing.T) {
	rawHex := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	rawB64 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xcd}, 32))

	if _, err := ParseKEK("not-a-valid-secret-kek-material"); err == nil || strings.Contains(err.Error(), "not-a-valid-secret-kek-material") {
		t.Fatalf("expected redacted ParseKEK error, got %v", err)
	}

	pHex, err := NewLocalKEKProviderFromString("", rawHex)
	if err != nil {
		t.Fatalf("NewLocalKEKProviderFromString hex: %v", err)
	}
	pB64, err := NewLocalKEKProviderFromString("kek-v2", rawB64)
	if err != nil {
		t.Fatalf("NewLocalKEKProviderFromString b64: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%v %#v", pHex, pHex), rawHex) {
		t.Fatal("LocalKEKProvider leaked raw KEK in string output")
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	logger.Info("kms check", "local", pHex)
	if strings.Contains(logBuf.String(), rawHex) {
		t.Fatal("LocalKEKProvider leaked KEK in slog output")
	}

	wrapped, err := pHex.Wrap(context.Background(), []byte("32-byte-dek-material-placeholder"))
	if err != nil {
		t.Fatalf("LocalKEK Wrap: %v", err)
	}
	unwrapped, err := pHex.Unwrap(context.Background(), wrapped)
	if err != nil || string(unwrapped) != "32-byte-dek-material-placeholder" {
		t.Fatalf("LocalKEK Unwrap: err=%v got=%q", err, unwrapped)
	}
	if _, err := pB64.Unwrap(context.Background(), wrapped); err == nil {
		t.Fatal("expected unwrap with wrong KEK to fail")
	}

	// Non-loopback plaintext HTTP must be rejected by default.
	if _, err := NewHTTPKMSProvider(HTTPKMSConfig{URL: "http://kms.example.com/v1"}); err == nil {
		t.Fatal("expected error for non-loopback http:// KMS URL")
	}

	// HTTPS TLS mock KMS server with bearer auth + TLS verification + redaction.
	const bearerSecret = "kms-super-secret-bearer-token"
	tlsSrv := newMockKMSServer(t, bearerSecret, "ext-key-1", true)
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())

	httpKMS, err := NewHTTPKMSProvider(HTTPKMSConfig{
		URL:         tlsSrv.URL,
		BearerToken: bearerSecret,
		KeyID:       "ext-key-1",
		Timeout:     3 * time.Second,
		TLSConfig:   &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatalf("NewHTTPKMSProvider TLS: %v", err)
	}
	logger.Info("http kms check", "remote", httpKMS)
	if strings.Contains(fmt.Sprintf("%v %#v %s", httpKMS, httpKMS, logBuf.String()), bearerSecret) {
		t.Fatal("HTTPKMSProvider leaked bearer token")
	}

	extWrapped, err := httpKMS.Wrap(context.Background(), bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("HTTPKMS Wrap: %v", err)
	}
	extPlain, err := httpKMS.Unwrap(context.Background(), extWrapped)
	if err != nil || !bytes.Equal(extPlain, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatalf("HTTPKMS Unwrap: err=%v got=%x", err, extPlain)
	}

	// Wrong bearer token must fail without leaking bearer token in error.
	badAuthKMS, err := NewHTTPKMSProvider(HTTPKMSConfig{
		URL:         tlsSrv.URL,
		BearerToken: "wrong-secret-token",
		KeyID:       "ext-key-1",
		TLSConfig:   &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badAuthKMS.Wrap(context.Background(), bytes.Repeat([]byte{0x11}, 32)); err == nil || strings.Contains(err.Error(), "wrong-secret-token") {
		t.Fatalf("expected redacted auth failure, got %v", err)
	}

	// Redirect responses must be refused so Authorization headers never follow redirects.
	redirKMS, err := NewHTTPKMSProvider(HTTPKMSConfig{
		URL:         tlsSrv.URL + "/redirect",
		BearerToken: bearerSecret,
		KeyID:       "ext-key-1",
		TLSConfig:   &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redirKMS.Wrap(context.Background(), bytes.Repeat([]byte{0x11}, 32)); err == nil {
		t.Fatal("expected redirect response to be rejected")
	}
}

func TestStore_KMSEnvelopeEncryptionAndLegacyPlaintextMigration(t *testing.T) {
	dir := t.TempDir()
	key := "cpod_test_kms_tenant"

	// 1. Legacy store (no KMS): meta.json stores plaintext "token".
	legacyStore, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	legacyVol, err := legacyStore.Create(key, "legacy-vol")
	if err != nil {
		t.Fatalf("create legacy-vol: %v", err)
	}
	legacyMetaBytes, err := os.ReadFile(filepath.Join(dir, legacyVol.VolumeID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyMetaBytes), legacyVol.Token) || strings.Contains(string(legacyMetaBytes), "encryptedToken") {
		t.Fatalf("expected legacy plaintext meta.json, got %s", legacyMetaBytes)
	}

	// 2. Reopen with LocalKEKProvider: legacy record is migrated on disk to envelope encryption,
	// while in-memory Get and Authenticate preserve 100% token compatibility.
	kekProv, err := NewLocalKEKProvider("kek-primary", bytes.Repeat([]byte{0x77}, 32))
	if err != nil {
		t.Fatal(err)
	}
	kmsStore, err := Open(dir, Config{KMS: kekProv})
	if err != nil {
		t.Fatalf("reopen with LocalKEKProvider: %v", err)
	}
	migratedMetaBytes, err := os.ReadFile(filepath.Join(dir, legacyVol.VolumeID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(migratedMetaBytes), legacyVol.Token) {
		t.Fatalf("plaintext token still present in migrated meta.json: %s", migratedMetaBytes)
	}
	var diskMigrated Volume
	if err := json.Unmarshal(migratedMetaBytes, &diskMigrated); err != nil {
		t.Fatal(err)
	}
	if diskMigrated.Token != "" || diskMigrated.EncryptedToken == "" || diskMigrated.KMSKeyID != "kek-primary" {
		t.Fatalf("unexpected migrated disk meta: %+v", diskMigrated)
	}
	gotLegacy, err := kmsStore.Get(key, legacyVol.VolumeID)
	if err != nil || gotLegacy.Token != legacyVol.Token {
		t.Fatalf("Get migrated legacy volume: err=%v got=%+v", err, gotLegacy)
	}
	if _, err := kmsStore.Authenticate(legacyVol.VolumeID, legacyVol.Token); err != nil {
		t.Fatalf("Authenticate migrated legacy token failed: %v", err)
	}

	// 3. Create new volume with mock HTTPKMSProvider on a fresh store dir and verify AAD binding.
	httpDir := t.TempDir()
	mockSrv := newMockKMSServer(t, "tok-http-kms", "vault-key-v1", false)
	httpProv, err := NewHTTPKMSProvider(HTTPKMSConfig{
		URL:         mockSrv.URL,
		BearerToken: "tok-http-kms",
		KeyID:       "vault-key-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpStore, err := Open(httpDir, Config{KMS: httpProv})
	if err != nil {
		t.Fatalf("open with HTTPKMSProvider: %v", err)
	}
	v1, err := httpStore.Create(key, "kms-vol-1")
	if err != nil {
		t.Fatalf("create kms-vol-1: %v", err)
	}
	v2, err := httpStore.Create(key, "kms-vol-2")
	if err != nil {
		t.Fatalf("create kms-vol-2: %v", err)
	}
	v1DiskBytes, err := os.ReadFile(filepath.Join(httpDir, v1.VolumeID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(v1DiskBytes), v1.Token) {
		t.Fatalf("new KMS volume leaked plaintext token on disk: %s", v1DiskBytes)
	}

	// Reopening with HTTPKMSProvider unwraps tokens cleanly.
	httpStoreReopened, err := Open(httpDir, Config{KMS: httpProv})
	if err != nil {
		t.Fatalf("reopen HTTPKMS store: %v", err)
	}
	if _, err := httpStoreReopened.Authenticate(v1.VolumeID, v1.Token); err != nil {
		t.Fatalf("Authenticate after HTTPKMS reopen: %v", err)
	}

	// Reopening an encrypted store WITHOUT KMS must fail rather than silently dropping volumes.
	if _, err := Open(httpDir, Config{}); err == nil {
		t.Fatal("expected Open without KMS on encrypted store to fail")
	}

	// Transplanting v1's encryptedToken into v2's volumeID must fail GCM AAD authentication.
	var v1Disk Volume
	_ = json.Unmarshal(v1DiskBytes, &v1Disk)
	if _, err := openVolumeToken(context.Background(), httpProv, v2.VolumeID, v1Disk.EncryptedToken); err == nil {
		t.Fatal("expected cross-volume encryptedToken transplant to fail AAD check")
	}
}

func TestStore_RotateAPIKey_MigrationRevocationMountsAndRWX(t *testing.T) {
	dir := t.TempDir()
	kekProv, err := NewLocalKEKProvider("rot-kek", bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st1, err := Open(dir, Config{KMS: kekProv})
	if err != nil {
		t.Fatal(err)
	}
	// Second open store over the same RWX directory simulates a peer Gateway HA replica.
	st2, err := Open(dir, Config{KMS: kekProv})
	if err != nil {
		t.Fatal(err)
	}

	oldKey := "cpod_old_api_key"
	newKey := "cpod_new_api_key"
	thirdKey := "cpod_third_api_key"
	otherTenantKey := "cpod_unrelated_tenant_key"

	v1, err := st1.Create(oldKey, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := st1.Create(oldKey, "beta")
	if err != nil {
		t.Fatal(err)
	}
	otherVol, err := st1.Create(otherTenantKey, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st1.WriteFile(v1.VolumeID, "/dataset.txt", strings.NewReader("payload-1"), WriteOpts{}); err != nil {
		t.Fatal(err)
	}

	planned, err := st1.LookupMounts(oldKey, []MountRequest{{Name: "alpha", Path: "/mnt/alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.CommitMounts("sb-rot-1", oldKey, planned); err != nil {
		t.Fatal(err)
	}
	// Reload st2 so it sees initial volumes before rotation.
	st2, err = Open(dir, Config{KMS: kekProv})
	if err != nil {
		t.Fatal(err)
	}

	// Validation & collision checks.
	if _, err := st1.RotateAPIKey("", newKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty oldKey: got %v, want ErrInvalidKey", err)
	}
	if _, err := st1.RotateAPIKey(oldKey, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty newKey: got %v, want ErrInvalidKey", err)
	}
	if _, err := st1.RotateAPIKey(oldKey, oldKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("identical keys: got %v, want ErrInvalidKey", err)
	}
	if _, err := st1.RotateAPIKey(oldKey, otherTenantKey); !errors.Is(err, ErrConflict) {
		t.Fatalf("collision into otherTenantKey: got %v, want ErrConflict", err)
	}

	// Execute key rotation on st1.
	rotated, err := st1.RotateAPIKey(oldKey, newKey)
	if err != nil || rotated != 2 {
		t.Fatalf("RotateAPIKey: rotated=%d err=%v", rotated, err)
	}
	if modeOf(t, filepath.Join(dir, "tenants.json"))&0o077 != 0 {
		t.Fatalf("tenants.json permissions too broad: %#o", modeOf(t, filepath.Join(dir, "tenants.json")))
	}

	// Old key must be strictly denied across all management and mount APIs on BOTH st1 and live peer st2.
	for idx, s := range []*Store{st1, st2} {
		if _, err := s.List(oldKey); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] List(oldKey): got %v, want ErrUnauthorized", idx, err)
		}
		if _, err := s.Get(oldKey, v1.VolumeID); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] Get(oldKey): got %v, want ErrUnauthorized", idx, err)
		}
		if _, err := s.Delete(oldKey, v2.VolumeID); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] Delete(oldKey): got %v, want ErrUnauthorized", idx, err)
		}
		if _, err := s.Create(oldKey, "gamma"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] Create(oldKey): got %v, want ErrUnauthorized", idx, err)
		}
		if _, err := s.LookupMounts(oldKey, []MountRequest{{Name: "alpha", Path: "/mnt/a"}}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] LookupMounts(oldKey): got %v, want ErrUnauthorized", idx, err)
		}
		if _, err := s.RotateAPIKey(oldKey, thirdKey); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("store[%d] RotateAPIKey(revoked oldKey): got %v, want ErrUnauthorized", idx, err)
		}

		listed, err := s.List(newKey)
		if err != nil || len(listed) != 2 {
			t.Fatalf("store[%d] List(newKey): len=%d err=%v", idx, len(listed), err)
		}
		gotV1, err := s.Get(newKey, v1.VolumeID)
		if err != nil || gotV1.Token != v1.Token {
			t.Fatalf("store[%d] Get(newKey, v1): err=%v got=%+v", idx, err, gotV1)
		}
	}

	// Unrelated tenant must be completely untouched.
	gotOther, err := st1.Get(otherTenantKey, otherVol.VolumeID)
	if err != nil || gotOther.VolumeID != otherVol.VolumeID {
		t.Fatalf("other tenant affected by rotation: err=%v got=%+v", err, gotOther)
	}

	// Reopen from disk: verify persistence of rotation + active mount protection + mount cleanup.
	reopened, err := Open(dir, Config{KMS: kekProv})
	if err != nil {
		t.Fatalf("reopen after RotateAPIKey: %v", err)
	}
	if _, err := reopened.Get(oldKey, v1.VolumeID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reopened Get(oldKey): got %v, want ErrUnauthorized", err)
	}
	if specs := reopened.MountsForSandbox("sb-rot-1"); len(specs) != 1 || specs[0].VolumeID != v1.VolumeID {
		t.Fatalf("expected active mount preserved across rotation + reopen, got %+v", specs)
	}
	if _, err := reopened.Delete(newKey, v1.VolumeID); !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse before mount cleanup, got %v", err)
	}
	reopened.UnmountSandbox("sb-rot-1")
	if _, err := reopened.Delete(newKey, v1.VolumeID); err != nil {
		t.Fatalf("expected Delete(newKey, v1) to succeed after mount cleanup: %v", err)
	}

	// Chained rotation (newKey -> thirdKey) and rejection of rotating back onto revoked oldKey.
	if _, err := reopened.RotateAPIKey(newKey, oldKey); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict rotating onto previously revoked key, got %v", err)
	}
	rot2, err := reopened.RotateAPIKey(newKey, thirdKey)
	if err != nil || rot2 != 1 {
		t.Fatalf("second RotateAPIKey: rot=%d err=%v", rot2, err)
	}
	if _, err := reopened.List(newKey); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected newKey revoked after second rotation, got %v", err)
	}
	gotV2, err := reopened.Get(thirdKey, v2.VolumeID)
	if err != nil || gotV2.VolumeID != v2.VolumeID {
		t.Fatalf("expected thirdKey to own v2: err=%v got=%+v", err, gotV2)
	}
}

func TestStore_MultiTenantByteQuotaOverwriteAndCapacityStats(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{
		MaxVolumesPerTenant: 3,
		MaxBytesPerTenant:   100,
	})
	if err != nil {
		t.Fatal(err)
	}

	tenantA := "cpod_quota_tenant_a"
	tenantB := "cpod_quota_tenant_b"

	vA1, err := st.Create(tenantA, "vol-a1")
	if err != nil {
		t.Fatal(err)
	}
	vA2, err := st.Create(tenantA, "vol-a2")
	if err != nil {
		t.Fatal(err)
	}
	vB1, err := st.Create(tenantB, "vol-b1")
	if err != nil {
		t.Fatal(err)
	}

	// Tenant A writes 60 + 35 = 95 bytes across two volumes.
	if _, err := st.WriteFile(vA1.VolumeID, "/f1.bin", strings.NewReader(strings.Repeat("a", 60)), WriteOpts{}); err != nil {
		t.Fatalf("write 60B: %v", err)
	}
	if _, err := st.WriteFile(vA2.VolumeID, "/f2.bin", strings.NewReader(strings.Repeat("b", 35)), WriteOpts{}); err != nil {
		t.Fatalf("write 35B: %v", err)
	}

	// Adding 10B (total 105 > 100) must fail with ErrLimitExceeded and leave no partial file.
	if _, err := st.WriteFile(vA2.VolumeID, "/f3.bin", strings.NewReader(strings.Repeat("c", 10)), WriteOpts{}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded on 105B total, got %v", err)
	}
	if _, err := st.Stat(vA2.VolumeID, "/f3.bin"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("rejected quota upload should not exist, got %v", err)
	}

	// Overwriting 60B /f1.bin with 65B brings total to 95 - 60 + 65 = 100 <= 100 -> must succeed!
	if _, err := st.WriteFile(vA1.VolumeID, "/f1.bin", strings.NewReader(strings.Repeat("z", 65)), WriteOpts{}); err != nil {
		t.Fatalf("overwrite within 100B quota failed: %v", err)
	}

	// Tenant B is isolated and can write its own 90 bytes unaffected by Tenant A being at 100%.
	if _, err := st.WriteFile(vB1.VolumeID, "/b.bin", strings.NewReader(strings.Repeat("q", 90)), WriteOpts{}); err != nil {
		t.Fatalf("tenant B isolated write failed: %v", err)
	}

	// Removing files frees Tenant A's quota; verify concurrent writes under -race never exceed 100B.
	if err := st.Remove(vA1.VolumeID, "/f1.bin"); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(vA2.VolumeID, "/f2.bin"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			targetVol := vA1.VolumeID
			if idx%2 == 1 {
				targetVol = vA2.VolumeID
			}
			_, _ = st.WriteFile(targetVol, fmt.Sprintf("/race-%d.bin", idx), strings.NewReader(strings.Repeat("x", 40)), WriteOpts{})
		}(i)
	}
	wg.Wait()

	usedA, err := st.tenantUsedBytes(rawKeyHash(tenantA))
	if err != nil {
		t.Fatal(err)
	}
	if usedA > 100 || usedA != 80 {
		t.Fatalf("expected concurrent writes capped at 80B (2x40B <= 100B), got %d", usedA)
	}

	cs, err := st.CapacityStats()
	if err != nil {
		t.Fatalf("CapacityStats: %v", err)
	}
	if cs.CapacityBytes == 0 || cs.AvailableBytes > cs.CapacityBytes || cs.VolumesTotal != 3 {
		t.Fatalf("unexpected CapacityStats: %+v", cs)
	}
}
