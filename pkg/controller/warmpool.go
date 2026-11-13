package controller

import (
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// WarmPodEntry represents a pre-warmed Kata MicroVM sandbox in a PodWarmPool,
// provisioned with pre-formatted thin block PVCs and a running Pod.
type WarmPodEntry struct {
	ID               string           `json:"id"`
	PoolName         string           `json:"poolName"`
	TemplateRef      string           `json:"templateRef"`
	PodName          string           `json:"podName"`
	RootfsPVCName    string           `json:"rootfsPVCName"`
	WorkspacePVCName string           `json:"workspacePVCName"`
	Claimed          bool             `json:"claimed"`
	ClaimedBy        string           `json:"claimedBy,omitempty"`
	CreatedAt        time.Time        `json:"createdAt"`
	ClaimedAt        *time.Time       `json:"claimedAt,omitempty"`
	PodManifest      PodManifest      `json:"podManifest"`
	RootfsPVC        BlockPVCManifest `json:"rootfsPVC"`
	WorkspacePVC     BlockPVCManifest `json:"workspacePVC"`
}

type warmPoolState struct {
	spec      v1alpha1.PodWarmPoolSpec
	entries   []*WarmPodEntry
	persisted bool
	createdAt time.Time
}
