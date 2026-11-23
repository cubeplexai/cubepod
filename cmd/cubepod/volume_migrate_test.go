package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/events"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

func TestMigrateLegacyWorkspaces_InPlaceLocalAndCloneModes(t *testing.T) {
	ctx := context.Background()
	kc := controller.NewMemoryKubeClient()
	ns := "cubepod-workloads"

	const apiKeyA = "cpod_migrate_tenant_a"
	const apiKeyB = "cpod_migrate_tenant_b"
	tenantAHash := events.TenantHash(apiKeyA)

	// 1. WarmPool standby PVC -> must be skipped.
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:      "pool-warm-1-workspace-pvc",
		Namespace: ns,
		Size:      "20Gi",
		Labels:    map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})

	// 2. Active ephemeral sandbox PVC (no VolumeKey) -> must be skipped.
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:      "eph-box-workspace-pvc",
		Namespace: ns,
		Size:      "20Gi",
		Labels:    map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})
	_ = kc.UpsertPodBox(ctx, v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata:   v1alpha1.ObjectMeta{Name: "eph-box", Namespace: ns},
		Spec:       v1alpha1.PodBoxSpec{TemplateRef: "ubuntu:24.04", DesiredState: v1alpha1.DesiredStateRunning},
		Status:     v1alpha1.PodBoxStatus{Phase: v1alpha1.PodBoxPhaseRunning, WorkspacePVCName: "eph-box-workspace-pvc"},
	})

	// 3. Active VolumeKey PodBox PVC -> resolved from PodBox annotation (precedence 1).
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:      "active-vk-workspace-pvc",
		Namespace: ns,
		Size:      "25Gi",
		Labels:    map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})
	_ = kc.UpsertPodBox(ctx, v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:        "vk-box",
			Namespace:   ns,
			Annotations: map[string]string{"cubepod.io/owner-tenant": tenantAHash},
		},
		Spec:   v1alpha1.PodBoxSpec{TemplateRef: "ubuntu:24.04", DesiredState: v1alpha1.DesiredStateRunning, VolumeKey: "active-vk"},
		Status: v1alpha1.PodBoxStatus{Phase: v1alpha1.PodBoxPhaseRunning, WorkspacePVCName: "active-vk-workspace-pvc"},
	})

	// 4. Retained orphaned legacy PVC from deleted sandbox -> resolved via OwnerMap (precedence 2).
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:      "orphan-mapped-workspace-pvc",
		Namespace: ns,
		Size:      "30Gi",
		Labels:    map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})

	// 5. Retained unmapped legacy PVC -> resolved via DefaultAPIKey (precedence 3).
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:      "orphan-default-workspace-pvc",
		Namespace: ns,
		Size:      "15Gi",
		Labels:    map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})

	// 6. Local workspace directory (<workspaceBaseDir>/volumes/local-proj/workspace/hello.txt).
	wsBaseDir := t.TempDir()
	localWS := filepath.Join(wsBaseDir, "volumes", "local-proj", "workspace")
	if err := os.MkdirAll(localWS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localWS, "hello.txt"), []byte("preserved-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := volume.Open(t.TempDir(), volume.Config{})
	if err != nil {
		t.Fatal(err)
	}

	report, err := MigrateLegacyWorkspaces(ctx, kc, st, LegacyMigrateOptions{
		Namespace: ns,
		OwnerMap: map[string]string{
			"orphan-mapped-workspace-pvc": apiKeyB,
		},
		DefaultAPIKey:    apiKeyA,
		WorkspaceBaseDir: wsBaseDir,
	})
	if err != nil {
		t.Fatalf("MigrateLegacyWorkspaces: %v", err)
	}
	if len(report.Migrated) != 4 {
		t.Fatalf("expected 4 migrated items (3 PVCs + 1 local), got %d: %+v", len(report.Migrated), report)
	}
	if len(report.Skipped) != 2 {
		t.Fatalf("expected 2 skipped items (warmpool + active ephemeral), got %d: %+v", len(report.Skipped), report)
	}

	// Verify local workspace file contents were preserved inside the adopted volume's DataDir.
	volsA, err := st.List(apiKeyA)
	if err != nil {
		t.Fatal(err)
	}
	foundLocal := false
	for _, v := range volsA {
		if v.Name == "local-proj" {
			foundLocal = true
			dir, ok := st.DataDir(v.VolumeID)
			if !ok {
				t.Fatal("missing DataDir for local-proj")
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "hello.txt"))
			if readErr != nil || string(got) != "preserved-data" {
				t.Fatalf("local workspace file = %q, err=%v; want preserved-data", string(got), readErr)
			}
		}
	}
	if !foundLocal {
		t.Fatalf("expected local-proj in tenant A volumes: %+v", volsA)
	}

	// Running MigrateLegacyWorkspaces a second time must skip already-labeled PVCs idempotently.
	report2, err := MigrateLegacyWorkspaces(ctx, kc, st, LegacyMigrateOptions{
		Namespace:     ns,
		DefaultAPIKey: apiKeyA,
	})
	if err != nil {
		t.Fatalf("second MigrateLegacyWorkspaces: %v", err)
	}
	if len(report2.Migrated) != 0 {
		t.Fatalf("expected 0 newly migrated PVCs on second run, got %+v", report2.Migrated)
	}

	// 7. Test Mode 2: --clone-to-canonical creates CSI VolumeSnapshot + canonical vol-<id> PVC while keeping original PVC intact.
	_ = kc.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:       "clone-src-workspace-pvc",
		Namespace:  ns,
		VolumeMode: "Block",
		Size:       "50Gi",
		Labels:     map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})
	stClone, err := volume.Open(t.TempDir(), volume.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cloneReport, err := MigrateLegacyWorkspaces(ctx, kc, stClone, LegacyMigrateOptions{
		Namespace:        ns,
		DefaultAPIKey:    apiKeyA,
		CloneToCanonical: true,
	})
	if err != nil {
		t.Fatalf("MigrateLegacyWorkspaces(--clone-to-canonical): %v", err)
	}
	if len(cloneReport.Migrated) != 1 || cloneReport.Migrated[0].Mode != "clone-to-canonical" {
		t.Fatalf("unexpected cloneReport: %+v", cloneReport)
	}
	canonicalPVCName := cloneReport.Migrated[0].PVCName
	canonicalPVC, exists := kc.GetPVC(ns, canonicalPVCName)
	if !exists || canonicalPVC.DataSourceKind != "VolumeSnapshot" || canonicalPVC.Size != "50Gi" {
		t.Fatalf("expected canonical cloned PVC %s from VolumeSnapshot, got exists=%v pvc=%+v", canonicalPVCName, exists, canonicalPVC)
	}
	if _, origStillExists := kc.GetPVC(ns, "clone-src-workspace-pvc"); !origStillExists {
		t.Fatal("original clone-src-workspace-pvc must remain untouched in --clone-to-canonical mode")
	}
}
