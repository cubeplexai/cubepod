package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

// LegacyMigrateOptions configures `cubepod volume migrate-legacy`.
type LegacyMigrateOptions struct {
	Namespace        string
	OwnerMap         map[string]string // pvcName or legacy volumeKey -> tenantHash or apiKey
	DefaultAPIKey    string
	WorkspaceBaseDir string
	CloneToCanonical bool
	SnapshotClass    string
	DryRun           bool
}

// LegacyMigrateEntry summarizes one migrated or skipped legacy asset.
type LegacyMigrateEntry struct {
	Source   string `json:"source"`
	VolumeID string `json:"volumeID,omitempty"`
	Name     string `json:"name,omitempty"`
	PVCName  string `json:"pvcName,omitempty"`
	Mode     string `json:"mode"` // "in-place" | "clone-to-canonical" | "local" | "skipped"
	Reason   string `json:"reason,omitempty"`
}

// LegacyMigrateReport is the structured result of MigrateLegacyWorkspaces.
type LegacyMigrateReport struct {
	Migrated []LegacyMigrateEntry `json:"migrated"`
	Skipped  []LegacyMigrateEntry `json:"skipped"`
}

// MigrateLegacyWorkspaces idempotently migrates pre-v0.8.0 legacy /workspace PVCs and local
// workspace directories into volume.Store as type: "block" volumes without losing data.
func MigrateLegacyWorkspaces(ctx context.Context, kc controller.KubeClient, st *volume.Store, opts LegacyMigrateOptions) (LegacyMigrateReport, error) {
	if st == nil {
		return LegacyMigrateReport{}, errors.New("volume store is required")
	}
	ns := strings.TrimSpace(opts.Namespace)
	if ns == "" {
		ns = "cubepod-workloads"
	}

	report := LegacyMigrateReport{
		Migrated: []LegacyMigrateEntry{},
		Skipped:  []LegacyMigrateEntry{},
	}

	if kc != nil {
		boxes, err := kc.ListPodBoxes(ctx, ns)
		if err != nil {
			return report, fmt.Errorf("list PodBoxes: %w", err)
		}
		activeEphemeralPVCs := map[string]struct{}{}
		boxOwnerByPVC := map[string]struct {
			tenantHash string
			volumeKey  string
			boxName    string
			active     bool
		}{}
		for _, box := range boxes {
			pvcName := strings.TrimSpace(box.Status.WorkspacePVCName)
			if pvcName == "" {
				pvcName = controller.WorkspacePVCName(box.Metadata.Name, box.Spec)
			}
			isActive := box.Status.Phase != v1alpha1.PodBoxPhaseTerminated && box.Status.Phase != v1alpha1.PodBoxPhaseFailed
			if box.Spec.VolumeKey == "" && box.Spec.WorkspaceBlockMount() == nil {
				if isActive {
					activeEphemeralPVCs[pvcName] = struct{}{}
				}
				continue
			}
			if box.Spec.VolumeKey != "" {
				ownerTenant := ""
				if box.Metadata.Annotations != nil {
					ownerTenant = strings.TrimSpace(box.Metadata.Annotations["cubepod.io/owner-tenant"])
				}
				boxOwnerByPVC[pvcName] = struct {
					tenantHash string
					volumeKey  string
					boxName    string
					active     bool
				}{tenantHash: ownerTenant, volumeKey: box.Spec.VolumeKey, boxName: box.Metadata.Name, active: isActive}
			}
		}

		pvcs, err := kc.ListPVCManifests(ctx, ns, "pod.cubeplex.ai/vol-type=workspace")
		if err != nil {
			return report, fmt.Errorf("list workspace PVCs: %w", err)
		}
		for _, pvc := range pvcs {
			if strings.Contains(pvc.Name, "-warm-") {
				report.Skipped = append(report.Skipped, LegacyMigrateEntry{Source: pvc.Name, Mode: "skipped", Reason: "warmpool standby PVC"})
				continue
			}
			if strings.HasPrefix(pvc.Name, "vol-") || (pvc.Labels != nil && pvc.Labels[controller.PVCLabelManagedBy] == controller.PVCLabelManagedByVolumes) {
				report.Skipped = append(report.Skipped, LegacyMigrateEntry{Source: pvc.Name, Mode: "skipped", Reason: "already managed by cubepod-volumes"})
				continue
			}
			if _, isEph := activeEphemeralPVCs[pvc.Name]; isEph {
				report.Skipped = append(report.Skipped, LegacyMigrateEntry{Source: pvc.Name, Mode: "skipped", Reason: "active ephemeral sandbox PVC"})
				continue
			}

			// Resolve ownership in strict precedence order:
			// 1. Active PodBox annotation (cubepod.io/owner-tenant + cubepod.io/volume-key)
			// 2. Operator-supplied --owner-map
			// 3. Explicit --default-api-key
			var adoptOpts volume.AdoptBlockVolumeOpts
			adoptOpts.PVCNamespace = ns
			adoptOpts.Size = pvc.Size
			adoptOpts.Name = strings.TrimSuffix(pvc.Name, "-workspace-pvc")

			if info, ok := boxOwnerByPVC[pvc.Name]; ok && info.tenantHash != "" {
				adoptOpts.OwnerTenantHash = info.tenantHash
				adoptOpts.LegacyVolumeKey = info.volumeKey
				if info.volumeKey != "" {
					adoptOpts.Name = info.volumeKey
				}
			} else if ownerVal, ok := lookupOwnerMap(opts.OwnerMap, pvc.Name, adoptOpts.Name); ok {
				if isSHA256Hex(ownerVal) {
					adoptOpts.TenantHash = ownerVal
				} else {
					adoptOpts.APIKey = ownerVal
				}
			} else if strings.TrimSpace(opts.DefaultAPIKey) != "" {
				adoptOpts.APIKey = strings.TrimSpace(opts.DefaultAPIKey)
			} else {
				report.Skipped = append(report.Skipped, LegacyMigrateEntry{
					Source: pvc.Name,
					Mode:   "skipped",
					Reason: "no tenant ownership resolved (provide --owner-map or --default-api-key)",
				})
				continue
			}

			if opts.CloneToCanonical {
				volID := volume.DeterministicVolumeID(ns + "/clone/" + pvc.Name)
				canonicalPVC := "vol-" + volID
				snapName := "migrate-" + volID[:12]
				if opts.DryRun {
					report.Migrated = append(report.Migrated, LegacyMigrateEntry{
						Source:   pvc.Name,
						VolumeID: volID,
						Name:     adoptOpts.Name,
						PVCName:  canonicalPVC,
						Mode:     "clone-to-canonical",
						Reason:   "dry-run",
					})
					continue
				}
				snap := controller.VolumeSnapshot{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshot",
					Metadata:   v1alpha1.ObjectMeta{Name: snapName, Namespace: ns},
					Spec: controller.VolumeSnapshotSpec{
						Source: controller.VolumeSnapshotSource{PersistentVolumeClaimName: pvc.Name},
					},
				}
				if class := strings.TrimSpace(opts.SnapshotClass); class != "" {
					snap.Spec.VolumeSnapshotClassName = &class
				}
				if err := kc.EnsureVolumeSnapshot(ctx, snap); err != nil {
					return report, fmt.Errorf("create snapshot for %s: %w", pvc.Name, err)
				}
				if err := kc.WaitVolumeSnapshotReady(ctx, ns, snapName); err != nil {
					return report, fmt.Errorf("wait snapshot %s ready: %w", snapName, err)
				}
				size := pvc.Size
				if size == "" {
					size = volume.DefaultBlockVolumeSize
				}
				volMode := pvc.VolumeMode
				if volMode == "" {
					volMode = "Block"
				}
				if err := kc.EnsurePVC(ctx, controller.BlockPVCManifest{
					Name:               canonicalPVC,
					Namespace:          ns,
					StorageClassName:   pvc.StorageClassName,
					VolumeMode:         volMode,
					Size:               size,
					AccessModes:        []string{"ReadWriteOnce"},
					DataSourceSnapshot: snapName,
					DataSourceKind:     "VolumeSnapshot",
					DataSourceAPIGroup: "snapshot.storage.k8s.io",
					Labels: map[string]string{
						"app.kubernetes.io/name":     "cubepod",
						"pod.cubeplex.ai/vol-type":   "workspace",
						controller.PVCLabelVolumeID:  volID,
						controller.PVCLabelManagedBy: controller.PVCLabelManagedByVolumes,
					},
				}); err != nil {
					return report, fmt.Errorf("create canonical PVC %s: %w", canonicalPVC, err)
				}
				adoptOpts.VolumeID = volID
				adoptOpts.PVCName = canonicalPVC
				vol, _, err := st.AdoptBlockVolume(adoptOpts)
				if err != nil {
					return report, fmt.Errorf("register cloned volume for %s: %w", pvc.Name, err)
				}
				report.Migrated = append(report.Migrated, LegacyMigrateEntry{
					Source:   pvc.Name,
					VolumeID: vol.VolumeID,
					Name:     vol.Name,
					PVCName:  vol.PVCName,
					Mode:     "clone-to-canonical",
				})
				continue
			}

			// Mode 1: Zero-Copy In-Place Adoption.
			adoptOpts.PVCName = pvc.Name
			if opts.DryRun {
				report.Migrated = append(report.Migrated, LegacyMigrateEntry{
					Source:  pvc.Name,
					Name:    adoptOpts.Name,
					PVCName: pvc.Name,
					Mode:    "in-place",
					Reason:  "dry-run",
				})
				continue
			}
			vol, _, err := st.AdoptBlockVolume(adoptOpts)
			if err != nil {
				return report, fmt.Errorf("adopt PVC %s: %w", pvc.Name, err)
			}
			if err := kc.PatchPVCLabels(ctx, ns, pvc.Name, map[string]string{
				controller.PVCLabelVolumeID:  vol.VolumeID,
				controller.PVCLabelManagedBy: controller.PVCLabelManagedByVolumes,
			}); err != nil {
				return report, fmt.Errorf("label adopted PVC %s: %w", pvc.Name, err)
			}
			if info, ok := boxOwnerByPVC[pvc.Name]; ok && info.active && info.boxName != "" {
				if curPVC, getErr := kc.GetPVCManifest(ctx, ns, pvc.Name); getErr == nil && curPVC != nil {
					holder := ""
					if curPVC.Annotations != nil {
						holder = strings.TrimSpace(curPVC.Annotations[controller.PVCAnnotationAttachedSandbox])
					}
					if holder == "" {
						_, _ = kc.UpdatePVCAnnotationsCAS(ctx, ns, pvc.Name, curPVC.ResourceVersion, map[string]string{
							controller.PVCAnnotationAttachedSandbox: info.boxName,
							controller.PVCAnnotationAttachedAt:      time.Now().UTC().Format(time.RFC3339Nano),
						})
					}
				}
			}
			report.Migrated = append(report.Migrated, LegacyMigrateEntry{
				Source:   pvc.Name,
				VolumeID: vol.VolumeID,
				Name:     vol.Name,
				PVCName:  vol.PVCName,
				Mode:     "in-place",
			})
		}
	}

	// Local development workspace directory adoption (<workspaceBaseDir>/volumes/<sanitized>/workspace).
	if baseDir := strings.TrimSpace(opts.WorkspaceBaseDir); baseDir != "" {
		legacyRoot := filepath.Join(baseDir, "volumes")
		entries, err := os.ReadDir(legacyRoot)
		if err == nil {
			for _, ent := range entries {
				if !ent.IsDir() {
					continue
				}
				keyName := ent.Name()
				srcDir := filepath.Join(legacyRoot, keyName, "workspace")
				if stInfo, statErr := os.Stat(srcDir); statErr != nil || !stInfo.IsDir() {
					continue
				}
				adoptOpts := volume.AdoptBlockVolumeOpts{
					Name:            keyName,
					PVCName:         "local-" + keyName,
					LegacyVolumeKey: keyName,
					Size:            volume.DefaultBlockVolumeSize,
				}
				if ownerVal, ok := lookupOwnerMap(opts.OwnerMap, keyName, srcDir); ok {
					if isSHA256Hex(ownerVal) {
						adoptOpts.TenantHash = ownerVal
					} else {
						adoptOpts.APIKey = ownerVal
					}
				} else if strings.TrimSpace(opts.DefaultAPIKey) != "" {
					adoptOpts.APIKey = strings.TrimSpace(opts.DefaultAPIKey)
				} else {
					report.Skipped = append(report.Skipped, LegacyMigrateEntry{
						Source: srcDir,
						Mode:   "skipped",
						Reason: "no tenant ownership resolved",
					})
					continue
				}
				if opts.DryRun {
					report.Migrated = append(report.Migrated, LegacyMigrateEntry{
						Source: srcDir,
						Name:   keyName,
						Mode:   "local",
						Reason: "dry-run",
					})
					continue
				}
				vol, created, err := st.AdoptBlockVolume(adoptOpts)
				if err != nil {
					return report, fmt.Errorf("adopt local workspace %s: %w", srcDir, err)
				}
				if created {
					if err := copyDirContents(srcDir, vol.DataDir); err != nil {
						return report, fmt.Errorf("copy local workspace %s -> %s: %w", srcDir, vol.DataDir, err)
					}
				}
				report.Migrated = append(report.Migrated, LegacyMigrateEntry{
					Source:   srcDir,
					VolumeID: vol.VolumeID,
					Name:     vol.Name,
					PVCName:  vol.PVCName,
					Mode:     "local",
				})
			}
		}
	}

	return report, nil
}

func lookupOwnerMap(m map[string]string, keys ...string) (string, bool) {
	if len(m) == 0 {
		return "", false
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func copyDirContents(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func handleVolume(ctx context.Context, args []string, jsonOutput bool) {
	if len(args) == 0 || args[0] != "migrate-legacy" {
		fmt.Fprintln(os.Stderr, "Usage: cubepod volume migrate-legacy --volumes-dir <dir> [--namespace <ns>] [--owner-map <file.json>] [--default-api-key <key>] [--workspace-base-dir <dir>] [--clone-to-canonical] [--snapshot-class <class>] [--dry-run]")
		os.Exit(1)
	}
	fs := flag.NewFlagSet("volume migrate-legacy", flag.ExitOnError)
	volumesDir := fs.String("volumes-dir", os.Getenv("CUBEPOD_VOLUMES_DIR"), "Path to gateway volume store directory")
	namespace := fs.String("namespace", "cubepod-workloads", "Kubernetes workload namespace")
	ownerMapPath := fs.String("owner-map", "", "Path to JSON file mapping pvcName/volumeKey to tenantHash or API key")
	defaultAPIKey := fs.String("default-api-key", "", "Explicit default API key for unowned legacy workspaces")
	workspaceBaseDir := fs.String("workspace-base-dir", "", "Optional local workspace base directory to migrate")
	cloneToCanonical := fs.Bool("clone-to-canonical", false, "Clone legacy PVCs via CSI VolumeSnapshot into canonical vol-<id> PVCs")
	snapshotClass := fs.String("snapshot-class", "", "Optional VolumeSnapshotClassName for --clone-to-canonical")
	kubeconfigPath := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig (in-cluster config used if omitted)")
	dryRun := fs.Bool("dry-run", false, "Preview migration actions without writing metadata or patching PVCs")
	_ = fs.Parse(args[1:])

	if strings.TrimSpace(*volumesDir) == "" {
		fatal(errors.New("--volumes-dir (or CUBEPOD_VOLUMES_DIR) is required"))
	}
	st, err := volume.Open(*volumesDir, volume.Config{})
	if err != nil {
		fatal(fmt.Errorf("open volume store: %w", err))
	}
	var ownerMap map[string]string
	if strings.TrimSpace(*ownerMapPath) != "" {
		raw, err := os.ReadFile(*ownerMapPath)
		if err != nil {
			fatal(fmt.Errorf("read --owner-map: %w", err))
		}
		if err := json.Unmarshal(raw, &ownerMap); err != nil {
			fatal(fmt.Errorf("parse --owner-map JSON: %w", err))
		}
	}

	var kc controller.KubeClient
	if strings.TrimSpace(*workspaceBaseDir) == "" || strings.TrimSpace(*kubeconfigPath) != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		info, cfgErr := controller.LoadKubeconfig(*kubeconfigPath)
		if cfgErr != nil || info == nil || info.Server == "" {
			info, cfgErr = controller.LoadInClusterConfig()
		}
		if cfgErr == nil && info != nil && info.Server != "" {
			if rc, rcErr := controller.NewRESTKubeClientFromKubeconfig(info); rcErr == nil {
				kc = rc
			}
		}
		if kc == nil && strings.TrimSpace(*workspaceBaseDir) == "" {
			fatal(errors.New("no Kubernetes configuration found (set --kubeconfig or run in-cluster), or pass --workspace-base-dir for local migration"))
		}
	}

	report, err := MigrateLegacyWorkspaces(ctx, kc, st, LegacyMigrateOptions{
		Namespace:        *namespace,
		OwnerMap:         ownerMap,
		DefaultAPIKey:    *defaultAPIKey,
		WorkspaceBaseDir: *workspaceBaseDir,
		CloneToCanonical: *cloneToCanonical,
		SnapshotClass:    *snapshotClass,
		DryRun:           *dryRun,
	})
	if err != nil {
		fatal(err)
	}
	output(report, jsonOutput, func() {
		fmt.Printf("Migrated: %d, Skipped: %d\n", len(report.Migrated), len(report.Skipped))
		for _, m := range report.Migrated {
			fmt.Printf("  [MIGRATED] %s -> volumeID=%s name=%s pvc=%s (%s)\n", m.Source, m.VolumeID, m.Name, m.PVCName, m.Mode)
		}
		for _, sk := range report.Skipped {
			fmt.Printf("  [SKIPPED]  %s (%s)\n", sk.Source, sk.Reason)
		}
	})
}
