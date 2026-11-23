package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	// ErrKubeNotFound is returned when a requested Kubernetes object does not exist (404 NotFound).
	ErrKubeNotFound = errors.New("kubernetes object not found")
	// ErrPodNotFound is returned by GetPodPhase when the Pod does not exist (HTTP 404).
	ErrPodNotFound = errors.New("pod not found")
	// ErrKubeAlreadyExists is returned when creating a Kubernetes object whose name is already taken (409 AlreadyExists).
	ErrKubeAlreadyExists = errors.New("kubernetes object already exists")
)

// PodIPHook is invoked the first time waitForPodRunning observes a non-empty
// podIP, and must run before the Pod becomes Ready. A non-nil error fails the wait.
type PodIPHook func(ctx context.Context, namespace, name, podIP string) error

// KubeClient abstracts Kubernetes resource operations for PodBox workloads.
type KubeClient interface {
	EnsurePVC(ctx context.Context, pvc BlockPVCManifest) error
	GetPVCManifest(ctx context.Context, namespace, name string) (*BlockPVCManifest, error)
	ListPVCManifests(ctx context.Context, namespace, labelSelector string) ([]BlockPVCManifest, error)
	PatchPVCLabels(ctx context.Context, namespace, name string, labels map[string]string) error
	UpdatePVCAnnotationsCAS(ctx context.Context, namespace, name, resourceVersion string, annotations map[string]string) (*BlockPVCManifest, error)
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
	UpdatePodTemplateStatusCAS(ctx context.Context, template v1alpha1.PodTemplate) (*v1alpha1.PodTemplate, error)
	DeletePodTemplate(ctx context.Context, namespace, name string) error

	GetPodPhase(ctx context.Context, namespace, name string) (string, error)
	CreateSecret(ctx context.Context, secret KubeSecret) error
	EnsureSecret(ctx context.Context, secret KubeSecret) error
	GetSecret(ctx context.Context, namespace, name string) (*KubeSecret, error)
	ListSecrets(ctx context.Context, namespace string, labelSelector string) ([]KubeSecret, error)
	DeleteSecret(ctx context.Context, namespace, name string) error

	EnsureVolumeSnapshot(ctx context.Context, snap VolumeSnapshot) error
	GetVolumeSnapshot(ctx context.Context, namespace, name string) (*VolumeSnapshot, error)
	WaitVolumeSnapshotReady(ctx context.Context, namespace, name string) error
	DeleteVolumeSnapshot(ctx context.Context, namespace, name string) error

	// UpsertSandboxWebhook creates a SandboxWebhook CR (including initial /status when non-zero)
	// or replaces metadata/spec when it already exists without overwriting /status.
	UpsertSandboxWebhook(ctx context.Context, webhook v1alpha1.SandboxWebhook) error
	// UpdateSandboxWebhookStatus replaces only the /status subresource. When
	// webhook.Metadata.ResourceVersion is set, a stale version yields ErrCASConflict.
	UpdateSandboxWebhookStatus(ctx context.Context, webhook v1alpha1.SandboxWebhook) error
	GetSandboxWebhook(ctx context.Context, namespace, name string) (*v1alpha1.SandboxWebhook, error)
	ListSandboxWebhooks(ctx context.Context, namespace string, labelSelector string) ([]v1alpha1.SandboxWebhook, error)
	DeleteSandboxWebhook(ctx context.Context, namespace, name string) error

	// CreateKubeEvent creates a core/v1 Event. An existing Event with the same name
	// returns an error wrapping ErrKubeAlreadyExists so callers can deduplicate.
	CreateKubeEvent(ctx context.Context, event KubeEvent) error
	ListKubeEvents(ctx context.Context, namespace string, labelSelector string) ([]KubeEvent, error)
}

// MemoryKubeClient is an in-memory, thread-safe implementation of KubeClient
// for unit testing and offline development.
type MemoryKubeClient struct {
	mu              sync.RWMutex
	PVCs            map[string]BlockPVCManifest
	Pods            map[string]PodManifest
	PodPhases       map[string]string
	IPs             map[string]string
	Boxes           map[string]v1alpha1.PodBox
	WarmPools       map[string]v1alpha1.PodWarmPool
	Templates       map[string]v1alpha1.PodTemplate
	Secrets         map[string]KubeSecret
	VolumeSnapshots map[string]VolumeSnapshot
	Webhooks        map[string]v1alpha1.SandboxWebhook
	// Events holds core/v1 Events in insertion order, bounded by MaxMemoryKubeEvents.
	Events []KubeEvent
	// GetPodPhaseErr, if set, is returned by GetPodPhase without inspecting the Pod.
	GetPodPhaseErr error

	rvCounter     int64
	templateRVSeq uint64
}

func (m *MemoryKubeClient) nextTemplateRVLocked() string {
	m.templateRVSeq++
	return strconv.FormatUint(m.templateRVSeq, 10)
}

// MaxMemoryKubeEvents bounds the number of core/v1 Events retained by MemoryKubeClient,
// emulating the APIServer event TTL so long-running local gateways do not leak memory.
const MaxMemoryKubeEvents = 2048

// NewMemoryKubeClient creates a new in-memory KubeClient.
func NewMemoryKubeClient() *MemoryKubeClient {
	m := &MemoryKubeClient{
		PVCs:            make(map[string]BlockPVCManifest),
		Pods:            make(map[string]PodManifest),
		PodPhases:       make(map[string]string),
		IPs:             make(map[string]string),
		Boxes:           make(map[string]v1alpha1.PodBox),
		WarmPools:       make(map[string]v1alpha1.PodWarmPool),
		Templates:       make(map[string]v1alpha1.PodTemplate),
		Secrets:         make(map[string]KubeSecret),
		VolumeSnapshots: make(map[string]VolumeSnapshot),
		Webhooks:        make(map[string]v1alpha1.SandboxWebhook),
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
	template.Metadata.Namespace = ns
	template.Metadata.ResourceVersion = m.nextTemplateRVLocked()
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
	existing.Metadata.Namespace = ns
	existing.Metadata.ResourceVersion = m.nextTemplateRVLocked()
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
	existing.Metadata.ResourceVersion = m.nextTemplateRVLocked()
	m.Templates[key] = existing
	persistLocalTemplate(existing)
	return nil
}

// UpdatePodTemplateStatusCAS atomically updates PodTemplate.Status only if template.Metadata.ResourceVersion matches.
func (m *MemoryKubeClient) UpdatePodTemplateStatusCAS(_ context.Context, template v1alpha1.PodTemplate) (*v1alpha1.PodTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := template.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	key := templateKey(ns, template.Metadata.Name)
	existing, ok := m.Templates[key]
	if !ok {
		return nil, fmt.Errorf("podtemplate %s/%s not found", ns, template.Metadata.Name)
	}
	if existing.Metadata.ResourceVersion != "" && template.Metadata.ResourceVersion != existing.Metadata.ResourceVersion {
		return nil, fmt.Errorf("%w: podtemplate %s/%s resourceVersion mismatch (got %q, current %q)", ErrCASConflict, ns, template.Metadata.Name, template.Metadata.ResourceVersion, existing.Metadata.ResourceVersion)
	}
	existing.Status = template.Status
	existing.Metadata.ResourceVersion = m.nextTemplateRVLocked()
	m.Templates[key] = existing
	persistLocalTemplate(existing)
	cp := existing
	return &cp, nil
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

// EnsurePVC records the PVC manifest in memory (idempotent if already present, matching K8s 409 semantics).
func (m *MemoryKubeClient) EnsurePVC(_ context.Context, pvc BlockPVCManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pvcKey(pvc.Namespace, pvc.Name)
	if _, exists := m.PVCs[key]; exists {
		return nil
	}
	cp := cloneBlockPVCManifest(pvc)
	if cp.Namespace == "" {
		cp.Namespace = "default"
	}
	m.rvCounter++
	cp.ResourceVersion = strconv.FormatInt(m.rvCounter, 10)
	m.PVCs[key] = cp
	return nil
}

// GetPVCManifest retrieves a typed PVC manifest by namespace and name.
func (m *MemoryKubeClient) GetPVCManifest(_ context.Context, namespace, name string) (*BlockPVCManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pvc, ok := m.PVCs[pvcKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, namespace, name)
	}
	cp := cloneBlockPVCManifest(pvc)
	return &cp, nil
}

// ListPVCManifests returns PVCs in namespace matching an optional comma-separated k=v label selector.
func (m *MemoryKubeClient) ListPVCManifests(_ context.Context, namespace, labelSelector string) ([]BlockPVCManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := namespace
	if ns == "" {
		ns = "default"
	}
	selector := parseLabelSelector(labelSelector)
	var out []BlockPVCManifest
	for k, pvc := range m.PVCs {
		if !strings.HasPrefix(k, ns+"/") {
			continue
		}
		if !matchesLabelSelectors(pvc.Labels, selector) {
			continue
		}
		out = append(out, cloneBlockPVCManifest(pvc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PatchPVCLabels merges labels onto an existing PVC in memory.
func (m *MemoryKubeClient) PatchPVCLabels(_ context.Context, namespace, name string, labels map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pvcKey(namespace, name)
	pvc, ok := m.PVCs[key]
	if !ok {
		return fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, namespace, name)
	}
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	for k, v := range labels {
		pvc.Labels[k] = v
	}
	m.rvCounter++
	pvc.ResourceVersion = strconv.FormatInt(m.rvCounter, 10)
	m.PVCs[key] = pvc
	return nil
}

// UpdatePVCAnnotationsCAS atomically updates annotations on an existing PVC when resourceVersion matches.
// A nil value or empty string for a key removes that annotation.
func (m *MemoryKubeClient) UpdatePVCAnnotationsCAS(_ context.Context, namespace, name, resourceVersion string, annotations map[string]string) (*BlockPVCManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := pvcKey(namespace, name)
	pvc, ok := m.PVCs[key]
	if !ok {
		return nil, fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, namespace, name)
	}
	if resourceVersion != "" && pvc.ResourceVersion != "" && pvc.ResourceVersion != resourceVersion {
		return nil, fmt.Errorf("%w: pvc %s/%s resourceVersion mismatch (%s != %s)", ErrCASConflict, namespace, name, pvc.ResourceVersion, resourceVersion)
	}
	nextAnns := map[string]string{}
	for k, v := range pvc.Annotations {
		nextAnns[k] = v
	}
	for k, v := range annotations {
		if v == "" {
			delete(nextAnns, k)
		} else {
			nextAnns[k] = v
		}
	}
	pvc.Annotations = nextAnns
	m.rvCounter++
	pvc.ResourceVersion = strconv.FormatInt(m.rvCounter, 10)
	m.PVCs[key] = pvc
	cp := cloneBlockPVCManifest(pvc)
	return &cp, nil
}

func cloneBlockPVCManifest(pvc BlockPVCManifest) BlockPVCManifest {
	cp := pvc
	if pvc.AccessModes != nil {
		cp.AccessModes = append([]string(nil), pvc.AccessModes...)
	}
	if pvc.Labels != nil {
		cp.Labels = make(map[string]string, len(pvc.Labels))
		maps.Copy(cp.Labels, pvc.Labels)
	}
	if pvc.Annotations != nil {
		cp.Annotations = make(map[string]string, len(pvc.Annotations))
		maps.Copy(cp.Annotations, pvc.Annotations)
	}
	return cp
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

// EnsurePodWithHook assigns a pod IP, invokes hook before the in-memory Pod is
// recorded as existing (standing in for Ready), then stores the Pod.
func (m *MemoryKubeClient) EnsurePodWithHook(ctx context.Context, pod PodManifest, hook PodIPHook) (string, error) {
	m.mu.Lock()
	key := podKey(pod.Namespace, pod.Name)
	if existing, ok := m.Pods[key]; ok {
		ip := m.IPs[key]
		if ip == "" {
			ip = "10.244.0.42"
			m.IPs[key] = ip
		}
		_ = existing
		m.mu.Unlock()
		return ip, fmt.Errorf("%w: pod %s/%s already exists (409 Conflict)", ErrPodAlreadyExists, pod.Namespace, pod.Name)
	}
	ip, ok := m.IPs[key]
	if !ok {
		ip = "10.244.0.42"
		m.IPs[key] = ip
	}
	m.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, pod.Namespace, pod.Name, ip); err != nil {
			return ip, err
		}
	}
	return m.EnsurePod(ctx, pod)
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
	if m.GetPodPhaseErr != nil {
		return "", m.GetPodPhaseErr
	}
	if _, ok := m.Pods[key]; !ok {
		return "", fmt.Errorf("pod %s/%s: %w", namespace, name, ErrPodNotFound)
	}
	if phase, ok := m.PodPhases[key]; ok && phase != "" {
		return phase, nil
	}
	return "Running", nil
}

func copyKubeSecret(secret KubeSecret) KubeSecret {
	cp := secret
	if secret.Metadata.Labels != nil {
		cp.Metadata.Labels = make(map[string]string, len(secret.Metadata.Labels))
		for k, v := range secret.Metadata.Labels {
			cp.Metadata.Labels[k] = v
		}
	}
	if secret.Metadata.Annotations != nil {
		cp.Metadata.Annotations = make(map[string]string, len(secret.Metadata.Annotations))
		for k, v := range secret.Metadata.Annotations {
			cp.Metadata.Annotations[k] = v
		}
	}
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
	return cp
}

func parseLabelSelector(labelSelector string) map[string]string {
	if labelSelector == "" {
		return nil
	}
	selectors := make(map[string]string)
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
	return selectors
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
	m.Secrets[key] = copyKubeSecret(secret)
	return nil
}

// EnsureSecret creates or updates a KubeSecret in memory.
func (m *MemoryKubeClient) EnsureSecret(_ context.Context, secret KubeSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Secrets == nil {
		m.Secrets = make(map[string]KubeSecret)
	}
	if secret.Metadata.Name == "" {
		return fmt.Errorf("Secret metadata.name is required")
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
	m.Secrets[podKey(ns, secret.Metadata.Name)] = copyKubeSecret(secret)
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
	cp := copyKubeSecret(sec)
	return &cp, nil
}

// ListSecrets retrieves KubeSecrets matching optional namespace and labelSelector from memory.
func (m *MemoryKubeClient) ListSecrets(_ context.Context, namespace string, labelSelector string) ([]KubeSecret, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	selectors := parseLabelSelector(labelSelector)
	res := make([]KubeSecret, 0, len(m.Secrets))
	for _, sec := range m.Secrets {
		secNs := sec.Metadata.Namespace
		if secNs == "" {
			secNs = "default"
		}
		if namespace != "" && secNs != namespace {
			continue
		}
		if len(selectors) > 0 {
			match := true
			for k, v := range selectors {
				if sec.Metadata.Labels == nil || sec.Metadata.Labels[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		res = append(res, copyKubeSecret(sec))
	}
	return res, nil
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

// EnsureVolumeSnapshot records a VolumeSnapshot in memory (idempotent on existing name).
// If Status.ReadyToUse is nil, it defaults to true so in-memory tests proceed immediately.
func (m *MemoryKubeClient) EnsureVolumeSnapshot(_ context.Context, snap VolumeSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snap.Metadata.Name == "" {
		return fmt.Errorf("VolumeSnapshot metadata.name is required")
	}
	if m.VolumeSnapshots == nil {
		m.VolumeSnapshots = make(map[string]VolumeSnapshot)
	}
	ns := snap.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	snap.Metadata.Namespace = ns
	if snap.APIVersion == "" {
		snap.APIVersion = "snapshot.storage.k8s.io/v1"
	}
	if snap.Kind == "" {
		snap.Kind = "VolumeSnapshot"
	}
	key := podKey(ns, snap.Metadata.Name)
	if _, exists := m.VolumeSnapshots[key]; exists {
		return nil
	}
	if snap.Status.ReadyToUse == nil {
		ready := true
		snap.Status.ReadyToUse = &ready
	}
	m.VolumeSnapshots[key] = snap
	return nil
}

// GetVolumeSnapshot retrieves a VolumeSnapshot from memory by namespace and name.
func (m *MemoryKubeClient) GetVolumeSnapshot(_ context.Context, namespace, name string) (*VolumeSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	snap, ok := m.VolumeSnapshots[podKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("volumesnapshot %s/%s not found", namespace, name)
	}
	cp := snap
	return &cp, nil
}

// WaitVolumeSnapshotReady blocks until the in-memory VolumeSnapshot has ReadyToUse=true or fails.
func (m *MemoryKubeClient) WaitVolumeSnapshotReady(ctx context.Context, namespace, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if namespace == "" {
		namespace = "default"
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap, err := m.GetVolumeSnapshot(ctx, namespace, name)
		if err != nil {
			return err
		}
		if snap.Status.Error != nil && snap.Status.Error.Message != "" {
			return fmt.Errorf("volumesnapshot %s/%s failed: %s", namespace, name, snap.Status.Error.Message)
		}
		if snap.Status.ReadyToUse != nil && *snap.Status.ReadyToUse {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for volumesnapshot %s/%s: %w", namespace, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// DeleteVolumeSnapshot removes a VolumeSnapshot from memory.
func (m *MemoryKubeClient) DeleteVolumeSnapshot(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	delete(m.VolumeSnapshots, podKey(namespace, name))
	return nil
}

func matchesLabelSelectors(labels, selectors map[string]string) bool {
	for k, v := range selectors {
		if labels == nil || labels[k] != v {
			return false
		}
	}
	return true
}

func copySandboxWebhook(wh v1alpha1.SandboxWebhook) v1alpha1.SandboxWebhook {
	cp := wh
	cp.Metadata.Labels = maps.Clone(wh.Metadata.Labels)
	cp.Metadata.Annotations = maps.Clone(wh.Metadata.Annotations)
	cp.Spec.Events = slices.Clone(wh.Spec.Events)
	if wh.Status.LastDeliveryTime != nil {
		t := *wh.Status.LastDeliveryTime
		cp.Status.LastDeliveryTime = &t
	}
	if wh.Status.DeadLetters != nil {
		cp.Status.DeadLetters = make([]v1alpha1.SandboxWebhookDeadLetter, len(wh.Status.DeadLetters))
		for i, dl := range wh.Status.DeadLetters {
			dl.Event = slices.Clone(dl.Event)
			cp.Status.DeadLetters[i] = dl
		}
	}
	return cp
}

func (m *MemoryKubeClient) nextResourceVersionLocked() string {
	m.rvCounter++
	return strconv.FormatInt(m.rvCounter, 10)
}

// UpsertSandboxWebhook creates a SandboxWebhook (with initial status) or replaces
// metadata and spec when it already exists, preserving existing status in memory.
func (m *MemoryKubeClient) UpsertSandboxWebhook(_ context.Context, webhook v1alpha1.SandboxWebhook) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if webhook.Metadata.Name == "" {
		return fmt.Errorf("SandboxWebhook metadata.name is required")
	}
	if m.Webhooks == nil {
		m.Webhooks = make(map[string]v1alpha1.SandboxWebhook)
	}
	ns := webhook.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	webhook.Metadata.Namespace = ns
	if webhook.APIVersion == "" {
		webhook.APIVersion = "cubepod.io/v1alpha1"
	}
	if webhook.Kind == "" {
		webhook.Kind = "SandboxWebhook"
	}
	key := podKey(ns, webhook.Metadata.Name)
	cp := copySandboxWebhook(webhook)
	if existing, exists := m.Webhooks[key]; exists {
		cp.Status = copySandboxWebhook(existing).Status
	}
	cp.Metadata.ResourceVersion = m.nextResourceVersionLocked()
	m.Webhooks[key] = cp
	return nil
}

// UpdateSandboxWebhookStatus replaces the status of an existing in-memory SandboxWebhook.
func (m *MemoryKubeClient) UpdateSandboxWebhookStatus(_ context.Context, webhook v1alpha1.SandboxWebhook) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ns := webhook.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	key := podKey(ns, webhook.Metadata.Name)
	existing, ok := m.Webhooks[key]
	if !ok {
		return fmt.Errorf("sandboxwebhook %s/%s: %w", ns, webhook.Metadata.Name, ErrKubeNotFound)
	}
	if rv := webhook.Metadata.ResourceVersion; rv != "" && rv != existing.Metadata.ResourceVersion {
		return fmt.Errorf("sandboxwebhook %s/%s resourceVersion %s is stale: %w", ns, webhook.Metadata.Name, rv, ErrCASConflict)
	}
	updated := copySandboxWebhook(webhook)
	existing.Status = updated.Status
	existing.Metadata.ResourceVersion = m.nextResourceVersionLocked()
	m.Webhooks[key] = existing
	return nil
}

// GetSandboxWebhook retrieves a SandboxWebhook from memory, returning ErrKubeNotFound when absent.
func (m *MemoryKubeClient) GetSandboxWebhook(_ context.Context, namespace, name string) (*v1alpha1.SandboxWebhook, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if namespace == "" {
		namespace = "default"
	}
	wh, ok := m.Webhooks[podKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("sandboxwebhook %s/%s: %w", namespace, name, ErrKubeNotFound)
	}
	cp := copySandboxWebhook(wh)
	return &cp, nil
}

// ListSandboxWebhooks lists in-memory SandboxWebhooks matching namespace and labelSelector.
func (m *MemoryKubeClient) ListSandboxWebhooks(_ context.Context, namespace string, labelSelector string) ([]v1alpha1.SandboxWebhook, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	selectors := parseLabelSelector(labelSelector)
	out := make([]v1alpha1.SandboxWebhook, 0, len(m.Webhooks))
	for _, wh := range m.Webhooks {
		if namespace != "" && wh.Metadata.Namespace != namespace {
			continue
		}
		if !matchesLabelSelectors(wh.Metadata.Labels, selectors) {
			continue
		}
		out = append(out, copySandboxWebhook(wh))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// DeleteSandboxWebhook removes a SandboxWebhook from memory (idempotent).
func (m *MemoryKubeClient) DeleteSandboxWebhook(_ context.Context, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if namespace == "" {
		namespace = "default"
	}
	delete(m.Webhooks, podKey(namespace, name))
	return nil
}

// CreateKubeEvent appends a core/v1 Event to memory. Duplicate names in the same
// namespace return ErrKubeAlreadyExists, mirroring APIServer 409 semantics.
func (m *MemoryKubeClient) CreateKubeEvent(_ context.Context, event KubeEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Metadata.Name == "" {
		return fmt.Errorf("Event metadata.name is required")
	}
	ns := event.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	event.Metadata.Namespace = ns
	if event.InvolvedObject.Namespace == "" {
		event.InvolvedObject.Namespace = ns
	}
	if event.APIVersion == "" {
		event.APIVersion = "v1"
	}
	if event.Kind == "" {
		event.Kind = "Event"
	}
	for _, existing := range m.Events {
		if existing.Metadata.Namespace == ns && existing.Metadata.Name == event.Metadata.Name {
			return fmt.Errorf("event %s/%s: %w", ns, event.Metadata.Name, ErrKubeAlreadyExists)
		}
	}
	event.Metadata.Labels = maps.Clone(event.Metadata.Labels)
	event.Metadata.Annotations = maps.Clone(event.Metadata.Annotations)
	event.Metadata.ResourceVersion = m.nextResourceVersionLocked()
	m.Events = append(m.Events, event)
	if over := len(m.Events) - MaxMemoryKubeEvents; over > 0 {
		m.Events = append([]KubeEvent(nil), m.Events[over:]...)
	}
	return nil
}

// ListKubeEvents lists in-memory core/v1 Events matching namespace and labelSelector in creation order.
func (m *MemoryKubeClient) ListKubeEvents(_ context.Context, namespace string, labelSelector string) ([]KubeEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	selectors := parseLabelSelector(labelSelector)
	out := make([]KubeEvent, 0)
	for _, ev := range m.Events {
		if namespace != "" && ev.Metadata.Namespace != namespace {
			continue
		}
		if !matchesLabelSelectors(ev.Metadata.Labels, selectors) {
			continue
		}
		cp := ev
		cp.Metadata.Labels = maps.Clone(ev.Metadata.Labels)
		cp.Metadata.Annotations = maps.Clone(ev.Metadata.Annotations)
		out = append(out, cp)
	}
	return out, nil
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

		if pod.PodIP == "" {
			pod.PodIP = m.IPs[podKey(podNs, pod.Name)]
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

// GetPVCManifest retrieves a typed PersistentVolumeClaim manifest.
func (c *RESTKubeClient) GetPVCManifest(ctx context.Context, namespace, name string) (*BlockPVCManifest, error) {
	ns := c.resolveNamespace(namespace)
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.baseURL, ns, name)
	resp, err := c.doRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("GetPVCManifest request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, ns, name)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GetPVCManifest failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var raw coreV1PVC
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("GetPVCManifest decode failed: %w", err)
	}
	m := fromCoreV1PVC(raw)
	return &m, nil
}

// ListPVCManifests lists PersistentVolumeClaims in namespace with an optional labelSelector.
func (c *RESTKubeClient) ListPVCManifests(ctx context.Context, namespace, labelSelector string) ([]BlockPVCManifest, error) {
	ns := c.resolveNamespace(namespace)
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims", c.baseURL, ns)
	if strings.TrimSpace(labelSelector) != "" {
		reqURL += "?labelSelector=" + url.QueryEscape(strings.TrimSpace(labelSelector))
	}
	resp, err := c.doRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ListPVCManifests request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ListPVCManifests failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var list coreV1PVCList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("ListPVCManifests decode failed: %w", err)
	}
	out := make([]BlockPVCManifest, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, fromCoreV1PVC(item))
	}
	return out, nil
}

// PatchPVCLabels merges metadata.labels onto an existing PVC using strategic/merge patch.
func (c *RESTKubeClient) PatchPVCLabels(ctx context.Context, namespace, name string, labels map[string]string) error {
	ns := c.resolveNamespace(namespace)
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.baseURL, ns, name)
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels": labels,
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PatchPVCLabels request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, ns, name)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PatchPVCLabels failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// UpdatePVCAnnotationsCAS updates PVC metadata.annotations using optimistic concurrency (resourceVersion).
// Empty values delete the corresponding annotation key.
func (c *RESTKubeClient) UpdatePVCAnnotationsCAS(ctx context.Context, namespace, name, resourceVersion string, annotations map[string]string) (*BlockPVCManifest, error) {
	ns := c.resolveNamespace(namespace)
	reqURL := fmt.Sprintf("%s/api/v1/namespaces/%s/persistentvolumeclaims/%s", c.baseURL, ns, name)
	patchAnns := make(map[string]any, len(annotations))
	for k, v := range annotations {
		if v == "" {
			patchAnns[k] = nil
		} else {
			patchAnns[k] = v
		}
	}
	meta := map[string]any{
		"annotations": patchAnns,
	}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	payload, err := json.Marshal(map[string]any{
		"metadata": meta,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("UpdatePVCAnnotationsCAS request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: pvc %s/%s", ErrKubeNotFound, ns, name)
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("%w: pvc %s/%s conflict: %s", ErrCASConflict, ns, name, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UpdatePVCAnnotationsCAS failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var raw coreV1PVC
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("UpdatePVCAnnotationsCAS decode failed: %w", err)
	}
	m := fromCoreV1PVC(raw)
	return &m, nil
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
	return c.ensurePod(ctx, pod, nil, false)
}

// EnsurePodWithHook is EnsurePod plus a synchronous onPodIP hook used to bind
// a volume NFS lease the first time podIP is observed, before Ready.
// A timeout without Ready is an error even if an IP was seen.
func (c *RESTKubeClient) EnsurePodWithHook(ctx context.Context, pod PodManifest, hook PodIPHook) (string, error) {
	return c.ensurePod(ctx, pod, hook, true)
}

func (c *RESTKubeClient) ensurePod(ctx context.Context, pod PodManifest, hook PodIPHook, requireReady bool) (string, error) {
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

	wait := c.waitForRunning || hook != nil
	if resp.StatusCode == http.StatusConflict {
		var podIP string
		if wait {
			// Do not bind a newly minted lease to an already-running Pod.
			podIP, _ = c.waitForPodRunning(ctx, ns, pod.Name, nil, requireReady)
		} else {
			podIP, _ = c.getPodIP(ctx, ns, pod.Name)
		}
		return podIP, fmt.Errorf("%w: %s/%s", ErrPodAlreadyExists, ns, pod.Name)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("EnsurePod failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	if wait {
		return c.waitForPodRunning(ctx, ns, pod.Name, hook, requireReady)
	}

	var podResp struct {
		Status struct {
			PodIP string `json:"podIP"`
		} `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&podResp)
	return podResp.Status.PodIP, nil
}

func (c *RESTKubeClient) waitForPodRunning(ctx context.Context, namespace, name string, hook PodIPHook, requireReady bool) (string, error) {
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

	hooked := false
	var lastIP, lastPhase string
	var lastErr error
	for {
		ip, phase, ready, err := c.getPodStatus(pollCtx, namespace, name)
		lastIP, lastPhase, lastErr = ip, phase, err
		if ip != "" && hook != nil && !hooked {
			if hookErr := hook(pollCtx, namespace, name, ip); hookErr != nil {
				return ip, fmt.Errorf("onPodIP hook failed for pod %s/%s: %w", namespace, name, hookErr)
			}
			hooked = true
		}
		if err == nil && ((phase == "Running" && ip != "" && ready) || phase == "Succeeded") {
			return ip, nil
		}
		if phase == "Failed" {
			return "", fmt.Errorf("pod %s/%s entered Failed phase", namespace, name)
		}

		select {
		case <-pollCtx.Done():
			if requireReady {
				return lastIP, fmt.Errorf("timed out waiting for pod %s/%s to become Ready (last phase=%q, ip=%q, err=%v)", namespace, name, lastPhase, lastIP, lastErr)
			}
			if lastIP != "" {
				return lastIP, nil
			}
			return "", fmt.Errorf("timed out waiting for pod %s/%s to reach Running phase (last phase=%q, err=%v)", namespace, name, lastPhase, lastErr)
		case <-ticker.C:
		}
	}
}

// GetPodPhase returns the current status.phase of a Pod.
func (c *RESTKubeClient) GetPodPhase(ctx context.Context, namespace, name string) (string, error) {
	ns := c.resolveNamespace(namespace)
	_, phase, _, err := c.getPodStatus(ctx, ns, name)
	return phase, err
}

func isPodStatusReady(status *coreV1PodStatus) bool {
	if status == nil {
		return true
	}
	if status.Phase != "" && status.Phase != "Running" {
		return false
	}
	for _, cond := range status.Conditions {
		if cond.Type == "Ready" {
			return strings.EqualFold(cond.Status, "True")
		}
	}
	return true
}

func (c *RESTKubeClient) getPodStatus(ctx context.Context, namespace, name string) (string, string, bool, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", c.baseURL, namespace, name)
	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false, fmt.Errorf("getPodStatus request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusNotFound {
			return "", "", false, fmt.Errorf("getPodStatus failed: status %d (%s): %w", resp.StatusCode, string(respBody), ErrPodNotFound)
		}
		return "", "", false, fmt.Errorf("getPodStatus failed: status %d (%s)", resp.StatusCode, string(respBody))
	}

	var podResp struct {
		Status coreV1PodStatus `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&podResp); err != nil {
		return "", "", false, fmt.Errorf("failed to decode pod status: %w", err)
	}
	return podResp.Status.PodIP, podResp.Status.Phase, isPodStatusReady(&podResp.Status), nil
}

func (c *RESTKubeClient) getPodIP(ctx context.Context, namespace, name string) (string, error) {
	ip, _, _, err := c.getPodStatus(ctx, namespace, name)
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
			_, _, _, err := c.getPodStatus(pollCtx, ns, name)
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
		pm := PodManifest{
			Name:             p.Metadata.Name,
			Namespace:        itemNs,
			Labels:           p.Metadata.Labels,
			RuntimeClassName: p.Spec.RuntimeClassName,
		}
		if p.Status != nil {
			pm.PodIP = p.Status.PodIP
			pm.NotReady = !isPodStatusReady(p.Status)
		}
		res = append(res, pm)
	}
	return res, nil
}

// GetServiceClusterIP returns spec.clusterIP for a namespaced Service.
func (c *RESTKubeClient) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s", c.baseURL, ns, name)
	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("get service %s/%s: %w", ns, name, err)
	}
	defer resp.Body.Close()
	body := readAllBody(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get service %s/%s: status %d (%s)", ns, name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	ip, err := decodeServiceClusterIP(body)
	if err != nil {
		return "", fmt.Errorf("decode service %s/%s: %w", ns, name, err)
	}
	return ip, nil
}

// ServiceHasReadyEndpoints reports whether an Endpoints object has at least one address.
func (c *RESTKubeClient) ServiceHasReadyEndpoints(ctx context.Context, namespace, name string) (bool, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/endpoints/%s", c.baseURL, ns, name)
	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("get endpoints %s/%s: %w", ns, name, err)
	}
	defer resp.Body.Close()
	body := readAllBody(resp)
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("get endpoints %s/%s: status %d (%s)", ns, name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return decodeEndpointsReady(body), nil
}

// GetRuntimeClass returns handler and labels for a cluster-scoped RuntimeClass.
func (c *RESTKubeClient) GetRuntimeClass(ctx context.Context, name string) (string, map[string]string, error) {
	url := fmt.Sprintf("%s/apis/node.k8s.io/v1/runtimeclasses/%s", c.baseURL, name)
	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("get runtimeclass %s: %w", name, err)
	}
	defer resp.Body.Close()
	body := readAllBody(resp)
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("get runtimeclass %s: status %d (%s)", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Handler string `json:"handler"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", nil, err
	}
	return obj.Handler, obj.Metadata.Labels, nil
}
