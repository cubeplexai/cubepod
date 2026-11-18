// Package v1alpha1 contains API Schema definitions for the pod.cubeplex.ai v1alpha1 API group.
package v1alpha1

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	GroupName    = "pod.cubeplex.ai"
	CRDGroup     = "cubepod.io"
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

	// Names are the E2B aliases and tags ("my-template", "my-template:v1", "ubuntu:24.04").
	Names []string `json:"names,omitempty"`

	// CPUCount, MemoryMB, and DiskSizeMB are the E2B resource view.
	// The controller also writes Resources and Storage.rootfsSize from them.
	CPUCount   int32 `json:"cpuCount,omitempty"`
	MemoryMB   int32 `json:"memoryMB,omitempty"`
	DiskSizeMB int32 `json:"diskSizeMB,omitempty"`
	Public     bool  `json:"public,omitempty"`

	// StartCmd is launched by podd after pivot_root. It is not the container entrypoint.
	StartCmd string `json:"startCmd,omitempty"`

	// ReadyCmd is probed during the build and after each sandbox start. Empty skips the probe.
	ReadyCmd string `json:"readyCmd,omitempty"`

	// RunAsUser is the uid from a numeric USER step. Nil means unset and podd uses 1000.
	// A pointer is required so USER 0 (root) is distinct from unset.
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// RunAsGroup is set when USER is "<uid>:<gid>" and gid is numeric.
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`

	// RunAsUsername is set when USER is not a bare integer. podd resolves it inside the image.
	RunAsUsername string `json:"runAsUsername,omitempty"`
}

// Build status constants for PodTemplateStatus.BuildStatus
const (
	BuildStatusWaiting  = "waiting"
	BuildStatusBuilding = "building"
	BuildStatusUploaded = "uploaded"
	BuildStatusReady    = "ready"
	BuildStatusError    = "error"
)

// PodTemplateBuildRecord tracks an individual build attempt on a PodTemplate.
type PodTemplateBuildRecord struct {
	BuildID    string `json:"buildID"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	FailedStep string `json:"failedStep,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	UpdatedAt  string `json:"updatedAt,omitempty"`
	CPUCount   int32  `json:"cpuCount,omitempty"`
	MemoryMB   int32  `json:"memoryMB,omitempty"`
}

// PodTemplateStatus reports the observed state of a PodTemplate instance.
type PodTemplateStatus struct {
	TemplateID    string `json:"templateID,omitempty"`
	ReadyBuildID  string `json:"readyBuildID,omitempty"`
	ReadyImage    string `json:"readyImage,omitempty"`
	LatestBuildID string `json:"latestBuildID,omitempty"`
	// BuildStatus is the E2B enum for LatestBuildID:
	// waiting, building, uploaded, ready, error.
	BuildStatus   string                   `json:"buildStatus,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
	FailedStep    string                   `json:"failedStep,omitempty"`
	Public        bool                     `json:"public,omitempty"`
	BuildCount    int64                    `json:"buildCount,omitempty"`
	SpawnCount    int64                    `json:"spawnCount,omitempty"`
	CreatedAt     string                   `json:"createdAt,omitempty"`
	UpdatedAt     string                   `json:"updatedAt,omitempty"`
	LastSpawnedAt string                   `json:"lastSpawnedAt,omitempty"`
	Builds        []PodTemplateBuildRecord `json:"builds,omitempty"`
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

// SecretBinding maps an environment variable placeholder (cpodref_...) to allowed egress hosts/headers.
type SecretBinding struct {
	EnvName        string   `json:"envName"`
	Placeholder    string   `json:"placeholder"`
	AllowedHosts   []string `json:"allowedHosts"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
}

// NetworkEgressRule represents a single ordered allow or deny egress rule for a host or wildcard pattern.
type NetworkEgressRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// HeaderTransformRule injects or rewrites HTTP headers on outbound HTTPS requests matching Host.
// Header values must contain only cpodref_ placeholders, never plaintext secrets.
type HeaderTransformRule struct {
	Host    string            `json:"host"`
	Headers map[string]string `json:"headers"`
}

// SecretRefBinding records non-secret metadata on PodBox so any HA gateway replica
// can re-hydrate egress.Vault from a shared secret store on cache miss.
type SecretRefBinding struct {
	Placeholder    string   `json:"placeholder"`
	TenantHash     string   `json:"tenantHash"`
	SecretID       string   `json:"secretID"`
	AllowedHosts   []string `json:"allowedHosts"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty"`
}

// NetworkPolicySpec configures per-sandbox DNS/nftables filtering and upstream proxy routing.
type NetworkPolicySpec struct {
	DefaultAction     NetworkDefaultAction  `json:"defaultAction"`
	AllowHosts        []string              `json:"allowHosts,omitempty"`
	DenyHosts         []string              `json:"denyHosts,omitempty"`
	Egress            []NetworkEgressRule   `json:"egress,omitempty"`
	UpstreamProxy     string                `json:"upstreamProxy,omitempty"`
	SecretBindings    []SecretBinding       `json:"secretBindings,omitempty"`
	HeaderTransforms  []HeaderTransformRule `json:"headerTransforms,omitempty"`
	SecretRefBindings []SecretRefBinding    `json:"secretRefBindings,omitempty"`
}

// E2BVolumeMount attaches one official E2B volume to the sandbox container.
// It is not the workspace VolumeKey. The claim is a Filesystem PVC named
// e2bvol-<volumeID>, even when the workspace volume is a block device.
type E2BVolumeMount struct {
	VolumeID string `json:"volumeID"`
	PVCName  string `json:"pvcName"`
	SubPath  string `json:"subPath,omitempty"`
	Path     string `json:"path"`
}

// reservedE2BMounts are container paths an official volume must not cover.
// "/" is rejected separately so every absolute path is not treated as reserved.
var reservedE2BMounts = []string{
	"/workspace",
	"/rootfs-upper",
	"/proc",
	"/sys",
	"/dev",
	"/etc",
	"/opt/cubepod",
	"/mnt/cubepod",
	"/bin",
	"/sbin",
	"/usr",
	"/lib",
	"/lib64",
	"/root",
	"/boot",
	"/run",
}

// ValidE2BContainerPath reports whether an absolute, cleaned container path may
// hold an official E2B volume. Data paths such as /mnt/cpod-test-vol are allowed.
// The path and anything under a reserved root are rejected.
func ValidE2BContainerPath(p string) bool {
	if p == "" || p == "/" || p == "." || !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.ContainsRune(p, 0) || strings.Contains(p, `\`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	for _, root := range reservedE2BMounts {
		if p == root || strings.HasPrefix(p, root+"/") {
			return false
		}
	}
	return true
}

// AllowedDockerVolumeName reports whether a Docker named volume may be attached
// to a sandbox. Product volumes use cubepod-e2bvol-. Tests use cpod-test-.
func AllowedDockerVolumeName(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\\x00 ") {
		return false
	}
	return strings.HasPrefix(name, "cubepod-e2bvol-") || strings.HasPrefix(name, "cpod-test-")
}

// PodBoxSpec defines the desired state of a single persistent agent execution pod.
type PodBoxSpec struct {
	// TemplateRef references a PodTemplate for default image, runtimeClass, and storage settings.
	TemplateRef string `json:"templateRef,omitempty"`

	// WarmPoolRef optionally claims a standby instance from the named PodWarmPool on initial creation.
	WarmPoolRef string `json:"warmPoolRef,omitempty"`

	// VolumeKey optionally specifies a deterministic workspace PVC prefix so workspace
	// data persists across sandbox delete -> create cycles.
	VolumeKey string `json:"volumeKey,omitempty"`

	// E2BVolumeMounts are official E2B volumes mounted when the pod is created.
	// They stay distinct from VolumeKey and are never raw block devices.
	E2BVolumeMounts []E2BVolumeMount `json:"e2bVolumeMounts,omitempty"`

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

// ObjectMeta represents Kubernetes metadata for a Custom Resource.
type ObjectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// PodBox is the Custom Resource representing an isolated agent execution sandbox.
type PodBox struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Metadata   ObjectMeta   `json:"metadata"`
	Spec       PodBoxSpec   `json:"spec"`
	Status     PodBoxStatus `json:"status,omitempty"`
}

// PodBoxList is a list of PodBox Custom Resources.
type PodBoxList struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata,omitempty"`
	Items      []PodBox   `json:"items"`
}

// PodWarmPool is the Custom Resource representing a standby pool of warm MicroVM sandboxes.
type PodWarmPool struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Spec       PodWarmPoolSpec   `json:"spec"`
	Status     PodWarmPoolStatus `json:"status,omitempty"`
}

// PodWarmPoolList is a list of PodWarmPool Custom Resources.
type PodWarmPoolList struct {
	APIVersion string        `json:"apiVersion,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Metadata   ObjectMeta    `json:"metadata,omitempty"`
	Items      []PodWarmPool `json:"items"`
}

// PodTemplate is the Custom Resource representing a reusable sandbox blueprint and template catalog entry.
type PodTemplate struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Spec       PodTemplateSpec   `json:"spec"`
	Status     PodTemplateStatus `json:"status,omitempty"`
}

// PodTemplateList is a list of PodTemplate Custom Resources.
type PodTemplateList struct {
	APIVersion string        `json:"apiVersion,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Metadata   ObjectMeta    `json:"metadata,omitempty"`
	Items      []PodTemplate `json:"items"`
}

// SandboxWebhookSpec defines a tenant-scoped webhook subscription (CEP-0028).
// The HMAC signatureKey is isolated in the referenced Kubernetes Secret (SecretName).
type SandboxWebhookSpec struct {
	TenantHash string   `json:"tenantHash"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Events     []string `json:"events"`
	Enabled    bool     `json:"enabled"`
	SecretName string   `json:"secretName"`
}

// SandboxWebhookStatus tracks delivery health for a SandboxWebhook.
type SandboxWebhookStatus struct {
	LastDeliveryTime    *time.Time `json:"lastDeliveryTime,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures,omitempty"`
	// DeadLetters is a bounded list of deliveries that failed after max retries or
	// were dropped on a full queue. It never contains the HMAC signatureKey.
	DeadLetters []SandboxWebhookDeadLetter `json:"deadLetters,omitempty"`
}

// SandboxWebhookDeadLetter records one undeliverable webhook event for inspection and retry.
type SandboxWebhookDeadLetter struct {
	ID             string          `json:"id"`
	EventID        string          `json:"eventId"`
	EventType      string          `json:"eventType"`
	SandboxID      string          `json:"sandboxId,omitempty"`
	Reason         string          `json:"reason"`
	Attempts       int             `json:"attempts,omitempty"`
	LastStatusCode int             `json:"lastStatusCode,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	RetryCount     int             `json:"retryCount,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Event          json.RawMessage `json:"event,omitempty"`
}

// SandboxWebhook is the Custom Resource representing a tenant webhook subscription.
type SandboxWebhook struct {
	APIVersion string               `json:"apiVersion,omitempty"`
	Kind       string               `json:"kind,omitempty"`
	Metadata   ObjectMeta           `json:"metadata"`
	Spec       SandboxWebhookSpec   `json:"spec"`
	Status     SandboxWebhookStatus `json:"status,omitempty"`
}

// SandboxWebhookList is a list of SandboxWebhook Custom Resources.
type SandboxWebhookList struct {
	APIVersion string           `json:"apiVersion,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Metadata   ObjectMeta       `json:"metadata,omitempty"`
	Items      []SandboxWebhook `json:"items"`
}
