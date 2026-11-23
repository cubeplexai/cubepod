package volumenfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the controller-facing HTTP client for the internal lease API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient returns a lease API client. baseURL should be the lease API
// (never the NFS port), e.g. http://cubepod-volume-nfs.cubepod-system.svc:8080
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: httpClient,
	}
}

// Put registers or refreshes a lease. Pass an empty PodIP to pre-register
// before EnsurePod; pass the observed podIP to bind before Ready.
func (c *Client) Put(ctx context.Context, leaseID string, req LeaseRequest) (Lease, error) {
	var zero Lease
	if c == nil || c.BaseURL == "" {
		return zero, fmt.Errorf("volume nfs lease client is not configured")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	u, err := url.JoinPath(c.BaseURL, "leases", leaseID)
	if err != nil {
		return zero, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(payload))
	if err != nil {
		return zero, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("put lease %s: %w", leaseID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("put lease %s: status %d (%s)", leaseID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var lease Lease
	if err := json.Unmarshal(body, &lease); err != nil {
		return zero, fmt.Errorf("decode lease: %w", err)
	}
	return lease, nil
}

// Delete revokes a lease. A missing lease is not an error.
func (c *Client) Delete(ctx context.Context, leaseID string) error {
	if c == nil || c.BaseURL == "" || leaseID == "" {
		return nil
	}
	u, err := url.JoinPath(c.BaseURL, "leases", leaseID)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete lease %s: %w", leaseID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("delete lease %s: status %d (%s)", leaseID, resp.StatusCode, strings.TrimSpace(string(body)))
}
