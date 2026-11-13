package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BuildOptions contains the arguments for an image build.
type BuildOptions struct {
	Tag        string
	ContextDir string
	Dockerfile string
	BuildID    string
	Passwords  []string
	NoCache    bool
}

// ImageBuilder abstracts the image build and probe operations (Docker or BuildKit).
type ImageBuilder interface {
	Build(ctx context.Context, opts BuildOptions) error
	Probe(ctx context.Context, tag string, readyCmd string, timeout time.Duration) error
	TagExists(ctx context.Context, tag string) (bool, error)
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

// WriteScrubbedLog writes build log lines into logs/<buildID>.log under baseDir, scrubbing passwords.
func WriteScrubbedLog(baseDir, buildID string, lines []string, passwords []string) error {
	if baseDir == "" {
		if env := os.Getenv("CUBEPOD_STATE"); env != "" {
			baseDir = filepath.Join(env, "template-context")
		} else {
			baseDir = filepath.Join(os.TempDir(), "cubepod-context")
		}
	}
	logsDir := filepath.Join(baseDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return err
	}
	filePath := filepath.Join(logsDir, fmt.Sprintf("%s.log", buildID))

	var scrubbed []string
	for _, line := range lines {
		scrubbed = append(scrubbed, ScrubString(line, passwords))
	}
	content := strings.Join(scrubbed, "\n") + "\n"
	return os.WriteFile(filePath, []byte(content), 0644)
}

// FakeImageBuilder is an in-memory test double for ImageBuilder.
type FakeImageBuilder struct {
	mu            sync.Mutex
	BuildCalls    []BuildOptions
	ProbeCalls    []string
	ExistingTags  map[string]bool
	BuildErr      error
	ProbeErr      error
	TagExistsErr  error
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

	_ = WriteScrubbedLog(opts.ContextDir, opts.BuildID, logLines, opts.Passwords)

	if f.BuildHook != nil {
		return f.BuildHook(opts)
	}
	return f.BuildErr
}

// Probe records the probe call and returns ProbeErr or result of ProbeHook.
func (f *FakeImageBuilder) Probe(_ context.Context, tag string, readyCmd string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ProbeCalls = append(f.ProbeCalls, fmt.Sprintf("%s:%s", tag, readyCmd))
	if f.ProbeHook != nil {
		return f.ProbeHook(tag, readyCmd)
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

// LocalDockerBuilder implements ImageBuilder using the host Docker daemon.
type LocalDockerBuilder struct {
	baseDir string
}

// NewLocalDockerBuilder creates a LocalDockerBuilder.
func NewLocalDockerBuilder(baseDir string) *LocalDockerBuilder {
	return &LocalDockerBuilder{baseDir: baseDir}
}

// Build runs docker build on the host socket.
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
	out, err := cmd.CombinedOutput()

	lines := strings.Split(string(out), "\n")
	_ = WriteScrubbedLog(b.baseDir, opts.BuildID, lines, opts.Passwords)

	if err != nil {
		return fmt.Errorf("docker build failed: %w (output: %s)", err, ScrubString(string(out), opts.Passwords))
	}
	return nil
}

// Probe executes docker run --rm to test the readyCmd.
func (b *LocalDockerBuilder) Probe(ctx context.Context, tag string, readyCmd string, timeout time.Duration) error {
	if readyCmd == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, "docker", "run", "--rm", tag, "/bin/sh", "-c", readyCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("probe failed: %w (output: %s)", err, string(out))
	}
	return nil
}

// TagExists checks whether the docker image tag exists locally.
func (b *LocalDockerBuilder) TagExists(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", tag)
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}
