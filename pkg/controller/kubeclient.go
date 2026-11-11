package controller

import (
	"bytes"
	"context"
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

// RESTKubeClient communicates with standard Kubernetes v1 JSON REST endpoints.
type RESTKubeClient struct {
	baseURL          string
	defaultNamespace string
	token            string
	httpClient       *http.Client
}

// NewRESTKubeClient creates a new RESTKubeClient targeting a Kubernetes APIServer.
func NewRESTKubeClient(baseURL, defaultNamespace, token string, client *http.Client) *RESTKubeClient {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if defaultNamespace == "" {
		defaultNamespace = "cubepod-workloads"
	}
	return &RESTKubeClient{
		baseURL:          strings.TrimRight(baseURL, "/"),
		defaultNamespace: defaultNamespace,
		token:            token,
		httpClient:       client,
	}
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
		"runtimeClassName": pod.RuntimeClassName,
		"initContainers":   initContainers,
		"containers":       containers,
		"volumes":          volumes,
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
		return c.getPodIP(ctx, ns, pod.Name)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("EnsurePod failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	var podResp struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&podResp)
	return podResp.Status.PodIP, nil
}

func (c *RESTKubeClient) getPodIP(ctx context.Context, namespace, name string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var podResp struct {
			Status struct {
				PodIP string `json:"podIP"`
			} `json:"status"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&podResp)
		return podResp.Status.PodIP, nil
	}
	return "", nil
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
