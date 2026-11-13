package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

var (
	// ErrPodAlreadyExists is returned when attempting to create a pod that already exists (409 Conflict).
	ErrPodAlreadyExists = errors.New("pod already exists")
	// ErrCRDNotInstalled is returned when a CustomResourceDefinition is not installed on the Kubernetes APIServer (404 NotFound).
	ErrCRDNotInstalled = errors.New("CRD not installed (404 NotFound)")
	// ErrCASConflict is returned when a conditional CAS claim fails because the pod was already claimed or precondition failed.
	ErrCASConflict = errors.New("CAS conflict: pod already claimed or precondition failed")
)

// KubeClient abstracts Kubernetes resource operations for PodBox workloads.
type KubeClient interface {
	EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error
	DeletePVC(ctx context.Context, namespace, name string) error
	EnsurePod(ctx context.Context, pod PodManifest) (podIP string, err error)
	DeletePod(ctx context.Context, namespace, name string) error
	ListPods(ctx context.Context, namespace string, labelSelector string) ([]PodManifest, error)
	PatchPodLabels(ctx context.Context, namespace, name string, labels map[string]string) error
	ClaimPod(ctx context.Context, namespace, name, boxName string) error
	UpsertPodBox(ctx context.Context, box v1alpha1.PodBox) error
	GetPodBox(ctx context.Context, namespace, name string) (*v1alpha1.PodBox, error)
	ListPodBoxes(ctx context.Context, namespace string) ([]v1alpha1.PodBox, error)
	DeletePodBox(ctx context.Context, namespace, name string) error
	UpsertPodWarmPool(ctx context.Context, pool v1alpha1.PodWarmPool) error
	UpdatePodWarmPoolStatus(ctx context.Context, namespace, name string, status v1alpha1.PodWarmPoolStatus) error
	GetPodWarmPool(ctx context.Context, namespace, name string) (*v1alpha1.PodWarmPool, error)
	ListPodWarmPools(ctx context.Context, namespace string) ([]v1alpha1.PodWarmPool, error)
	DeletePodWarmPool(ctx context.Context, namespace, name string) error

	ListPodTemplates(ctx context.Context, namespace string) ([]v1alpha1.PodTemplate, error)
	GetPodTemplate(ctx context.Context, namespace, name string) (*v1alpha1.PodTemplate, error)
	CreatePodTemplate(ctx context.Context, template v1alpha1.PodTemplate) error
	UpdatePodTemplate(ctx context.Context, template v1alpha1.PodTemplate) error
	UpdatePodTemplateStatus(ctx context.Context, template v1alpha1.PodTemplate) error
	DeletePodTemplate(ctx context.Context, namespace, name string) error

	GetPodPhase(ctx context.Context, namespace, name string) (string, error)
	CreateSecret(ctx context.Context, secret KubeSecret) error
	GetSecret(ctx context.Context, namespace, name string) (*KubeSecret, error)
	DeleteSecret(ctx context.Context, namespace, name string) error
}

// MemoryKubeClient is an in-memory, thread-safe implementation of KubeClient
// for unit testing and offline development.
type MemoryKubeClient struct {
	mu        sync.RWMutex
	PVCs      map[string]BlockPVCManifest
	Pods      map[string]PodManifest
	PodPhases map[string]string
	IPs       map[string]string
	Boxes     map[string]v1alpha1.PodBox
	WarmPools map[string]v1alpha1.PodWarmPool
	Templates map[string]v1alpha1.PodTemplate
	Secrets   map[string]KubeSecret
}

// NewMemoryKubeClient creates a new in-memory KubeClient.
func NewMemoryKubeClient() *MemoryKubeClient {
	m := &MemoryKubeClient{
		PVCs:      make(map[string]BlockPVCManifest),
		Pods:      make(map[string]PodManifest),
		PodPhases: make(map[string]string),
		IPs:       make(map[string]string),
		Boxes:     make(map[string]v1alpha1.PodBox),
		WarmPools: make(map[string]v1alpha1.PodWarmPool),
		Templates: make(map[string]v1alpha1.PodTemplate),
		Secrets:   make(map[string]KubeSecret),
	}
	if stateDir := os.Getenv("CUBEPOD_STATE"); stateDir != "" {
		m.LoadLocalTemplates(stateDir)
	}
	return m
}

// LoadLocalTemplates loads persisted PodTemplate JSON files from <stateDir>/templates/*.json.
func (m *MemoryKubeClient) LoadLocalTemplates(stateDir string) {
	if stateDir == "" {
		return
	}
	dir := filepath.Join(stateDir, "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Templates == nil {
		m.Templates = make(map[string]v1alpha1.PodTemplate)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var tpl v1alpha1.PodTemplate
		if err := json.Unmarshal(data, &tpl); err != nil || tpl.Metadata.Name == "" {
			continue
		}
		ns := tpl.Metadata.Namespace
		if ns == "" {
			ns = "default"
		}
		key := templateKey(ns, tpl.Metadata.Name)
		if _, exists := m.Templates[key]; !exists {
			m.Templates[key] = tpl
		}
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

func boxKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return fmt.Sprintf("%s/%s", namespace, name)
}

func templateKey(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return fmt.Sprintf("%s/%s", namespace, name)
}

func persistLocalTemplate(template v1alpha1.PodTemplate) {
	stateDir := os.Getenv("CUBEPOD_STATE")
	if stateDir == "" {
		return
	}
	tplID := template.Status.TemplateID
	if tplID == "" {
		tplID = template.Metadata.Name
	}
	if tplID == "" {
		return
	}
	dir := filepath.Join(stateDir, "templates")
	_ = os.MkdirAll(dir, 0755)
	filePath := filepath.Join(dir, tplID+".json")
	data, err := json.MarshalIndent(template, "", "  ")
	if err == nil {
		_ = os.WriteFile(filePath, data, 0644)
	}
}

func removeLocalTemplate(template v1alpha1.PodTemplate) {
	stateDir := os.Getenv("CUBEPOD_STATE")
	if stateDir == "" {
		return
	}
	tplID := template.Status.TemplateID
	if tplID == "" {
		tplID = template.Metadata.Name
	}
	if tplID == "" {
		return
	}
	filePath := filepath.Join(stateDir, "templates", tplID+".json")
	_ = os.Remove(filePath)
}

// CreatePodTemplate stores a PodTemplate in memory.
func (m *MemoryKubeClient) CreatePodTemplate(_ context.Context, template v1alpha1.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := template.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	key := templateKey(ns, template.Metadata.Name)
	if _, exists := m.Templates[key]; exists {
		return fmt.Errorf("podtemplate %s already exists: status 409", key)
	}
	m.Templates[key] = template
	persistLocalTemplate(template)
	return nil
}

// GetPodTemplate retrieves a stored PodTemplate by namespace and name.
func (m *MemoryKubeClient) GetPodTemplate(_ context.Context, namespace, name string) (*v1alpha1.PodTemplate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	tpl, ok := m.Templates[templateKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("podtemplate %s/%s not found", namespace, name)
	}
	cp := tpl
	return &cp, nil
}

// ListPodTemplates returns all PodTemplate instances matching the namespace (or all if namespace is empty).
func (m *MemoryKubeClient) ListPodTemplates(_ context.Context, namespace string) ([]v1alpha1.PodTemplate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]v1alpha1.PodTemplate, 0, len(m.Templates))
	for _, tpl := range m.Templates {
		tplNs := tpl.Metadata.Namespace
		if tplNs == "" {
			tplNs = "default"
		}
		if namespace == "" || tplNs == namespace {
			res = append(res, tpl)
		}
	}
	return res, nil
}

// UpdatePodTemplate updates the spec and metadata of an existing PodTemplate in memory.
func (m *MemoryKubeClient) UpdatePodTemplate(_ context.Context, template v1alpha1.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := template.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	key := templateKey(ns, template.Metadata.Name)
	existing, ok := m.Templates[key]
	if !ok {
		return fmt.Errorf("podtemplate %s/%s not found", ns, template.Metadata.Name)
	}
	existing.Spec = template.Spec
	existing.Metadata = template.Metadata
	m.Templates[key] = existing
	persistLocalTemplate(existing)
	return nil
}

// UpdatePodTemplateStatus updates the status only of an existing PodTemplate in memory.
func (m *MemoryKubeClient) UpdatePodTemplateStatus(_ context.Context, template v1alpha1.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := template.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	key := templateKey(ns, template.Metadata.Name)
	existing, ok := m.Templates[key]
	if !ok {
		return fmt.Errorf("podtemplate %s/%s not found", ns, template.Metadata.Name)
	}
	existing.Status = template.Status
	m.Templates[key] = existing
	persistLocalTemplate(existing)
	return nil
}

// DeletePodTemplate removes a PodTemplate from memory.
func (m *MemoryKubeClient) DeletePodTemplate(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	key := templateKey(namespace, name)
	if tpl, ok := m.Templates[key]; ok {
		removeLocalTemplate(tpl)
		delete(m.Templates, key)
	}
	return nil
}

// UpsertPodBox records or updates the PodBox in memory.
func (m *MemoryKubeClient) UpsertPodBox(_ context.Context, box v1alpha1.PodBox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := box.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	m.Boxes[boxKey(ns, box.Metadata.Name)] = box
	return nil
}

// GetPodBox retrieves a stored PodBox by namespace and name.
func (m *MemoryKubeClient) GetPodBox(_ context.Context, namespace, name string) (*v1alpha1.PodBox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	box, ok := m.Boxes[boxKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("podbox %s/%s not found", namespace, name)
	}
	cp := box
	return &cp, nil
}

// ListPodBoxes returns all PodBox instances matching the given namespace (or all if namespace is empty).
func (m *MemoryKubeClient) ListPodBoxes(_ context.Context, namespace string) ([]v1alpha1.PodBox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]v1alpha1.PodBox, 0, len(m.Boxes))
	for _, box := range m.Boxes {
		boxNs := box.Metadata.Namespace
		if boxNs == "" {
			boxNs = "default"
		}
		if namespace == "" || boxNs == namespace {
			res = append(res, box)
		}
	}
	return res, nil
}

// DeletePodBox removes the PodBox from memory.
func (m *MemoryKubeClient) DeletePodBox(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	delete(m.Boxes, boxKey(namespace, name))
	return nil
}

// UpsertPodWarmPool stores or updates a PodWarmPool in memory.
func (m *MemoryKubeClient) UpsertPodWarmPool(_ context.Context, pool v1alpha1.PodWarmPool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := pool.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	m.WarmPools[boxKey(ns, pool.Metadata.Name)] = pool
	return nil
}

// UpdatePodWarmPoolStatus updates only the status field of a stored PodWarmPool in memory.
func (m *MemoryKubeClient) UpdatePodWarmPoolStatus(_ context.Context, namespace, name string, status v1alpha1.PodWarmPoolStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	key := boxKey(namespace, name)
	pool, ok := m.WarmPools[key]
	if !ok {
		return fmt.Errorf("podwarmpool %s/%s not found", namespace, name)
	}
	pool.Status = status
	m.WarmPools[key] = pool
	return nil
}

// GetPodWarmPool retrieves a PodWarmPool from memory by namespace and name.
func (m *MemoryKubeClient) GetPodWarmPool(_ context.Context, namespace, name string) (*v1alpha1.PodWarmPool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	pool, ok := m.WarmPools[boxKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("podwarmpool %s/%s not found", namespace, name)
	}
	cp := pool
	return &cp, nil
}

// ListPodWarmPools returns all PodWarmPool instances matching the given namespace (or all if empty).
func (m *MemoryKubeClient) ListPodWarmPools(_ context.Context, namespace string) ([]v1alpha1.PodWarmPool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]v1alpha1.PodWarmPool, 0, len(m.WarmPools))
	for _, pool := range m.WarmPools {
		poolNs := pool.Metadata.Namespace
		if poolNs == "" {
			poolNs = "default"
		}
		if namespace == "" || poolNs == namespace {
			res = append(res, pool)
		}
	}
	return res, nil
}

// DeletePodWarmPool removes the PodWarmPool from memory.
func (m *MemoryKubeClient) DeletePodWarmPool(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	delete(m.WarmPools, boxKey(namespace, name))
	return nil
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
// If the Pod already exists, it preserves existing labels and returns ErrPodAlreadyExists.
func (m *MemoryKubeClient) EnsurePod(_ context.Context, pod PodManifest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := podKey(pod.Namespace, pod.Name)
	if _, ok := m.Pods[key]; ok {
		ip := m.IPs[key]
		if ip == "" {
			ip = "10.244.0.42"
			m.IPs[key] = ip
		}
		return ip, fmt.Errorf("%w: pod %s/%s already exists (409 Conflict)", ErrPodAlreadyExists, pod.Namespace, pod.Name)
	}
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
	delete(m.PodPhases, key)
	return nil
}

// GetPodPhase returns the phase for a Pod in memory (defaults to "Running" when present).
func (m *MemoryKubeClient) GetPodPhase(_ context.Context, namespace, name string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := podKey(namespace, name)
	if _, ok := m.Pods[key]; !ok {
		return "", fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	if phase, ok := m.PodPhases[key]; ok && phase != "" {
		return phase, nil
	}
	return "Running", nil
}

// CreateSecret stores a KubeSecret in memory.
func (m *MemoryKubeClient) CreateSecret(_ context.Context, secret KubeSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Secrets == nil {
		m.Secrets = make(map[string]KubeSecret)
	}
	ns := secret.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	secret.Metadata.Namespace = ns
	if secret.APIVersion == "" {
		secret.APIVersion = "v1"
	}
	if secret.Kind == "" {
		secret.Kind = "Secret"
	}
	key := podKey(ns, secret.Metadata.Name)
	if _, exists := m.Secrets[key]; exists {
		return fmt.Errorf("secret %s/%s already exists: status 409", ns, secret.Metadata.Name)
	}
	cp := secret
	cp.Data = make(map[string][]byte, len(secret.Data)+len(secret.StringData))
	for k, v := range secret.Data {
		b := make([]byte, len(v))
		copy(b, v)
		cp.Data[k] = b
	}
	for k, v := range secret.StringData {
		cp.Data[k] = []byte(v)
	}
	cp.StringData = nil
	m.Secrets[key] = cp
	return nil
}

// GetSecret retrieves a KubeSecret from memory by namespace and name.
func (m *MemoryKubeClient) GetSecret(_ context.Context, namespace, name string) (*KubeSecret, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	sec, ok := m.Secrets[podKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s not found", namespace, name)
	}
	cp := sec
	return &cp, nil
}

// DeleteSecret removes a KubeSecret from memory.
func (m *MemoryKubeClient) DeleteSecret(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	delete(m.Secrets, podKey(namespace, name))
	return nil
}

// ClaimPod conditionally claims an unclaimed standby pod in memory.
// Returns an error if the pod is already claimed (CAS failure).
func (m *MemoryKubeClient) ClaimPod(_ context.Context, namespace, name, boxName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := podKey(namespace, name)
	pod, ok := m.Pods[key]
	if !ok {
		return fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if pod.Labels["cubepod.io/claimed"] == "true" {
		return fmt.Errorf("%w: pod %s already claimed", ErrCASConflict, name)
	}
	if pod.Labels["cubepod.io/claimed"] != "false" {
		return fmt.Errorf("%w: pod %s cannot be claimed: cubepod.io/claimed is not false (got %q)", ErrCASConflict, name, pod.Labels["cubepod.io/claimed"])
	}
	pod.Labels["cubepod.io/claimed"] = "true"
	pod.Labels["cubepod.io/claimed-by"] = boxName
	m.Pods[key] = pod
	return nil
}

// PatchPodLabels merges the provided labels into the in-memory Pod manifest.
// If claiming a pod (cubepod.io/claimed == "true"), it enforces CAS semantics.
func (m *MemoryKubeClient) PatchPodLabels(ctx context.Context, namespace, name string, labels map[string]string) error {
	if labels["cubepod.io/claimed"] == "true" {
		boxName := labels["cubepod.io/claimed-by"]
		return m.ClaimPod(ctx, namespace, name, boxName)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := podKey(namespace, name)
	pod, ok := m.Pods[key]
	if !ok {
		return fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	for k, v := range labels {
		pod.Labels[k] = v
	}
	m.Pods[key] = pod
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

// ListPods returns Pod manifests matching the optional namespace and labelSelector.
func (m *MemoryKubeClient) ListPods(_ context.Context, namespace string, labelSelector string) ([]PodManifest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var selectors map[string]string
	if labelSelector != "" {
		selectors = make(map[string]string)
		for _, part := range strings.Split(labelSelector, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				selectors[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
			} else {
				selectors[part] = ""
			}
		}
	}

	res := make([]PodManifest, 0, len(m.Pods))
	for _, pod := range m.Pods {
		podNs := pod.Namespace
		if podNs == "" {
			podNs = "default"
		}
		if namespace != "" && podNs != namespace {
			continue
		}

		if len(selectors) > 0 {
			match := true
			for k, v := range selectors {
				if pod.Labels == nil || pod.Labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}

		res = append(res, pod)
	}
	return res, nil
}

// AllPods returns all Pod manifests recorded in memory.
func (m *MemoryKubeClient) AllPods() []PodManifest {
	pods, _ := m.ListPods(context.Background(), "", "")
	return pods
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
// In volumeMode: Block, privileged containers have securityContext.privileged=true
// serialized into the Pod JSON payload so podd-init has CAP_SYS_ADMIN.
// If the APIServer returns 409 Conflict, it returns ErrPodAlreadyExists.
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
		var podIP string
		if c.waitForRunning {
			podIP, _ = c.waitForPodRunning(ctx, ns, pod.Name)
		} else {
			podIP, _ = c.getPodIP(ctx, ns, pod.Name)
		}
		return podIP, fmt.Errorf("%w: %s/%s", ErrPodAlreadyExists, ns, pod.Name)
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
		if err == nil && ((phase == "Running" && ip != "") || phase == "Succeeded") {
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

// GetPodPhase returns the current status.phase of a Pod.
func (c *RESTKubeClient) GetPodPhase(ctx context.Context, namespace, name string) (string, error) {
	ns := c.resolveNamespace(namespace)
	_, phase, err := c.getPodStatus(ctx, ns, name)
	return phase, err
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

// ClaimPod atomically claims a standby pod using RFC 6902 JSON Patch with conditional CAS.
// It sends an HTTP PATCH with Content-Type: application/json-patch+json:
// [
//
//	{"op": "test", "path": "/metadata/labels/cubepod.io~1claimed", "value": "false"},
//	{"op": "replace", "path": "/metadata/labels/cubepod.io~1claimed", "value": "true"},
//	{"op": "add", "path": "/metadata/labels/cubepod.io~1claimed-by", "value": boxName}
//
// ]
// If another replica already claimed the pod, the Kubernetes APIServer rejects the request with 422 or 409.
func (c *RESTKubeClient) ClaimPod(ctx context.Context, namespace, name, boxName string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, ns, name)

	patchOps := []map[string]any{
		{
			"op":    "test",
			"path":  "/metadata/labels/cubepod.io~1claimed",
			"value": "false",
		},
		{
			"op":    "replace",
			"path":  "/metadata/labels/cubepod.io~1claimed",
			"value": "true",
		},
		{
			"op":    "add",
			"path":  "/metadata/labels/cubepod.io~1claimed-by",
			"value": boxName,
		},
	}
	payload, err := json.Marshal(patchOps)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON Patch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ClaimPod request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ClaimPod request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusUnprocessableEntity {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: status %d (%s)", ErrCASConflict, resp.StatusCode, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ClaimPod failed: status %d (%s)", resp.StatusCode, string(respBody))
	}
	return nil
}

// PatchPodLabels patches the labels of a Pod using strategic merge patch,
// or performs conditional CAS claim if cubepod.io/claimed is set to true.
func (c *RESTKubeClient) PatchPodLabels(ctx context.Context, namespace, name string, labels map[string]string) error {
	if labels["cubepod.io/claimed"] == "true" {
		boxName := labels["cubepod.io/claimed-by"]
		return c.ClaimPod(ctx, namespace, name, boxName)
	}

	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, ns, name)

	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": labels,
		},
	}
	payload, err := json.Marshal(patchData)
	if err != nil {
		return fmt.Errorf("failed to marshal Pod patch JSON: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("PatchPodLabels request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PatchPodLabels request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PatchPodLabels failed: status %d (%s)", resp.StatusCode, string(respBody))
	}
	return nil
}

// ListPods queries pods in the specified namespace matching the optional labelSelector.
func (c *RESTKubeClient) ListPods(ctx context.Context, namespace string, labelSelector string) ([]PodManifest, error) {
	ns := c.resolveNamespace(namespace)
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/pods", c.baseURL, ns)
	if labelSelector != "" {
		reqURL += "?labelSelector=" + url.QueryEscape(labelSelector)
	}

	resp, err := c.doRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ListPods request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []PodManifest{}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ListPods failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var list struct {
		Items []coreV1Pod `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode PodList: %w", err)
	}

	res := make([]PodManifest, 0, len(list.Items))
	for _, p := range list.Items {
		itemNs := p.Metadata.Namespace
		if itemNs == "" {
			itemNs = ns
		}
		res = append(res, PodManifest{
			Name:             p.Metadata.Name,
			Namespace:        itemNs,
			Labels:           p.Metadata.Labels,
			RuntimeClassName: p.Spec.RuntimeClassName,
		})
	}
	return res, nil
}
