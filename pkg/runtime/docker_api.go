package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ensureRunningAPI creates and starts a container through the Docker Engine
// HTTP API. The caller holds DockerBackend.mu.
func (d *DockerBackend) ensureRunningAPI(ctx context.Context, spec SandboxSpec, sandboxDir, workspaceDir, rootfsDir string) (*InstanceInfo, error) {
	plan, err := d.planRun(spec)
	if err != nil {
		return nil, err
	}
	_ = d.apiRemove(ctx, plan.Name)
	id, err := d.apiCreateStart(ctx, plan)
	if err != nil {
		return nil, err
	}
	ip, running, inspErr := d.apiInspect(ctx, id)
	if inspErr != nil || !running {
		_ = d.apiRemove(ctx, id)
		if inspErr != nil {
			return nil, inspErr
		}
		return nil, fmt.Errorf("sandbox container is not running")
	}
	poddURL := poddEndpoint(plan.Name, plan.Network, ip)
	if err := d.waitPodd(ctx, poddURL); err != nil {
		_ = d.apiRemove(ctx, id)
		return nil, err
	}
	info := InstanceInfo{
		SandboxID:    spec.SandboxID,
		PoddURL:      poddURL,
		WorkspaceDir: workspaceDir,
		RootfsDir:    rootfsDir,
		ContainerID:  id,
	}
	d.instances[spec.SandboxID] = &dockerInstance{
		spec:        spec,
		info:        info,
		running:     true,
		containerID: id,
		sandboxDir:  sandboxDir,
	}
	infoCopy := info
	return &infoCopy, nil
}

func (d *DockerBackend) waitPodd(ctx context.Context, poddURL string) error {
	if d.healthCheck != nil {
		return d.healthCheck(ctx, poddURL)
	}
	timeout := d.cfg.HealthTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	healthURL := strings.TrimSuffix(poddURL, "/") + "/health"
	var last error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			if last == nil {
				last = fmt.Errorf("timeout")
			}
			return fmt.Errorf("podd health %s: %w", poddURL, last)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func poddEndpoint(containerName, network, ip string) string {
	const port = 49983
	if network == "host" {
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	if network != "" && network != "bridge" && network != "default" && network != "none" && containerName != "" {
		return fmt.Sprintf("http://%s:%d", containerName, port)
	}
	if ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (d *DockerBackend) apiStop(ctx context.Context, name string) error {
	code, body, err := d.engineDo(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?t=5", nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusOK || code == http.StatusNotModified || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("docker stop %s: %d %s", name, code, strings.TrimSpace(string(body)))
}

func (d *DockerBackend) apiRemove(ctx context.Context, name string) error {
	code, body, err := d.engineDo(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=true", nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("docker rm %s: %d %s", name, code, strings.TrimSpace(string(body)))
}

func (d *DockerBackend) apiCreateStart(ctx context.Context, plan dockerRunPlan) (string, error) {
	hostConfig := map[string]any{}
	if len(plan.Binds) > 0 {
		hostConfig["Binds"] = plan.Binds
	}
	if len(plan.Mounts) > 0 {
		mounts := make([]map[string]any, 0, len(plan.Mounts))
		for _, m := range plan.Mounts {
			mounts = append(mounts, map[string]any{
				"Type":   "volume",
				"Source": m.Source,
				"Target": m.Target,
			})
		}
		hostConfig["Mounts"] = mounts
	}
	if plan.Network != "" {
		hostConfig["NetworkMode"] = plan.Network
	}
	if d.cfg.MemoryBytes > 0 {
		hostConfig["Memory"] = d.cfg.MemoryBytes
		hostConfig["MemorySwap"] = d.cfg.MemoryBytes
	}
	if d.cfg.NanoCPUs > 0 {
		hostConfig["NanoCpus"] = d.cfg.NanoCPUs
	}
	body := map[string]any{
		"Image":      plan.Image,
		"Env":        plan.Env,
		"HostConfig": hostConfig,
	}
	if len(plan.Entrypoint) > 0 {
		body["Entrypoint"] = plan.Entrypoint
	}
	if len(plan.Cmd) > 0 {
		body["Cmd"] = plan.Cmd
	}
	if plan.Network != "" && plan.Network != "host" && plan.Network != "none" && plan.Network != "bridge" && plan.Network != "default" {
		body["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{
				plan.Network: map[string]any{},
			},
		}
	}
	code, resp, err := d.engineDo(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(plan.Name), body)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", fmt.Errorf("docker create %s: %d %s", plan.Name, code, strings.TrimSpace(string(resp)))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(resp, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("docker create %s: bad response", plan.Name)
	}
	code, resp, err = d.engineDo(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start", nil)
	if err != nil {
		_ = d.apiRemove(ctx, created.ID)
		return "", err
	}
	if code >= 300 {
		_ = d.apiRemove(ctx, created.ID)
		return "", fmt.Errorf("docker start %s: %d %s", plan.Name, code, strings.TrimSpace(string(resp)))
	}
	return created.ID, nil
}

func (d *DockerBackend) apiInspect(ctx context.Context, id string) (string, bool, error) {
	code, resp, err := d.engineDo(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return "", false, err
	}
	if code >= 300 {
		return "", false, fmt.Errorf("docker inspect %s: %d %s", id, code, strings.TrimSpace(string(resp)))
	}
	var info struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(resp, &info); err != nil {
		return "", false, err
	}
	ip := ""
	if n, ok := info.NetworkSettings.Networks[d.cfg.Network]; ok {
		ip = n.IPAddress
	}
	return ip, info.State.Running, nil
}

func (d *DockerBackend) engineDo(ctx context.Context, method, p string, body any) (int, []byte, error) {
	base, client, err := dockerEngine(d.cfg.DockerHost)
	if err != nil {
		return 0, nil, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

func dockerEngine(host string) (string, *http.Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		return "http://docker/v1.44", &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var nd net.Dialer
					return nd.DialContext(ctx, "unix", sock)
				},
			},
		}, nil
	case strings.HasPrefix(host, "tcp://"):
		return "http://" + strings.TrimPrefix(host, "tcp://") + "/v1.44", &http.Client{Timeout: 60 * time.Second}, nil
	case strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://"):
		return strings.TrimRight(host, "/") + "/v1.44", &http.Client{Timeout: 60 * time.Second}, nil
	default:
		return "", nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}
}
