package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"google.golang.org/grpc"
)

// BuildOptions contains the arguments for an image build.
type BuildOptions struct {
	Tag            string
	ContextDir     string
	LogDir         string
	Dockerfile     string
	BuildID        string
	Passwords      []string
	NoCache        bool
	Registry       string
	RepoPrefix     string
	Slug           string
	SourceImage    string
	SourceAuth     *RegistryAuth
	PushSecretName string
	Namespace      string
	KubeClient     KubeClient
}

// ProbeOptions configures readiness probing for a built template image.
type ProbeOptions struct {
	BuildID          string
	Namespace        string
	StartCmd         string
	ReadyCmd         string
	RunAsUser        *int64
	RunAsGroup       *int64
	RunAsUsername    string
	Timeout          time.Duration
	PullSecretName   string
	SourceSecretName string
}

// ImageBuilder abstracts the image build and probe operations (Docker or BuildKit).
type ImageBuilder interface {
	Build(ctx context.Context, opts BuildOptions) error
	Probe(ctx context.Context, tag string, opts ProbeOptions) error
	TagExists(ctx context.Context, tag string) (bool, error)
	Ping(ctx context.Context) error
}

// ScrubString replaces sensitive passwords with [REDACTED].
func ScrubString(s string, passwords []string) string {
	res := s
	for _, p := range passwords {
		if p != "" {
			res = strings.ReplaceAll(res, p, "[REDACTED]")
		}
	}
	return res
}

func resolveLogBaseDir(baseDir string) string {
	if baseDir != "" {
		return baseDir
	}
	if env := os.Getenv("CUBEPOD_STATE"); env != "" {
		return filepath.Join(env, "template-context")
	}
	return filepath.Join(os.TempDir(), "cubepod-context")
}

// WriteScrubbedLog writes build log lines into logs/<buildID>.log under baseDir, scrubbing passwords.
func WriteScrubbedLog(baseDir, buildID string, lines []string, passwords []string) error {
	baseDir = resolveLogBaseDir(baseDir)
	logsDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return err
	}
	filePath := filepath.Join(logsDir, fmt.Sprintf("%s.log", buildID))

	var payload []byte
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, line := range lines {
		scrubbed := ScrubString(line, passwords)
		entry := BuildLogEntry{
			Timestamp: now,
			Message:   scrubbed,
			Level:     inferLogLevel(scrubbed),
		}
		b, _ := json.Marshal(entry)
		payload = append(payload, b...)
		payload = append(payload, '\n')
	}
	return os.WriteFile(filePath, payload, 0644)
}

// AppendScrubbedLog appends scrubbed JSONL build log entries into logs/<buildID>.log under baseDir.
func AppendScrubbedLog(baseDir, buildID string, lines []string, passwords []string) error {
	if buildID == "" || len(lines) == 0 {
		return nil
	}
	baseDir = resolveLogBaseDir(baseDir)
	logsDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return err
	}
	filePath := filepath.Join(logsDir, fmt.Sprintf("%s.log", buildID))

	var payload []byte
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, line := range lines {
		scrubbed := ScrubString(line, passwords)
		entry := BuildLogEntry{
			Timestamp: now,
			Message:   scrubbed,
			Level:     inferLogLevel(scrubbed),
		}
		b, _ := json.Marshal(entry)
		payload = append(payload, b...)
		payload = append(payload, '\n')
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(payload)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// FakeImageBuilder is an in-memory test double for ImageBuilder.
type FakeImageBuilder struct {
	mu            sync.Mutex
	BuildCalls    []BuildOptions
	ProbeCalls    []string
	ProbeOpts     []ProbeOptions
	PingCalls     int
	ExistingTags  map[string]bool
	BuildErr      error
	ProbeErr      error
	TagExistsErr  error
	PingErr       error
	BuildHook     func(opts BuildOptions) error
	ProbeHook     func(tag, readyCmd string) error
	TagExistsHook func(tag string) (bool, error)
	Logs          map[string][]string
}

// NewFakeImageBuilder creates a new FakeImageBuilder.
func NewFakeImageBuilder() *FakeImageBuilder {
	return &FakeImageBuilder{
		ExistingTags: make(map[string]bool),
		Logs:         make(map[string][]string),
	}
}

// Build records the call and returns the configured BuildErr or result of BuildHook.
func (f *FakeImageBuilder) Build(_ context.Context, opts BuildOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BuildCalls = append(f.BuildCalls, opts)
	f.ExistingTags[opts.Tag] = true

	logLines := []string{
		fmt.Sprintf("Step 1/1 : Building %s", opts.Tag),
	}
	for _, p := range opts.Passwords {
		if p != "" {
			logLines = append(logLines, fmt.Sprintf("Using secret password=%s for registry", p))
		}
	}
	f.Logs[opts.BuildID] = logLines

	logBase := opts.LogDir
	if logBase == "" {
		logBase = opts.ContextDir
	}
	_ = WriteScrubbedLog(logBase, opts.BuildID, logLines, opts.Passwords)

	if f.BuildHook != nil {
		return f.BuildHook(opts)
	}
	return f.BuildErr
}

// Probe records the probe call and returns ProbeErr or result of ProbeHook.
func (f *FakeImageBuilder) Probe(_ context.Context, tag string, opts ProbeOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProbeCalls = append(f.ProbeCalls, fmt.Sprintf("%s:%s", tag, opts.ReadyCmd))
	f.ProbeOpts = append(f.ProbeOpts, opts)
	if f.ProbeHook != nil {
		return f.ProbeHook(tag, opts.ReadyCmd)
	}
	return f.ProbeErr
}

// TagExists checks if the image tag exists in FakeImageBuilder.
func (f *FakeImageBuilder) TagExists(_ context.Context, tag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.TagExistsHook != nil {
		return f.TagExistsHook(tag)
	}
	if f.TagExistsErr != nil {
		return false, f.TagExistsErr
	}
	return f.ExistingTags[tag], nil
}

// Ping simulates checking builder connectivity.
func (f *FakeImageBuilder) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PingCalls++
	return f.PingErr
}

// LocalDockerBuilder implements ImageBuilder using the host Docker daemon.
type LocalDockerBuilder struct {
	baseDir string
}

// NewLocalDockerBuilder creates a LocalDockerBuilder.
func NewLocalDockerBuilder(baseDir string) *LocalDockerBuilder {
	return &LocalDockerBuilder{baseDir: baseDir}
}

func (b *LocalDockerBuilder) logDir(opts BuildOptions) string {
	if opts.LogDir != "" {
		return opts.LogDir
	}
	if b.baseDir != "" {
		return b.baseDir
	}
	return opts.ContextDir
}

func writeBuildPIDFile(baseDir, buildID string, pid int) string {
	if buildID == "" || pid <= 0 {
		return ""
	}
	dir := resolveLogBaseDir(baseDir)
	pidsDir := filepath.Join(dir, "pids")
	if err := os.MkdirAll(pidsDir, 0755); err != nil {
		return ""
	}
	pidPath := filepath.Join(pidsDir, buildID+".pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return ""
	}
	return pidPath
}

// Build runs docker build on the host socket while tracking the subprocess PID on disk.
func (b *LocalDockerBuilder) Build(ctx context.Context, opts BuildOptions) error {
	dockerfilePath := filepath.Join(opts.ContextDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(opts.Dockerfile), 0644); err != nil {
		return fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	args := []string{"build", "-t", opts.Tag, "-f", dockerfilePath}
	if opts.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, opts.ContextDir)

	cmd := exec.CommandContext(ctx, "docker", args...)
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start docker build: %w", err)
	}

	logBase := b.logDir(opts)
	if pidPath := writeBuildPIDFile(logBase, opts.BuildID, cmd.Process.Pid); pidPath != "" {
		defer func() { _ = os.Remove(pidPath) }()
	}

	err := cmd.Wait()
	rawOut := outBuf.String()
	lines := strings.Split(strings.TrimSuffix(rawOut, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	_ = AppendScrubbedLog(logBase, opts.BuildID, lines, opts.Passwords)

	if err != nil {
		return fmt.Errorf("docker build failed: %w (output: %s)", err, ScrubString(rawOut, opts.Passwords))
	}
	return nil
}

func dockerUserArg(opts ProbeOptions) string {
	if opts.RunAsUser != nil {
		if opts.RunAsGroup != nil {
			return fmt.Sprintf("%d:%d", *opts.RunAsUser, *opts.RunAsGroup)
		}
		return strconv.FormatInt(*opts.RunAsUser, 10)
	}
	return opts.RunAsUsername
}

// Probe starts a detached container (running StartCmd in the background when set) and polls ReadyCmd via docker exec.
func (b *LocalDockerBuilder) Probe(ctx context.Context, tag string, opts ProbeOptions) error {
	if opts.ReadyCmd == "" {
		return nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	userArg := dockerUserArg(opts)
	entryCmd := "while true; do sleep 3600; done"
	if opts.StartCmd != "" {
		entryCmd = opts.StartCmd
	}

	runArgs := []string{"run", "-d", "--rm"}
	if userArg != "" {
		runArgs = append(runArgs, "--user", userArg)
	}
	runArgs = append(runArgs, tag, "/bin/sh", "-c", entryCmd)

	runCmd := exec.CommandContext(probeCtx, "docker", runArgs...)
	cidOut, err := runCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe container start failed: %w (output: %s)", err, strings.TrimSpace(string(cidOut)))
	}
	cid := strings.TrimSpace(string(cidOut))
	if cid == "" {
		return fmt.Errorf("probe container start returned empty container id")
	}
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rmCancel()
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", cid).Run()
	}()

	execArgs := []string{"exec"}
	if userArg != "" {
		execArgs = append(execArgs, "--user", userArg)
	}
	execArgs = append(execArgs, cid, "/bin/sh", "-c", opts.ReadyCmd)

	var lastOut string
	for {
		execCmd := exec.CommandContext(probeCtx, "docker", execArgs...)
		out, execErr := execCmd.CombinedOutput()
		if execErr == nil {
			return nil
		}
		lastOut = strings.TrimSpace(string(out))

		select {
		case <-probeCtx.Done():
			return fmt.Errorf("probe timed out after %s: %w (last output: %s)", timeout, probeCtx.Err(), lastOut)
		case <-time.After(1 * time.Second):
		}
	}
}

// TagExists checks whether the docker image tag exists locally.
func (b *LocalDockerBuilder) TagExists(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", tag)
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

// Ping verifies local Docker daemon availability.
func (b *LocalDockerBuilder) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker ping failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// BuildKitBuilderOption configures optional BuildKitBuilder dependencies.
type BuildKitBuilderOption func(*BuildKitBuilder)

// WithBuildKitKubeClient attaches a KubeClient and namespace for push-secret lookup and probe pods.
func WithBuildKitKubeClient(kc KubeClient, namespace string) BuildKitBuilderOption {
	return func(b *BuildKitBuilder) {
		b.kubeClient = kc
		if namespace != "" {
			b.namespace = namespace
		}
	}
}

// WithBuildKitStateDir configures the state directory used for scrubbed build logs.
func WithBuildKitStateDir(dir string) BuildKitBuilderOption {
	return func(b *BuildKitBuilder) {
		b.stateDir = dir
	}
}

// WithBuildKitPushSecret configures the default Kubernetes Docker config secret used for registry HEAD checks.
func WithBuildKitPushSecret(secretName string) BuildKitBuilderOption {
	return func(b *BuildKitBuilder) {
		b.pushSecretName = secretName
	}
}

// WithBuildKitHTTPClient overrides the HTTP client used by TagExists registry checks.
func WithBuildKitHTTPClient(c *http.Client) BuildKitBuilderOption {
	return func(b *BuildKitBuilder) {
		if c != nil {
			b.httpClient = c
		}
	}
}

// WithBuildKitInsecureRegistry toggles plain HTTP for registry V2 manifest checks (useful in tests/dev).
func WithBuildKitInsecureRegistry(insecure bool) BuildKitBuilderOption {
	return func(b *BuildKitBuilder) {
		b.insecureRegistry = insecure
	}
}

// BuildKitBuilder implements ImageBuilder using a remote BuildKit daemon (`buildkitd`) and Kubernetes probe Pods.
type BuildKitBuilder struct {
	addr             string
	kubeClient       KubeClient
	namespace        string
	stateDir         string
	pushSecretName   string
	httpClient       *http.Client
	insecureRegistry bool
}

// NewBuildKitBuilder creates a BuildKitBuilder targeting the given buildkitd address.
func NewBuildKitBuilder(addr string, opts ...BuildKitBuilderOption) *BuildKitBuilder {
	b := &BuildKitBuilder{
		addr:       addr,
		namespace:  "default",
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

type dockerConfigJSON struct {
	Auths map[string]dockerConfigAuthEntry `json:"auths"`
}

type dockerConfigAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func loadDockerConfigAuths(ctx context.Context, kc KubeClient, namespace, secretName string) (map[string]RegistryAuth, []string, error) {
	if kc == nil || secretName == "" {
		return nil, nil, nil
	}
	if namespace == "" {
		namespace = "default"
	}
	sec, err := kc.GetSecret(ctx, namespace, secretName)
	if err != nil {
		return nil, nil, err
	}
	var raw []byte
	if sec.Data != nil {
		raw = sec.Data[".dockerconfigjson"]
	}
	if len(raw) == 0 && sec.StringData != nil {
		raw = []byte(sec.StringData[".dockerconfigjson"])
	}
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var cfg dockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("invalid .dockerconfigjson in secret %s: %w", secretName, err)
	}
	res := make(map[string]RegistryAuth, len(cfg.Auths))
	var secrets []string
	for host, entry := range cfg.Auths {
		u, p := entry.Username, entry.Password
		if (u == "" || p == "") && entry.Auth != "" {
			if decoded, decErr := base64.StdEncoding.DecodeString(entry.Auth); decErr == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					u = parts[0]
					p = parts[1]
				}
			}
		}
		res[normalizeRegistryHost(host)] = RegistryAuth{Username: u, Password: p}
		if p != "" {
			secrets = append(secrets, p)
		}
	}
	return res, secrets, nil
}

func normalizeRegistryHost(raw string) string {
	h := strings.TrimSpace(raw)
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	if idx := strings.Index(h, "/"); idx >= 0 {
		h = h[:idx]
	}
	switch h {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	default:
		return h
	}
}

func extractImageRegistryHost(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "registry-1.docker.io"
	}
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		return normalizeRegistryHost(parts[0])
	}
	return "registry-1.docker.io"
}

type buildKitRegistryAuthProvider struct {
	auth.UnimplementedAuthServer
	sourceHost string
	sourceAuth *RegistryAuth
	pushAuths  map[string]RegistryAuth
}

func (p *buildKitRegistryAuthProvider) Register(srv *grpc.Server) {
	auth.RegisterAuthServer(srv, p)
}

func (p *buildKitRegistryAuthProvider) Credentials(_ context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	host := normalizeRegistryHost(req.GetHost())
	if p.sourceAuth != nil && host == normalizeRegistryHost(p.sourceHost) {
		return &auth.CredentialsResponse{
			Username: p.sourceAuth.Username,
			Secret:   p.sourceAuth.Password,
		}, nil
	}
	if cred, ok := p.pushAuths[host]; ok {
		return &auth.CredentialsResponse{
			Username: cred.Username,
			Secret:   cred.Password,
		}, nil
	}
	return &auth.CredentialsResponse{}, nil
}

// Build solves the Dockerfile on remote buildkitd, streams scrubbed logs, and optionally pushes to a registry.
func (b *BuildKitBuilder) Build(ctx context.Context, opts BuildOptions) error {
	if b.addr == "" {
		return fmt.Errorf("buildkit address is empty")
	}
	dockerfilePath := filepath.Join(opts.ContextDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(opts.Dockerfile), 0644); err != nil {
		return fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	kc := opts.KubeClient
	if kc == nil {
		kc = b.kubeClient
	}
	ns := opts.Namespace
	if ns == "" {
		ns = b.namespace
	}
	pushSecret := opts.PushSecretName
	if pushSecret == "" {
		pushSecret = b.pushSecretName
	}

	passwords := append([]string(nil), opts.Passwords...)
	if opts.SourceAuth != nil && opts.SourceAuth.Password != "" {
		passwords = append(passwords, opts.SourceAuth.Password)
	}
	pushAuths, pushPasswords, err := loadDockerConfigAuths(ctx, kc, ns, pushSecret)
	if err != nil {
		return fmt.Errorf("failed to load push secret %s: %w", pushSecret, err)
	}
	passwords = append(passwords, pushPasswords...)

	c, err := client.New(ctx, b.addr)
	if err != nil {
		return fmt.Errorf("failed to connect to buildkitd at %s: %w", b.addr, err)
	}
	defer c.Close()

	frontendAttrs := map[string]string{}
	if opts.NoCache {
		frontendAttrs["no-cache"] = ""
	}

	exportAttrs := map[string]string{
		"name": opts.Tag,
	}
	if opts.Registry != "" {
		exportAttrs["push"] = "true"
	}

	solveOpt := client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		LocalDirs: map[string]string{
			"context":    opts.ContextDir,
			"dockerfile": opts.ContextDir,
		},
		Exports: []client.ExportEntry{
			{
				Type:  client.ExporterImage,
				Attrs: exportAttrs,
			},
		},
		Session: []session.Attachable{
			&buildKitRegistryAuthProvider{
				sourceHost: extractImageRegistryHost(opts.SourceImage),
				sourceAuth: opts.SourceAuth,
				pushAuths:  pushAuths,
			},
		},
	}

	if opts.Registry != "" && opts.Slug != "" {
		reg := strings.TrimRight(opts.Registry, "/")
		prefix := strings.Trim(opts.RepoPrefix, "/")
		var cacheRef string
		if prefix != "" {
			cacheRef = fmt.Sprintf("%s/%s/%s-cache", reg, prefix, opts.Slug)
		} else {
			cacheRef = fmt.Sprintf("%s/%s-cache", reg, opts.Slug)
		}
		solveOpt.CacheImports = []client.CacheOptionsEntry{
			{
				Type:  "registry",
				Attrs: map[string]string{"ref": cacheRef},
			},
		}
		solveOpt.CacheExports = []client.CacheOptionsEntry{
			{
				Type:  "registry",
				Attrs: map[string]string{"ref": cacheRef, "mode": "max"},
			},
		}
	}

	logBase := opts.LogDir
	if logBase == "" {
		logBase = b.stateDir
	}

	statusCh := make(chan *client.SolveStatus)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		for st := range statusCh {
			if st == nil {
				continue
			}
			var lines []string
			for _, v := range st.Vertexes {
				if v.Name != "" && (v.Started != nil || v.Completed != nil || v.Cached) {
					lines = append(lines, v.Name)
				}
				if v.Error != "" {
					lines = append(lines, "error: "+v.Error)
				}
			}
			for _, l := range st.Logs {
				for _, line := range strings.Split(strings.TrimSuffix(string(l.Data), "\n"), "\n") {
					if line != "" {
						lines = append(lines, line)
					}
				}
			}
			if len(lines) > 0 {
				_ = AppendScrubbedLog(logBase, opts.BuildID, lines, passwords)
			}
		}
	}()

	_, solveErr := c.Solve(ctx, nil, solveOpt, statusCh)
	<-logDone
	if solveErr != nil {
		return fmt.Errorf("buildkit solve failed: %s", ScrubString(solveErr.Error(), passwords))
	}
	return nil
}

// Probe runs a short-lived runc Pod (no kata RuntimeClass, no PVCs) to verify StartCmd/ReadyCmd readiness.
func (b *BuildKitBuilder) Probe(ctx context.Context, tag string, opts ProbeOptions) error {
	if opts.ReadyCmd == "" {
		return nil
	}
	kc := b.kubeClient
	if kc == nil {
		return fmt.Errorf("buildkit probe requires kubeClient")
	}
	ns := opts.Namespace
	if ns == "" {
		ns = b.namespace
	}
	if ns == "" {
		ns = "default"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	probePodName := sanitizeProbePodName(opts.BuildID)
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var pullSecrets []string
	seen := make(map[string]struct{})
	for _, s := range []string{opts.PullSecretName, opts.SourceSecretName} {
		if s != "" {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				pullSecrets = append(pullSecrets, s)
			}
		}
	}

	probeScript := fmt.Sprintf(`if [ -n "$CUBEPOD_PROBE_START_CMD" ]; then /bin/sh -c "$CUBEPOD_PROBE_START_CMD" >/tmp/probe-start.log 2>&1 & fi
deadline=$(( $(date +%%s) + %d ))
while :; do
  if /bin/sh -c "$CUBEPOD_PROBE_READY_CMD"; then
    exit 0
  fi
  if [ "$(date +%%s)" -ge "$deadline" ]; then
    exit 1
  fi
  sleep 1
done`, max(1, int(timeout.Seconds())))

	pod := PodManifest{
		Name:             probePodName,
		Namespace:        ns,
		RuntimeClassName: "", // Explicit empty RuntimeClassName uses default runc runtime.
		RestartPolicy:    "Never",
		Labels: map[string]string{
			"cubepod.io/probe":    "true",
			"cubepod.io/build-id": opts.BuildID,
		},
		Volumes: []VolumeManifest{
			{Name: "tmp", EmptyDir: true},
		},
		ImagePullSecrets: pullSecrets,
		Containers: []ContainerManifest{
			{
				Name:            "probe",
				Image:           tag,
				ImagePullPolicy: "IfNotPresent",
				Command:         []string{"/bin/sh", "-c", probeScript},
				VolumeMounts:    []VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
				Env: []EnvVar{
					{Name: "CUBEPOD_PROBE_START_CMD", Value: opts.StartCmd},
					{Name: "CUBEPOD_PROBE_READY_CMD", Value: opts.ReadyCmd},
				},
				RunAsUser:  opts.RunAsUser,
				RunAsGroup: opts.RunAsGroup,
			},
		},
	}

	if _, err := kc.EnsurePod(probeCtx, pod); err != nil {
		return fmt.Errorf("failed to create probe pod %s: %w", probePodName, err)
	}
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer delCancel()
		_ = kc.DeletePod(delCtx, ns, probePodName)
	}()

	for {
		phase, err := kc.GetPodPhase(probeCtx, ns, probePodName)
		if err == nil {
			switch phase {
			case "Succeeded":
				return nil
			case "Failed":
				return fmt.Errorf("probe pod %s failed readiness check", probePodName)
			}
		}

		select {
		case <-probeCtx.Done():
			return fmt.Errorf("probe pod %s timed out after %s: %w", probePodName, timeout, probeCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func sanitizeProbePodName(buildID string) string {
	clean := strings.ToLower(strings.ReplaceAll(buildID, "_", "-"))
	if clean == "" {
		clean = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	name := "cubepod-probe-" + clean
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func splitImageRef(tag string) (host, repo, ref string) {
	host = "registry-1.docker.io"
	rest := tag
	if parts := strings.SplitN(tag, "/", 2); len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		host = normalizeRegistryHost(parts[0])
		rest = parts[1]
	}

	ref = "latest"
	if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
		repo = rest[:atIdx]
		ref = rest[atIdx+1:]
		return host, repo, ref
	}
	if colonIdx := strings.LastIndex(rest, ":"); colonIdx >= 0 && !strings.Contains(rest[colonIdx:], "/") {
		repo = rest[:colonIdx]
		ref = rest[colonIdx+1:]
		return host, repo, ref
	}
	repo = rest
	return host, repo, ref
}

// TagExists checks if an image manifest exists in the OCI registry via a V2 HEAD request.
func (b *BuildKitBuilder) TagExists(ctx context.Context, tag string) (bool, error) {
	host, repo, ref := splitImageRef(tag)
	scheme := "https"
	if b.insecureRegistry {
		scheme = "http"
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, repo, ref)

	pushAuths, _, err := loadDockerConfigAuths(ctx, b.kubeClient, b.namespace, b.pushSecretName)
	if err != nil {
		return false, fmt.Errorf("failed to load registry auth secret %s: %w", b.pushSecretName, err)
	}
	cred, hasCred := pushAuths[host]

	doHead := func(authHeader string) (*http.Response, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
		}, ", "))
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		return b.httpClient.Do(req)
	}

	var initialAuth string
	if hasCred && (cred.Username != "" || cred.Password != "") {
		token := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
		initialAuth = "Basic " + token
	}
	resp, err := doHead(initialAuth)
	if err != nil {
		return false, fmt.Errorf("registry HEAD %s failed: %w", manifestURL, err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		if bearerHeader := b.exchangeBearerToken(ctx, resp.Header.Get("WWW-Authenticate"), cred, hasCred); bearerHeader != "" {
			resp2, err2 := doHead("Bearer " + bearerHeader)
			if err2 != nil {
				return false, fmt.Errorf("registry HEAD %s retry failed: %w", manifestURL, err2)
			}
			resp2.Body.Close()
			resp = resp2
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("registry HEAD %s returned status %d", manifestURL, resp.StatusCode)
	}
}

func (b *BuildKitBuilder) exchangeBearerToken(ctx context.Context, challenge string, cred RegistryAuth, hasCred bool) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "bearer ") {
		return ""
	}
	params := parseBearerChallengeParams(challenge)
	realm := params["realm"]
	if realm == "" {
		return ""
	}
	var query []string
	if svc := params["service"]; svc != "" {
		query = append(query, "service="+svc)
	}
	if scope := params["scope"]; scope != "" {
		query = append(query, "scope="+scope)
	}
	tokenURL := realm
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(tokenURL, "?") {
			sep = "&"
		}
		tokenURL += sep + strings.Join(query, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return ""
	}
	if hasCred && (cred.Username != "" || cred.Password != "") {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return ""
	}
	if tok.Token != "" {
		return tok.Token
	}
	return tok.AccessToken
}

func parseBearerChallengeParams(header string) map[string]string {
	out := make(map[string]string)
	idx := strings.IndexByte(header, ' ')
	if idx < 0 {
		return out
	}
	rest := header[idx+1:]
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return out
}

// Ping checks connectivity to buildkitd by listing configured workers.
func (b *BuildKitBuilder) Ping(ctx context.Context) error {
	if b.addr == "" {
		return fmt.Errorf("buildkit address is empty")
	}
	c, err := client.New(ctx, b.addr)
	if err != nil {
		return fmt.Errorf("buildkit ping failed to connect to %s: %w", b.addr, err)
	}
	defer c.Close()

	if _, err := c.ListWorkers(ctx); err != nil {
		return fmt.Errorf("buildkit list workers failed: %w", err)
	}
	return nil
}
