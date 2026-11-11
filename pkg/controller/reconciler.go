// Package controller implements the Kubernetes reconciliation logic for PodBox,
// PodTemplate, and PodWarmPool custom resources.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// Reconciler manages the lifecycle of PodBox instances, pre-attached PodWarmPools,
// and dual Raw Block PVCs.
type Reconciler struct {
	mu          sync.RWMutex
	boxes       map[string]*v1alpha1.PodBoxStatus
	specs       map[string]v1alpha1.PodBoxSpec
	templates   map[string]v1alpha1.PodTemplateSpec
	warmPools   map[string]*warmPoolState
	warmCounter int64
}

// NewReconciler creates a new in-memory/cluster Reconciler.
func NewReconciler() *Reconciler {
	return &Reconciler{
		boxes:     make(map[string]*v1alpha1.PodBoxStatus),
		specs:     make(map[string]v1alpha1.PodBoxSpec),
		templates: make(map[string]v1alpha1.PodTemplateSpec),
		warmPools: make(map[string]*warmPoolState),
	}
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

func defaultTemplate(templateID string) v1alpha1.PodTemplateSpec {
	if templateID == "" {
		templateID = "ubuntu:24.04"
	}
	return v1alpha1.PodTemplateSpec{
		RuntimeClassName: "kata-qemu",
		Image:            templateID,
		PoddImage:        "cubepod-podd:latest",
		Resources: v1alpha1.ResourceRequirements{
			CPURequest:    "1",
			CPULimit:      "2",
			MemoryRequest: "2Gi",
			MemoryLimit:   "4Gi",
		},
		Storage: v1alpha1.StorageSpec{
			StorageClassName: "standard",
			RootfsSize:       "10Gi",
			WorkspaceSize:    "20Gi",
		},
	}
}

// ReconcileWarmPool manages standby Kata MicroVM instances with pre-attached thin block PVCs.
func (r *Reconciler) ReconcileWarmPool(_ context.Context, poolName string, spec v1alpha1.PodWarmPoolSpec) (*v1alpha1.PodWarmPoolStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

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
		tpl = defaultTemplate(spec.TemplateRef)
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
			}

			rootfsPVC, wsPVC := BuildBlockPVCs(entryID, tpl, warmBoxSpec)
			dummyStatus := v1alpha1.PodBoxStatus{
				PodName:          fmt.Sprintf("%s-kata-pod", entryID),
				RootfsPVCName:    rootfsPVC.Name,
				WorkspacePVCName: wsPVC.Name,
				Phase:            v1alpha1.PodBoxPhaseRunning,
			}
			podManifest := BuildKataPod(entryID, tpl, warmBoxSpec, dummyStatus)

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
	} else if currentAvail > spec.Replicas {
		// Scale down excess unclaimed entries.
		toRemove := currentAvail - spec.Replicas
		var filtered []*WarmPodEntry
		var removedCount int32
		for _, entry := range pool.entries {
			if !entry.Claimed && removedCount < toRemove {
				removedCount++
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

	return &v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     finalAvail,
		AvailableReplicas: finalAvail,
	}, nil
}

// ClaimWarmPod claims a standby warm pod from the given pool for a new PodBox.
func (r *Reconciler) ClaimWarmPod(_ context.Context, poolName string, boxName string) (*WarmPodEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claimWarmPodLocked(poolName, boxName)
}

func (r *Reconciler) claimWarmPodLocked(poolName string, boxName string) (*WarmPodEntry, error) {
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
func (r *Reconciler) Reconcile(_ context.Context, name string, spec v1alpha1.PodBoxSpec) (*v1alpha1.PodBoxStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	status, exists := r.boxes[name]

	if !exists {
		// 1. Pre-Attached WarmPool Claim (when warmPoolRef is set and no ForkFrom)
		if spec.WarmPoolRef != "" && spec.ForkFrom == nil {
			entry, err := r.claimWarmPodLocked(spec.WarmPoolRef, name)
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
		status.ObservedRootfsGen = spec.RootfsGeneration
		status.RootfsPVCName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
		status.Reason = "RootFSReset"
		status.Message = fmt.Sprintf("RootFS reset to generation %d", spec.RootfsGeneration)
	}

	switch spec.DesiredState {
	case v1alpha1.DesiredStatePaused:
		status.Phase = v1alpha1.PodBoxPhasePaused
		status.PodName = ""
		status.PodIP = ""
		status.PausedAt = &now
	default:
		status.Phase = v1alpha1.PodBoxPhaseRunning
		if status.PodName == "" {
			status.PodName = fmt.Sprintf("%s-kata-pod", name)
		}
		status.LastActivityAt = &now
		status.PausedAt = nil
	}

	r.specs[name] = spec
	cp := *status
	return &cp, nil
}

// ReapIdle inspects all managed sandboxes and enforces idle auto-pause and paused TTL expiration.
// Running sandboxes are paused when LastActivityAt + IdleTimeoutSeconds <= now, provided
// LeaseExpiresAt is unset or already expired. Paused sandboxes are terminated when
// PausedAt + PausedTTLSeconds <= now.
func (r *Reconciler) ReapIdle(_ context.Context, now time.Time) (paused []string, terminated []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

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
