package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// Labels and reasons for Kubernetes core/v1 Events emitted on PodBox lifecycle transitions.
const (
	PodBoxEventResourceLabel = "cubepod.io/resource"
	PodBoxEventResourceValue = "sandbox-event"
	PodBoxEventSandboxLabel  = "cubepod.io/sandbox"
	PodBoxEventSourceLabel   = "cubepod.io/source"
	PodBoxEventSourceValue   = "controller"

	PodBoxEventReasonRunning          = "PodBoxRunning"
	PodBoxEventReasonPaused           = "PodBoxPaused"
	PodBoxEventReasonIdlePaused       = "IdlePaused"
	PodBoxEventReasonPausedTTLExpired = "PausedTTLExpired"
	PodBoxEventReasonDeleted          = "PodBoxDeleted"

	podBoxEventTimeout = 2 * time.Second
)

// WithPodBoxEvents enables or disables emission of labeled Kubernetes Events on PodBox
// lifecycle transitions (enabled by default).
func WithPodBoxEvents(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.podBoxEventsDisabled = !enabled
	}
}

func sanitizeDNSLabel(v string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(v) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(byte(c))
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "podbox"
	}
	return s
}

// podBoxEventName returns a deterministic, DNS-subdomain-safe Event name for a transition,
// so concurrent emitters (gateway + controller replicas) observing the same transition
// collide on 409 AlreadyExists instead of producing duplicates.
func podBoxEventName(box, reason string, at time.Time) string {
	sum := sha256.Sum256([]byte(box + "|" + reason + "|" + strconv.FormatInt(at.UTC().UnixNano(), 10)))
	return sanitizeDNSLabel(box) + "." + sanitizeDNSLabel(reason) + "." + hex.EncodeToString(sum[:6])
}

func isLabelValue(v string) bool {
	if v == "" || len(v) > 63 {
		return false
	}
	for i, c := range v {
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && (i == 0 || i == len(v)-1 || (c != '-' && c != '_' && c != '.')) {
			return false
		}
	}
	return true
}

// recordPodBoxEventLocked emits a labeled core/v1 Event attached to the PodBox. It is
// best-effort: failures are logged and never fail the lifecycle operation. Callers hold r.mu.
func (r *Reconciler) recordPodBoxEventLocked(ctx context.Context, box, reason, eventType, message string, at time.Time) {
	if r.podBoxEventsDisabled || r.kubeClient == nil || box == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	labels := map[string]string{
		PodBoxEventResourceLabel: PodBoxEventResourceValue,
		PodBoxEventSourceLabel:   PodBoxEventSourceValue,
	}
	if isLabelValue(box) {
		labels[PodBoxEventSandboxLabel] = box
	}
	ev := KubeEvent{
		APIVersion: "v1",
		Kind:       "Event",
		Metadata: v1alpha1.ObjectMeta{
			Name:      podBoxEventName(box, reason, at),
			Namespace: r.namespace,
			Labels:    labels,
		},
		InvolvedObject: KubeObjectReference{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Name:       box,
			Namespace:  r.namespace,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         KubeEventSource{Component: "cubepod-controller"},
		FirstTimestamp: &at,
		LastTimestamp:  &at,
		Count:          1,
	}
	evCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), podBoxEventTimeout)
	defer cancel()
	if err := r.kubeClient.CreateKubeEvent(evCtx, ev); err != nil && !errors.Is(err, ErrKubeAlreadyExists) {
		slog.Debug("failed to emit PodBox lifecycle Event", "podbox", box, "reason", reason, "err", err)
	}
}

// recordPhaseTransitionLocked emits a PodBox Event when Reconcile changed the observed phase.
func (r *Reconciler) recordPhaseTransitionLocked(ctx context.Context, box string, prevPhase v1alpha1.PodBoxPhase, status v1alpha1.PodBoxStatus) {
	if prevPhase == status.Phase {
		return
	}
	switch status.Phase {
	case v1alpha1.PodBoxPhaseRunning:
		msg := "PodBox is running"
		if prevPhase == v1alpha1.PodBoxPhasePaused {
			msg = "PodBox resumed from Paused"
		}
		at := time.Time{}
		if status.LastActivityAt != nil {
			at = *status.LastActivityAt
		}
		r.recordPodBoxEventLocked(ctx, box, PodBoxEventReasonRunning, KubeEventTypeNormal, msg, at)
	case v1alpha1.PodBoxPhasePaused:
		at := time.Time{}
		if status.PausedAt != nil {
			at = *status.PausedAt
		}
		r.recordPodBoxEventLocked(ctx, box, PodBoxEventReasonPaused, KubeEventTypeNormal,
			"PodBox paused: compute Pod deleted, RootFS and Workspace PVCs retained", at)
	}
}
