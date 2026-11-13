package runtime

import (
	"context"
	"errors"
	"fmt"
)

// Common runtime errors.
var (
	ErrSandboxNotFound       = errors.New("sandbox not found")
	ErrSandboxAlreadyRunning = errors.New("sandbox already running")
	ErrSourceSandboxNotFound = errors.New("source sandbox not found")
)

// LocalTemplateTag formats the Docker tag for local template builds: cubepod-templates/<slug>:<buildID>.
func LocalTemplateTag(slug, buildID string) string {
	return fmt.Sprintf("cubepod-templates/%s:%s", slug, buildID)
}

// SandboxSpec describes the configuration and metadata for running a sandbox instance.
type SandboxSpec struct {
	SandboxID        string            `json:"sandboxID"`
	TemplateID       string            `json:"templateID"`
	VolumeKey        string            `json:"volumeKey,omitempty"`
	RootfsGeneration int64             `json:"rootfsGeneration,omitempty"`
	Image            string            `json:"image,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
}

// InstanceInfo represents the operational status and endpoints of a running/provisioned sandbox.
type InstanceInfo struct {
	SandboxID    string `json:"sandboxID"`
	PoddURL      string `json:"poddURL"` // e.g. "http://127.0.0.1:54321" or "http://<ip>:49983"
	WorkspaceDir string `json:"workspaceDir"`
	RootfsDir    string `json:"rootfsDir"`
	PID          int    `json:"pid,omitempty"`
	ContainerID  string `json:"containerID,omitempty"`
}

// Backend provides an abstraction over sandbox execution environments (e.g. LocalProcessBackend, DockerBackend).
type Backend interface {
	// EnsureRunning starts or resumes the sandbox, ensuring podd is active and healthy.
	EnsureRunning(ctx context.Context, spec SandboxSpec) (*InstanceInfo, error)

	// Pause prepares the filesystem for stop and terminates the execution process/container.
	Pause(ctx context.Context, sandboxID string) error

	// ResetRootFS wipes the rootfs upper layer while preserving the workspace directory.
	ResetRootFS(ctx context.Context, sandboxID string) error

	// Fork clones the storage of the source sandbox into a new sandbox and starts it.
	Fork(ctx context.Context, sourceSandboxID, newSandboxID string) (*InstanceInfo, error)

	// Delete stops any active execution and permanently deletes the sandbox storage.
	Delete(ctx context.Context, sandboxID string) error

	// Inspect retrieves the current state and endpoint information for a sandbox.
	Inspect(ctx context.Context, sandboxID string) (*InstanceInfo, error)

	// Close terminates all active instances managed by this backend.
	Close() error
}
