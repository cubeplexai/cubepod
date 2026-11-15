package volume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// dockerClient talks to the Docker Engine HTTP API. The compose gateway image
// has no docker CLI; DOCKER_HOST is a TCP socket. Unit tests leave this disabled.
type dockerClient struct {
	host    string
	enabled bool
}

func (d dockerClient) ensureBindVolume(name, device string) error {
	if !d.enabled {
		return errors.New("docker volumes disabled")
	}
	if name == "" || device == "" || !v1alpha1.AllowedDockerVolumeName(name) {
		return errors.New("refusing docker volume create")
	}
	base, client, err := dockerAPI(d.host)
	if err != nil {
		return err
	}
	body := map[string]any{
		"Name":   name,
		"Driver": "local",
		"DriverOpts": map[string]string{
			"type":   "none",
			"o":      "bind",
			"device": device,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, resp, err := dockerDo(ctx, client, base, http.MethodPost, "/volumes/create", body)
	if err != nil {
		return err
	}
	if code == http.StatusCreated || code == http.StatusOK || code == http.StatusConflict {
		return nil
	}
	return fmt.Errorf("docker volume create %s: %d %s", name, code, strings.TrimSpace(string(resp)))
}

func (d dockerClient) removeVolume(name string) error {
	if !d.enabled {
		return nil
	}
	if !v1alpha1.AllowedDockerVolumeName(name) {
		return errors.New("refusing docker volume delete")
	}
	base, client, err := dockerAPI(d.host)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, resp, err := dockerDo(ctx, client, base, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("docker volume rm %s: %d %s", name, code, strings.TrimSpace(string(resp)))
}

func dockerAPI(host string) (string, *http.Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		return "http://docker/v1.44", &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var nd net.Dialer
					return nd.DialContext(ctx, "unix", sock)
				},
			},
		}, nil
	case strings.HasPrefix(host, "tcp://"):
		return "http://" + strings.TrimPrefix(host, "tcp://") + "/v1.44", &http.Client{Timeout: 5 * time.Second}, nil
	case strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://"):
		return strings.TrimRight(host, "/") + "/v1.44", &http.Client{Timeout: 5 * time.Second}, nil
	default:
		return "", nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}
}

func dockerDo(ctx context.Context, client *http.Client, base, method, p string, body any) (int, []byte, error) {
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
