// Package controller implements the Kubernetes reconciliation logic for PodBox,
// PodTemplate, and PodWarmPool custom resources.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
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

// Reconciler manages the lifecycle of PodBox instances, pre-attached PodWarmPools,
// and dual Raw Block PVCs.
type Reconciler struct {
	mu                  sync.RWMutex
	boxes               map[string]*v1alpha1.PodBoxStatus
	specs               map[string]v1alpha1.PodBoxSpec
	templates           map[string]v1alpha1.PodTemplateSpec
	warmPools           map[string]*warmPoolState
	warmCounter         int64
	kubeClient          KubeClient
	namespace           string
	volumeMode          string // "Block" or "Filesystem", default "Block"
	defaultStorageClass string // default "standard"
	defaultRuntimeClass string // default "kata-qemu"
	runtimeClassSet     bool
	poddInjectorImage   string // default "cubepod-podd:latest"
	defaultSandboxImage string // default "ubuntu:24.04"
	FailWarmPoolClaim   bool
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
		boxes:               make(map[string]*v1alpha1.PodBoxStatus),
		specs:               make(map[string]v1alpha1.PodBoxSpec),
		templates:           make(map[string]v1alpha1.PodTemplateSpec),
		warmPools:           make(map[string]*warmPoolState),
		kubeClient:          NewMemoryKubeClient(),
		namespace:           "cubepod-workloads",
		volumeMode:          "Block",
		defaultStorageClass: "standard",
		defaultRuntimeClass: "kata-qemu",
		poddInjectorImage:   "cubepod-podd:latest",
		defaultSandboxImage: "ubuntu:24.04",
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
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

// RegisterTemplate saves or updates a reusable PodTemplateSpec.
func (r *Reconciler) RegisterTemplate(name string, spec v1alpha1.PodTemplateSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templates[name] = spec
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
			spec:    spec,
			entries: make([]*WarmPodEntry, 0),
		}
		r.warmPools[poolName] = pool
	}
	pool.spec = spec

	tpl, ok := r.templates[spec.TemplateRef]
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

	// Count existing available (unclaimed) replicas.
	var availableEntries []*WarmPodEntry
	for _, entry := range pool.entries {
		if !entry.Claimed {
			availableEntries = append(availableEntries, entry)
		}
	}
	currentAvail := int32(len(availableEntries))

	// Scale up if needed.
	if currentAvail < spec.Replicas {
		toAdd := spec.Replicas - currentAvail
		for i := int32(0); i < toAdd; i++ {
			r.warmCounter++
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
			podManifest := BuildKataPod(entryID, tpl, warmBoxSpec, dummyStatus, r.volumeMode)
			podManifest.Namespace = r.namespace
			if podManifest.Labels == nil {
				podManifest.Labels = make(map[string]string)
			}
			podManifest.Labels["cubepod.io/warmpool"] = poolName
			podManifest.Labels["cubepod.io/template"] = spec.TemplateRef
			podManifest.Labels["cubepod.io/claimed"] = "false"

			podIP, _ := r.kubeClient.EnsurePod(ctx, podManifest)
			if podIP != "" {
				dummyStatus.PodIP = podIP
			}

			entry := &WarmPodEntry{
				ID:               entryID,
				PoolName:         poolName,
				TemplateRef:      spec.TemplateRef,
				PodName:          dummyStatus.PodName,
				RootfsPVCName:    rootfsPVC.Name,
				WorkspacePVCName: wsPVC.Name,
				Claimed:          false,
				CreatedAt:        time.Now().UTC(),
				PodManifest:      podManifest,
				RootfsPVC:        rootfsPVC,
				WorkspacePVC:     wsPVC,
			}
			pool.entries = append(pool.entries, entry)
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

	// Persist the PodWarmPool CRD state to Kubernetes API via status update (avoiding spec overwrite)
	if r.kubeClient != nil {
		err := r.kubeClient.UpdatePodWarmPoolStatus(ctx, r.namespace, poolName, *st)
		if err != nil {
			errStr := strings.ToLower(err.Error())
			if strings.Contains(errStr, "not found") || strings.Contains(errStr, "404") {
				slog.Debug("PodWarmPool not found in Kubernetes; skipping status update and recreation", "pool", poolName, "err", err)
			} else {
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
				_ = r.kubeClient.UpsertPodWarmPool(ctx, cr)
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

	for _, entry := range pool.entries {
		if !entry.Claimed {
			entry.Claimed = true
			entry.ClaimedBy = boxName
			now := time.Now().UTC()
			entry.ClaimedAt = &now
			if entry.PodManifest.Labels == nil {
				entry.PodManifest.Labels = make(map[string]string)
			}
			entry.PodManifest.Labels["cubepod.io/claimed"] = "true"
			entry.PodManifest.Labels["cubepod.io/claimed-by"] = boxName

			if r.kubeClient != nil {
				if ctx == nil {
					ctx = context.Background()
				}
				if err := r.kubeClient.PatchPodLabels(ctx, r.namespace, entry.PodName, map[string]string{
					"cubepod.io/claimed":    "true",
					"cubepod.io/claimed-by": boxName,
				}); err != nil {
					slog.Warn("failed to patch pod labels on claim", "pod", entry.PodName, "box", boxName, "err", err)
				}
			}

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

	tpl, ok := r.templates[spec.TemplateRef]
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
			rootfsPVC, wsPVC := BuildBlockPVCs(name, tpl, spec, r.volumeMode)
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
		status.Phase = v1alpha1.PodBoxPhasePaused
		status.PodName = ""
		status.PodIP = ""
		status.PausedAt = &now
	default:
		status.Phase = v1alpha1.PodBoxPhaseRunning
		if status.PodName == "" {
			status.PodName = fmt.Sprintf("%s-kata-pod", name)
		}
		podManifest := BuildKataPod(name, tpl, spec, *status, r.volumeMode)
		podManifest.Namespace = r.namespace
		podIP, err := r.kubeClient.EnsurePod(ctx, podManifest)
		if err == nil && podIP != "" {
			status.PodIP = podIP
		}
		status.PodName = podManifest.Name
		status.LastActivityAt = &now
		status.PausedAt = nil
	}

	r.specs[name] = spec
	cp := *status

	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
		Spec:   spec,
		Status: cp,
	}
	_ = r.kubeClient.UpsertPodBox(ctx, box)

	return &cp, nil
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

	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
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

	status, ok := r.boxes[name]
	if !ok {
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

	pools, err := r.kubeClient.ListPodWarmPools(ctx, r.namespace)
	if err != nil {
		return 0, fmt.Errorf("failed to list PodWarmPools from Kubernetes: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	recovered := 0
	for _, box := range boxes {
		name := box.Metadata.Name
		if name == "" {
			continue
		}
		// If box already exists in memory with a newer or modified state, don't clobber
		if _, exists := r.boxes[name]; !exists {
			st := box.Status
			r.boxes[name] = &st
			r.specs[name] = box.Spec
		}
		if idx := parseWarmIndex(box.Status.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		recovered++
	}

	// Sync PodWarmPools from Kubernetes
	activePools := make(map[string]bool)
	for _, p := range pools {
		pName := p.Metadata.Name
		if pName == "" {
			continue
		}
		activePools[pName] = true
		if pool, exists := r.warmPools[pName]; exists {
			pool.spec = p.Spec
		} else {
			r.warmPools[pName] = &warmPoolState{
				spec:    p.Spec,
				entries: make([]*WarmPodEntry, 0),
			}
		}
	}

	for name, pool := range r.warmPools {
		if !activePools[name] {
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

	for _, pool := range r.warmPools {
		for _, entry := range pool.entries {
			if idx := parseWarmIndex(entry.ID); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.PodName); idx > r.warmCounter {
				r.warmCounter = idx
			}
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
						if status.PodName != "" {
							_ = r.kubeClient.DeletePod(ctx, r.namespace, status.PodName)
						}
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
