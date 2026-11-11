package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// DockerConfig defines settings for the Docker-based MicroVM/container runtime.
type DockerConfig struct {
	BinDir           string        `json:"binDir"`           // Host path containing podd and podd-init, e.g. /opt/cubepod/bin
	BaseDir          string        `json:"baseDir"`          // Host path for sandbox storage
	Network          string        `json:"network"`          // Docker network, e.g. "bridge"
	EnableSparseExt4 bool          `json:"enableSparseExt4"` // Create and mount sparse ext4 images
	SparseExt4Size   int64         `json:"sparseExt4Size"`
	HealthTimeout    time.Duration `json:"healthTimeout"`
}

type dockerInstance struct {
	spec        SandboxSpec
	info        InstanceInfo
	running     bool
	containerID string
	sandboxDir  string
}

// DockerBackend implements Backend using Docker containers with runtime binary injection.
type DockerBackend struct {
	cfg       DockerConfig
	mu        sync.RWMutex
	instances map[string]*dockerInstance
}

// NewDockerBackend creates a new DockerBackend.
func NewDockerBackend(cfg DockerConfig) *DockerBackend {
	if cfg.BinDir == "" {
		cfg.BinDir = "/opt/cubepod/bin"
	}
	if cfg.BaseDir == "" {
		cfg.BaseDir = filepath.Join(os.TempDir(), "cubepod-docker")
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 5 * time.Second
	}
	if cfg.SparseExt4Size == 0 {
		cfg.SparseExt4Size = 100 * 1024 * 1024
	}
	return &DockerBackend{
		cfg:       cfg,
		instances: make(map[string]*dockerInstance),
	}
}

func (d *DockerBackend) getSandboxDir(sandboxID string) string {
	return filepath.Join(d.cfg.BaseDir, sandboxID)
}

// BuildRunArgs generates the CLI arguments for "docker run" with binary injection and storage mounts.
func (d *DockerBackend) BuildRunArgs(spec SandboxSpec) ([]string, error) {
	if spec.SandboxID == "" {
		return nil, errors.New("sandbox ID is required")
	}

	sandboxDir := d.getSandboxDir(spec.SandboxID)
	workspaceDir := filepath.Join(sandboxDir, "workspace")
	rootfsDir := filepath.Join(sandboxDir, "rootfs")

	binDir := d.cfg.BinDir
	if binDir == "" {
		binDir = "/opt/cubepod/bin"
	}

	containerName := fmt.Sprintf("cubepod-%s", spec.SandboxID)

	image := spec.Image
	if image == "" {
		if spec.TemplateID != "" {
			image = spec.TemplateID
		} else {
			image = "cubeplex-ubuntu24-desktop:latest"
		}
	}

	args := []string{
		"run",
		"-d",
		"--name", containerName,
		"-v", fmt.Sprintf("%s:/opt/cubepod/bin:ro", binDir),
		"--entrypoint", "/opt/cubepod/bin/podd-init",
		"-v", fmt.Sprintf("%s:/workspace", workspaceDir),
		"-v", fmt.Sprintf("%s:/rootfs-upper", rootfsDir),
	}

	if d.cfg.EnableSparseExt4 {
		rootfsExt4 := filepath.Join(sandboxDir, "rootfs.ext4")
		workspaceExt4 := filepath.Join(sandboxDir, "workspace.ext4")
		args = append(args,
			"-v", fmt.Sprintf("%s:/opt/cubepod/disks/rootfs.ext4", rootfsExt4),
			"-v", fmt.Sprintf("%s:/opt/cubepod/disks/workspace.ext4", workspaceExt4),
		)
	}

	if d.cfg.Network != "" {
		args = append(args, "--network", d.cfg.Network)
	}

	args = append(args,
		"-e", "CUBEPOD_ADDR=0.0.0.0:49983",
		"-e", "CUBEPOD_WORKDIR=/workspace",
	)

	for k, v := range spec.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	args = append(args, image, "/opt/cubepod/bin/podd")

	return args, nil
}

// EnsureRunning starts or resumes the Docker container for the sandbox.
func (d *DockerBackend) EnsureRunning(ctx context.Context, spec SandboxSpec) (*InstanceInfo, error) {
	if spec.SandboxID == "" {
		return nil, errors.New("sandbox ID is required")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	sandboxDir := d.getSandboxDir(spec.SandboxID)
	workspaceDir := filepath.Join(sandboxDir, "workspace")
	rootfsDir := filepath.Join(sandboxDir, "rootfs")

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace dir: %w", err)
	}
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return nil, fmt.Errorf("create rootfs dir: %w", err)
	}

	if d.cfg.EnableSparseExt4 {
		rootfsExt4 := filepath.Join(sandboxDir, "rootfs.ext4")
		workspaceExt4 := filepath.Join(sandboxDir, "workspace.ext4")
		if _, err := os.Stat(rootfsExt4); os.IsNotExist(err) {
			if err := createSparseFile(rootfsExt4, d.cfg.SparseExt4Size); err != nil {
				return nil, fmt.Errorf("create sparse rootfs.ext4: %w", err)
			}
		}
		if _, err := os.Stat(workspaceExt4); os.IsNotExist(err) {
			if err := createSparseFile(workspaceExt4, d.cfg.SparseExt4Size); err != nil {
				return nil, fmt.Errorf("create sparse workspace.ext4: %w", err)
			}
		}
	}

	containerName := fmt.Sprintf("cubepod-%s", spec.SandboxID)

	inst, exists := d.instances[spec.SandboxID]
	if exists && inst.running {
		infoCopy := inst.info
		return &infoCopy, nil
	}

	args, err := d.BuildRunArgs(spec)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed (%s): %w", string(out), err)
	}

	info := InstanceInfo{
		SandboxID:    spec.SandboxID,
		PoddURL:      "http://127.0.0.1:49983",
		WorkspaceDir: workspaceDir,
		RootfsDir:    rootfsDir,
		ContainerID:  containerName,
	}

	d.instances[spec.SandboxID] = &dockerInstance{
		spec:        spec,
		info:        info,
		running:     true,
		containerID: containerName,
		sandboxDir:  sandboxDir,
	}

	infoCopy := info
	return &infoCopy, nil
}

// Pause calls prepare-stop on podd and stops the Docker container.
func (d *DockerBackend) Pause(ctx context.Context, sandboxID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	inst, ok := d.instances[sandboxID]
	if !ok {
		return ErrSandboxNotFound
	}

	if !inst.running {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.info.PoddURL+"/v1/lifecycle/prepare-stop", nil)
	if err == nil {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}

	containerName := fmt.Sprintf("cubepod-%s", sandboxID)
	_ = exec.CommandContext(ctx, "docker", "stop", containerName).Run()
	inst.running = false

	return nil
}

// ResetRootFS wipes rootfs/ while preserving workspace/.
func (d *DockerBackend) ResetRootFS(ctx context.Context, sandboxID string) error {
	d.mu.Lock()
	inst, ok := d.instances[sandboxID]
	if !ok {
		d.mu.Unlock()
		return ErrSandboxNotFound
	}
	wasRunning := inst.running
	spec := inst.spec
	d.mu.Unlock()

	if wasRunning {
		if err := d.Pause(ctx, sandboxID); err != nil {
			return fmt.Errorf("pause before reset: %w", err)
		}
	}

	d.mu.Lock()
	inst, ok = d.instances[sandboxID]
	if !ok {
		d.mu.Unlock()
		return ErrSandboxNotFound
	}

	rootfsDir := filepath.Join(inst.sandboxDir, "rootfs")
	if err := os.RemoveAll(rootfsDir); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("remove rootfs dir: %w", err)
	}
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		d.mu.Unlock()
		return fmt.Errorf("recreate rootfs dir: %w", err)
	}

	if d.cfg.EnableSparseExt4 {
		rootfsExt4 := filepath.Join(inst.sandboxDir, "rootfs.ext4")
		_ = os.Remove(rootfsExt4)
		if err := createSparseFile(rootfsExt4, d.cfg.SparseExt4Size); err != nil {
			d.mu.Unlock()
			return fmt.Errorf("recreate sparse rootfs.ext4: %w", err)
		}
	}

	inst.spec.RootfsGeneration++
	spec = inst.spec
	d.mu.Unlock()

	if wasRunning {
		_, err := d.EnsureRunning(ctx, spec)
		return err
	}

	return nil
}

// Fork clones source sandbox storage to new sandbox.
func (d *DockerBackend) Fork(ctx context.Context, sourceSandboxID, newSandboxID string) (*InstanceInfo, error) {
	d.mu.RLock()
	sourceInst, ok := d.instances[sourceSandboxID]
	if !ok {
		d.mu.RUnlock()
		return nil, ErrSourceSandboxNotFound
	}
	sourceDir := sourceInst.sandboxDir
	sourceSpec := sourceInst.spec
	sourceURL := sourceInst.info.PoddURL
	isRunning := sourceInst.running
	d.mu.RUnlock()

	if isRunning {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sourceURL+"/v1/lifecycle/prepare-stop", nil)
		if err == nil {
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}

	newDir := d.getSandboxDir(newSandboxID)
	_ = os.RemoveAll(newDir)
	if err := os.MkdirAll(newDir, 0755); err != nil {
		return nil, fmt.Errorf("create new sandbox dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", sourceDir+"/.", newDir)
	if err := cmd.Run(); err != nil {
		if err := copyDir(sourceDir, newDir); err != nil {
			return nil, fmt.Errorf("copy sandbox storage: %w", err)
		}
	}

	newSpec := SandboxSpec{
		SandboxID:        newSandboxID,
		TemplateID:       sourceSpec.TemplateID,
		VolumeKey:        newSandboxID,
		RootfsGeneration: sourceSpec.RootfsGeneration,
		Image:            sourceSpec.Image,
		Env:              copyEnv(sourceSpec.Env),
	}

	return d.EnsureRunning(ctx, newSpec)
}

// Delete permanently removes the Docker container and sandbox storage.
func (d *DockerBackend) Delete(ctx context.Context, sandboxID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	inst, ok := d.instances[sandboxID]
	if !ok {
		return ErrSandboxNotFound
	}

	containerName := fmt.Sprintf("cubepod-%s", sandboxID)
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()
	_ = os.RemoveAll(inst.sandboxDir)
	delete(d.instances, sandboxID)

	return nil
}

// Inspect returns current instance information.
func (d *DockerBackend) Inspect(ctx context.Context, sandboxID string) (*InstanceInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	inst, ok := d.instances[sandboxID]
	if !ok {
		return nil, ErrSandboxNotFound
	}

	infoCopy := inst.info
	return &infoCopy, nil
}

// Close stops all active containers.
func (d *DockerBackend) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for sandboxID, inst := range d.instances {
		if inst.running {
			containerName := fmt.Sprintf("cubepod-%s", sandboxID)
			_ = exec.Command("docker", "stop", containerName).Run()
			inst.running = false
		}
	}
	return nil
}
