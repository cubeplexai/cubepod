package volumenfs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willscott/go-nfs"
)

func TestNewLeaseIDIsUnguessableHex(t *testing.T) {
	a, err := NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("lease IDs should be unique")
	}
	if !ValidLeaseID(a) || !ValidLeaseID(b) {
		t.Fatalf("generated IDs not valid: %s %s", a, b)
	}
}

func TestStorePutRejectsEmptyOrInvalidVolumeIDs(t *testing.T) {
	s := NewStore()
	id, _ := NewLeaseID()
	if _, err := s.Put(id, LeaseRequest{PodName: "p"}); err == nil {
		t.Fatal("expected error for missing volumeIDs")
	}
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{"not-a-volume"}}); err == nil {
		t.Fatal("expected error for invalid volumeID")
	}
}

func TestStoreDoesNotReplaceVolumeSet(t *testing.T) {
	s := NewStore()
	id, _ := NewLeaseID()
	vol1 := "0123456789abcdef0123456789abcdef"
	vol2 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol1}, PodName: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol2}, PodName: "p"}); err == nil {
		t.Fatal("expected volume set conflict")
	}
	// same set (possibly reordered) plus podIP bind is allowed
	lease, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol1}, PodIP: "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Bound() || lease.PodIP != "10.0.0.5" {
		t.Fatalf("expected bound lease, got %+v", lease)
	}
}

func TestAdmitMountRequiresBoundMatchingSourceIP(t *testing.T) {
	s := NewStore()
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol, other}, PodName: "pod", PodBoxName: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitMount(id, vol, "10.0.0.5:1234"); err != ErrLeaseUnbound {
		t.Fatalf("unbound lease must be denied, got %v", err)
	}
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol, other}, PodIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitMount(id, vol, "10.0.0.5:2049"); err != nil {
		t.Fatalf("matching source IP should be admitted: %v", err)
	}
	if err := s.AdmitMount(id, vol, "10.0.0.9:2049"); err != ErrSourceIPMismatch {
		t.Fatalf("mismatched source IP must be denied, got %v", err)
	}
	unknown := "cccccccccccccccccccccccccccccccc"
	if err := s.AdmitMount(id, unknown, "10.0.0.5:2049"); err != ErrVolumeNotAllowed {
		t.Fatalf("unknown volume must be denied, got %v", err)
	}
	if err := s.AdmitMount("00112233445566778899aabbccddeeff", vol, "10.0.0.5:2049"); err != ErrLeaseNotFound {
		t.Fatalf("unknown lease must be denied, got %v", err)
	}
}

func TestUnboundLeaseExpiresByTTL(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	clock := &now
	s := NewStore(WithUnboundTTL(60*time.Second), WithNow(func() time.Time { return *clock }))
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol}}); err != nil {
		t.Fatal(err)
	}
	*clock = now.Add(59 * time.Second)
	if _, ok := s.Get(id); !ok {
		t.Fatal("lease should still be live before TTL")
	}
	*clock = now.Add(60 * time.Second)
	if _, ok := s.Get(id); ok {
		t.Fatal("unbound lease should expire at TTL")
	}
}

func TestBoundLeaseDoesNotExpire(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	clock := &now
	s := NewStore(WithUnboundTTL(60*time.Second), WithNow(func() time.Time { return *clock }))
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodIP: "10.1.2.3"}); err != nil {
		t.Fatal(err)
	}
	*clock = now.Add(10 * time.Minute)
	if _, ok := s.Get(id); !ok {
		t.Fatal("bound lease must not expire")
	}
	s.Delete(id)
	if _, ok := s.Get(id); ok {
		t.Fatal("revoked lease must be gone")
	}
	if err := s.AdmitMount(id, vol, "10.1.2.3:1"); err != ErrLeaseNotFound {
		t.Fatalf("revoked lease must be denied, got %v", err)
	}
}

func TestParseExportPath(t *testing.T) {
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	ref, err := ParseExportPath("/" + id + "/" + vol)
	if err != nil {
		t.Fatal(err)
	}
	if ref.LeaseID != id || ref.VolumeID != vol {
		t.Fatalf("unexpected ref %+v", ref)
	}
	if _, err := ParseExportPath("/" + id); err == nil {
		t.Fatal("single-component path must be rejected")
	}
	if _, err := ParseExportPath("/" + id + "/" + vol + "/extra"); err == nil {
		t.Fatal("three-component path must be rejected")
	}
	if _, err := ParseExportPath("/" + id + "/../" + vol); err == nil {
		t.Fatal("traversal path must be rejected")
	}
}

func TestJailedFSRejectsParentLookup(t *testing.T) {
	root := t.TempDir()
	vol := "0123456789abcdef0123456789abcdef"
	data := filepath.Join(root, vol, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "meta.json")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ok.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _, err := OpenVolumeFS(root, vol)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open("../meta.json"); err == nil {
		t.Fatal("expected parent lookup to fail")
	}
	if _, err := fs.Open("../../meta.json"); err == nil {
		t.Fatal("expected multi-level parent lookup to fail")
	}
	if s, ok := fs.(interface{ Symlink(string, string) error }); ok {
		if err := s.Symlink("../../meta.json", "escape"); err == nil {
			t.Fatal("expected escaping relative symlink to be rejected")
		}
		if err := s.Symlink("ok.txt", "alias"); err != nil {
			t.Fatalf("in-export symlink failed: %v", err)
		}
	} else {
		t.Fatal("jailedFS should support Symlink")
	}
	if _, err := fs.Open("ok.txt"); err != nil {
		t.Fatalf("in-export open failed: %v", err)
	}
	if c, ok := fs.(interface {
		Chmod(string, os.FileMode) error
	}); !ok {
		t.Fatal("jailedFS must implement billy.Change")
	} else if err := c.Chmod("ok.txt", 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
}

func TestNFSTargetAndCanonicalIP(t *testing.T) {
	id := "00112233445566778899aabbccddeeff"
	vol := "0123456789abcdef0123456789abcdef"
	got := NFSTarget("10.96.10.20", id, vol)
	want := "10.96.10.20:/" + id + "/" + vol
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if canonicalSourceIP("10.0.0.5:2049") != "10.0.0.5" {
		t.Fatal(canonicalSourceIP("10.0.0.5:2049"))
	}
	if !ipEqual("127.0.0.1", "127.0.0.1") {
		t.Fatal("ipv4 equal")
	}
	_ = net.IPv4zero
}

func TestAdmitMountIncrementsDenialMetric(t *testing.T) {
	m := NewMetrics()
	s := NewStore(WithMetrics(m))
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitMount(id, vol, "10.0.0.1:1"); err != ErrLeaseUnbound {
		t.Fatalf("got %v", err)
	}
	if m.ExportDenials() < 1 {
		t.Fatal("expected denial metric increment")
	}
}

func TestLeasedFSStatFailsAfterLeaseDelete(t *testing.T) {
	root := t.TempDir()
	vol := "0123456789abcdef0123456789abcdef"
	data := filepath.Join(root, vol, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ok.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	id, err := NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	base, _, err := OpenVolumeFS(root, vol)
	if err != nil {
		t.Fatal(err)
	}
	fs := newLeasedFS(base, store, id, vol)
	if _, err := fs.Stat("ok.txt"); err != nil {
		t.Fatalf("stat before revoke: %v", err)
	}
	if !store.Delete(id) {
		t.Fatal("delete")
	}
	if _, err := fs.Stat("ok.txt"); err == nil {
		t.Fatal("expected ESTALE after lease revoke")
	}
}

func TestParseExportPathDenialMetricOnMount(t *testing.T) {
	m := NewMetrics()
	h := NewHandler(NewStore(WithMetrics(m)), t.TempDir(), m, nil)
	st, fs, _ := h.Mount(context.Background(), nil, nfs.MountRequest{Dirpath: []byte("/not-a-valid-path")})
	if st == 0 {
		t.Fatalf("expected denied mount, fs=%v", fs)
	}
	if m.ExportDenials() < 1 {
		t.Fatal("malformed export path must increment cubepod_volume_nfs_export_denials_total")
	}
}

func TestJailedFSRejectsRenameSymlinkEscapeAndLeadingSlash(t *testing.T) {
	root := t.TempDir()
	vol := "0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210"
	data := filepath.Join(root, vol, "data")
	otherData := filepath.Join(root, other, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherData, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(otherData, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meta.json"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, _, err := OpenVolumeFS(root, vol)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := fs.(interface{ Symlink(string, string) error })
	if !ok {
		t.Fatal("symlink")
	}
	if err := s.Symlink("../../meta.json", "/escape"); err == nil {
		t.Fatal("leading-slash escaping symlink must be rejected")
	}
	if err := fs.MkdirAll("a/b/c/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Symlink("../../../"+other+"/data/secret.txt", "a/b/c/d/link"); err == nil {
		t.Fatal("relative .. symlink that can escape after rename must be rejected")
	}
	c, ok := fs.(interface {
		Chmod(string, os.FileMode) error
		Chtimes(string, time.Time, time.Time) error
	})
	if !ok {
		t.Fatal("Change")
	}
	// Plant an already-escaping symlink as if it came from the shared PVC, then SETATTR must not follow it.
	if err := os.Symlink(secret, filepath.Join(data, "planted")); err != nil {
		t.Fatal(err)
	}
	if err := c.Chmod("planted", 0o600); err == nil {
		t.Fatal("Chmod must not follow an escaping symlink")
	}
	info, err := os.Stat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("sibling file mode changed: %v", info.Mode())
	}
}

func TestUnboundLeaseExpiryInvokesOnExpire(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	clock := &now
	var expired []string
	s := NewStore(WithUnboundTTL(60*time.Second), WithNow(func() time.Time { return *clock }), WithOnExpire(func(id string) {
		expired = append(expired, id)
	}))
	id, err := NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	vol := "0123456789abcdef0123456789abcdef"
	if _, err := s.Put(id, LeaseRequest{VolumeIDs: []string{vol}}); err != nil {
		t.Fatal(err)
	}
	*clock = now.Add(60 * time.Second)
	if _, ok := s.Get(id); ok {
		t.Fatal("unbound lease should expire at TTL")
	}
	if len(expired) != 1 || expired[0] != id {
		t.Fatalf("onExpire = %v, want [%s]", expired, id)
	}
}
