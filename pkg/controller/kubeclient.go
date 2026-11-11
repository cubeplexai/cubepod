package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// KubeconfigInfo holds parsed Kubernetes authentication and endpoint details.
type KubeconfigInfo struct {
	Server                string
	Namespace             string
	Token                 string
	ClientCertificateData []byte
	ClientKeyData         []byte
	CACertificateData     []byte
	InsecureSkipTLSVerify bool
}

// ParseKubeconfig parses kubeconfig YAML bytes into KubeconfigInfo using standard library line parsing and regexes.
func ParseKubeconfig(content []byte) (*KubeconfigInfo, error) {
	info := &KubeconfigInfo{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if cIdx := strings.Index(val, " #"); cIdx != -1 {
			val = strings.TrimSpace(val[:cIdx])
		}
		val = strings.Trim(val, `"'`)

		switch key {
		case "server":
			if info.Server == "" {
				info.Server = val
			}
		case "namespace":
			if info.Namespace == "" {
				info.Namespace = val
			}
		case "token":
			if info.Token == "" {
				info.Token = val
			}
		case "insecure-skip-tls-verify":
			info.InsecureSkipTLSVerify = strings.EqualFold(val, "true")
		case "certificate-authority-data":
			if len(info.CACertificateData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.CACertificateData = data
				}
			}
		case "certificate-authority":
			if len(info.CACertificateData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.CACertificateData = data
				}
			}
		case "client-certificate-data":
			if len(info.ClientCertificateData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.ClientCertificateData = data
				}
			}
		case "client-certificate":
			if len(info.ClientCertificateData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.ClientCertificateData = data
				}
			}
		case "client-key-data":
			if len(info.ClientKeyData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.ClientKeyData = data
				}
			}
		case "client-key":
			if len(info.ClientKeyData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.ClientKeyData = data
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan kubeconfig: %w", err)
	}
	return info, nil
}

// LoadKubeconfig reads and parses a kubeconfig file from the specified path,
// or defaults to $KUBECONFIG or ~/.kube/config.
func LoadKubeconfig(path string) (*KubeconfigInfo, error) {
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("unable to determine home directory: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	} else if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read kubeconfig at %s: %w", path, err)
	}
	return ParseKubeconfig(data)
}

// LoadInClusterConfig reads Kubernetes credentials provided by in-cluster service account secrets.
func LoadInClusterConfig() (*KubeconfigInfo, error) {
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("in-cluster token not found: %w", err)
	}

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host = "kubernetes.default.svc"
	}
	if port == "" {
		port = "443"
	}
	serverURL := fmt.Sprintf("https://%s:%s", host, port)

	info := &KubeconfigInfo{
		Server: serverURL,
		Token:  strings.TrimSpace(string(tokenBytes)),
	}

	if nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		info.Namespace = strings.TrimSpace(string(nsBytes))
	}
	if caBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
		info.CACertificateData = caBytes
	}

	return info, nil
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
		tlsConfig := &tls.Config{
			InsecureSkipVerify: cfg.InsecureTLS,
		}
		if len(cfg.ClientCertificateData) > 0 && len(cfg.ClientKeyData) > 0 {
			cert, err := tls.X509KeyPair(cfg.ClientCertificateData, cfg.ClientKeyData)
			if err != nil {
				return nil, fmt.Errorf("failed to parse client certificate/key: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
		if len(cfg.CACertificateData) > 0 && !cfg.InsecureTLS {
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(cfg.CACertificateData) {
				return nil, fmt.Errorf("failed to append CA certificate to pool")
			}
			tlsConfig.RootCAs = caPool
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

// NewRESTKubeClientFromEnv initializes a RESTKubeClient by checking $KUBECONFIG, ~/.kube/config,
// or in-cluster ServiceAccount credentials.
func NewRESTKubeClientFromEnv(opts ...RESTKubeClientOption) (*RESTKubeClient, error) {
	info, err := LoadKubeconfig("")
	if err == nil && info.Server != "" {
		cfg := RESTKubeClientConfig{
			APIServerURL:          info.Server,
			Namespace:             info.Namespace,
			BearerToken:           info.Token,
			InsecureTLS:           info.InsecureSkipTLSVerify,
			ClientCertificateData: info.ClientCertificateData,
			ClientKeyData:         info.ClientKeyData,
			CACertificateData:     info.CACertificateData,
		}
		c, err := NewRESTKubeClientWithConfig(cfg)
		if err == nil {
			for _, opt := range opts {
				opt(c)
			}
			return c, nil
		}
	}

	inClusterInfo, inErr := LoadInClusterConfig()
	if inErr == nil && inClusterInfo.Server != "" {
		cfg := RESTKubeClientConfig{
			APIServerURL:      inClusterInfo.Server,
			Namespace:         inClusterInfo.Namespace,
			BearerToken:       inClusterInfo.Token,
			CACertificateData: inClusterInfo.CACertificateData,
		}
		c, err := NewRESTKubeClientWithConfig(cfg)
		if err == nil {
			for _, opt := range opts {
				opt(c)
			}
			return c, nil
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to initialize KubeClient from env: kubeconfig error (%w), in-cluster error (%v)", err, inErr)
	}
	return nil, fmt.Errorf("no valid Kubernetes configuration found in environment")
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

// EnsurePVC applies a PersistentVolumeClaim with volumeMode: Block via POST.
// If the APIServer returns 409 Conflict (AlreadyExists), it is treated as idempotent success.
func (c *RESTKubeClient) EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error {
	ns := c.pvcNamespace(pvc)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims", c.baseURL, ns)

	spec := map[string]any{
		"accessModes": pvc.AccessModes,
		"volumeMode":  pvc.VolumeMode,
		"resources": map[string]any{
			"requests": map[string]any{
				"storage": pvc.Size,
			},
		},
	}
	if pvc.StorageClassName != "" {
		spec["storageClassName"] = pvc.StorageClassName
	}
	if pvc.DataSourcePVC != "" {
		spec["dataSource"] = map[string]any{
			"kind": "PersistentVolumeClaim",
			"name": pvc.DataSourcePVC,
		}
	}

	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      pvc.Name,
			"namespace": ns,
			"labels":    pvc.Labels,
		},
		"spec": spec,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal PVC JSON: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create EnsurePVC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create DeletePVC request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
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

	var volumes []map[string]any
	for _, v := range pod.Volumes {
		vol := map[string]any{"name": v.Name}
		if v.EmptyDir {
			vol["emptyDir"] = map[string]any{}
		}
		if v.PersistentVolumeClaim != "" {
			vol["persistentVolumeClaim"] = map[string]any{"claimName": v.PersistentVolumeClaim}
		}
		if v.SecretName != "" {
			vol["secret"] = map[string]any{"secretName": v.SecretName}
		}
		volumes = append(volumes, vol)
	}

	var initContainers []map[string]any
	for _, ic := range pod.InitContainers {
		cObj := map[string]any{
			"name":  ic.Name,
			"image": ic.Image,
		}
		if len(ic.Command) > 0 {
			cObj["command"] = ic.Command
		}
		if len(ic.Args) > 0 {
			cObj["args"] = ic.Args
		}
		var mounts []map[string]any
		for _, vm := range ic.VolumeMounts {
			mounts = append(mounts, map[string]any{
				"name":      vm.Name,
				"mountPath": vm.MountPath,
				"readOnly":  vm.ReadOnly,
			})
		}
		if len(mounts) > 0 {
			cObj["volumeMounts"] = mounts
		}
		initContainers = append(initContainers, cObj)
	}

	var containers []map[string]any
	for _, cn := range pod.Containers {
		cObj := map[string]any{
			"name":  cn.Name,
			"image": cn.Image,
		}
		if len(cn.Command) > 0 {
			cObj["command"] = cn.Command
		}
		if len(cn.Args) > 0 {
			cObj["args"] = cn.Args
		}
		var mounts []map[string]any
		for _, vm := range cn.VolumeMounts {
			mounts = append(mounts, map[string]any{
				"name":      vm.Name,
				"mountPath": vm.MountPath,
				"readOnly":  vm.ReadOnly,
			})
		}
		if len(mounts) > 0 {
			cObj["volumeMounts"] = mounts
		}
		var devices []map[string]any
		for _, vd := range cn.VolumeDevices {
			devices = append(devices, map[string]any{
				"name":       vd.Name,
				"devicePath": vd.DevicePath,
			})
		}
		if len(devices) > 0 {
			cObj["volumeDevices"] = devices
		}

		resources := map[string]any{}
		requests := map[string]any{}
		limits := map[string]any{}
		if cn.Resources.CPURequest != "" {
			requests["cpu"] = cn.Resources.CPURequest
		}
		if cn.Resources.MemoryRequest != "" {
			requests["memory"] = cn.Resources.MemoryRequest
		}
		if cn.Resources.CPULimit != "" {
			limits["cpu"] = cn.Resources.CPULimit
		}
		if cn.Resources.MemoryLimit != "" {
			limits["memory"] = cn.Resources.MemoryLimit
		}
		if len(requests) > 0 {
			resources["requests"] = requests
		}
		if len(limits) > 0 {
			resources["limits"] = limits
		}
		if len(resources) > 0 {
			cObj["resources"] = resources
		}

		var envVars []map[string]any
		for _, ev := range cn.Env {
			envVars = append(envVars, map[string]any{
				"name":  ev.Name,
				"value": ev.Value,
			})
		}
		if len(envVars) > 0 {
			cObj["env"] = envVars
		}
		containers = append(containers, cObj)
	}

	podSpec := map[string]any{
		"initContainers": initContainers,
		"containers":     containers,
		"volumes":        volumes,
	}
	if pod.RuntimeClassName != "" {
		podSpec["runtimeClassName"] = pod.RuntimeClassName
	}

	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      pod.Name,
			"namespace": ns,
			"labels":    pod.Labels,
		},
		"spec": podSpec,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Pod JSON: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed to create EnsurePod request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to create getPodStatus request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
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

// DeletePod deletes a Pod by namespace and name.
func (c *RESTKubeClient) DeletePod(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, ns, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create DeletePod request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DeletePod request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("DeletePod failed: status %d (%s)", resp.StatusCode, string(respBody))
}
