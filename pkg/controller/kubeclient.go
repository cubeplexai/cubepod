package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KubeClient abstracts Kubernetes resource operations for PodBox workloads.
type KubeClient interface {
	EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error
	DeletePVC(ctx context.Context, namespace, name string) error
	EnsurePod(ctx context.Context, pod PodManifest) (podIP string, err error)
	DeletePod(ctx context.Context, namespace, name string) error
}

// MemoryKubeClient is an in-memory, thread-safe implementation of KubeClient
// for unit testing and offline development.
type MemoryKubeClient struct {
	mu   sync.RWMutex
	PVCs map[string]BlockPVCManifest
	Pods map[string]PodManifest
	IPs  map[string]string
}

// NewMemoryKubeClient creates a new in-memory KubeClient.
func NewMemoryKubeClient() *MemoryKubeClient {
	return &MemoryKubeClient{
		PVCs: make(map[string]BlockPVCManifest),
		Pods: make(map[string]PodManifest),
		IPs:  make(map[string]string),
	}
}

func pvcKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return fmt.Sprintf("%s/%s", namespace, name)
}

func podKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return fmt.Sprintf("%s/%s", namespace, name)
}

// EnsurePVC records the PVC manifest in memory.
func (m *MemoryKubeClient) EnsurePVC(_ context.Context, pvc BlockPVCManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PVCs[pvcKey(pvc.Namespace, pvc.Name)] = pvc
	return nil
}

// DeletePVC removes the PVC manifest from memory.
func (m *MemoryKubeClient) DeletePVC(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.PVCs, pvcKey(namespace, name))
	return nil
}

// EnsurePod records the Pod manifest in memory and returns a mock Pod IP.
func (m *MemoryKubeClient) EnsurePod(_ context.Context, pod PodManifest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := podKey(pod.Namespace, pod.Name)
	m.Pods[key] = pod
	ip, ok := m.IPs[key]
	if !ok {
		ip = "10.244.0.42"
		m.IPs[key] = ip
	}
	return ip, nil
}

// DeletePod removes the Pod manifest from memory.
func (m *MemoryKubeClient) DeletePod(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := podKey(namespace, name)
	delete(m.Pods, key)
	delete(m.IPs, key)
	return nil
}

// GetPVC retrieves a stored PVC manifest by namespace and name.
func (m *MemoryKubeClient) GetPVC(namespace, name string) (BlockPVCManifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pvc, ok := m.PVCs[pvcKey(namespace, name)]
	return pvc, ok
}

// GetPod retrieves a stored Pod manifest by namespace and name.
func (m *MemoryKubeClient) GetPod(namespace, name string) (PodManifest, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pod, ok := m.Pods[podKey(namespace, name)]
	return pod, ok
}

// ListPVCs returns all PVC manifests recorded in memory.
func (m *MemoryKubeClient) ListPVCs() []BlockPVCManifest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]BlockPVCManifest, 0, len(m.PVCs))
	for _, pvc := range m.PVCs {
		res = append(res, pvc)
	}
	return res
}

// ListPods returns all Pod manifests recorded in memory.
func (m *MemoryKubeClient) ListPods() []PodManifest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]PodManifest, 0, len(m.Pods))
	for _, pod := range m.Pods {
		res = append(res, pod)
	}
	return res
}

// RESTKubeClientConfig holds configuration options for initializing RESTKubeClient.
type RESTKubeClientConfig struct {
	APIServerURL          string
	Namespace             string
	BearerToken           string
	InsecureTLS           bool
	WaitForRunning        bool
	PollInterval          time.Duration
	PollTimeout           time.Duration
	ClientCertificateData []byte
	ClientKeyData         []byte
	CACertificateData     []byte
	HTTPClient            *http.Client
}

// RESTKubeClientOption configures a RESTKubeClient.
type RESTKubeClientOption func(*RESTKubeClient)

func WithAPIServerURL(baseURL string) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		c.baseURL = strings.TrimRight(baseURL, "/")
	}
}

func WithKubeClientNamespace(ns string) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		if ns != "" {
			c.defaultNamespace = ns
		}
	}
}

func WithBearerToken(token string) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		c.token = token
	}
}

func WithInsecureTLS(insecure bool) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		if c.httpClient != nil {
			if transport, ok := c.httpClient.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
				transport.TLSClientConfig.InsecureSkipVerify = insecure
			} else if insecure {
				c.httpClient.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}
		}
	}
}

func WithWaitForRunning(wait bool) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		c.waitForRunning = wait
	}
}

func WithHTTPClient(client *http.Client) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		if client != nil {
			c.httpClient = client
		}
	}
}

func WithPollInterval(d time.Duration) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		if d > 0 {
			c.pollInterval = d
		}
	}
}

func WithPollTimeout(d time.Duration) RESTKubeClientOption {
	return func(c *RESTKubeClient) {
		if d > 0 {
			c.pollTimeout = d
		}
	}
}

// RESTKubeClient communicates with standard Kubernetes v1 JSON REST endpoints.
type RESTKubeClient struct {
	baseURL          string
	defaultNamespace string
	token            string
	httpClient       *http.Client
	waitForRunning   bool
	pollInterval     time.Duration
	pollTimeout      time.Duration
}

// NewRESTKubeClient creates a new RESTKubeClient targeting a Kubernetes APIServer.
func NewRESTKubeClient(baseURL, defaultNamespace, token string, client *http.Client, opts ...RESTKubeClientOption) *RESTKubeClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if defaultNamespace == "" {
		defaultNamespace = "cubepod-workloads"
	}
	c := &RESTKubeClient{
		baseURL:          strings.TrimRight(baseURL, "/"),
		defaultNamespace: defaultNamespace,
		token:            token,
		httpClient:       client,
		pollInterval:     500 * time.Millisecond,
		pollTimeout:      60 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewRESTKubeClientWithConfig creates a RESTKubeClient with full configuration including mTLS.
func NewRESTKubeClientWithConfig(cfg RESTKubeClientConfig) (*RESTKubeClient, error) {
	client := cfg.HTTPClient
	if client == nil {
		tlsConfig, err := buildTLSConfigFromKubeconfig(&KubeconfigInfo{
			InsecureSkipTLSVerify: cfg.InsecureTLS,
			ClientCertificateData: cfg.ClientCertificateData,
			ClientKeyData:         cfg.ClientKeyData,
			CACertificateData:     cfg.CACertificateData,
		})
		if err != nil {
			return nil, err
		}

		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
			Timeout: 30 * time.Second,
		}
	}

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = 60 * time.Second
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = "cubepod-workloads"
	}

	return &RESTKubeClient{
		baseURL:          strings.TrimRight(cfg.APIServerURL, "/"),
		defaultNamespace: ns,
		token:            cfg.BearerToken,
		httpClient:       client,
		waitForRunning:   cfg.WaitForRunning,
		pollInterval:     pollInterval,
		pollTimeout:      pollTimeout,
	}, nil
}

func (c *RESTKubeClient) pvcNamespace(pvc BlockPVCManifest) string {
	if pvc.Namespace != "" {
		return pvc.Namespace
	}
	return c.defaultNamespace
}

func (c *RESTKubeClient) podNamespace(pod PodManifest) string {
	if pod.Namespace != "" {
		return pod.Namespace
	}
	return c.defaultNamespace
}

func (c *RESTKubeClient) resolveNamespace(ns string) string {
	if ns != "" {
		return ns
	}
	return c.defaultNamespace
}

// doRequest performs an HTTP request against the APIServer with standard headers and token authentication.
func (c *RESTKubeClient) doRequest(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.httpClient.Do(req)
}

// EnsurePVC applies a PersistentVolumeClaim with volumeMode: Block via POST.
// If the APIServer returns 409 Conflict (AlreadyExists), it is treated as idempotent success.
func (c *RESTKubeClient) EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error {
	ns := c.pvcNamespace(pvc)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims", c.baseURL, ns)

	v1PVC := toCoreV1PVC(pvc, ns)
	payload, err := json.Marshal(v1PVC)
	if err != nil {
		return fmt.Errorf("failed to marshal PVC JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("EnsurePVC request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Idempotent success when PVC already exists.
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("EnsurePVC failed: status %d (%s)", resp.StatusCode, string(respBody))
	}
	return nil
}

// DeletePVC deletes a PersistentVolumeClaim by namespace and name.
func (c *RESTKubeClient) DeletePVC(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("DeletePVC request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("DeletePVC failed: status %d (%s)", resp.StatusCode, string(respBody))
}

// EnsurePod applies a Kata MicroVM Pod via POST.
// 409 Conflict is treated as idempotent success.
func (c *RESTKubeClient) EnsurePod(ctx context.Context, pod PodManifest) (string, error) {
	ns := c.podNamespace(pod)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods", c.baseURL, ns)

	v1Pod := toCoreV1Pod(pod, ns)
	payload, err := json.Marshal(v1Pod)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Pod JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("EnsurePod request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Idempotent success: fetch existing Pod IP if possible.
		if c.waitForRunning {
			return c.waitForPodRunning(ctx, ns, pod.Name)
		}
		return c.getPodIP(ctx, ns, pod.Name)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("EnsurePod failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	if c.waitForRunning {
		return c.waitForPodRunning(ctx, ns, pod.Name)
	}

	var podResp struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&podResp)
	return podResp.Status.PodIP, nil
}

func (c *RESTKubeClient) waitForPodRunning(ctx context.Context, namespace, name string) (string, error) {
	interval := c.pollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	timeout := c.pollTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		ip, phase, err := c.getPodStatus(pollCtx, namespace, name)
		if err == nil && phase == "Running" && ip != "" {
			return ip, nil
		}
		if phase == "Failed" {
			return "", fmt.Errorf("pod %s/%s entered Failed phase", namespace, name)
		}

		select {
		case <-pollCtx.Done():
			if ip != "" {
				return ip, nil
			}
			return "", fmt.Errorf("timed out waiting for pod %s/%s to reach Running phase (last phase=%q, err=%v)", namespace, name, phase, err)
		case <-ticker.C:
		}
	}
}

func (c *RESTKubeClient) getPodStatus(ctx context.Context, namespace, name string) (string, string, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, namespace, name)
	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("getPodStatus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("getPodStatus failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	var podResp struct {
		Status struct {
			Phase string `json:"phase"`
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&podResp); err != nil {
		return "", "", fmt.Errorf("failed to decode pod status: %w", err)
	}
	return podResp.Status.PodIP, podResp.Status.Phase, nil
}

func (c *RESTKubeClient) getPodIP(ctx context.Context, namespace, name string) (string, error) {
	ip, _, err := c.getPodStatus(ctx, namespace, name)
	if err != nil {
		return "", nil
	}
	return ip, nil
}

// SetWaitForRunning enables or disables waiting for the Pod to reach Running phase.
func (c *RESTKubeClient) SetWaitForRunning(wait bool) {
	c.waitForRunning = wait
}

// WaitForRunning returns whether waiting for Running phase is enabled.
func (c *RESTKubeClient) WaitForRunning() bool {
	return c.waitForRunning
}

// GetPod retrieves a Pod manifest as a generic map.
func (c *RESTKubeClient) GetPod(ctx context.Context, namespace, name string) (map[string]any, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, ns, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetPod failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res, nil
}

// GetPVC retrieves a PersistentVolumeClaim manifest as a generic map.
func (c *RESTKubeClient) GetPVC(ctx context.Context, namespace, name string) (map[string]any, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.baseURL, ns, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetPVC failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res, nil
}

// DeletePod deletes a Pod by namespace and name.
func (c *RESTKubeClient) DeletePod(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s?gracePeriodSeconds=0", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("DeletePod request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted &&
		resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DeletePod failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	if c.waitForRunning {
		pollTimeout := 15 * time.Second
		pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()
		ticker := time.NewTicker(c.pollInterval)
		defer ticker.Stop()
		for {
			_, _, err := c.getPodStatus(pollCtx, ns, name)
			if err != nil {
				// Pod is gone
				break
			}
			select {
			case <-pollCtx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
	return nil
}
