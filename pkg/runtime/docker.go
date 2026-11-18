package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// DockerConfig defines settings for the Docker-based MicroVM/container runtime.
type DockerConfig struct {
	BinDir           string        `json:"binDir"`           // Host path containing podd and podd-init, e.g. /opt/cubepod/bin
	BaseDir          string        `json:"baseDir"`          // Host path for sandbox storage, as seen by this process
	HostBaseDir      string        `json:"hostBaseDir"`      // Docker daemon view of BaseDir when they differ
	Network          string        `json:"network"`          // Docker network, e.g. "bridge"
	EnableSparseExt4 bool          `json:"enableSparseExt4"` // Create and mount sparse ext4 images
	SparseExt4Size   int64         `json:"sparseExt4Size"`
	HealthTimeout    time.Duration `json:"healthTimeout"`
	// DockerHost is DOCKER_HOST (tcp:// or unix://). When set, the Engine HTTP
	// API is used so a gateway image without the docker CLI can still run.
	DockerHost string `json:"dockerHost,omitempty"`
	// DefaultImage is used when SandboxSpec.Image and TemplateID are empty.
	DefaultImage string `json:"defaultImage,omitempty"`
	// InjectPoddBin bind-mounts BinDir and starts podd-init. Nil means true so
	// existing callers keep injection. Setting false is test-only (violates
	// AGENTS.md Invariant 2 if used in production).
	InjectPoddBin *bool `json:"injectPoddBin,omitempty"`
	// BindSandboxStorage bind-mounts per-sandbox workspace and rootfs directories.
	// Nil means true. Setting false is test-only (violates AGENTS.md Invariant 1
	// if used in production).
	BindSandboxStorage *bool `json:"bindSandboxStorage,omitempty"`
	// MemoryBytes and NanoCPUs are optional container limits. Zero leaves them unset.
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	NanoCPUs    int64 `json:"nanoCPUs,omitempty"`
	// ContainerNamePrefix defaults to "cubepod-". Compose may set
	// "cubepod-compat-compose-sb-" so official tests can inspect the container.
	ContainerNamePrefix string `json:"containerNamePrefix,omitempty"`
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
	cfg         DockerConfig
	mu          sync.RWMutex
	instances   map[string]*dockerInstance
	runDocker   func(ctx context.Context, args []string) ([]byte, error)
	httpClient  *http.Client
	healthCheck func(ctx context.Context, poddURL string) error
}

func (d *DockerBackend) execDocker(ctx context.Context, args ...string) ([]byte, error) {
	if d.runDocker != nil {
		return d.runDocker(ctx, args)
	}
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func (d *DockerBackend) client() *http.Client {
	if d.httpClient != nil {
		return d.httpClient
	}
	return &http.Client{Timeout: 2 * time.Second}
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

func (d *DockerBackend) daemonSandboxDir(sandboxID string) string {
	base := d.cfg.BaseDir
	if d.cfg.HostBaseDir != "" {
		base = d.cfg.HostBaseDir
	}
	return filepath.Join(base, sandboxID)
}

func (d *DockerBackend) injectBin() bool {
	if d.cfg.InjectPoddBin == nil {
		return true
	}
	return *d.cfg.InjectPoddBin
}

func (d *DockerBackend) bindSandboxStorage() bool {
	if d.cfg.BindSandboxStorage == nil {
		return true
	}
	return *d.cfg.BindSandboxStorage
}

func (d *DockerBackend) containerName(sandboxID string) string {
	p := d.cfg.ContainerNamePrefix
	if p == "" {
		p = "cubepod-"
	}
	return p + sandboxID
}

func (d *DockerBackend) useEngineAPI() bool {
	if d.runDocker != nil {
		return false
	}
	if strings.TrimSpace(d.cfg.DockerHost) != "" {
		return true
	}
	_, err := exec.LookPath("docker")
	return err != nil
}

type dockerNamedMount struct {
	Source string
	Target string
}

type dockerRunPlan struct {
	Name       string
	Hostname   string
	Image      string
	Env        []string
	Binds      []string
	Mounts     []dockerNamedMount
	Entrypoint []string
	Cmd        []string
	Network    string
}

// BuildRunArgs generates the CLI arguments for "docker run" with binary injection and storage mounts.
// A volume mount that is missing, relative, reserved, or an disallowed Docker volume name is an error.
func (d *DockerBackend) BuildRunArgs(spec SandboxSpec) ([]string, error) {
	plan, err := d.planRun(spec)
	if err != nil {
		return nil, err
	}
	args := []string{"run", "-d", "--name", plan.Name, "--hostname", plan.Hostname}
	if len(plan.Entrypoint) > 0 {
		// Injection keeps the historical flag order: bin bind, then entrypoint.
		if len(plan.Binds) > 0 && strings.Contains(plan.Binds[0], ":/opt/cubepod/bin:") {
			args = append(args, "-v", plan.Binds[0], "--entrypoint", plan.Entrypoint[0])
			for _, b := range plan.Binds[1:] {
				args = append(args, "-v", b)
			}
		} else {
			args = append(args, "--entrypoint", plan.Entrypoint[0])
			for _, b := range plan.Binds {
				args = append(args, "-v", b)
			}
		}
	} else {
		for _, b := range plan.Binds {
			args = append(args, "-v", b)
		}
	}
	for _, m := range plan.Mounts {
		args = append(args, "-v", m.Source+":"+m.Target)
	}
	if plan.Network != "" {
		args = append(args, "--network", plan.Network)
	}
	if d.cfg.MemoryBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(d.cfg.MemoryBytes, 10))
	}
	if d.cfg.NanoCPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(d.cfg.NanoCPUs)/1e9, 'f', -1, 64))
	}
	for _, e := range plan.Env {
		args = append(args, "-e", e)
	}
	args = append(args, plan.Image)
	args = append(args, plan.Cmd...)
	return args, nil
}

func (d *DockerBackend) planRun(spec SandboxSpec) (dockerRunPlan, error) {
	if spec.SandboxID == "" {
		return dockerRunPlan{}, errors.New("sandbox ID is required")
	}
	daemonDir := d.daemonSandboxDir(spec.SandboxID)
	workspaceDir := filepath.Join(daemonDir, "workspace")
	rootfsDir := filepath.Join(daemonDir, "rootfs")

	binDir := d.cfg.BinDir
	if binDir == "" {
		binDir = "/opt/cubepod/bin"
	}
	image := spec.Image
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		if spec.TemplateID != "" {
			image = spec.TemplateID
		} else {
			image = "cubeplex-ubuntu24-desktop:latest"
		}
	}

	plan := dockerRunPlan{
		Name:     d.containerName(spec.SandboxID),
		Hostname: spec.SandboxID,
		Image:    image,
		Network:  d.cfg.Network,
		Env: []string{
			"CUBEPOD_ADDR=0.0.0.0:49983",
			"CUBEPOD_WORKDIR=/workspace",
		},
	}
	// Filesystem-backed Docker containers cannot pivot/mount raw block devices.
	// Keep runtime injection and both independent storage binds, without granting
	// privileged containers or CAP_SYS_ADMIN. Sparse-ext4 mode keeps its pivot path.
	if !d.cfg.EnableSparseExt4 {
		plan.Env = append(plan.Env, "CUBEPOD_SKIP_PIVOT=1")
	}
	if d.injectBin() {
		plan.Binds = append(plan.Binds, fmt.Sprintf("%s:/opt/cubepod/bin:ro", binDir))
		plan.Entrypoint = []string{"/opt/cubepod/bin/podd-init"}
		plan.Cmd = []string{"/opt/cubepod/bin/podd"}
	}
	if d.bindSandboxStorage() {
		plan.Binds = append(plan.Binds,
			fmt.Sprintf("%s:/workspace", workspaceDir),
			fmt.Sprintf("%s:/rootfs-upper", rootfsDir),
		)
		if d.cfg.EnableSparseExt4 {
			plan.Binds = append(plan.Binds,
				fmt.Sprintf("%s:/opt/cubepod/disks/rootfs.ext4", filepath.Join(daemonDir, "rootfs.ext4")),
				fmt.Sprintf("%s:/opt/cubepod/disks/workspace.ext4", filepath.Join(daemonDir, "workspace.ext4")),
			)
		}
	}
	for _, m := range spec.VolumeMounts {
		path := filepath.Clean(m.ContainerPath)
		if !v1alpha1.ValidE2BContainerPath(path) {
			return dockerRunPlan{}, fmt.Errorf("invalid volume mount path %q", m.ContainerPath)
		}
		if m.DockerVolume != "" {
			if !v1alpha1.AllowedDockerVolumeName(m.DockerVolume) {
				return dockerRunPlan{}, fmt.Errorf("refusing docker volume %q", m.DockerVolume)
			}
			plan.Mounts = append(plan.Mounts, dockerNamedMount{Source: m.DockerVolume, Target: path})
			continue
		}
		if m.HostPath == "" || !filepath.IsAbs(m.HostPath) || hasDotDotSegment(m.HostPath) {
			return dockerRunPlan{}, fmt.Errorf("invalid volume host path for %s", path)
		}
		if filepath.Base(m.HostPath) != "data" {
			return dockerRunPlan{}, fmt.Errorf("volume host path must be the data directory")
		}
		plan.Binds = append(plan.Binds, fmt.Sprintf("%s:%s", m.HostPath, path))
	}
	for k, v := range spec.Env {
		plan.Env = append(plan.Env, fmt.Sprintf("%s=%s", k, v))
	}
	return plan, nil
}

func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
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

	if d.bindSandboxStorage() {
		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			return nil, fmt.Errorf("create workspace dir: %w", err)
		}
		if err := os.MkdirAll(rootfsDir, 0755); err != nil {
			return nil, fmt.Errorf("create rootfs dir: %w", err)
		}
	}

	if d.bindSandboxStorage() && d.cfg.EnableSparseExt4 {
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

	containerName := d.containerName(spec.SandboxID)

	inst, exists := d.instances[spec.SandboxID]
	if exists && inst.running {
		infoCopy := inst.info
		return &infoCopy, nil
	}

	if d.useEngineAPI() {
		return d.ensureRunningAPI(ctx, spec, sandboxDir, workspaceDir, rootfsDir)
	}

	args, err := d.BuildRunArgs(spec)
	if err != nil {
		return nil, err
	}

	out, err := d.execDocker(ctx, args...)
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
		resp, err := d.client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}

	containerName := d.containerName(sandboxID)
	if d.useEngineAPI() {
		_ = d.apiStop(ctx, containerName)
	} else {
		_, _ = d.execDocker(ctx, "stop", containerName)
	}
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
	if newSandboxID == "" {
		return nil, errors.New("new sandbox ID is required")
	}

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

	newDir := d.getSandboxDir(newSandboxID)
	if err := cloneWithQuiesce(ctx, d.client(), sourceURL, isRunning, sourceDir, newDir); err != nil {
		return nil, err
	}

	env := copyEnv(sourceSpec.Env)
	if env == nil {
		env = make(map[string]string)
	}
	env["CUBEPOD_FORKED"] = "1"

	newSpec := SandboxSpec{
		SandboxID:        newSandboxID,
		TemplateID:       sourceSpec.TemplateID,
		VolumeKey:        newSandboxID,
		RootfsGeneration: sourceSpec.RootfsGeneration,
		Image:            sourceSpec.Image,
		Env:              env,
		VolumeMounts:     append([]SandboxVolumeMount(nil), sourceSpec.VolumeMounts...),
	}

	return d.EnsureRunning(ctx, newSpec)
}

// Delete permanently removes the Docker container and sandbox storage.
func (d *DockerBackend) Delete(ctx context.Context, sandboxID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	containerName := d.containerName(sandboxID)
	inst, ok := d.instances[sandboxID]
	if !ok {
		if d.useEngineAPI() {
			_ = d.apiRemove(ctx, containerName)
			return nil
		}
		return ErrSandboxNotFound
	}

	if d.useEngineAPI() {
		_ = d.apiRemove(ctx, containerName)
	} else {
		_, _ = d.execDocker(ctx, "rm", "-f", containerName)
	}
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
			containerName := d.containerName(sandboxID)
			if d.useEngineAPI() {
				_ = d.apiStop(context.Background(), containerName)
			} else {
				_, _ = d.execDocker(context.Background(), "stop", containerName)
			}
			inst.running = false
		}
	}
	return nil
}
