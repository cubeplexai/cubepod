package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// PoddProcessBackendConfig configures the local process-based podd backend.
type PoddProcessBackendConfig struct {
	BaseDir          string        // Base directory where sandboxes storage will be placed
	BinaryPath       string        // Path to compiled bin/podd binary
	EnableSparseExt4 bool          // Create sparse rootfs.ext4 and workspace.ext4 image files
	SparseExt4Size   int64         // Size of sparse ext4 images in bytes (default 100MB)
	HealthTimeout    time.Duration // Max time to wait for GET /health (default 5s)
}

type poddInstance struct {
	spec         SandboxSpec
	info         InstanceInfo
	cmd          *exec.Cmd
	running      bool
	port         int
	workspaceDir string
	rootfsDir    string
	sandboxDir   string
}

// PoddProcessBackend manages local podd daemon processes and persistent storage directories.
type PoddProcessBackend struct {
	cfg       PoddProcessBackendConfig
	mu        sync.RWMutex
	instances map[string]*poddInstance
	binary    string
}

// NewPoddProcessBackend initializes a new PoddProcessBackend.
func NewPoddProcessBackend(cfg PoddProcessBackendConfig) (*PoddProcessBackend, error) {
	if cfg.BaseDir == "" {
		cfg.BaseDir = filepath.Join(os.TempDir(), "cubepod-sandboxes")
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 5 * time.Second
	}
	if cfg.SparseExt4Size == 0 {
		cfg.SparseExt4Size = 100 * 1024 * 1024 // 100 MiB
	}

	bin, err := resolvePoddBinary(cfg.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("resolve podd binary: %w", err)
	}

	if err := os.MkdirAll(cfg.BaseDir, 0755); err != nil {
		return nil, fmt.Errorf("create base dir %s: %w", cfg.BaseDir, err)
	}

	return &PoddProcessBackend{
		cfg:       cfg,
		instances: make(map[string]*poddInstance),
		binary:    bin,
	}, nil
}

func resolvePoddBinary(explicit string) (string, error) {
	if explicit != "" {
		if fi, err := os.Stat(explicit); err == nil && !fi.IsDir() {
			return filepath.Abs(explicit)
		}
		return "", fmt.Errorf("specified podd binary not found: %s", explicit)
	}

	candidates := []string{
		"bin/podd",
		"../bin/podd",
		"../../bin/podd",
		"/tmp/cubepod-wt-cep7/bin/podd",
	}

	for _, cand := range candidates {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return filepath.Abs(cand)
		}
	}

	if path, err := exec.LookPath("podd"); err == nil {
		return path, nil
	}

	return "", errors.New("podd binary not found (looked in bin/podd, ../bin/podd, ../../bin/podd, PATH)")
}

func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func createSparseFile(path string, sizeBytes int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(sizeBytes)
}

// EnsureRunning starts or resumes the sandbox, ensuring podd is active and healthy.
func (b *PoddProcessBackend) EnsureRunning(ctx context.Context, spec SandboxSpec) (*InstanceInfo, error) {
	if spec.SandboxID == "" {
		return nil, errors.New("sandbox ID is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	inst, exists := b.instances[spec.SandboxID]
	if exists && inst.running {
		if b.isHealthy(ctx, inst.info.PoddURL) {
			infoCopy := inst.info
			return &infoCopy, nil
		}
		b.stopInstanceProcess(inst)
	}

	sandboxDir := filepath.Join(b.cfg.BaseDir, spec.SandboxID)
	workspaceDir := filepath.Join(sandboxDir, "workspace")
	rootfsDir := filepath.Join(sandboxDir, "rootfs")

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("create workspace dir: %w", err)
	}
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		return nil, fmt.Errorf("create rootfs dir: %w", err)
	}

	if b.cfg.EnableSparseExt4 {
		rootfsExt4 := filepath.Join(sandboxDir, "rootfs.ext4")
		workspaceExt4 := filepath.Join(sandboxDir, "workspace.ext4")
		if _, err := os.Stat(rootfsExt4); os.IsNotExist(err) {
			if err := createSparseFile(rootfsExt4, b.cfg.SparseExt4Size); err != nil {
				return nil, fmt.Errorf("create sparse rootfs.ext4: %w", err)
			}
		}
		if _, err := os.Stat(workspaceExt4); os.IsNotExist(err) {
			if err := createSparseFile(workspaceExt4, b.cfg.SparseExt4Size); err != nil {
				return nil, fmt.Errorf("create sparse workspace.ext4: %w", err)
			}
		}
	}

	port, err := getFreePort()
	if err != nil {
		return nil, fmt.Errorf("allocate free port: %w", err)
	}

	poddURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command(b.binary)
	cmd.Dir = workspaceDir
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("CUBEPOD_ADDR=127.0.0.1:%d", port),
		fmt.Sprintf("CUBEPOD_WORKDIR=%s", workspaceDir),
	)
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start podd binary: %w", err)
	}

	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
	}()

	// Wait for GET /health with exponential backoff up to HealthTimeout
	if err := b.waitForHealth(ctx, poddURL, b.cfg.HealthTimeout, cmd); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("wait for podd health check on %s: %w", poddURL, err)
	}

	info := InstanceInfo{
		SandboxID:    spec.SandboxID,
		PoddURL:      poddURL,
		WorkspaceDir: workspaceDir,
		RootfsDir:    rootfsDir,
		PID:          pid,
	}

	if inst == nil {
		inst = &poddInstance{
			spec:         spec,
			info:         info,
			cmd:          cmd,
			running:      true,
			port:         port,
			workspaceDir: workspaceDir,
			rootfsDir:    rootfsDir,
			sandboxDir:   sandboxDir,
		}
		b.instances[spec.SandboxID] = inst
	} else {
		inst.spec = spec
		inst.info = info
		inst.cmd = cmd
		inst.running = true
		inst.port = port
	}

	infoCopy := inst.info
	return &infoCopy, nil
}

func (b *PoddProcessBackend) waitForHealth(ctx context.Context, poddURL string, timeout time.Duration, cmd *exec.Cmd) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	backoff := 20 * time.Millisecond

	for {
		if time.Now().After(deadline) {
			return errors.New("health check timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if cmd != nil && cmd.Process != nil {
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return fmt.Errorf("podd process died unexpectedly: %w", err)
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, poddURL+"/health", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}

		time.Sleep(backoff)
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

func (b *PoddProcessBackend) isHealthy(ctx context.Context, poddURL string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, poddURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Pause calls prepare-stop on podd, terminates the process, and keeps storage intact.
func (b *PoddProcessBackend) Pause(ctx context.Context, sandboxID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	inst, ok := b.instances[sandboxID]
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

	b.stopInstanceProcess(inst)
	inst.running = false
	inst.info.PID = 0
	inst.cmd = nil

	return nil
}

func (b *PoddProcessBackend) stopInstanceProcess(inst *poddInstance) {
	if inst.cmd == nil || inst.cmd.Process == nil {
		inst.running = false
		return
	}

	proc := inst.cmd.Process
	_ = proc.Signal(syscall.SIGTERM)

	stopped := false
	for i := 0; i < 30; i++ {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !stopped {
		_ = proc.Kill()
		for i := 0; i < 20; i++ {
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				stopped = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	inst.running = false
}

// ResetRootFS wipes rootfs/ while preserving workspace/.
func (b *PoddProcessBackend) ResetRootFS(ctx context.Context, sandboxID string) error {
	b.mu.Lock()
	inst, ok := b.instances[sandboxID]
	if !ok {
		b.mu.Unlock()
		return ErrSandboxNotFound
	}

	wasRunning := inst.running
	spec := inst.spec
	b.mu.Unlock()

	if wasRunning {
		if err := b.Pause(ctx, sandboxID); err != nil {
			return fmt.Errorf("pause before reset: %w", err)
		}
	}

	b.mu.Lock()
	inst, ok = b.instances[sandboxID]
	if !ok {
		b.mu.Unlock()
		return ErrSandboxNotFound
	}

	if err := os.RemoveAll(inst.rootfsDir); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("remove rootfs dir: %w", err)
	}
	if err := os.MkdirAll(inst.rootfsDir, 0755); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("recreate rootfs dir: %w", err)
	}

	if b.cfg.EnableSparseExt4 {
		rootfsExt4 := filepath.Join(inst.sandboxDir, "rootfs.ext4")
		_ = os.Remove(rootfsExt4)
		if err := createSparseFile(rootfsExt4, b.cfg.SparseExt4Size); err != nil {
			b.mu.Unlock()
			return fmt.Errorf("recreate sparse rootfs.ext4: %w", err)
		}
	}

	inst.spec.RootfsGeneration++
	spec = inst.spec
	b.mu.Unlock()

	if wasRunning {
		_, err := b.EnsureRunning(ctx, spec)
		return err
	}

	return nil
}

// Fork copies rootfs/ and workspace/ to a new sandbox and starts it.
func (b *PoddProcessBackend) Fork(ctx context.Context, sourceSandboxID, newSandboxID string) (*InstanceInfo, error) {
	b.mu.RLock()
	sourceInst, ok := b.instances[sourceSandboxID]
	if !ok {
		b.mu.RUnlock()
		return nil, ErrSourceSandboxNotFound
	}
	sourceDir := sourceInst.sandboxDir
	sourceSpec := sourceInst.spec
	sourceURL := sourceInst.info.PoddURL
	isRunning := sourceInst.running
	b.mu.RUnlock()

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

	newDir := filepath.Join(b.cfg.BaseDir, newSandboxID)
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

	return b.EnsureRunning(ctx, newSpec)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func copyEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// Delete permanently removes the sandbox.
func (b *PoddProcessBackend) Delete(ctx context.Context, sandboxID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	inst, ok := b.instances[sandboxID]
	if !ok {
		return ErrSandboxNotFound
	}

	b.stopInstanceProcess(inst)
	_ = os.RemoveAll(inst.sandboxDir)
	delete(b.instances, sandboxID)
	return nil
}

// Inspect returns current instance information.
func (b *PoddProcessBackend) Inspect(ctx context.Context, sandboxID string) (*InstanceInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	inst, ok := b.instances[sandboxID]
	if !ok {
		return nil, ErrSandboxNotFound
	}

	infoCopy := inst.info
	return &infoCopy, nil
}

// Close terminates all running podd daemon processes.
func (b *PoddProcessBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, inst := range b.instances {
		b.stopInstanceProcess(inst)
	}
	return nil
}
