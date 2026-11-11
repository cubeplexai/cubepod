// Package v1alpha1 contains API Schema definitions for the pod.cubeplex.ai v1alpha1 API group.
package v1alpha1

import "time"

const (
	GroupName    = "pod.cubeplex.ai"
	GroupVersion = "v1alpha1"
)

// DesiredState represents the target lifecycle state of a PodBox.
type DesiredState string

const (
	DesiredStateRunning DesiredState = "Running"
	DesiredStatePaused  DesiredState = "Paused"
)

// PodBoxPhase represents the observed phase of a PodBox instance.
type PodBoxPhase string

const (
	PodBoxPhasePending      PodBoxPhase = "Pending"
	PodBoxPhaseProvisioning PodBoxPhase = "Provisioning"
	PodBoxPhaseRunning      PodBoxPhase = "Running"
	PodBoxPhasePausing      PodBoxPhase = "Pausing"
	PodBoxPhasePaused       PodBoxPhase = "Paused"
	PodBoxPhaseResuming     PodBoxPhase = "Resuming"
	PodBoxPhaseResetting    PodBoxPhase = "ResettingRootFS"
	PodBoxPhaseFailed       PodBoxPhase = "Failed"
	PodBoxPhaseTerminated   PodBoxPhase = "Terminated"
)

// NetworkDefaultAction specifies the default egress firewall behavior.
type NetworkDefaultAction string

const (
	NetworkDefaultAllow NetworkDefaultAction = "Allow"
	NetworkDefaultDeny  NetworkDefaultAction = "Deny"
)

// StorageSpec defines the dual raw-block PVC configuration for a PodBox or PodTemplate.
type StorageSpec struct {
	// StorageClassName is the CSI StorageClass supporting volumeMode: Block
	// (e.g., "longhorn-v2-block-thin" or "mayastor-nvmeof").
	StorageClassName string `json:"storageClassName"`

	// RootfsSize is the thin-provisioned size of the persistent OverlayFS upperdir block device (e.g., "10Gi").
	RootfsSize string `json:"rootfsSize"`

	// WorkspaceSize is the thin-provisioned size of the /workspace block device (e.g., "20Gi").
	WorkspaceSize string `json:"workspaceSize"`
}

// ResourceRequirements defines CPU and memory limits/requests for the sandbox MicroVM.
type ResourceRequirements struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// EgressConfig configures the zero-trust egress sidecar and mTLS secret exchange.
type EgressConfig struct {
	Enabled     bool   `json:"enabled"`
	Image       string `json:"image,omitempty"`
	ExchangeURL string `json:"exchangeUrl,omitempty"`
}

// PodTemplateSpec defines the reusable blueprint for PodBox instances and PodWarmPools.
type PodTemplateSpec struct {
	// RuntimeClassName specifies the Kubernetes RuntimeClass (default: "kata-qemu" or "kata-clh").
	RuntimeClassName string `json:"runtimeClassName"`

	// Image is the user/workload OCI container image (e.g., ubuntu:24.04 or cubeplex-sandbox).
	// Note: podd-init and podd are NOT baked into this image; they are injected at runtime
	// via an initContainer into /opt/cubepod/bin.
	Image string `json:"image"`

	// PoddImage is the optional override for the initContainer image that injects
	// /opt/cubepod/bin/podd-init and /opt/cubepod/bin/podd into the sandbox pod.
	PoddImage string `json:"poddImage,omitempty"`

	// Resources specifies CPU and memory bounds for the MicroVM.
	Resources ResourceRequirements `json:"resources"`

	// Storage defines the default block storage configuration for rootfs and workspace volumes.
	Storage StorageSpec `json:"storage"`

	// Egress defines the cluster-level egress sidecar configuration.
	Egress EgressConfig `json:"egress,omitempty"`
}

// PodWarmPoolSpec defines a pool of pre-warmed Kata MicroVMs with pre-attached thin block PVCs.
type PodWarmPoolSpec struct {
	// TemplateRef references the PodTemplate used to spawn warm standby pods.
	TemplateRef string `json:"templateRef"`

	// Replicas is the target number of warm MicroVM pods to keep in standby.
	Replicas int32 `json:"replicas"`
}

// PodWarmPoolStatus reports the current state of the warm pool.
type PodWarmPoolStatus struct {
	ReadyReplicas     int32 `json:"readyReplicas"`
	AvailableReplicas int32 `json:"availableReplicas"`
}

// ForkSource references a parent PodBox to clone both block PVCs from via CSI Copy-on-Write.
type ForkSource struct {
	PodBoxName string `json:"podBoxName"`
}

// LifecyclePolicy controls automatic idle pause, lease protection, and expiration.
type LifecyclePolicy struct {
	IdleTimeoutSeconds int64      `json:"idleTimeoutSeconds,omitempty"`
	PausedTTLSeconds   int64      `json:"pausedTTLSeconds,omitempty"`
	LeaseExpiresAt     *time.Time `json:"leaseExpiresAt,omitempty"`
}

// SecretBinding maps an environment variable placeholder (cbxref_...) to allowed egress hosts/headers.
type SecretBinding struct {
	EnvName        string   `json:"envName"`
	Placeholder    string   `json:"placeholder"`
	AllowedHosts   []string `json:"allowedHosts"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
}

// NetworkPolicySpec configures per-sandbox DNS/nftables filtering and upstream proxy routing.
type NetworkPolicySpec struct {
	DefaultAction  NetworkDefaultAction `json:"defaultAction"`
	AllowHosts     []string             `json:"allowHosts,omitempty"`
	DenyHosts      []string             `json:"denyHosts,omitempty"`
	UpstreamProxy  string               `json:"upstreamProxy,omitempty"`
	SecretBindings []SecretBinding      `json:"secretBindings,omitempty"`
}

// PodBoxSpec defines the desired state of a single persistent agent execution pod.
type PodBoxSpec struct {
	// TemplateRef references a PodTemplate for default image, runtimeClass, and storage settings.
	TemplateRef string `json:"templateRef,omitempty"`

	// WarmPoolRef optionally claims a standby instance from the named PodWarmPool on initial creation.
	WarmPoolRef string `json:"warmPoolRef,omitempty"`

	// DesiredState controls whether the MicroVM Pod is active ("Running") or suspended ("Paused").
	// When Paused, the compute Pod is deleted (0 CPU/RAM) while RootFS and Workspace PVCs are retained.
	DesiredState DesiredState `json:"desiredState"`

	// RootfsGeneration is incremented to trigger a clean reset of the RootFS Upper PVC
	// while keeping the Workspace PVC intact.
	RootfsGeneration int64 `json:"rootfsGeneration"`

	// ForkFrom optionally clones both RootFS and Workspace block volumes from an existing PodBox.
	ForkFrom *ForkSource `json:"forkFrom,omitempty"`

	// Lifecycle configures idle auto-pause and active lease locks.
	Lifecycle LifecyclePolicy `json:"lifecycle,omitempty"`

	// NetworkPolicy defines zero-trust egress rules and secret bindings for this sandbox.
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// PodBoxStatus represents the observed status of a PodBox instance.
type PodBoxStatus struct {
	Phase             PodBoxPhase `json:"phase"`
	PodName           string      `json:"podName,omitempty"`
	PodIP             string      `json:"podIP,omitempty"`
	NodeName          string      `json:"nodeName,omitempty"`
	RootfsPVCName     string      `json:"rootfsPVCName,omitempty"`
	WorkspacePVCName  string      `json:"workspacePVCName,omitempty"`
	ObservedRootfsGen int64       `json:"observedRootfsGeneration,omitempty"`
	LastActivityAt    *time.Time  `json:"lastActivityAt,omitempty"`
	PausedAt          *time.Time  `json:"pausedAt,omitempty"`
	Reason            string      `json:"reason,omitempty"`
	Message           string      `json:"message,omitempty"`
}
