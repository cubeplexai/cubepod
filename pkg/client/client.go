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
		return nil, fmt.Errorf("cubepod error (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
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
	if req.UID == 0 && req.GID == 0 {
		req.UID = 1000
		req.GID = 1000
	}
	body, err := json.Marshal(req)
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
	if mode == "" {
		mode = "0644"
	}
	u := fmt.Sprintf("/sandboxes/%s/files?path=%s&mode=%s", sandboxID, url.QueryEscape(path), url.QueryEscape(mode))
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

	var list []*WarmPool
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
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
