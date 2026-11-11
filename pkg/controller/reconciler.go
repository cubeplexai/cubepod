// Package controller implements the Kubernetes reconciliation logic for PodBox,
// PodTemplate, and PodWarmPool custom resources.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// Reconciler manages the lifecycle of PodBox instances and their dual Raw Block PVCs.
type Reconciler struct {
	mu    sync.RWMutex
	boxes map[string]*v1alpha1.PodBoxStatus
	specs map[string]v1alpha1.PodBoxSpec
}

// NewReconciler creates a new in-memory/cluster Reconciler.
func NewReconciler() *Reconciler {
	return &Reconciler{
		boxes: make(map[string]*v1alpha1.PodBoxStatus),
		specs: make(map[string]v1alpha1.PodBoxSpec),
	}
}

// Reconcile drives a PodBox towards its DesiredState (Running or Paused),
// preserving both RootFS Upper PVC and Workspace PVC across pause/resume cycles.
func (r *Reconciler) Reconcile(_ context.Context, name string, spec v1alpha1.PodBoxSpec) (*v1alpha1.PodBoxStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	status, exists := r.boxes[name]
	if !exists {
		status = &v1alpha1.PodBoxStatus{
			Phase:             v1alpha1.PodBoxPhaseProvisioning,
			RootfsPVCName:     fmt.Sprintf("%s-rootfs-pvc", name),
			WorkspacePVCName:  fmt.Sprintf("%s-workspace-pvc", name),
			ObservedRootfsGen: spec.RootfsGeneration,
			LastActivityAt:    &now,
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
	}

	switch spec.DesiredState {
	case v1alpha1.DesiredStatePaused:
		status.Phase = v1alpha1.PodBoxPhasePaused
		status.PodName = ""
		status.PodIP = ""
		status.PausedAt = &now
	default:
		status.Phase = v1alpha1.PodBoxPhaseRunning
		status.PodName = fmt.Sprintf("%s-kata-pod", name)
		status.LastActivityAt = &now
		status.PausedAt = nil
	}

	r.specs[name] = spec
	cp := *status
	return &cp, nil
}
