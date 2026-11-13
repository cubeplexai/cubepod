// Package controller implements the Kubernetes reconciliation logic for PodBox,
// PodTemplate, and PodWarmPool custom resources.
package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

// ReconcilerOption configures a Reconciler instance.
type ReconcilerOption func(*Reconciler)

// WithKubeClient configures the Kubernetes API client for resource lifecycle operations.
func WithKubeClient(client KubeClient) ReconcilerOption {
	return func(r *Reconciler) {
		if client != nil {
			r.kubeClient = client
		}
	}
}

// WithNamespace configures the default workload namespace.
func WithNamespace(ns string) ReconcilerOption {
	return func(r *Reconciler) {
		if ns != "" {
			r.namespace = ns
		}
	}
}

// WithVolumeMode configures the PVC storage volume mode ("Block" or "Filesystem", default "Block").
func WithVolumeMode(mode string) ReconcilerOption {
	return func(r *Reconciler) {
		if mode != "" {
			r.volumeMode = mode
		}
	}
}

// WithDefaultStorageClass configures the default CSI StorageClassName.
func WithDefaultStorageClass(sc string) ReconcilerOption {
	return func(r *Reconciler) {
		if sc != "" {
			r.defaultStorageClass = sc
		}
	}
}

// WithDefaultRuntimeClass configures the default container/microVM RuntimeClassName.
// Passing "" explicitly unsets the runtimeClass for standard runc/containerd runtimes.
func WithDefaultRuntimeClass(rc string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultRuntimeClass = rc
		r.runtimeClassSet = true
	}
}

// WithPoddInjectorImage configures the image used by the podd-installer initContainer.
func WithPoddInjectorImage(img string) ReconcilerOption {
	return func(r *Reconciler) {
		if img != "" {
			r.poddInjectorImage = img
		}
	}
}

// WithDefaultSandboxImage configures the default workload container image.
func WithDefaultSandboxImage(img string) ReconcilerOption {
	return func(r *Reconciler) {
		if img != "" {
			r.defaultSandboxImage = img
		}
	}
}

// WithE2BTemplateAPI configures whether the E2B template API feature gate is enabled.
func WithE2BTemplateAPI(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.e2bTemplateAPI = enabled
	}
}

// WithTemplateBuildEnabled configures whether Beta image builds are enabled.
func WithTemplateBuildEnabled(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.templateBuildEnabled = enabled
	}
}

// WithImageBuilder configures the builder used for template image builds.
func WithImageBuilder(builder ImageBuilder) ReconcilerOption {
	return func(r *Reconciler) {
		if builder != nil {
			r.imageBuilder = builder
		}
	}
}

// WithContextStore configures the context store for upload tars.
func WithContextStore(store ContextStore) ReconcilerOption {
	return func(r *Reconciler) {
		if store != nil {
			r.contextStore = store
		}
	}
}

// WithContextDir configures the directory for storing context and build logs.
func WithContextDir(dir string) ReconcilerOption {
	return func(r *Reconciler) {
		if dir != "" {
			r.contextDir = dir
			r.contextDirSet = true
		}
	}
}

// WithTemplatePullSecret configures the imagePullSecret for template images.
func WithTemplatePullSecret(secret string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templatePullSecret = secret
	}
}

// WithTemplateRegistry configures the target OCI registry used when pushing built templates.
func WithTemplateRegistry(registry string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templateRegistry = strings.TrimRight(strings.TrimSpace(registry), "/")
	}
}

// WithTemplateRepoPrefix configures the repository prefix under the target template registry.
func WithTemplateRepoPrefix(prefix string) ReconcilerOption {
	return func(r *Reconciler) {
		if strings.TrimSpace(prefix) != "" {
			r.templateRepoPrefix = strings.Trim(strings.TrimSpace(prefix), "/")
		}
	}
}

// WithTemplatePushSecret configures the Kubernetes dockerconfigjson secret used to push built templates.
func WithTemplatePushSecret(secret string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templatePushSecret = strings.TrimSpace(secret)
	}
}

// WithTemplateProbeTimeout configures the timeout for template ReadyCmd probes.
func WithTemplateProbeTimeout(timeout time.Duration) ReconcilerOption {
	return func(r *Reconciler) {
		if timeout > 0 {
			r.templateProbeTimeout = timeout
		}
	}
}

// WithStateDir configures the local state directory for persisted template JSON files, context tars, and logs.
func WithStateDir(dir string) ReconcilerOption {
	return func(r *Reconciler) {
		if dir != "" {
			r.stateDir = dir
			if !r.contextDirSet {
				r.contextDir = filepath.Join(dir, "template-context")
			}
		}
	}
}

// WithEgressCA configures the root CA used to mint per-sandbox mTLS client certificates.
func WithEgressCA(ca *egress.CA) ReconcilerOption {
	return func(r *Reconciler) {
		if ca != nil {
			r.egressCA = ca
		}
	}
}

// WithDefaultImagePullSecrets configures default Kubernetes imagePullSecrets attached to sandbox Pods.
func WithDefaultImagePullSecrets(secrets ...string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultImagePullSecrets = nil
		for _, item := range secrets {
			for _, s := range strings.Split(item, ",") {
				s = strings.TrimSpace(s)
				if s != "" && !slices.Contains(r.defaultImagePullSecrets, s) {
					r.defaultImagePullSecrets = append(r.defaultImagePullSecrets, s)
				}
			}
		}
	}
}

// Reconciler manages the lifecycle of PodBox instances, pre-attached PodWarmPools,
// and dual Raw Block PVCs.
type Reconciler struct {
	mu                      sync.RWMutex
	boxes                   map[string]*v1alpha1.PodBoxStatus
	specs                   map[string]v1alpha1.PodBoxSpec
	resolvedImages          map[string]string
	boxAnnotations          map[string]map[string]string
	templates               map[string]v1alpha1.PodTemplateSpec
	warmPools               map[string]*warmPoolState
	warmCounter             int64
	kubeClient              KubeClient
	namespace               string
	volumeMode              string // "Block" or "Filesystem", default "Block"
	defaultStorageClass     string // default "standard"
	defaultRuntimeClass     string // default "kata-qemu"
	runtimeClassSet         bool
	poddInjectorImage       string // default "cubepod-podd:latest"
	defaultSandboxImage     string // default "ubuntu:24.04"
	FailWarmPoolClaim       bool
	e2bTemplateAPI          bool
	templateBuildEnabled    bool
	imageBuilder            ImageBuilder
	contextStore            ContextStore
	contextDir              string
	contextDirSet           bool
	stateDir                string
	templatePullSecret      string
	defaultImagePullSecrets []string
	templatePushSecret      string
	templateRegistry        string
	templateRepoPrefix      string
	templateProbeTimeout    time.Duration
	templateObjects         map[string]*v1alpha1.PodTemplate
	templateIndex           map[string]string
	idempotency             map[string]idempotencyRecord
	buildingPids            map[string]int
	buildMetrics            templateBuildMetricsCollector
	egressCA                *egress.CA
}

// SetFailWarmPoolClaim sets whether warm pool claims should fail (used for testing fallback).
func (r *Reconciler) SetFailWarmPoolClaim(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FailWarmPoolClaim = fail
}

// NewReconciler creates a new in-memory/cluster Reconciler.
func NewReconciler(opts ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		boxes:                make(map[string]*v1alpha1.PodBoxStatus),
		specs:                make(map[string]v1alpha1.PodBoxSpec),
		resolvedImages:       make(map[string]string),
		boxAnnotations:       make(map[string]map[string]string),
		templates:            make(map[string]v1alpha1.PodTemplateSpec),
		warmPools:            make(map[string]*warmPoolState),
		kubeClient:           NewMemoryKubeClient(),
		namespace:            "cubepod-workloads",
		volumeMode:           "Block",
		defaultStorageClass:  "standard",
		defaultRuntimeClass:  "kata-qemu",
		poddInjectorImage:    "cubepod-podd:latest",
		defaultSandboxImage:  "ubuntu:24.04",
		e2bTemplateAPI:       false,
		templateBuildEnabled: false,
		imageBuilder:         NewFakeImageBuilder(),
		contextStore:         NewMemoryContextStore(),
		contextDir:           filepath.Join(os.TempDir(), "cubepod-context"),
		stateDir:             os.Getenv("CUBEPOD_STATE"),
		templateRepoPrefix:   "cubepod-templates",
		templateProbeTimeout: 120 * time.Second,
		templateObjects:      make(map[string]*v1alpha1.PodTemplate),
		templateIndex:        make(map[string]string),
		idempotency:          make(map[string]idempotencyRecord),
		buildingPids:         make(map[string]int),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.egressCA == nil {
		ca, err := egress.NewCA()
		if err != nil {
			slog.Warn("failed to initialize default egress CA", "err", err)
		} else {
			r.egressCA = ca
		}
	}
	if r.stateDir != "" {
		if mk, ok := r.kubeClient.(*MemoryKubeClient); ok {
			mk.LoadLocalTemplates(r.stateDir)
		}
		_ = r.loadLocalTemplates(context.Background())
	}
	_ = r.SyncTemplatesFromKube(context.Background())
	_ = r.EnsureSeededTemplates(context.Background())
	return r
}

// EgressCA returns the configured root CA used to mint per-sandbox mTLS client certificates.
func (r *Reconciler) EgressCA() *egress.CA {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.egressCA
}

// SetKubeClient updates the Kubernetes API client.
func (r *Reconciler) SetKubeClient(client KubeClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client != nil {
		r.kubeClient = client
	}
}

// KubeClient returns the currently configured Kubernetes client.
func (r *Reconciler) KubeClient() KubeClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.kubeClient
}

// ContextStore returns the currently configured build context and log store.
func (r *Reconciler) ContextStore() ContextStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.contextStore
}

// PingBuilder verifies connectivity to the configured ImageBuilder (BuildKit or Docker).
func (r *Reconciler) PingBuilder(ctx context.Context) error {
	r.mu.RLock()
	builder := r.imageBuilder
	r.mu.RUnlock()
	if builder == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return builder.Ping(ctx)
}

// SweepTemplateContexts removes expired template build contexts (24h TTL) and logs (7d TTL).
func (r *Reconciler) SweepTemplateContexts(now time.Time) error {
	r.mu.RLock()
	store := r.contextStore
	r.mu.RUnlock()
	if store == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return store.SweepExpired(now, 24*time.Hour, 7*24*time.Hour)
}

// RegisterTemplate saves or updates a reusable PodTemplateSpec.
func (r *Reconciler) RegisterTemplate(name string, spec v1alpha1.PodTemplateSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templates[name] = spec
	if r.templateObjects[name] == nil {
		tpl := v1alpha1.PodTemplate{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodTemplate",
			Metadata: v1alpha1.ObjectMeta{
				Name:      name,
				Namespace: r.namespace,
			},
			Spec: spec,
			Status: v1alpha1.PodTemplateStatus{
				TemplateID:   "tpl_" + Slugify(name),
				BuildStatus:  v1alpha1.BuildStatusReady,
				ReadyImage:   spec.Image,
				ReadyBuildID: "bld_reg",
			},
		}
		r.templateObjects[name] = &tpl
		_ = r.indexTemplateLocked(&tpl)
		if r.kubeClient != nil {
			_ = r.kubeClient.CreatePodTemplate(context.Background(), tpl)
		}
	} else {
		tpl := r.templateObjects[name]
		tpl.Spec = spec
		if tpl.Status.ReadyImage == "" {
			tpl.Status.ReadyImage = spec.Image
			tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
		}
		_ = r.indexTemplateLocked(tpl)
		if r.kubeClient != nil {
			_ = r.kubeClient.UpdatePodTemplateStatus(context.Background(), *tpl)
		}
	}
}

// E2BTemplateAPIEnabled reports whether the E2B template API feature gate is on.
func (r *Reconciler) E2BTemplateAPIEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.e2bTemplateAPI
}

// SetE2BTemplateAPI toggles the E2B template API feature gate.
func (r *Reconciler) SetE2BTemplateAPI(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.e2bTemplateAPI = enabled
}

// TemplateBuildEnabled reports whether Beta image builds are enabled.
func (r *Reconciler) TemplateBuildEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.templateBuildEnabled
}

// SetTemplateBuildEnabled toggles whether Beta image builds are enabled.
func (r *Reconciler) SetTemplateBuildEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templateBuildEnabled = enabled
}

// SetImageBuilder updates the builder used for template image builds.
func (r *Reconciler) SetImageBuilder(builder ImageBuilder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if builder != nil {
		r.imageBuilder = builder
	}
}

// SetContextStore updates the store used for uploading context tars.
func (r *Reconciler) SetContextStore(store ContextStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if store != nil {
		r.contextStore = store
	}
}

// GetTemplate retrieves a registered PodTemplateSpec by name.
func (r *Reconciler) GetTemplate(name string) (v1alpha1.PodTemplateSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tpl, ok := r.templates[name]
	return tpl, ok
}

func (r *Reconciler) defaultTemplate(templateID string) v1alpha1.PodTemplateSpec {
	img := templateID
	if img == "" {
		img = r.defaultSandboxImage
	}
	if img == "" {
		img = "ubuntu:24.04"
	}

	rtClass := "kata-qemu"
	if r.runtimeClassSet {
		rtClass = r.defaultRuntimeClass
	} else if strings.EqualFold(r.volumeMode, "Filesystem") {
		rtClass = ""
	}

	poddImg := r.poddInjectorImage
	if poddImg == "" {
		poddImg = "cubepod-podd:latest"
	}

	sc := r.defaultStorageClass
	if sc == "" {
		sc = "standard"
	}

	return v1alpha1.PodTemplateSpec{
		RuntimeClassName: rtClass,
		Image:            img,
		PoddImage:        poddImg,
		Resources: v1alpha1.ResourceRequirements{
			CPURequest:    "1",
			CPULimit:      "2",
			MemoryRequest: "2Gi",
			MemoryLimit:   "4Gi",
		},
		Storage: v1alpha1.StorageSpec{
			StorageClassName: sc,
			RootfsSize:       "10Gi",
			WorkspaceSize:    "20Gi",
		},
	}
}

func defaultTemplate(templateID string) v1alpha1.PodTemplateSpec {
	return NewReconciler().defaultTemplate(templateID)
}

// ReconcileWarmPool manages standby Kata MicroVM instances with pre-attached thin block PVCs.
func (r *Reconciler) ReconcileWarmPool(ctx context.Context, poolName string, spec v1alpha1.PodWarmPoolSpec) (*v1alpha1.PodWarmPoolStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	pool, exists := r.warmPools[poolName]
	if !exists {
		pool = &warmPoolState{
			spec:      spec,
			entries:   make([]*WarmPodEntry, 0),
			persisted: false,
			createdAt: time.Now().UTC(),
		}
		r.warmPools[poolName] = pool
	}
	pool.spec = spec

	var tpl v1alpha1.PodTemplateSpec
	resolved, err := r.resolveTemplateLocked(spec.TemplateRef)
	if err == nil && resolved != nil {
		tpl = resolved.TemplateSpec
		tpl.Image = resolved.Image
		tpl.StartCmd = resolved.StartCmd
		tpl.ReadyCmd = resolved.ReadyCmd
		tpl.RunAsUser = resolved.RunAsUser
		tpl.RunAsGroup = resolved.RunAsGroup
		tpl.RunAsUsername = resolved.RunAsUsername
		if resolved.CPUCount > 0 {
			tpl.Resources.CPULimit = fmt.Sprintf("%d", resolved.CPUCount)
		}
		if resolved.MemoryMB > 0 {
			tpl.Resources.MemoryLimit = fmt.Sprintf("%dMi", resolved.MemoryMB)
		}
		if resolved.DiskSizeMB > 0 {
			tpl.Storage.RootfsSize = fmt.Sprintf("%dMi", resolved.DiskSizeMB)
		}
	} else {
		var ok bool
		tpl, ok = r.templates[spec.TemplateRef]
		if !ok {
			tpl = r.defaultTemplate(spec.TemplateRef)
		} else {
			if tpl.RuntimeClassName == "" && r.runtimeClassSet {
				tpl.RuntimeClassName = r.defaultRuntimeClass
			}
			if tpl.Storage.StorageClassName == "" && r.defaultStorageClass != "" {
				tpl.Storage.StorageClassName = r.defaultStorageClass
			}
			if tpl.Image == "" && r.defaultSandboxImage != "" {
				tpl.Image = r.defaultSandboxImage
			}
			if tpl.PoddImage == "" && r.poddInjectorImage != "" {
				tpl.PoddImage = r.poddInjectorImage
			}
		}
	}

	// Count existing available (unclaimed) replicas.
	var availableEntries []*WarmPodEntry
	for _, entry := range pool.entries {
		if !entry.Claimed {
			availableEntries = append(availableEntries, entry)
		}
	}
	currentAvail := int32(len(availableEntries))

	// Ensure warmCounter advances past existing indexes
	for _, entry := range pool.entries {
		if idx := parseWarmIndex(entry.ID); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(entry.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(entry.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(entry.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
	}
	for _, box := range r.boxes {
		if box == nil {
			continue
		}
		if idx := parseWarmIndex(box.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
	}

	// Scale up if needed.
	if currentAvail < spec.Replicas {
		for attempts := 0; currentAvail < spec.Replicas && attempts < int(spec.Replicas)*3; attempts++ {
			r.warmCounter++
			for {
				entryID := fmt.Sprintf("%s-warm-%d", poolName, r.warmCounter)
				rootfsName := entryID + "-rootfs-pvc"
				wsName := entryID + "-workspace-pvc"
				conflict := false
				for _, b := range r.boxes {
					if b != nil && (b.RootfsPVCName == rootfsName || b.WorkspacePVCName == wsName) {
						conflict = true
						break
					}
				}
				if !conflict {
					for _, entry := range pool.entries {
						if entry != nil && (entry.ID == entryID || entry.RootfsPVCName == rootfsName || entry.WorkspacePVCName == wsName) {
							conflict = true
							break
						}
					}
				}
				if !conflict {
					break
				}
				r.warmCounter++
			}
			entryID := fmt.Sprintf("%s-warm-%d", poolName, r.warmCounter)
			warmBoxSpec := v1alpha1.PodBoxSpec{
				TemplateRef:      spec.TemplateRef,
				DesiredState:     v1alpha1.DesiredStateRunning,
				RootfsGeneration: 1,
				WarmPoolRef:      poolName,
			}

			rootfsPVC, wsPVC := BuildBlockPVCs(entryID, tpl, warmBoxSpec, r.volumeMode)
			rootfsPVC.Namespace = r.namespace
			wsPVC.Namespace = r.namespace

			_ = r.kubeClient.EnsurePVC(ctx, rootfsPVC)
			_ = r.kubeClient.EnsurePVC(ctx, wsPVC)

			dummyStatus := v1alpha1.PodBoxStatus{
				PodName:          fmt.Sprintf("%s-kata-pod", entryID),
				RootfsPVCName:    rootfsPVC.Name,
				WorkspacePVCName: wsPVC.Name,
				Phase:            v1alpha1.PodBoxPhaseRunning,
			}
			podArgs := append([]string{r.volumeMode, r.templatePullSecret}, r.defaultImagePullSecrets...)
			podManifest := BuildKataPod(entryID, tpl, warmBoxSpec, dummyStatus, podArgs...)
			podManifest.Namespace = r.namespace
			if podManifest.Labels == nil {
				podManifest.Labels = make(map[string]string)
			}
			podManifest.Labels["cubepod.io/warmpool"] = poolName
			podManifest.Labels["cubepod.io/template"] = spec.TemplateRef
			podManifest.Labels["cubepod.io/claimed"] = "false"

			is409 := false
			podIP, err := r.kubeClient.EnsurePod(ctx, podManifest)
			if err != nil {
				if errors.Is(err, ErrPodAlreadyExists) || strings.Contains(strings.ToLower(err.Error()), "409") || strings.Contains(strings.ToLower(err.Error()), "already exists") || strings.Contains(strings.ToLower(err.Error()), "conflict") {
					is409 = true
				} else {
					slog.Warn("failed to ensure warm pod", "pod", dummyStatus.PodName, "err", err)
					continue
				}
			}
			if podIP != "" {
				dummyStatus.PodIP = podIP
			}

			var claimedBy string
			// When EnsurePod returns 409 (pod already exists), check its label:
			// if it's already claimed, do NOT add it as an unclaimed standby entry!
			if is409 {
				existingPods, listErr := r.kubeClient.ListPods(ctx, r.namespace, "")
				if listErr != nil {
					slog.Warn("failed to list pods to verify existing warm pod", "pod", dummyStatus.PodName, "err", listErr)
					continue
				}
				var foundPod *PodManifest
				for _, p := range existingPods {
					if p.Name == dummyStatus.PodName {
						foundPod = &p
						break
					}
				}
				if foundPod == nil {
					slog.Warn("existing pod not found in pod list", "pod", dummyStatus.PodName)
					continue
				}
				if foundPod.Labels != nil && foundPod.Labels["cubepod.io/claimed"] == "true" {
					slog.Info("pod already exists and is claimed, skipping standby adoption", "pod", dummyStatus.PodName)
					continue
				}
				// If false: copy labels and claimed-by
				if foundPod.Labels != nil {
					for k, v := range foundPod.Labels {
						podManifest.Labels[k] = v
					}
					claimedBy = foundPod.Labels["cubepod.io/claimed-by"]
				}
			}

			entry := &WarmPodEntry{
				ID:               entryID,
				PoolName:         poolName,
				TemplateRef:      spec.TemplateRef,
				PodName:          dummyStatus.PodName,
				RootfsPVCName:    rootfsPVC.Name,
				WorkspacePVCName: wsPVC.Name,
				Image:            tpl.Image,
				Claimed:          false,
				ClaimedBy:        claimedBy,
				CreatedAt:        time.Now().UTC(),
				PodManifest:      podManifest,
				RootfsPVC:        rootfsPVC,
				WorkspacePVC:     wsPVC,
			}
			pool.entries = append(pool.entries, entry)
			currentAvail++
		}
	} else if currentAvail > spec.Replicas && len(availableEntries) > 0 && len(pool.entries) > 0 {
		// Scale down excess unclaimed entries.
		toRemove := currentAvail - spec.Replicas
		var filtered []*WarmPodEntry
		var removedCount int32
		for _, entry := range pool.entries {
			if !entry.Claimed && removedCount < toRemove {
				removedCount++
				_ = r.kubeClient.DeletePod(ctx, r.namespace, entry.PodName)
				_ = r.kubeClient.DeletePVC(ctx, r.namespace, entry.RootfsPVCName)
				_ = r.kubeClient.DeletePVC(ctx, r.namespace, entry.WorkspacePVCName)
				continue
			}
			filtered = append(filtered, entry)
		}
		pool.entries = filtered
	}

	var finalAvail int32
	for _, entry := range pool.entries {
		if !entry.Claimed {
			finalAvail++
		}
	}

	st := &v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     finalAvail,
		AvailableReplicas: finalAvail,
	}

	// Persist the PodWarmPool CRD state to Kubernetes API
	if r.kubeClient != nil {
		if !pool.persisted {
			cr := v1alpha1.PodWarmPool{
				APIVersion: "cubepod.io/v1alpha1",
				Kind:       "PodWarmPool",
				Metadata: v1alpha1.ObjectMeta{
					Name:      poolName,
					Namespace: r.namespace,
				},
				Spec:   spec,
				Status: *st,
			}
			if err := r.kubeClient.UpsertPodWarmPool(ctx, cr); err != nil {
				if errors.Is(err, ErrCRDNotInstalled) {
					slog.Debug("PodWarmPool CRD not installed on APIServer; skipping pool persistence", "pool", poolName)
				} else {
					slog.Warn("failed to upsert initial PodWarmPool CR", "pool", poolName, "err", err)
				}
			} else {
				pool.persisted = true
			}
		} else {
			err := r.kubeClient.UpdatePodWarmPoolStatus(ctx, r.namespace, poolName, *st)
			if err != nil {
				if errors.Is(err, ErrCRDNotInstalled) {
					slog.Debug("PodWarmPool CRD not installed on APIServer; skipping status update", "pool", poolName)
				} else {
					errStr := strings.ToLower(err.Error())
					if strings.Contains(errStr, "not found") || strings.Contains(errStr, "404") {
						slog.Debug("PodWarmPool not found in Kubernetes; skipping status update and recreation", "pool", poolName, "err", err)
					} else {
						slog.Warn("failed to update PodWarmPool status", "pool", poolName, "err", err)
					}
				}
			}
		}
	}

	return st, nil
}

// ClaimWarmPod claims a standby warm pod from the given pool for a new PodBox.
func (r *Reconciler) ClaimWarmPod(ctx context.Context, poolName string, boxName string) (*WarmPodEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claimWarmPodLocked(ctx, poolName, boxName)
}

func (r *Reconciler) claimWarmPodLocked(ctx context.Context, poolName string, boxName string) (*WarmPodEntry, error) {
	if r.FailWarmPoolClaim {
		return nil, fmt.Errorf("simulated warm pool claim failure")
	}

	pool, ok := r.warmPools[poolName]
	if !ok {
		return nil, fmt.Errorf("warm pool %q not found", poolName)
	}

	expectedImage := ""
	if resolved, err := r.resolveTemplateLocked(pool.spec.TemplateRef); err == nil && resolved != nil {
		expectedImage = resolved.Image
	}
	if expectedImage == "" {
		if t, ok := r.templates[pool.spec.TemplateRef]; ok {
			expectedImage = t.Image
		}
	}

	for _, entry := range pool.entries {
		if !entry.Claimed {
			if entry.Image != "" && expectedImage != "" && entry.Image != expectedImage {
				continue
			}
			if r.kubeClient != nil {
				if ctx == nil {
					ctx = context.Background()
				}
				if err := r.kubeClient.ClaimPod(ctx, r.namespace, entry.PodName, boxName); err != nil {
					if errors.Is(err, ErrCASConflict) {
						slog.Warn("CAS conflict claiming standby pod, marked as claimed by other replica", "pod", entry.PodName, "box", boxName, "err", err)
						entry.Claimed = true
						entry.ClaimedBy = "other-replica"
					} else {
						slog.Warn("transient/unknown error claiming standby pod, leaving unclaimed for retry", "pod", entry.PodName, "box", boxName, "err", err)
					}
					continue
				}
			}

			entry.Claimed = true
			entry.ClaimedBy = boxName
			now := time.Now().UTC()
			entry.ClaimedAt = &now
			if entry.PodManifest.Labels == nil {
				entry.PodManifest.Labels = make(map[string]string)
			}
			entry.PodManifest.Labels["cubepod.io/claimed"] = "true"
			entry.PodManifest.Labels["cubepod.io/claimed-by"] = boxName

			return entry, nil
		}
	}

	return nil, fmt.Errorf("warm pool %q has no available standby replicas", poolName)
}

// GetWarmPoolStatus returns the current ready and available count for a warm pool.
func (r *Reconciler) GetWarmPoolStatus(poolName string) (*v1alpha1.PodWarmPoolStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pool, ok := r.warmPools[poolName]
	if !ok {
		return nil, false
	}

	var avail int32
	for _, entry := range pool.entries {
		if !entry.Claimed {
			avail++
		}
	}

	return &v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     avail,
		AvailableReplicas: avail,
	}, true
}

// Reconcile drives a PodBox towards its DesiredState (Running or Paused),
// preserving both RootFS Upper PVC and Workspace PVC across pause/resume cycles,
// claiming from PodWarmPool if configured, and supporting CoW Fork cloning.
func (r *Reconciler) Reconcile(ctx context.Context, name string, spec v1alpha1.PodBoxSpec) (*v1alpha1.PodBoxStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	status, exists := r.boxes[name]

	resolved, err := r.resolveTemplateLocked(spec.TemplateRef)
	if err != nil {
		return nil, err
	}
	effectiveImage := resolved.Image
	if exists {
		if existingBox, getErr := r.kubeClient.GetPodBox(ctx, r.namespace, name); getErr == nil && existingBox != nil && existingBox.Metadata.Annotations != nil {
			r.mergeBoxAnnotationsLocked(name, existingBox.Metadata.Annotations)
			if pinned := existingBox.Metadata.Annotations["cubepod.io/resolved-image"]; pinned != "" && r.resolvedImages[name] == "" {
				r.resolvedImages[name] = pinned
			}
		}
		if pinned := r.resolvedImages[name]; pinned != "" {
			effectiveImage = pinned
		}
	} else if effectiveImage != "" {
		r.resolvedImages[name] = effectiveImage
	}

	tpl := resolved.TemplateSpec
	tpl.Image = effectiveImage
	tpl.StartCmd = resolved.StartCmd
	tpl.ReadyCmd = resolved.ReadyCmd
	tpl.RunAsUser = resolved.RunAsUser
	tpl.RunAsGroup = resolved.RunAsGroup
	tpl.RunAsUsername = resolved.RunAsUsername
	if resolved.CPUCount > 0 {
		tpl.Resources.CPULimit = fmt.Sprintf("%d", resolved.CPUCount)
	}
	if resolved.MemoryMB > 0 {
		tpl.Resources.MemoryLimit = fmt.Sprintf("%dMi", resolved.MemoryMB)
	}
	if resolved.DiskSizeMB > 0 {
		tpl.Storage.RootfsSize = fmt.Sprintf("%dMi", resolved.DiskSizeMB)
	}

	if !exists {
		// 1. Pre-Attached WarmPool Claim (when warmPoolRef is set and no ForkFrom)
		if spec.WarmPoolRef != "" && spec.ForkFrom == nil {
			entry, err := r.claimWarmPodLocked(ctx, spec.WarmPoolRef, name)
			if err != nil {
				return nil, fmt.Errorf("failed to claim from warm pool %q for box %q: %w", spec.WarmPoolRef, name, err)
			}
			status = &v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseRunning,
				PodName:           entry.PodName,
				RootfsPVCName:     entry.RootfsPVCName,
				WorkspacePVCName:  entry.WorkspacePVCName,
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "WarmPoolClaimed",
				Message:           fmt.Sprintf("Claimed standby warm pod %s from pool %s", entry.ID, spec.WarmPoolRef),
			}
		} else if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
			// 2. CoW Fork provisioning from parent sandbox
			parentRootfsPVC, parentWorkspacePVC := r.resolveForkParentPVCsLocked(ctx, spec.ForkFrom.PodBoxName)
			rootfsName := fmt.Sprintf("%s-rootfs-pvc", name)
			if spec.RootfsGeneration > 1 {
				rootfsName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
			}
			status = &v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseRunning,
				PodName:           fmt.Sprintf("%s-kata-pod", name),
				RootfsPVCName:     rootfsName,
				WorkspacePVCName:  fmt.Sprintf("%s-workspace-pvc", name),
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "ForkCloned",
				Message:           fmt.Sprintf("Forked from parent sandbox %s", spec.ForkFrom.PodBoxName),
			}
			rootfsPVC, wsPVC := BuildBlockPVCs(name, tpl, spec, r.volumeMode, parentRootfsPVC, parentWorkspacePVC)
			rootfsPVC.Namespace = r.namespace
			wsPVC.Namespace = r.namespace
			_ = r.kubeClient.EnsurePVC(ctx, rootfsPVC)
			_ = r.kubeClient.EnsurePVC(ctx, wsPVC)
		} else {
			// 3. Standard fresh sandbox provisioning
			rootfsName := fmt.Sprintf("%s-rootfs-pvc", name)
			if spec.RootfsGeneration > 1 {
				rootfsName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
			}
			status = &v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseProvisioning,
				RootfsPVCName:     rootfsName,
				WorkspacePVCName:  fmt.Sprintf("%s-workspace-pvc", name),
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "Provisioned",
				Message:           "Provisioned fresh sandbox",
			}
			rootfsPVC, wsPVC := BuildBlockPVCs(name, tpl, spec, r.volumeMode)
			rootfsPVC.Namespace = r.namespace
			wsPVC.Namespace = r.namespace
			_ = r.kubeClient.EnsurePVC(ctx, rootfsPVC)
			_ = r.kubeClient.EnsurePVC(ctx, wsPVC)
		}
		r.boxes[name] = status
	}

	// Handle independent RootFS reset while keeping Workspace PVC intact.
	if spec.RootfsGeneration > status.ObservedRootfsGen {
		slog.Info("resetting RootFS Upper PVC while preserving Workspace PVC",
			"podBox", name,
			"oldGen", status.ObservedRootfsGen,
			"newGen", spec.RootfsGeneration,
			"workspacePVC", status.WorkspacePVCName,
		)
		oldRootfsPVC := status.RootfsPVCName
		status.ObservedRootfsGen = spec.RootfsGeneration
		status.RootfsPVCName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
		status.Reason = "RootFSReset"
		status.Message = fmt.Sprintf("RootFS reset to generation %d", spec.RootfsGeneration)

		// Delete old compute Pod first so it unmounts the old RootFS PVC and allows a new Pod to be created
		if status.PodName != "" {
			_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
			status.PodName = ""
			status.PodIP = ""
		}

		// Delete old RootFS PVC and apply new RootFS PVC.
		if oldRootfsPVC != "" {
			_ = r.kubeClient.DeletePVC(ctx, r.namespace, oldRootfsPVC)
		}
		newRootfsPVC, _ := BuildBlockPVCs(name, tpl, spec, r.volumeMode)
		newRootfsPVC.Namespace = r.namespace
		_ = r.kubeClient.EnsurePVC(ctx, newRootfsPVC)
	}

	switch spec.DesiredState {
	case v1alpha1.DesiredStatePaused:
		if status.PodName != "" {
			_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
		}
		r.deleteEgressSecretsLocked(ctx, name)
		status.Phase = v1alpha1.PodBoxPhasePaused
		status.PodName = ""
		status.PodIP = ""
		status.PausedAt = &now
	default:
		if tpl.Egress.Enabled {
			if err := r.ensureEgressSecretsLocked(ctx, name, spec); err != nil {
				return nil, fmt.Errorf("failed to ensure egress secrets: %w", err)
			}
		}
		status.Phase = v1alpha1.PodBoxPhaseRunning
		if status.PodName == "" {
			status.PodName = fmt.Sprintf("%s-kata-pod", name)
		}
		podArgs := append([]string{r.volumeMode, r.templatePullSecret}, r.defaultImagePullSecrets...)
		podManifest := BuildKataPod(name, tpl, spec, *status, podArgs...)
		podManifest.Namespace = r.namespace
		podIP, err := r.kubeClient.EnsurePod(ctx, podManifest)
		if err != nil {
			errStr := strings.ToLower(err.Error())
			if strings.Contains(errStr, "409") || strings.Contains(errStr, "already exists") || strings.Contains(errStr, "conflict") {
				err = nil
			} else {
				return nil, fmt.Errorf("failed to ensure Pod: %w", err)
			}
		}
		if podIP != "" {
			status.PodIP = podIP
		}
		status.PodName = podManifest.Name
		status.LastActivityAt = &now
		status.PausedAt = nil
	}

	r.specs[name] = spec
	cp := *status

	anns := r.podBoxAnnotationsLocked(name, effectiveImage)
	if anns != nil {
		r.boxAnnotations[name] = maps.Clone(anns)
	}

	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:        name,
			Namespace:   r.namespace,
			Annotations: anns,
		},
		Spec:   spec,
		Status: cp,
	}
	_ = r.kubeClient.UpsertPodBox(ctx, box)

	return &cp, nil
}

func (r *Reconciler) mergeBoxAnnotationsLocked(name string, incoming map[string]string) {
	if len(incoming) == 0 {
		return
	}
	cur := r.boxAnnotations[name]
	if cur == nil {
		cur = make(map[string]string, len(incoming))
	}
	for k, v := range incoming {
		cur[k] = v
	}
	r.boxAnnotations[name] = cur
}

func (r *Reconciler) podBoxAnnotationsLocked(name, fallbackImage string) map[string]string {
	anns := make(map[string]string)
	for k, v := range r.boxAnnotations[name] {
		anns[k] = v
	}

	img := r.resolvedImages[name]
	if img == "" {
		img = fallbackImage
	}
	if img != "" {
		anns["cubepod.io/resolved-image"] = img
	}

	if spec, ok := r.specs[name]; ok {
		templateID := spec.TemplateRef
		if templateID == "" {
			templateID = "base"
		}
		anns["cubepod.io/template-id"] = templateID

		if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
			anns["cubepod.io/forked-from"] = spec.ForkFrom.PodBoxName
		} else {
			delete(anns, "cubepod.io/forked-from")
		}

		gen := spec.RootfsGeneration
		if st, ok := r.boxes[name]; ok && st != nil && st.ObservedRootfsGen > gen {
			gen = st.ObservedRootfsGen
		}
		if gen <= 0 {
			gen = 1
		}
		anns["cubepod.io/rootfs-generation"] = strconv.FormatInt(gen, 10)
	}

	if len(anns) == 0 {
		return nil
	}
	return anns
}

func (r *Reconciler) resolveForkParentPVCsLocked(ctx context.Context, parentName string) (rootfsPVC, workspacePVC string) {
	if parentStatus, ok := r.boxes[parentName]; ok && parentStatus != nil {
		rootfsPVC = parentStatus.RootfsPVCName
		workspacePVC = parentStatus.WorkspacePVCName
		if rootfsPVC == "" && parentStatus.ObservedRootfsGen > 1 {
			rootfsPVC = fmt.Sprintf("%s-rootfs-gen%d-pvc", parentName, parentStatus.ObservedRootfsGen)
		}
	}
	if (rootfsPVC == "" || workspacePVC == "") && r.kubeClient != nil {
		if parentBox, err := r.kubeClient.GetPodBox(ctx, r.namespace, parentName); err == nil && parentBox != nil {
			if rootfsPVC == "" {
				rootfsPVC = parentBox.Status.RootfsPVCName
				if rootfsPVC == "" {
					gen := parentBox.Spec.RootfsGeneration
					if parentBox.Status.ObservedRootfsGen > gen {
						gen = parentBox.Status.ObservedRootfsGen
					}
					if gen > 1 {
						rootfsPVC = fmt.Sprintf("%s-rootfs-gen%d-pvc", parentName, gen)
					}
				}
			}
			if workspacePVC == "" {
				workspacePVC = parentBox.Status.WorkspacePVCName
			}
		}
	}
	return rootfsPVC, workspacePVC
}

func isSecretNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

func isSecretAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "409") || strings.Contains(msg, "conflict")
}

func (r *Reconciler) ensureSecret(ctx context.Context, secret KubeSecret) error {
	if r.kubeClient == nil {
		return nil
	}
	ns := secret.Metadata.Namespace
	if ns == "" {
		ns = r.namespace
		secret.Metadata.Namespace = ns
	}
	if _, err := r.kubeClient.GetSecret(ctx, ns, secret.Metadata.Name); err == nil {
		if delErr := r.kubeClient.DeleteSecret(ctx, ns, secret.Metadata.Name); delErr != nil && !isSecretNotFoundError(delErr) {
			return delErr
		}
	}
	err := r.kubeClient.CreateSecret(ctx, secret)
	if isSecretAlreadyExistsError(err) {
		if delErr := r.kubeClient.DeleteSecret(ctx, ns, secret.Metadata.Name); delErr != nil && !isSecretNotFoundError(delErr) {
			return delErr
		}
		return r.kubeClient.CreateSecret(ctx, secret)
	}
	return err
}

func generateMITMCAPEM(boxName string) (certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate MITM CA key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate MITM CA serial: %w", err)
	}
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("CubePod MITM CA (%s)", boxName),
			Organization: []string{"CubePod"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create MITM CA certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal MITM CA private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func serializeNetworkPolicy(policy *v1alpha1.NetworkPolicySpec) ([]byte, error) {
	if policy == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(policy)
}

func (r *Reconciler) ensureEgressSecretsLocked(ctx context.Context, boxName string, spec v1alpha1.PodBoxSpec) error {
	if r.kubeClient == nil {
		return nil
	}
	if r.egressCA == nil {
		ca, err := egress.NewCA()
		if err != nil {
			return fmt.Errorf("failed to initialize egress CA: %w", err)
		}
		r.egressCA = ca
	}

	policyJSON, err := serializeNetworkPolicy(spec.NetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to serialize network policy: %w", err)
	}

	mtlsSecretName := fmt.Sprintf("%s-egress-mtls", boxName)
	caSecretName := fmt.Sprintf("%s-egress-ca", boxName)
	labels := map[string]string{
		"app.kubernetes.io/name": "cubepod",
		"pod.cubeplex.ai/box":    boxName,
	}

	existingMtls, mtlsErr := r.kubeClient.GetSecret(ctx, r.namespace, mtlsSecretName)
	existingCA, caErr := r.kubeClient.GetSecret(ctx, r.namespace, caSecretName)
	if mtlsErr == nil && existingMtls != nil && caErr == nil && existingCA != nil {
		d := existingMtls.Data
		if len(d["tls.crt"]) > 0 && len(d["tls.key"]) > 0 && len(d["mitm-ca.crt"]) > 0 && len(d["mitm-ca.key"]) > 0 &&
			bytes.Equal(d["ca.crt"], r.egressCA.CACertPEM()) &&
			bytes.Equal(existingCA.Data["mitm-ca.crt"], d["mitm-ca.crt"]) {
			if bytes.Equal(d["policy.json"], policyJSON) {
				return nil
			}
			updatedMtls := KubeSecret{
				APIVersion: "v1",
				Kind:       "Secret",
				Metadata: v1alpha1.ObjectMeta{
					Name:      mtlsSecretName,
					Namespace: r.namespace,
					Labels:    labels,
				},
				Type: "Opaque",
				Data: map[string][]byte{
					"tls.crt":     d["tls.crt"],
					"tls.key":     d["tls.key"],
					"ca.crt":      d["ca.crt"],
					"mitm-ca.crt": d["mitm-ca.crt"],
					"mitm-ca.key": d["mitm-ca.key"],
					"policy.json": policyJSON,
				},
			}
			return r.ensureSecret(ctx, updatedMtls)
		}
	}

	tlsCertPEM, tlsKeyPEM, err := r.egressCA.MintSandboxCert(boxName, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("failed to mint sandbox mTLS cert: %w", err)
	}
	mitmCertPEM, mitmKeyPEM, err := generateMITMCAPEM(boxName)
	if err != nil {
		return err
	}

	mtlsSecret := KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: v1alpha1.ObjectMeta{
			Name:      mtlsSecretName,
			Namespace: r.namespace,
			Labels:    labels,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"tls.crt":     tlsCertPEM,
			"tls.key":     tlsKeyPEM,
			"ca.crt":      r.egressCA.CACertPEM(),
			"mitm-ca.crt": mitmCertPEM,
			"mitm-ca.key": mitmKeyPEM,
			"policy.json": policyJSON,
		},
	}
	if err := r.ensureSecret(ctx, mtlsSecret); err != nil {
		return fmt.Errorf("failed to ensure egress mTLS secret %s: %w", mtlsSecretName, err)
	}

	caSecret := KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: v1alpha1.ObjectMeta{
			Name:      caSecretName,
			Namespace: r.namespace,
			Labels:    labels,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"mitm-ca.crt": mitmCertPEM,
		},
	}
	if err := r.ensureSecret(ctx, caSecret); err != nil {
		return fmt.Errorf("failed to ensure egress CA secret %s: %w", caSecretName, err)
	}
	return nil
}

func (r *Reconciler) deleteEgressSecretsLocked(ctx context.Context, boxName string) {
	if r.kubeClient == nil {
		return
	}
	for _, secretName := range []string{
		fmt.Sprintf("%s-egress-mtls", boxName),
		fmt.Sprintf("%s-egress-ca", boxName),
	} {
		if err := r.kubeClient.DeleteSecret(ctx, r.namespace, secretName); err != nil && !isSecretNotFoundError(err) {
			slog.Warn("failed to delete egress secret", "secret", secretName, "err", err)
		}
	}
}

// Pause pauses a running PodBox by updating its DesiredState to Paused and reconciling.
func (r *Reconciler) Pause(ctx context.Context, name string) (*v1alpha1.PodBoxStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	spec, ok := r.specs[name]
	statusVal, statusOk := r.boxes[name]
	r.mu.RUnlock()
	if !ok && !statusOk {
		return nil, fmt.Errorf("podbox %s not found", name)
	}
	if !ok && statusOk {
		spec = v1alpha1.PodBoxSpec{
			DesiredState:     v1alpha1.DesiredStatePaused,
			RootfsGeneration: statusVal.ObservedRootfsGen,
		}
	} else {
		spec.DesiredState = v1alpha1.DesiredStatePaused
	}

	status, err := r.Reconcile(ctx, name, spec)
	if err != nil {
		return nil, err
	}

	r.mu.RLock()
	anns := r.podBoxAnnotationsLocked(name, "")
	r.mu.RUnlock()

	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:        name,
			Namespace:   r.namespace,
			Annotations: anns,
		},
		Spec:   spec,
		Status: *status,
	}
	_ = r.kubeClient.UpsertPodBox(ctx, box)

	return status, nil
}

// DeleteBox terminates a PodBox compute Pod and optionally deletes its underlying PVCs.
func (r *Reconciler) DeleteBox(ctx context.Context, name string, deleteVolumes bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	_ = r.kubeClient.DeletePodBox(ctx, r.namespace, name)
	r.deleteEgressSecretsLocked(ctx, name)

	status, ok := r.boxes[name]
	if !ok {
		delete(r.boxAnnotations, name)
		return nil
	}

	if status.PodName != "" {
		_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
	}
	if deleteVolumes {
		if status.RootfsPVCName != "" {
			_ = r.kubeClient.DeletePVC(ctx, r.namespace, status.RootfsPVCName)
		}
		if status.WorkspacePVCName != "" {
			_ = r.kubeClient.DeletePVC(ctx, r.namespace, status.WorkspacePVCName)
		}
	}

	delete(r.boxes, name)
	delete(r.specs, name)
	delete(r.resolvedImages, name)
	delete(r.boxAnnotations, name)
	return nil
}

func parseWarmIndex(s string) int64 {
	idx := strings.Index(s, "-warm-")
	if idx == -1 {
		return 0
	}
	rem := s[idx+len("-warm-"):]
	var n int64
	for _, ch := range rem {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int64(ch-'0')
		} else {
			break
		}
	}
	return n
}

// SyncFromKube recovers PodBox and PodWarmPool states from the Kubernetes API into memory.
// It queries the APIServer for all PodBoxes and PodWarmPools in the reconciler's namespace,
// populates r.boxes, r.specs, and r.warmPools in memory, and returns the count of recovered PodBoxes.
func (r *Reconciler) SyncFromKube(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if r.kubeClient == nil {
		return 0, nil
	}

	boxes, err := r.kubeClient.ListPodBoxes(ctx, r.namespace)
	if err != nil {
		return 0, fmt.Errorf("failed to list PodBoxes from Kubernetes: %w", err)
	}

	var crdMissing bool
	pools, err := r.kubeClient.ListPodWarmPools(ctx, r.namespace)
	if err != nil {
		if errors.Is(err, ErrCRDNotInstalled) {
			crdMissing = true
			slog.Debug("PodWarmPool CRD not installed, skipping warm pool sync and cleanup")
		} else {
			return 0, fmt.Errorf("failed to list PodWarmPools from Kubernetes: %w", err)
		}
	}

	// Query pods for each warm pool
	poolPods := make(map[string][]PodManifest, len(pools))
	if !crdMissing {
		for _, p := range pools {
			pName := p.Metadata.Name
			if pName == "" {
				continue
			}
			pods, err := r.kubeClient.ListPods(ctx, r.namespace, "cubepod.io/warmpool="+pName)
			if err != nil {
				return 0, fmt.Errorf("failed to list pods for warmpool %s: %w", pName, err)
			}
			poolPods[pName] = pods
		}
	}

	if err := r.SyncTemplatesFromKube(ctx); err != nil {
		return 0, fmt.Errorf("failed to sync PodTemplates from Kubernetes: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	recovered := 0
	for _, box := range boxes {
		name := box.Metadata.Name
		if name == "" {
			continue
		}
		st := box.Status
		r.boxes[name] = &st
		r.specs[name] = box.Spec
		if box.Metadata.Annotations != nil {
			r.mergeBoxAnnotationsLocked(name, box.Metadata.Annotations)
			if img := box.Metadata.Annotations["cubepod.io/resolved-image"]; img != "" {
				r.resolvedImages[name] = img
			}
		}
		if idx := parseWarmIndex(box.Status.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.Status.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.Status.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		recovered++
	}

	// Sync PodWarmPools from Kubernetes
	activePools := make(map[string]bool)
	if !crdMissing {
		for _, p := range pools {
			pName := p.Metadata.Name
			if pName == "" {
				continue
			}
			activePools[pName] = true
			pool, exists := r.warmPools[pName]
			if !exists {
				pool = &warmPoolState{
					spec:      p.Spec,
					entries:   make([]*WarmPodEntry, 0),
					persisted: true,
					createdAt: time.Now().UTC(),
				}
				r.warmPools[pName] = pool
			} else {
				pool.spec = p.Spec
				pool.persisted = true
			}

			// Reconstruct or update pool.entries from Kubernetes pods
			pods := poolPods[pName]
			existingEntries := make(map[string]*WarmPodEntry, len(pool.entries))
			for _, entry := range pool.entries {
				existingEntries[entry.PodName] = entry
			}

			var updatedEntries []*WarmPodEntry
			for _, pod := range pods {
				claimed := (pod.Labels != nil && pod.Labels["cubepod.io/claimed"] == "true")
				var claimedBy string
				if pod.Labels != nil {
					claimedBy = pod.Labels["cubepod.io/claimed-by"]
				}

				if entry, ok := existingEntries[pod.Name]; ok {
					entry.Claimed = claimed
					entry.ClaimedBy = claimedBy
					if entry.PodManifest.Labels == nil {
						entry.PodManifest.Labels = make(map[string]string)
					}
					for k, v := range pod.Labels {
						entry.PodManifest.Labels[k] = v
					}
					updatedEntries = append(updatedEntries, entry)
				} else {
					entryID := strings.TrimSuffix(pod.Name, "-kata-pod")
					tplRef := ""
					if pod.Labels != nil {
						tplRef = pod.Labels["cubepod.io/template"]
					}
					if tplRef == "" {
						tplRef = p.Spec.TemplateRef
					}
					newEntry := &WarmPodEntry{
						ID:               entryID,
						PoolName:         pName,
						TemplateRef:      tplRef,
						PodName:          pod.Name,
						RootfsPVCName:    entryID + "-rootfs-pvc",
						WorkspacePVCName: entryID + "-workspace-pvc",
						Claimed:          claimed,
						ClaimedBy:        claimedBy,
						CreatedAt:        time.Now().UTC(),
						PodManifest:      pod,
					}
					updatedEntries = append(updatedEntries, newEntry)
				}

				if idx := parseWarmIndex(pod.Name); idx > r.warmCounter {
					r.warmCounter = idx
				}
			}
			pool.entries = updatedEntries
		}

		for name, pool := range r.warmPools {
			if !activePools[name] {
				// Either it was persisted in K8s and deleted by user, OR it failed to persist for more than 10 seconds:
				if time.Since(pool.createdAt) > 10*time.Second {
					for _, entry := range pool.entries {
						if !entry.Claimed {
							_ = r.kubeClient.DeletePod(ctx, r.namespace, entry.PodName)
							_ = r.kubeClient.DeletePVC(ctx, r.namespace, entry.RootfsPVCName)
							_ = r.kubeClient.DeletePVC(ctx, r.namespace, entry.WorkspacePVCName)
						}
					}
					delete(r.warmPools, name)
				}
			}
		}
	}

	for _, pool := range r.warmPools {
		for _, entry := range pool.entries {
			if idx := parseWarmIndex(entry.ID); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.PodName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.RootfsPVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.WorkspacePVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
		}
	}
	for _, box := range r.boxes {
		if box == nil {
			continue
		}
		if idx := parseWarmIndex(box.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
	}

	return recovered, nil
}

// FindAvailableWarmPool looks for a warm pool that matches the template and has available warm replicas.
func (r *Reconciler) FindAvailableWarmPool(templateRef string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for name, pool := range r.warmPools {
		if pool.spec.TemplateRef == templateRef {
			for _, entry := range pool.entries {
				if !entry.Claimed {
					return name
				}
			}
		}
	}
	return ""
}

// Delete tears down a PodBox, removing its compute Pod and dual Block PVCs.
func (r *Reconciler) Delete(ctx context.Context, name string) error {
	return r.DeleteBox(ctx, name, true)
}

// ReapIdle inspects all managed sandboxes and enforces idle auto-pause and paused TTL expiration.
// Running sandboxes are paused when LastActivityAt + IdleTimeoutSeconds <= now, provided
// LeaseExpiresAt is unset or already expired. Paused sandboxes are terminated when
// PausedAt + PausedTTLSeconds <= now.
func (r *Reconciler) ReapIdle(ctx context.Context, now time.Time) (paused []string, terminated []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}

	for name, status := range r.boxes {
		spec := r.specs[name]

		if status.Phase == v1alpha1.PodBoxPhaseRunning {
			if spec.Lifecycle.IdleTimeoutSeconds > 0 && status.LastActivityAt != nil {
				idleDeadline := status.LastActivityAt.Add(time.Duration(spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
				if !now.Before(idleDeadline) {
					// Check active lease lock.
					leaseActive := spec.Lifecycle.LeaseExpiresAt != nil && spec.Lifecycle.LeaseExpiresAt.After(now)
					if !leaseActive {
						specWithPaused := spec
						specWithPaused.DesiredState = v1alpha1.DesiredStatePaused

						statusWithPaused := *status
						statusWithPaused.Phase = v1alpha1.PodBoxPhasePaused
						statusWithPaused.PodName = ""
						statusWithPaused.PodIP = ""
						statusWithPaused.PausedAt = &now
						statusWithPaused.Reason = "IdlePaused"
						statusWithPaused.Message = "Paused due to idle timeout"

						if r.kubeClient != nil {
							err := r.kubeClient.UpsertPodBox(ctx, v1alpha1.PodBox{
								APIVersion: "cubepod.io/v1alpha1",
								Kind:       "PodBox",
								Metadata: v1alpha1.ObjectMeta{
									Name:        name,
									Namespace:   r.namespace,
									Annotations: r.podBoxAnnotationsLocked(name, ""),
								},
								Spec:   specWithPaused,
								Status: statusWithPaused,
							})
							if err != nil {
								slog.Warn("failed to persist paused state to Kubernetes; rolling back in-memory state", "name", name, "err", err)
								continue // do not commit pause in memory, do not append to paused!
							}
						}

						if status.PodName != "" && r.kubeClient != nil {
							_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
						}
						r.deleteEgressSecretsLocked(ctx, name)
						status.Phase = v1alpha1.PodBoxPhasePaused
						status.PodName = ""
						status.PodIP = ""
						status.PausedAt = &now
						status.Reason = "IdlePaused"
						status.Message = "Paused due to idle timeout"
						spec.DesiredState = v1alpha1.DesiredStatePaused
						r.specs[name] = spec
						paused = append(paused, name)
					}
				}
			}
		} else if status.Phase == v1alpha1.PodBoxPhasePaused {
			if spec.Lifecycle.PausedTTLSeconds > 0 && status.PausedAt != nil {
				ttlDeadline := status.PausedAt.Add(time.Duration(spec.Lifecycle.PausedTTLSeconds) * time.Second)
				if !now.Before(ttlDeadline) {
					if status.PodName != "" {
						_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
					}
					r.deleteEgressSecretsLocked(ctx, name)
					status.Phase = v1alpha1.PodBoxPhaseTerminated
					status.Reason = "PausedTTLExpired"
					status.Message = "Terminated due to paused TTL expiration"
					terminated = append(terminated, name)
				}
			}
		}
	}

	slices.Sort(paused)
	slices.Sort(terminated)
	return paused, terminated
}

// ActiveSandboxesCount returns the number of Running sandboxes.
func (r *Reconciler) ActiveSandboxesCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, status := range r.boxes {
		if status.Phase == v1alpha1.PodBoxPhaseRunning {
			count++
		}
	}
	return count
}

// PausedSandboxesCount returns the number of Paused sandboxes.
func (r *Reconciler) PausedSandboxesCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, status := range r.boxes {
		if status.Phase == v1alpha1.PodBoxPhasePaused {
			count++
		}
	}
	return count
}

// WarmPoolReadyCount returns the total number of ready warm standby sandboxes across all pools.
func (r *Reconciler) WarmPoolReadyCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, pool := range r.warmPools {
		for _, entry := range pool.entries {
			if !entry.Claimed {
				total++
			}
		}
	}
	return total
}

// GetWarmPoolSpecs returns a snapshot of registered warm pool specs for background replenishment.
func (r *Reconciler) GetWarmPoolSpecs() map[string]v1alpha1.PodWarmPoolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make(map[string]v1alpha1.PodWarmPoolSpec, len(r.warmPools))
	for name, pool := range r.warmPools {
		specs[name] = pool.spec
	}
	return specs
}

// GetWarmPoolEntries returns a copy of the warm pod entries for a pool.
func (r *Reconciler) GetWarmPoolEntries(poolName string) []*WarmPodEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pool, ok := r.warmPools[poolName]
	if !ok {
		return nil
	}
	res := make([]*WarmPodEntry, len(pool.entries))
	copy(res, pool.entries)
	return res
}

// GetBox returns a copy of the observed status for a PodBox.
func (r *Reconciler) GetBox(name string) (*v1alpha1.PodBoxStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status, ok := r.boxes[name]
	if !ok {
		return nil, false
	}
	cp := *status
	return &cp, true
}

// GetBoxSpec returns a copy of the stored spec for a PodBox.
func (r *Reconciler) GetBoxSpec(name string) (v1alpha1.PodBoxSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[name]
	return spec, ok
}

// TouchActivity updates the LastActivityAt timestamp of a PodBox.
func (r *Reconciler) TouchActivity(name string, t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, ok := r.boxes[name]
	if !ok {
		return fmt.Errorf("podbox %q not found", name)
	}
	status.LastActivityAt = &t
	if r.kubeClient != nil {
		spec := r.specs[name]
		box := v1alpha1.PodBox{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Metadata: v1alpha1.ObjectMeta{
				Name:        name,
				Namespace:   r.namespace,
				Annotations: r.podBoxAnnotationsLocked(name, ""),
			},
			Spec:   spec,
			Status: *status,
		}
		_ = r.kubeClient.UpsertPodBox(context.Background(), box)
	}
	return nil
}

// SetPausedAt updates the PausedAt timestamp of a PodBox.
func (r *Reconciler) SetPausedAt(name string, t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, ok := r.boxes[name]
	if !ok {
		return fmt.Errorf("podbox %q not found", name)
	}
	status.PausedAt = &t
	if r.kubeClient != nil {
		spec := r.specs[name]
		box := v1alpha1.PodBox{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Metadata: v1alpha1.ObjectMeta{
				Name:        name,
				Namespace:   r.namespace,
				Annotations: r.podBoxAnnotationsLocked(name, ""),
			},
			Spec:   spec,
			Status: *status,
		}
		_ = r.kubeClient.UpsertPodBox(context.Background(), box)
	}
	return nil
}

// AcquireLease updates the LeaseExpiresAt timestamp of a PodBox.
func (r *Reconciler) AcquireLease(name string, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	spec, ok := r.specs[name]
	if !ok {
		return fmt.Errorf("podbox %q not found", name)
	}
	spec.Lifecycle.LeaseExpiresAt = &expiresAt
	r.specs[name] = spec
	return nil
}

// SetPoolCreatedAt sets the createdAt timestamp of an in-memory warm pool (useful for testing stale list deletion).
func (r *Reconciler) SetPoolCreatedAt(name string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pool, ok := r.warmPools[name]; ok {
		pool.createdAt = t
	}
}

// WarmCounter returns the current warm replica counter.
func (r *Reconciler) WarmCounter() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.warmCounter
}
