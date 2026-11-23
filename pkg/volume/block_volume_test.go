package volume

import (
	"errors"
	"strings"
	"testing"
)

func openTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func TestParseBlockSize(t *testing.T) {
	cases := []struct {
		in        string
		wantStr   string
		wantBytes int64
		wantErr   bool
	}{
		{"", "20Gi", 20 << 30, false},
		{"20Gi", "20Gi", 20 << 30, false},
		{"1Gi", "1Gi", 1 << 30, false},
		{"1024Mi", "1024Mi", 1 << 30, false},
		{"1Ti", "1Ti", 1 << 40, false},
		{"2G", "2G", 2_000_000_000, false},
		{"512Mi", "", 0, true}, // below 1GiB minimum
		{"2Ti", "", 0, true},   // above 1TiB maximum
		{"0Gi", "", 0, true},
		{"-1Gi", "", 0, true},
		{"20", "", 0, true},   // missing unit
		{"20Xi", "", 0, true}, // unknown unit
		{"Gi", "", 0, true},   // missing number
		{"99999999999999999999Gi", "", 0, true},
	}
	for _, tc := range cases {
		gotStr, gotBytes, err := ParseBlockSize(tc.in)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalidSize) {
				t.Errorf("ParseBlockSize(%q) err = %v; want ErrInvalidSize", tc.in, err)
			}
			continue
		}
		if err != nil || gotStr != tc.wantStr || gotBytes != tc.wantBytes {
			t.Errorf("ParseBlockSize(%q) = (%q, %d, %v); want (%q, %d, nil)", tc.in, gotStr, gotBytes, err, tc.wantStr, tc.wantBytes)
		}
	}
}

func TestStore_CreateWithOptions_TypeAndSizeValidation(t *testing.T) {
	st := openTestStore(t, Config{})

	shared, err := st.CreateWithOptions("key-1", "ds", CreateOpts{})
	if err != nil {
		t.Fatalf("create default shared: %v", err)
	}
	if shared.EffectiveType() != "shared" || shared.IsBlock() {
		t.Fatalf("default type = %q; want shared", shared.EffectiveType())
	}

	if _, err := st.CreateWithOptions("key-1", "ds-sized", CreateOpts{Type: "shared", Size: "5Gi"}); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("shared with size err = %v; want ErrInvalidSize", err)
	}
	if _, err := st.CreateWithOptions("key-1", "bad", CreateOpts{Type: "ext4"}); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("unknown type err = %v; want ErrInvalidType", err)
	}
	if _, err := st.CreateWithOptions("key-1", "tiny", CreateOpts{Type: "block", Size: "10Mi"}); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("tiny block err = %v; want ErrInvalidSize", err)
	}

	blk, err := st.CreateWithOptions("key-1", "ws", CreateOpts{Type: "block"})
	if err != nil {
		t.Fatalf("create block: %v", err)
	}
	if !blk.IsBlock() || blk.Size != DefaultBlockVolumeSize || blk.SizeBytes != 20<<30 {
		t.Fatalf("block volume = %+v; want block with default 20Gi", blk)
	}

	got, err := st.Get("key-1", blk.VolumeID)
	if err != nil || got.Type != "block" || got.Size != "20Gi" {
		t.Fatalf("Get block = %+v, %v", got, err)
	}
}

func TestStore_BlockVolume_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	blk, err := st.CreateWithOptions("key-1", "ws", CreateOpts{Type: "block", Size: "8Gi"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get("key-1", blk.VolumeID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsBlock() || got.Size != "8Gi" || got.SizeBytes != 8<<30 {
		t.Fatalf("after reopen = %+v", got)
	}
}

func TestStore_BlockVolume_TenantByteQuotaCountsDeclaredSize(t *testing.T) {
	st := openTestStore(t, Config{MaxBytesPerTenant: 30 << 30})

	if _, err := st.CreateWithOptions("tenant-a", "b1", CreateOpts{Type: "block", Size: "20Gi"}); err != nil {
		t.Fatalf("first block: %v", err)
	}
	if _, err := st.CreateWithOptions("tenant-a", "b2", CreateOpts{Type: "block", Size: "20Gi"}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("second block err = %v; want ErrLimitExceeded (20Gi+20Gi > 30Gi)", err)
	}
	if _, err := st.CreateWithOptions("tenant-a", "b3", CreateOpts{Type: "block", Size: "10Gi"}); err != nil {
		t.Fatalf("10Gi block fits quota exactly: %v", err)
	}
	// Other tenants are unaffected.
	if _, err := st.CreateWithOptions("tenant-b", "b1", CreateOpts{Type: "block", Size: "20Gi"}); err != nil {
		t.Fatalf("tenant-b block: %v", err)
	}
}

func planBlock(t *testing.T, st *Store, key, name, path string) []PlannedMount {
	t.Helper()
	planned, err := st.LookupMounts(key, []MountRequest{{Name: name, Path: path}})
	if err != nil {
		t.Fatalf("LookupMounts(%s %s): %v", name, path, err)
	}
	return planned
}

func TestStore_LookupMounts_BlockConstraints(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWithOptions("k", "ws2", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("k", "data"); err != nil {
		t.Fatal(err)
	}

	// Block volumes only mount at /workspace.
	if _, err := st.LookupMounts("k", []MountRequest{{Name: "ws", Path: "/mnt/data"}}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("block at /mnt/data err = %v; want ErrInvalidPath", err)
	}
	if _, err := st.LookupMounts("k", []MountRequest{{Name: "ws", Path: "/workspace/../etc"}}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("block traversal err = %v; want ErrInvalidPath", err)
	}

	// At most one block volume per sandbox.
	if _, err := st.LookupMounts("k", []MountRequest{
		{Name: "ws", Path: "/workspace"},
		{Name: "ws2", Path: "/workspace"},
	}); err == nil {
		t.Fatal("two block volumes in one sandbox must be rejected")
	}

	// A block volume coexists with shared mounts on other paths.
	planned, err := st.LookupMounts("k", []MountRequest{
		{Name: "ws", Path: "/workspace"},
		{Name: "data", Path: "/mnt/datasets"},
	})
	if err != nil {
		t.Fatalf("block + shared: %v", err)
	}
	if planned[0].Type != "block" || planned[1].Type != "shared" {
		t.Fatalf("planned types = %q, %q", planned[0].Type, planned[1].Type)
	}

	// Shared volumes still cannot use the reserved /workspace path.
	if _, err := st.LookupMounts("k", []MountRequest{{Name: "data", Path: "/workspace"}}); err == nil {
		t.Fatal("shared volume at /workspace must stay rejected")
	}
}

func TestStore_CommitMounts_BlockSingleWriter(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	p := planBlock(t, st, "k", "ws", "/workspace")

	if err := st.CommitMounts("sb-1", "k", p); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if err := st.CommitMounts("sb-2", "k", p); !errors.Is(err, ErrInUse) {
		t.Fatalf("second sandbox commit err = %v; want ErrInUse", err)
	}
	// Re-committing for the same sandbox is idempotent.
	if err := st.CommitMounts("sb-1", "k", p); err != nil {
		t.Fatalf("idempotent recommit: %v", err)
	}

	// Releasing the owner frees the volume for reuse.
	st.UnmountSandbox("sb-1")
	if err := st.CommitMounts("sb-2", "k", p); err != nil {
		t.Fatalf("commit after release: %v", err)
	}
}

func TestStore_CommitMounts_BlockSingleWriterConcurrent(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	p := planBlock(t, st, "k", "ws", "/workspace")

	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		id := "sb-" + strings.Repeat("x", i+1)
		go func() { errs <- st.CommitMounts(id, "k", p) }()
	}
	wins, losses := 0, 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			wins++
		case errors.Is(err, ErrInUse):
			losses++
		default:
			t.Fatalf("unexpected commit error: %v", err)
		}
	}
	if wins != 1 || losses != n-1 {
		t.Fatalf("wins=%d losses=%d; want exactly 1 winner", wins, losses)
	}
}

func TestStore_CopyMounts_SkipsBlockKeepsShared(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("k", "data"); err != nil {
		t.Fatal(err)
	}
	planned, err := st.LookupMounts("k", []MountRequest{
		{Name: "ws", Path: "/workspace"},
		{Name: "data", Path: "/mnt/datasets"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMounts("parent", "k", planned); err != nil {
		t.Fatal(err)
	}

	if err := st.CopyMounts("parent", "child"); err != nil {
		t.Fatal(err)
	}
	child := st.MountsForSandbox("child")
	if len(child) != 1 || child[0].Type != "shared" || child[0].ContainerPath != "/mnt/datasets" {
		t.Fatalf("child mounts = %+v; want only the shared mount", child)
	}
	// The parent still owns the block volume exclusively.
	if owners := st.ActiveMountOwners(planned[0].VolumeID); len(owners) != 1 || owners[0] != "parent" {
		t.Fatalf("block owners = %v; want [parent]", owners)
	}
}

func TestStore_CopyMounts_BlockOnlyParentCreatesNoChildRecord(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMounts("parent", "k", planBlock(t, st, "k", "ws", "/workspace")); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyMounts("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if got := st.MountsForSandbox("child"); len(got) != 0 {
		t.Fatalf("child mounts = %+v; want none", got)
	}
}

func TestStore_BlockVolume_ContentAPIRejected(t *testing.T) {
	st := openTestStore(t, Config{})
	blk, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Stat(blk.VolumeID, "/"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("Stat err = %v; want ErrUnsupportedType", err)
	}
	if _, err := st.ListDir(blk.VolumeID, "/", 1); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("ListDir err = %v; want ErrUnsupportedType", err)
	}
	if _, err := st.WriteFile(blk.VolumeID, "/f", strings.NewReader("x"), WriteOpts{}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("WriteFile err = %v; want ErrUnsupportedType", err)
	}
	if err := st.Remove(blk.VolumeID, "/f"); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("Remove err = %v; want ErrUnsupportedType", err)
	}
}

func TestStore_RollbackCreate(t *testing.T) {
	st := openTestStore(t, Config{})
	blk, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RollbackCreate(blk.VolumeID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("k", blk.VolumeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after rollback err = %v; want ErrNotFound", err)
	}
	// The name is free again.
	if _, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatalf("recreate after rollback: %v", err)
	}
	// Rolling back an unknown id is a no-op; a malformed id is rejected.
	if err := st.RollbackCreate(strings.Repeat("a", 32)); err != nil {
		t.Fatalf("rollback unknown: %v", err)
	}
	if err := st.RollbackCreate("../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback malformed err = %v; want ErrNotFound", err)
	}
}

func TestStore_AdoptBlockVolume_IdempotentAndTenantScoped(t *testing.T) {
	st := openTestStore(t, Config{})
	opts := AdoptBlockVolumeOpts{
		APIKey:          "key-a",
		Name:            "my-key",
		PVCName:         "my-key-workspace-pvc",
		PVCNamespace:    "cubepod-workloads",
		Size:            "30Gi",
		LegacyVolumeKey: "my-key",
	}
	v1, created, err := st.AdoptBlockVolume(opts)
	if err != nil || !created {
		t.Fatalf("first adopt = %+v created=%v err=%v", v1, created, err)
	}
	if !v1.IsBlock() || v1.PVCName != opts.PVCName || v1.LegacyVolumeKey != "my-key" || v1.SizeBytes != 30<<30 {
		t.Fatalf("adopted = %+v", v1)
	}

	// Idempotent: same PVC, same tenant -> same volume, created=false.
	v2, created, err := st.AdoptBlockVolume(opts)
	if err != nil || created || v2.VolumeID != v1.VolumeID {
		t.Fatalf("second adopt = %+v created=%v err=%v; want same volume", v2, created, err)
	}

	// A different tenant cannot take over an already-adopted PVC.
	other := opts
	other.APIKey = "key-b"
	if _, _, err := st.AdoptBlockVolume(other); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-tenant adopt err = %v; want ErrConflict", err)
	}

	// Lookup helpers.
	if got, ok := st.FindByPVCName(opts.PVCName); !ok || got.VolumeID != v1.VolumeID {
		t.Fatalf("FindByPVCName = %+v ok=%v", got, ok)
	}
	// The adopted volume is only visible to its owner.
	if _, err := st.Get("key-b", v1.VolumeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant Get err = %v; want ErrNotFound", err)
	}
	if _, err := st.Get("key-a", v1.VolumeID); err != nil {
		t.Fatalf("owner Get: %v", err)
	}
}

func TestStore_AdoptBlockVolume_NameCollisionGetsSuffix(t *testing.T) {
	st := openTestStore(t, Config{})
	if _, err := st.CreateWithOptions("key-a", "ws", CreateOpts{Type: "block"}); err != nil {
		t.Fatal(err)
	}
	adopted, created, err := st.AdoptBlockVolume(AdoptBlockVolumeOpts{
		APIKey:       "key-a",
		Name:         "ws",
		PVCName:      "legacy-workspace-pvc",
		PVCNamespace: "ns",
	})
	if err != nil || !created {
		t.Fatalf("adopt = %+v created=%v err=%v", adopted, created, err)
	}
	if adopted.Name == "ws" || !strings.HasPrefix(adopted.Name, "ws-") {
		t.Fatalf("collided name = %q; want ws-<suffix>", adopted.Name)
	}
}

func TestStore_AdoptBlockVolume_DeterministicIDAcrossStores(t *testing.T) {
	opts := AdoptBlockVolumeOpts{TenantHash: strings.Repeat("a", 64), PVCName: "x-workspace-pvc", PVCNamespace: "ns"}
	a, _, err := openTestStore(t, Config{}).AdoptBlockVolume(opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := openTestStore(t, Config{}).AdoptBlockVolume(opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.VolumeID != b.VolumeID {
		t.Fatalf("ids differ across replicas: %s vs %s", a.VolumeID, b.VolumeID)
	}
}

func TestStore_EvictMount_And_BlockVolumeCount(t *testing.T) {
	st := openTestStore(t, Config{})
	blk, err := st.CreateWithOptions("k", "ws", CreateOpts{Type: "block"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("k", "data"); err != nil {
		t.Fatal(err)
	}
	if got := st.BlockVolumeCount(); got != 1 {
		t.Fatalf("BlockVolumeCount = %d; want 1 (shared volumes excluded)", got)
	}
	if err := st.CommitMounts("stale", "k", planBlock(t, st, "k", "ws", "/workspace")); err != nil {
		t.Fatal(err)
	}
	if !st.EvictMount("stale", blk.VolumeID) {
		t.Fatal("EvictMount should report removal")
	}
	if st.EvictMount("stale", blk.VolumeID) {
		t.Fatal("second EvictMount should be a no-op")
	}
	// Eviction frees the volume for another sandbox.
	if err := st.CommitMounts("fresh", "k", planBlock(t, st, "k", "ws", "/workspace")); err != nil {
		t.Fatalf("commit after evict: %v", err)
	}
}
