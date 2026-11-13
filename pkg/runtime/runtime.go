package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const defaultQuiesceMaxFreezeMs = 1500

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

// quiesceSandbox calls POST /v1/lifecycle/quiesce on a reachable podd instance
// and returns a thaw cleanup function suitable for `defer`.
func quiesceSandbox(ctx context.Context, client *http.Client, poddURL string, maxFreezeMs int) func() {
	if poddURL == "" {
		return func() {}
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	if maxFreezeMs <= 0 {
		maxFreezeMs = defaultQuiesceMaxFreezeMs
	}

	payload := fmt.Sprintf(`{"maxFreezeMs":%d}`, maxFreezeMs)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, poddURL+"/v1/lifecycle/quiesce", bytes.NewBufferString(payload))
	if err != nil {
		return func() {}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// Parent podd not reachable; skip thaw.
		return func() {}
	}
	_ = resp.Body.Close()

	return func() {
		thawCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		thawReq, err := http.NewRequestWithContext(thawCtx, http.MethodPost, poddURL+"/v1/lifecycle/thaw", nil)
		if err != nil {
			return
		}
		thawResp, err := client.Do(thawReq)
		if err == nil {
			_ = thawResp.Body.Close()
		}
	}
}

// cloneSandboxStorage clones a sandbox's storage directory (including sparse .ext4 loopback images)
// using CoW reflink + sparse copy (`cp --reflink=auto --sparse=always -a`), falling back to `cp -a`
// on non-GNU platforms and Go directory copy if needed.
func cloneSandboxStorage(ctx context.Context, srcDir, dstDir string) error {
	_ = os.RemoveAll(dstDir)
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("create new sandbox dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", "-a", srcDir+"/.", dstDir)
	if err := cmd.Run(); err == nil {
		return nil
	}

	fallbackCmd := exec.CommandContext(ctx, "cp", "-a", srcDir+"/.", dstDir)
	if err := fallbackCmd.Run(); err == nil {
		return nil
	}

	if err := copyDir(srcDir, dstDir); err != nil {
		return fmt.Errorf("copy sandbox storage: %w", err)
	}
	return nil
}

// cloneWithQuiesce quiesces a running parent podd instance, defers thaw, and clones storage.
func cloneWithQuiesce(ctx context.Context, client *http.Client, parentURL string, isRunning bool, srcDir, dstDir string) error {
	if isRunning && parentURL != "" {
		thaw := quiesceSandbox(ctx, client, parentURL, defaultQuiesceMaxFreezeMs)
		defer thaw()
	}
	return cloneSandboxStorage(ctx, srcDir, dstDir)
}
