// Package client provides an idiomatic Go SDK for the CubePod Control Plane and Sandbox Data Plane.
package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SandboxRecord represents a managed MicroVM execution sandbox.
type SandboxRecord struct {
	SandboxID        string            `json:"sandboxID"`
	TemplateID       string            `json:"templateID"`
	State            string            `json:"state"`
	EnvdPort         int               `json:"envdPort"`
	PoddURL          string            `json:"poddURL,omitempty"`
	RootfsGeneration int64             `json:"rootfsGeneration"`
	VolumeKey        string            `json:"volumeKey,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"createdAt,omitempty"`
	AutoResumeCount  int               `json:"autoResumeCount,omitempty"`
	Reason           string            `json:"reason,omitempty"`
}

// CreateSandboxRequest specifies the parameters for provisioning a sandbox.
type CreateSandboxRequest struct {
	TemplateID    string                 `json:"templateID"`
	VolumeKey     string                 `json:"volumeKey,omitempty"`
	Metadata      map[string]string      `json:"metadata,omitempty"`
	NetworkPolicy map[string]interface{} `json:"networkPolicy,omitempty"`
}

// RunCommandRequest parameters for in-sandbox command execution.
type RunCommandRequest struct {
	Command    string            `json:"command"`
	Background bool              `json:"background,omitempty"`
	Workdir    string            `json:"workdir,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
	UID        int               `json:"uid,omitempty"`
	GID        int               `json:"gid,omitempty"`
	UIDPtr     *int              `json:"-"`
	GIDPtr     *int              `json:"-"`
	RunAsRoot  bool              `json:"-"`
}

// CommandResult represents the final or initial execution output of a command.
type CommandResult struct {
	ID       string `json:"id"`
	ExitCode *int   `json:"exit_code"`
	Output   string `json:"output"`
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	Cursor   int    `json:"cursor"`
}

// CommandInfo provides execution status for a background command.
type CommandInfo struct {
	ID       string `json:"id"`
	PID      int    `json:"pid"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code"`
	Cursor   int    `json:"cursor"`
	Output   string `json:"output"`
}

// CommandLogs contains incremental stdout/stderr output from a background command.
type CommandLogs struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Cursor   int    `json:"cursor"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code"`
}

// FileEntry represents a file or directory entry inside the sandbox.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode,omitempty"`
}

// PanelEndpoint represents an HMAC-signed live-view proxy URL for browser panels.
type PanelEndpoint struct {
	URL       string     `json:"url"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// WarmPool represents a pre-warmed standby MicroVM pool.
type WarmPool struct {
	Name              string `json:"name"`
	TemplateID        string `json:"templateID"`
	Replicas          int32  `json:"replicas"`
	ReadyReplicas     int32  `json:"readyReplicas,omitempty"`
	AvailableReplicas int32  `json:"availableReplicas,omitempty"`
}

// RegistryAuth provides pull credentials for a base image during template builds.
type RegistryAuth struct {
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// BuildLogEntry represents a single structured template build log entry.
type BuildLogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

// BuildStatusReason describes why a template build failed.
type BuildStatusReason struct {
	Message    string          `json:"message"`
	Step       string          `json:"step,omitempty"`
	LogEntries []BuildLogEntry `json:"logEntries,omitempty"`
}

// UnmarshalJSON decodes either the canonical BuildStatusReason object or a plain string fallback.
func (r *BuildStatusReason) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var msg string
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return err
		}
		r.Message = msg
		return nil
	}
	type rawReason BuildStatusReason
	var raw rawReason
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	*r = BuildStatusReason(raw)
	return nil
}

// TemplateBuildStatus represents the response returned by GET /templates/{templateID}/builds/{buildID}/status.
type TemplateBuildStatus struct {
	TemplateID string             `json:"templateID"`
	BuildID    string             `json:"buildID"`
	Status     string             `json:"status"`
	Reason     *BuildStatusReason `json:"reason,omitempty"`
	Logs       []string           `json:"logs,omitempty"`
	LogEntries []BuildLogEntry    `json:"logEntries,omitempty"`
}

// TemplateBuildRecord summarizes a build entry on a TemplateWithBuilds response.
type TemplateBuildRecord struct {
	BuildID    string             `json:"buildID"`
	TemplateID string             `json:"templateID,omitempty"`
	Status     string             `json:"status"`
	CreatedAt  string             `json:"createdAt"`
	UpdatedAt  string             `json:"updatedAt"`
	CPUCount   int                `json:"cpuCount"`
	MemoryMB   int                `json:"memoryMB"`
	Reason     *BuildStatusReason `json:"reason,omitempty"`
}

// Template represents an E2B-compatible template catalog entry.
type Template struct {
	TemplateID    string                `json:"templateID,omitempty"`
	ID            string                `json:"id,omitempty"` // legacy compatibility
	BuildID       string                `json:"buildID,omitempty"`
	CPUCount      int                   `json:"cpuCount,omitempty"`
	MemoryMB      int                   `json:"memoryMB,omitempty"`
	DiskSizeMB    int                   `json:"diskSizeMB,omitempty"`
	Public        bool                  `json:"public,omitempty"`
	Aliases       []string              `json:"aliases,omitempty"`
	Names         []string              `json:"names,omitempty"`
	BuildStatus   string                `json:"buildStatus,omitempty"`
	EnvdVersion   string                `json:"envdVersion,omitempty"`
	SpawnCount    int                   `json:"spawnCount,omitempty"`
	BuildCount    int                   `json:"buildCount,omitempty"`
	CreatedAt     string                `json:"createdAt,omitempty"`
	UpdatedAt     string                `json:"updatedAt,omitempty"`
	LastSpawnedAt *string               `json:"lastSpawnedAt,omitempty"`
	Builds        []TemplateBuildRecord `json:"builds,omitempty"`
	Image         string                `json:"image,omitempty"`       // legacy compatibility
	Name          string                `json:"name,omitempty"`        // legacy compatibility
	Description   string                `json:"description,omitempty"` // legacy compatibility
}

// CreateTemplateFromImageRequest parameters for registering a template from an existing OCI image.
type CreateTemplateFromImageRequest struct {
	Name              string        `json:"name,omitempty"`
	Alias             string        `json:"alias,omitempty"`
	Tags              []string      `json:"tags,omitempty"`
	FromImage         string        `json:"fromImage"`
	FromImageRegistry *RegistryAuth `json:"fromImageRegistry,omitempty"`
	CPUCount          int           `json:"cpuCount,omitempty"`
	MemoryMB          int           `json:"memoryMB,omitempty"`
	MinFreeDiskMB     int           `json:"minFreeDiskMb,omitempty"`
	IdempotencyKey    string        `json:"-"`
	NoWait            bool          `json:"-"`
	PollInterval      time.Duration `json:"-"`
}

// TemplateCreateResult represents the response returned by template registration/build calls.
type TemplateCreateResult struct {
	TemplateID string   `json:"templateID"`
	BuildID    string   `json:"buildID"`
	Aliases    []string `json:"aliases"`
	Names      []string `json:"names"`
	Tags       []string `json:"tags,omitempty"`
	Public     bool     `json:"public,omitempty"`
}

// BuildStep represents a single step in an E2B-compatible template build recipe.
type BuildStep struct {
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	FilesHash string   `json:"filesHash,omitempty"`
	Force     bool     `json:"force,omitempty"`
}

// BuildTemplateRequest parameters for building a template via rootless BuildKit.
type BuildTemplateRequest struct {
	Name              string        `json:"name,omitempty"`
	Alias             string        `json:"alias,omitempty"`
	Tags              []string      `json:"tags,omitempty"`
	CPUCount          int           `json:"cpuCount,omitempty"`
	MemoryMB          int           `json:"memoryMB,omitempty"`
	MinFreeDiskMB     int           `json:"minFreeDiskMb,omitempty"`
	FromImage         string        `json:"fromImage,omitempty"`
	FromTemplate      string        `json:"fromTemplate,omitempty"`
	FromImageRegistry *RegistryAuth `json:"fromImageRegistry,omitempty"`
	Steps             []BuildStep   `json:"steps"`
	StartCmd          string        `json:"startCmd,omitempty"`
	ReadyCmd          string        `json:"readyCmd,omitempty"`
	Force             bool          `json:"force,omitempty"`
	IdempotencyKey    string        `json:"-"`
	NoWait            bool          `json:"-"`
	PollInterval      time.Duration `json:"-"`
}

// Option configures a Client instance.
type Option func(*Client)

// WithAPIKey sets the Bearer authorization token.
func WithAPIKey(apiKey string) Option {
	return func(c *Client) {
		c.apiKey = apiKey
	}
}

// WithHTTPClient customizes the underlying http.Client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// WithTimeout sets a custom HTTP client timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.httpClient.Timeout = timeout
	}
}

// APIError represents an HTTP error response from the CubePod API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cubepod error (HTTP %d): %s", e.StatusCode, e.Body)
}

// Client is the Go client for interacting with the CubePod Control Plane Gateway.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// New constructs a new CubePod API Client.
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := fmt.Sprintf("%s%s", c.baseURL, path)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
	}
	return resp, nil
}

// Health checks Gateway server availability.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res, nil
}

// CreateSandbox provisions a new persistent MicroVM sandbox.
func (c *Client) CreateSandbox(ctx context.Context, createReq CreateSandboxRequest) (*SandboxRecord, error) {
	payload := map[string]interface{}{
		"templateID": createReq.TemplateID,
		"metadata":   createReq.Metadata,
	}
	if createReq.VolumeKey != "" {
		payload["extension"] = map[string]string{"volumeKey": createReq.VolumeKey}
	}
	if createReq.NetworkPolicy != nil {
		payload["networkPolicy"] = createReq.NetworkPolicy
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/sandboxes", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// GetSandbox returns status and metadata for a specific sandbox.
func (c *Client) GetSandbox(ctx context.Context, sandboxID string) (*SandboxRecord, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("/sandboxes/%s", sandboxID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// ListSandboxes lists managed sandboxes, optionally filtered by templateID.
func (c *Client) ListSandboxes(ctx context.Context, templateID string) ([]*SandboxRecord, error) {
	path := "/sandboxes"
	if templateID != "" {
		path = fmt.Sprintf("/sandboxes?templateID=%s", url.QueryEscape(templateID))
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var list []*SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// DeleteSandbox terminates a sandbox and its compute Pod.
func (c *Client) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return c.DeleteSandboxWithOptions(ctx, sandboxID, false)
}

// DeleteSandboxWithOptions terminates a sandbox with optional volume deletion.
func (c *Client) DeleteSandboxWithOptions(ctx context.Context, sandboxID string, deleteVolumes bool) error {
	path := fmt.Sprintf("/sandboxes/%s", sandboxID)
	if deleteVolumes {
		path += "?deleteVolumes=true"
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// PauseSandbox stops the compute Pod while retaining RootFS and Workspace volumes.
func (c *Client) PauseSandbox(ctx context.Context, sandboxID string) (*SandboxRecord, error) {
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/pause", sandboxID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// ConnectSandbox resumes an idle/paused sandbox to Running state.
func (c *Client) ConnectSandbox(ctx context.Context, sandboxID string) (*SandboxRecord, error) {
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/connect", sandboxID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// ResetRootFS resets the persistent RootFS Upper volume while preserving /workspace.
func (c *Client) ResetRootFS(ctx context.Context, sandboxID string) (*SandboxRecord, error) {
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/reset-rootfs", sandboxID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// ForkSandbox creates a child copy-on-write clone of both RootFS and Workspace volumes.
func (c *Client) ForkSandbox(ctx context.Context, sandboxID string) (*SandboxRecord, error) {
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/fork", sandboxID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var record SandboxRecord
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// SetTimeout renews the idle auto-pause timeout.
func (c *Client) SetTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) error {
	body, _ := json.Marshal(map[string]int{"timeoutSeconds": timeoutSeconds})
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/timeout", sandboxID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// AcquireLease acquires or extends an exclusive lock preventing idle pause.
func (c *Client) AcquireLease(ctx context.Context, sandboxID string, leaseSeconds int) error {
	body, _ := json.Marshal(map[string]int{"leaseSeconds": leaseSeconds})
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/lease", sandboxID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// RunCommand executes a command inside the sandbox.
func (c *Client) RunCommand(ctx context.Context, sandboxID string, req RunCommandRequest) (*CommandResult, error) {
	var (
		body []byte
		err  error
	)
	if !req.RunAsRoot && req.UIDPtr == nil && req.GIDPtr == nil {
		body, err = json.Marshal(req)
	} else {
		type runCommandWire struct {
			Command    string            `json:"command"`
			Background bool              `json:"background,omitempty"`
			Workdir    string            `json:"workdir,omitempty"`
			Envs       map[string]string `json:"envs,omitempty"`
			UID        *int              `json:"uid,omitempty"`
			GID        *int              `json:"gid,omitempty"`
		}
		wire := runCommandWire{
			Command:    req.Command,
			Background: req.Background,
			Workdir:    req.Workdir,
			Envs:       req.Envs,
		}
		if req.UIDPtr != nil {
			wire.UID = req.UIDPtr
		} else if req.RunAsRoot {
			zero := 0
			wire.UID = &zero
		} else if req.UID != 0 {
			u := req.UID
			wire.UID = &u
		}
		if req.GIDPtr != nil {
			wire.GID = req.GIDPtr
		} else if req.RunAsRoot {
			zero := 0
			wire.GID = &zero
		} else if req.GID != 0 {
			g := req.GID
			wire.GID = &g
		}
		body, err = json.Marshal(wire)
	}
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/commands/run", sandboxID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetCommandStatus fetches the process status for a background command.
func (c *Client) GetCommandStatus(ctx context.Context, sandboxID, commandID string) (*CommandInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("/sandboxes/%s/commands/%s/status", sandboxID, commandID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var info CommandInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetCommandLogs retrieves incremental stdout/stderr logs for a background command.
func (c *Client) GetCommandLogs(ctx context.Context, sandboxID, commandID string, cursor int) (*CommandLogs, error) {
	path := fmt.Sprintf("/sandboxes/%s/commands/%s/logs?cursor=%d", sandboxID, commandID, cursor)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var logs CommandLogs
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		return nil, err
	}
	return &logs, nil
}

// KillCommand sends SIGKILL to a running command.
func (c *Client) KillCommand(ctx context.Context, sandboxID, commandID string) error {
	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("/sandboxes/%s/commands/%s/kill", sandboxID, commandID), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// WriteFile writes a file at the target path inside the sandbox.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, content []byte, mode string) error {
	return c.WriteFileWithOwner(ctx, sandboxID, path, content, mode, nil, nil)
}

// WriteFileWithOwner writes a file at the target path inside the sandbox with optional uid/gid ownership.
func (c *Client) WriteFileWithOwner(ctx context.Context, sandboxID, path string, content []byte, mode string, uid, gid *int) error {
	if mode == "" {
		mode = "0644"
	}
	u := fmt.Sprintf("/sandboxes/%s/files?path=%s&mode=%s", sandboxID, url.QueryEscape(path), url.QueryEscape(mode))
	if uid != nil {
		u += fmt.Sprintf("&uid=%d", *uid)
	}
	if gid != nil {
		u += fmt.Sprintf("&gid=%d", *gid)
	}
	req, err := c.newRequest(ctx, http.MethodPut, u, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ReadFile retrieves the binary contents of a file inside the sandbox.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	u := fmt.Sprintf("/sandboxes/%s/files?path=%s", sandboxID, url.QueryEscape(path))
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// DeleteFile removes a file or directory inside the sandbox.
func (c *Client) DeleteFile(ctx context.Context, sandboxID, path string) error {
	u := fmt.Sprintf("/sandboxes/%s/files?path=%s", sandboxID, url.QueryEscape(path))
	req, err := c.newRequest(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ListFiles lists the directory entries at path inside the sandbox.
func (c *Client) ListFiles(ctx context.Context, sandboxID, path string) ([]FileEntry, error) {
	if path == "" {
		path = "/workspace"
	}
	u := fmt.Sprintf("/sandboxes/%s/files/list?path=%s", sandboxID, url.QueryEscape(path))
	req, err := c.newRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Entries []FileEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Entries, nil
}

// ArchiveSync packs in-memory files into a tar.gz archive and extracts inside the sandbox.
func (c *Client) ArchiveSync(ctx context.Context, sandboxID, dest string, files map[string][]byte) error {
	return c.ArchiveSyncWithOwner(ctx, sandboxID, dest, files, nil, nil)
}

// ArchiveSyncWithOwner packs in-memory files into a tar.gz archive and extracts inside the sandbox with optional uid/gid ownership.
func (c *Client) ArchiveSyncWithOwner(ctx context.Context, sandboxID, dest string, files map[string][]byte, uid, gid *int) error {
	if dest == "" {
		dest = "/workspace"
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for p, content := range files {
		relName := strings.TrimPrefix(p, "/")
		hdr := &tar.Header{
			Name:    relName,
			Mode:    0644,
			Size:    int64(len(content)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	u := fmt.Sprintf("/sandboxes/%s/files/archive-sync?dest=%s", sandboxID, url.QueryEscape(dest))
	if uid != nil {
		u += fmt.Sprintf("&uid=%d", *uid)
	}
	if gid != nil {
		u += fmt.Sprintf("&gid=%d", *gid)
	}
	req, err := c.newRequest(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ArchiveSyncDir walks a local directory and syncs all files to dest in the sandbox in a single request.
func (c *Client) ArchiveSyncDir(ctx context.Context, sandboxID, dest, localDir string) error {
	files := make(map[string][]byte)
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = content
		return nil
	})
	if err != nil {
		return err
	}
	return c.ArchiveSync(ctx, sandboxID, dest, files)
}

// GetPanelEndpoint requests an HMAC-signed live-view proxy URL for browser panels.
func (c *Client) GetPanelEndpoint(ctx context.Context, sandboxID string, port int, expiresInSeconds int) (*PanelEndpoint, error) {
	if expiresInSeconds <= 0 {
		expiresInSeconds = 3600
	}
	body, _ := json.Marshal(map[string]int{"expiresInSeconds": expiresInSeconds})
	u := fmt.Sprintf("/sandboxes/%s/endpoints/%d", sandboxID, port)
	req, err := c.newRequest(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var endpoint PanelEndpoint
	if err := json.NewDecoder(resp.Body).Decode(&endpoint); err != nil {
		return nil, err
	}
	return &endpoint, nil
}

// CreateWarmPool registers and provisions a pre-warmed standby MicroVM pool.
func (c *Client) CreateWarmPool(ctx context.Context, name, templateID string, replicas int32) (*WarmPool, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"name":       name,
		"templateID": templateID,
		"replicas":   replicas,
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/warmpools", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var wp WarmPool
	if err := json.NewDecoder(resp.Body).Decode(&wp); err != nil {
		return nil, err
	}
	return &wp, nil
}

// ListWarmPools returns all configured PodWarmPools.
func (c *Client) ListWarmPools(ctx context.Context) ([]*WarmPool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/warmpools", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var wrapped struct {
		WarmPools []*WarmPool `json:"warmPools"`
	}
	if err := json.Unmarshal(bodyBytes, &wrapped); err == nil && wrapped.WarmPools != nil {
		return wrapped.WarmPools, nil
	}

	var direct []*WarmPool
	if err := json.Unmarshal(bodyBytes, &direct); err == nil {
		return direct, nil
	}

	return nil, fmt.Errorf("failed to decode warmpools response: %s", string(bodyBytes))
}

// ListTemplates retrieves available sandbox templates, supporting both standard JSON array and legacy wrapper shapes.
func (c *Client) ListTemplates(ctx context.Context) ([]*Template, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/templates", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(bodyBytes)
	var list []*Template
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
	} else {
		var wrapper struct {
			Templates []*Template `json:"templates"`
		}
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, err
		}
		list = wrapper.Templates
	}

	if list == nil {
		list = []*Template{}
	}
	for _, tpl := range list {
		if tpl.TemplateID == "" && tpl.ID != "" {
			tpl.TemplateID = tpl.ID
		}
	}
	return list, nil
}

// GetTemplate fetches status and metadata for a specific template by templateID.
func (c *Client) GetTemplate(ctx context.Context, templateID string) (*Template, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("/templates/%s", url.PathEscape(templateID)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tpl Template
	if err := json.NewDecoder(resp.Body).Decode(&tpl); err != nil {
		return nil, err
	}
	if tpl.TemplateID == "" && tpl.ID != "" {
		tpl.TemplateID = tpl.ID
	}
	return &tpl, nil
}

// GetTemplateByAlias fetches a template by its alias name or tag.
func (c *Client) GetTemplateByAlias(ctx context.Context, alias string) (*Template, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("/templates/aliases/%s", url.PathEscape(alias)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tpl Template
	if err := json.NewDecoder(resp.Body).Decode(&tpl); err != nil {
		return nil, err
	}
	if tpl.TemplateID == "" && tpl.ID != "" {
		tpl.TemplateID = tpl.ID
	}
	return &tpl, nil
}

// CreateTemplateFromImage provisions a template from an existing OCI image without triggering BuildKit.
// It sends only the caller-provided fields to POST /v3/templates, starts the build on
// POST /v2/templates/{templateID}/builds/{buildID}, and polls status until ready unless NoWait is set.
func (c *Client) CreateTemplateFromImage(ctx context.Context, req CreateTemplateFromImageRequest) (*TemplateCreateResult, error) {
	v3Payload := buildTemplateV3Payload(req.Name, req.Alias, req.Tags, req.CPUCount, req.MemoryMB, req.MinFreeDiskMB)
	buildPayload := map[string]interface{}{
		"fromImage": req.FromImage,
		"steps":     []BuildStep{},
	}
	if req.FromImageRegistry != nil {
		buildPayload["fromImageRegistry"] = req.FromImageRegistry
	}
	return c.createAndStartTemplateBuild(ctx, v3Payload, req.IdempotencyKey, buildPayload, req.NoWait, req.PollInterval)
}

// DeleteTemplate deletes a template by templateID. If force is true, ?force=true is appended.
func (c *Client) DeleteTemplate(ctx context.Context, templateID string, force bool) error {
	path := fmt.Sprintf("/templates/%s", url.PathEscape(templateID))
	if force {
		path += "?force=true"
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// UpdateTemplateVisibility updates a template's public visibility via PATCH /templates/{templateID}.
func (c *Client) UpdateTemplateVisibility(ctx context.Context, templateID string, public bool) error {
	body, _ := json.Marshal(map[string]bool{"public": public})
	req, err := c.newRequest(ctx, http.MethodPatch, fmt.Sprintf("/templates/%s", url.PathEscape(templateID)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// SetTemplatePublic marks a template as public via PATCH /templates/{templateID} with {"public":true}.
func (c *Client) SetTemplatePublic(ctx context.Context, templateID string) error {
	return c.UpdateTemplateVisibility(ctx, templateID, true)
}

// GetTemplateBuildStatus fetches the current build status and logs for a template build.
func (c *Client) GetTemplateBuildStatus(ctx context.Context, templateID, buildID string) (*TemplateBuildStatus, error) {
	path := fmt.Sprintf("/templates/%s/builds/%s/status", url.PathEscape(templateID), url.PathEscape(buildID))
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var status TemplateBuildStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}
	return &status, nil
}

// BuildTemplate is a thin helper for Beta template builds: registers via POST /v3/templates,
// triggers the build via POST /v2/templates/{templateID}/builds/{buildID}, and polls status until ready unless NoWait is set.
// Note: uploading context files (tar archives) via PUT /templates/{templateID}/files/{hash} is the caller's responsibility.
func (c *Client) BuildTemplate(ctx context.Context, req BuildTemplateRequest) (*TemplateCreateResult, error) {
	v3Payload := buildTemplateV3Payload(req.Name, req.Alias, req.Tags, req.CPUCount, req.MemoryMB, req.MinFreeDiskMB)

	steps := req.Steps
	if steps == nil {
		steps = []BuildStep{}
	}
	buildPayload := map[string]interface{}{
		"steps": steps,
	}
	if req.FromImage != "" {
		buildPayload["fromImage"] = req.FromImage
	}
	if req.FromTemplate != "" {
		buildPayload["fromTemplate"] = req.FromTemplate
	}
	if req.FromImageRegistry != nil {
		buildPayload["fromImageRegistry"] = req.FromImageRegistry
	}
	if req.StartCmd != "" {
		buildPayload["startCmd"] = req.StartCmd
	}
	if req.ReadyCmd != "" {
		buildPayload["readyCmd"] = req.ReadyCmd
	}
	if req.Force {
		buildPayload["force"] = true
	}

	return c.createAndStartTemplateBuild(ctx, v3Payload, req.IdempotencyKey, buildPayload, req.NoWait, req.PollInterval)
}

func buildTemplateV3Payload(name, alias string, tags []string, cpuCount, memoryMB, minFreeDiskMB int) map[string]interface{} {
	payload := make(map[string]interface{})
	if name != "" {
		payload["name"] = name
	}
	if alias != "" {
		payload["alias"] = alias
	}
	if len(tags) > 0 {
		payload["tags"] = tags
	}
	if cpuCount != 0 {
		payload["cpuCount"] = cpuCount
	}
	if memoryMB != 0 {
		payload["memoryMB"] = memoryMB
	}
	if minFreeDiskMB != 0 {
		payload["minFreeDiskMb"] = minFreeDiskMB
	}
	return payload
}

func (c *Client) createAndStartTemplateBuild(
	ctx context.Context,
	v3Payload map[string]interface{},
	idempotencyKey string,
	buildPayload map[string]interface{},
	noWait bool,
	pollInterval time.Duration,
) (*TemplateCreateResult, error) {
	bufV3, err := json.Marshal(v3Payload)
	if err != nil {
		return nil, err
	}
	reqV3, err := c.newRequest(ctx, http.MethodPost, "/v3/templates", bytes.NewReader(bufV3))
	if err != nil {
		return nil, err
	}
	reqV3.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		reqV3.Header.Set("Idempotency-Key", idempotencyKey)
	}
	respV3, err := c.do(reqV3)
	if err != nil {
		return nil, err
	}
	defer respV3.Body.Close()

	var res TemplateCreateResult
	if err := json.NewDecoder(respV3.Body).Decode(&res); err != nil {
		return nil, err
	}

	bufBuild, err := json.Marshal(buildPayload)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/v2/templates/%s/builds/%s", url.PathEscape(res.TemplateID), url.PathEscape(res.BuildID))
	reqBuild, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(bufBuild))
	if err != nil {
		return nil, err
	}
	reqBuild.Header.Set("Content-Type", "application/json")
	respBuild, err := c.do(reqBuild)
	if err != nil {
		return nil, err
	}
	defer respBuild.Body.Close()

	if res.Aliases == nil {
		res.Aliases = []string{}
	}
	if res.Names == nil {
		res.Names = []string{}
	}
	if res.Tags == nil {
		res.Tags = []string{}
	}

	if noWait {
		return &res, nil
	}

	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}

	for {
		status, err := c.GetTemplateBuildStatus(ctx, res.TemplateID, res.BuildID)
		if err != nil {
			return nil, err
		}
		switch status.Status {
		case "ready":
			return &res, nil
		case "error":
			msg := "template build failed"
			if status.Reason != nil && status.Reason.Message != "" {
				msg = status.Reason.Message
				if status.Reason.Step != "" {
					msg = fmt.Sprintf("%s (step: %s)", msg, status.Reason.Step)
				}
			}
			return nil, fmt.Errorf("template build %s failed: %s", res.BuildID, msg)
		case "waiting", "building", "uploaded", "":
			// Poll again until ready or error.
		default:
			return nil, fmt.Errorf("unexpected build status %q for build %s", status.Status, res.BuildID)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// FormatTime formats a time pointer or returns empty string.
func FormatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// ParseIntOrDefault parses a string to int with a default fallback.
func ParseIntOrDefault(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
