// Package controller implements the Kubernetes reconciliation logic for PodBox,
// PodTemplate, and PodWarmPool custom resources.
package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

const (
	// PVCAnnotationAttachedSandbox records the single sandbox currently holding an exclusive RWO block volume lease.
	PVCAnnotationAttachedSandbox = "cubepod.io/attached-sandbox"
	// PVCAnnotationAttachedAt records the RFC3339 timestamp when the attach lease was acquired.
	PVCAnnotationAttachedAt = "cubepod.io/attached-at"
	// PVCLabelVolumeID identifies the /volumes ID owning a managed block PVC.
	PVCLabelVolumeID = "cubepod.io/volume-id"
	// PVCLabelManagedBy is the standard Kubernetes management label.
	PVCLabelManagedBy = "app.kubernetes.io/managed-by"
	// PVCLabelManagedByVolumes marks PVCs whose lifecycle is owned by /volumes (never deleted by DeleteBox).
	PVCLabelManagedByVolumes = "cubepod-volumes"
	// ReasonWorkspaceVolumeLeaseLost is set on a PodBox that fails controller fencing before Pod startup.
	ReasonWorkspaceVolumeLeaseLost = "WorkspaceVolumeLeaseLost"
)

// ReconcilerOption configures a Reconciler instance.
type ReconcilerOption func(*Reconciler)

// WithKubeClient configures the Kubernetes API client for resource lifecycle operations.
func WithKubeClient(client KubeClient) ReconcilerOption {
	return func(r *Reconciler) {
		if client != nil {
			r.kubeClient = client
		}
	}
}

// WithNamespace configures the default workload namespace.
func WithNamespace(ns string) ReconcilerOption {
	return func(r *Reconciler) {
		if ns != "" {
			r.namespace = ns
		}
	}
}

// WithVolumeMode configures the PVC storage volume mode ("Block" or "Filesystem", default "Block").
func WithVolumeMode(mode string) ReconcilerOption {
	return func(r *Reconciler) {
		if mode != "" {
			r.volumeMode = mode
		}
	}
}

// WithDefaultStorageClass configures the default CSI StorageClassName.
func WithDefaultStorageClass(sc string) ReconcilerOption {
	return func(r *Reconciler) {
		if sc != "" {
			r.defaultStorageClass = sc
		}
	}
}

// WithDefaultRuntimeClass configures the default container/microVM RuntimeClassName.
// Passing "" explicitly unsets the runtimeClass for standard runc/containerd runtimes.
func WithDefaultRuntimeClass(rc string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultRuntimeClass = rc
		r.runtimeClassSet = true
	}
}

// WithPoddInjectorImage configures the image used by the podd-installer initContainer.
func WithPoddInjectorImage(img string) ReconcilerOption {
	return func(r *Reconciler) {
		if img != "" {
			r.poddInjectorImage = img
		}
	}
}

// WithDefaultSandboxImage configures the default workload container image.
func WithDefaultSandboxImage(img string) ReconcilerOption {
	return func(r *Reconciler) {
		if img != "" {
			r.defaultSandboxImage = img
		}
	}
}

// WithDefaultEgressImage configures the default cubepod-egress sidecar image when a template leaves egress.image empty.
func WithDefaultEgressImage(img string) ReconcilerOption {
	return func(r *Reconciler) {
		if strings.TrimSpace(img) != "" {
			r.defaultEgressImage = strings.TrimSpace(img)
		}
	}
}

// WithTransparentEgress configures whether transparent nftables/DNS egress interception is enabled on egress sidecars (default true).
func WithTransparentEgress(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.transparentEgress = enabled
	}
}

// WithDefaultEgressEnabled configures whether seeded and fallback templates enable the egress sidecar by default.
func WithDefaultEgressEnabled(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultEgressEnabled = enabled
	}
}

// WithDefaultEgressExchangeURL configures the default secret exchange URL when a template leaves egress.exchangeUrl empty.
func WithDefaultEgressExchangeURL(exchangeURL string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultEgressExchangeURL = strings.TrimSpace(exchangeURL)
	}
}

// WithEgressDefaults configures default egress sidecar enablement, image, and exchange URL in one option.
func WithEgressDefaults(enabled bool, image, exchangeURL string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultEgressEnabled = enabled
		if strings.TrimSpace(image) != "" {
			r.defaultEgressImage = strings.TrimSpace(image)
		}
		if strings.TrimSpace(exchangeURL) != "" {
			r.defaultEgressExchangeURL = strings.TrimSpace(exchangeURL)
		}
	}
}

// WithForkMode configures the volume cloning strategy used for ForkFrom sandboxes ("pvc-clone" or "snapshot").
func WithForkMode(mode, snapshotClass string) ReconcilerOption {
	return func(r *Reconciler) {
		normalized := strings.ToLower(strings.TrimSpace(mode))
		switch normalized {
		case "snapshot":
			r.forkMode = "snapshot"
		case "pvc-clone", "pvc_clone", "":
			r.forkMode = "pvc-clone"
		}
		r.volumeSnapshotClass = strings.TrimSpace(snapshotClass)
	}
}

// WithE2BTemplateAPI configures whether the E2B template API feature gate is enabled.
func WithE2BTemplateAPI(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.e2bTemplateAPI = enabled
	}
}

// WithTemplateBuildEnabled configures whether Beta image builds are enabled.
func WithTemplateBuildEnabled(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.templateBuildEnabled = enabled
	}
}

// WithImageBuilder configures the builder used for template image builds.
func WithImageBuilder(builder ImageBuilder) ReconcilerOption {
	return func(r *Reconciler) {
		if builder != nil {
			r.imageBuilder = builder
		}
	}
}

// WithContextStore configures the context store for upload tars.
func WithContextStore(store ContextStore) ReconcilerOption {
	return func(r *Reconciler) {
		if store != nil {
			r.contextStore = store
		}
	}
}

// WithContextDir configures the directory for storing context and build logs.
func WithContextDir(dir string) ReconcilerOption {
	return func(r *Reconciler) {
		if dir != "" {
			r.contextDir = dir
			r.contextDirSet = true
		}
	}
}

// WithTemplatePullSecret configures the imagePullSecret for template images.
func WithTemplatePullSecret(secret string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templatePullSecret = secret
	}
}

// WithTemplateRegistry configures the target OCI registry used when pushing built templates.
func WithTemplateRegistry(registry string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templateRegistry = strings.TrimRight(strings.TrimSpace(registry), "/")
	}
}

// WithTemplateRepoPrefix configures the repository prefix under the target template registry.
func WithTemplateRepoPrefix(prefix string) ReconcilerOption {
	return func(r *Reconciler) {
		if strings.TrimSpace(prefix) != "" {
			r.templateRepoPrefix = strings.Trim(strings.TrimSpace(prefix), "/")
		}
	}
}

// WithTemplatePushSecret configures the Kubernetes dockerconfigjson secret used to push built templates.
func WithTemplatePushSecret(secret string) ReconcilerOption {
	return func(r *Reconciler) {
		r.templatePushSecret = strings.TrimSpace(secret)
	}
}

// WithTemplateProbeTimeout configures the timeout for template ReadyCmd probes.
func WithTemplateProbeTimeout(timeout time.Duration) ReconcilerOption {
	return func(r *Reconciler) {
		if timeout > 0 {
			r.templateProbeTimeout = timeout
		}
	}
}

// WithStateDir configures the local state directory for persisted template JSON files, context tars, and logs.
func WithStateDir(dir string) ReconcilerOption {
	return func(r *Reconciler) {
		if dir != "" {
			r.stateDir = dir
			if !r.contextDirSet {
				r.contextDir = filepath.Join(dir, "template-context")
			}
		}
	}
}

// WithEgressCA configures the root CA used to mint per-sandbox mTLS client certificates.
func WithEgressCA(ca *egress.CA) ReconcilerOption {
	return func(r *Reconciler) {
		if ca != nil {
			r.egressCA = ca
		}
	}
}

// WithDefaultImagePullSecrets configures default Kubernetes imagePullSecrets attached to sandbox Pods.
func WithDefaultImagePullSecrets(secrets ...string) ReconcilerOption {
	return func(r *Reconciler) {
		r.defaultImagePullSecrets = nil
		for _, item := range secrets {
			for _, s := range strings.Split(item, ",") {
				s = strings.TrimSpace(s)
				if s != "" && !slices.Contains(r.defaultImagePullSecrets, s) {
					r.defaultImagePullSecrets = append(r.defaultImagePullSecrets, s)
				}
			}
		}
	}
}

type keyedMutex struct {
	mu   sync.Mutex
	refs int
}

// Reconciler manages the lifecycle of PodBox instances, pre-attached PodWarmPools,
// and dual Raw Block PVCs.
type Reconciler struct {
	mu                       sync.RWMutex
	boxLocksMu               sync.Mutex
	boxLocks                 map[string]*keyedMutex
	poolLocksMu              sync.Mutex
	poolLocks                map[string]*keyedMutex
	podBoxEventsDisabled     bool
	boxes                    map[string]*v1alpha1.PodBoxStatus
	specs                    map[string]v1alpha1.PodBoxSpec
	resolvedImages           map[string]string
	boxAnnotations           map[string]map[string]string
	boxResourceVersions      map[string]string
	templates                map[string]v1alpha1.PodTemplateSpec
	warmPools                map[string]*warmPoolState
	warmCounter              int64
	replenishInitOnce        sync.Once
	warmPoolReplenishCh      chan string
	kubeClient               KubeClient
	namespace                string
	volumeMode               string // "Block" or "Filesystem", default "Block"
	defaultStorageClass      string // default "standard"
	defaultRuntimeClass      string // default "kata-qemu"
	runtimeClassSet          bool
	poddInjectorImage        string // default "cubepod-podd:latest"
	defaultSandboxImage      string // default "ubuntu:24.04"
	defaultEgressImage       string
	defaultEgressExchangeURL string
	defaultEgressEnabled     bool
	transparentEgress        bool
	forkMode                 string // "pvc-clone" or "snapshot", default "pvc-clone"
	volumeSnapshotClass      string
	FailWarmPoolClaim        bool
	e2bTemplateAPI           bool
	templateBuildEnabled     bool
	imageBuilder             ImageBuilder
	contextStore             ContextStore
	contextDir               string
	contextDirSet            bool
	stateDir                 string
	templatePullSecret       string
	defaultImagePullSecrets  []string
	templatePushSecret       string
	templateRegistry         string
	templateRepoPrefix       string
	templateProbeTimeout     time.Duration
	templateObjects          map[string]*v1alpha1.PodTemplate
	templateIndex            map[string]string
	idempotency              map[string]idempotencyRecord
	buildingPids             map[string]int
	buildMetrics             templateBuildMetricsCollector
	forkMetrics              forkMetricsCollector
	phaseMetrics             reconcilePhaseMetricsCollector
	egressCA                 *egress.CA
	egressUpstreamCAPEM      []byte
	volumeNFS                VolumeNFSSettings
	volumeNFSLeases          map[string]string
}

// ForkMetricsSnapshot holds Prometheus-ready sandbox fork counters and duration histogram buckets.
type ForkMetricsSnapshot struct {
	ForksTotal          map[string]map[string]int64
	ForkDurationBuckets map[string]int64
	ForkDurationSum     float64
	ForkDurationCount   int64
}

type forkMetricsCollector struct {
	mu       sync.Mutex
	total    map[string]map[string]int64
	buckets  map[string]int64
	sumSecs  float64
	countObs int64
}

func (m *forkMetricsCollector) observe(mode, result string, dur time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == nil {
		m.total = make(map[string]map[string]int64)
	}
	if m.total[mode] == nil {
		m.total[mode] = make(map[string]int64)
	}
	if m.buckets == nil {
		m.buckets = map[string]int64{
			"le_0_1": 0,
			"le_0_5": 0,
			"le_1":   0,
			"le_5":   0,
			"le_15":  0,
			"le_60":  0,
			"le_inf": 0,
		}
	}
	m.total[mode][result]++
	secs := dur.Seconds()
	if secs < 0 {
		secs = 0
	}
	m.sumSecs += secs
	m.countObs++
	if secs <= 0.1 {
		m.buckets["le_0_1"]++
	}
	if secs <= 0.5 {
		m.buckets["le_0_5"]++
	}
	if secs <= 1 {
		m.buckets["le_1"]++
	}
	if secs <= 5 {
		m.buckets["le_5"]++
	}
	if secs <= 15 {
		m.buckets["le_15"]++
	}
	if secs <= 60 {
		m.buckets["le_60"]++
	}
	m.buckets["le_inf"]++
}

func (m *forkMetricsCollector) snapshot() ForkMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	tot := make(map[string]map[string]int64, len(m.total))
	for mode, results := range m.total {
		cp := make(map[string]int64, len(results))
		for res, v := range results {
			cp[res] = v
		}
		tot[mode] = cp
	}
	bkt := map[string]int64{
		"le_0_1": m.buckets["le_0_1"],
		"le_0_5": m.buckets["le_0_5"],
		"le_1":   m.buckets["le_1"],
		"le_5":   m.buckets["le_5"],
		"le_15":  m.buckets["le_15"],
		"le_60":  m.buckets["le_60"],
		"le_inf": m.buckets["le_inf"],
	}
	return ForkMetricsSnapshot{
		ForksTotal:          tot,
		ForkDurationBuckets: bkt,
		ForkDurationSum:     m.sumSecs,
		ForkDurationCount:   m.countObs,
	}
}

// ForkMetrics returns a snapshot of sandbox CoW fork counters and duration histogram buckets.
func (r *Reconciler) ForkMetrics() ForkMetricsSnapshot {
	if r == nil {
		return ForkMetricsSnapshot{}
	}
	return r.forkMetrics.snapshot()
}

// TransparentEgressEnabled reports whether transparent egress interception is enabled.
func (r *Reconciler) TransparentEgressEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transparentEgress
}

// DefaultEgressEnabled reports whether default egress sidecar injection is enabled.
func (r *Reconciler) DefaultEgressEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultEgressEnabled
}

// ForkMode returns the configured CoW fork strategy ("pvc-clone" or "snapshot").
func (r *Reconciler) ForkMode() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.forkMode == "" {
		return "pvc-clone"
	}
	return r.forkMode
}

// VolumeSnapshotClass returns the configured VolumeSnapshotClassName for snapshot fork mode.
func (r *Reconciler) VolumeSnapshotClass() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.volumeSnapshotClass
}

// WithEgressUpstreamCA configures an optional upstream root CA PEM bundle distributed
// to egress sidecar Secrets (<box>-egress-mtls key "upstream-ca.crt").
func WithEgressUpstreamCA(pem []byte) ReconcilerOption {
	return func(r *Reconciler) {
		if len(pem) > 0 {
			r.egressUpstreamCAPEM = append([]byte(nil), pem...)
		}
	}
}

// SetFailWarmPoolClaim sets whether warm pool claims should fail (used for testing fallback).
func (r *Reconciler) SetFailWarmPoolClaim(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FailWarmPoolClaim = fail
}

// NewReconciler creates a new in-memory/cluster Reconciler.
func NewReconciler(opts ...ReconcilerOption) *Reconciler {
	r := &Reconciler{
		boxes:                make(map[string]*v1alpha1.PodBoxStatus),
		specs:                make(map[string]v1alpha1.PodBoxSpec),
		resolvedImages:       make(map[string]string),
		boxAnnotations:       make(map[string]map[string]string),
		boxResourceVersions:  make(map[string]string),
		volumeNFSLeases:      make(map[string]string),
		templates:            make(map[string]v1alpha1.PodTemplateSpec),
		warmPools:            make(map[string]*warmPoolState),
		warmPoolReplenishCh:  make(chan string, 64),
		kubeClient:           NewMemoryKubeClient(),
		namespace:            "cubepod-workloads",
		volumeMode:           "Block",
		defaultStorageClass:  "standard",
		defaultRuntimeClass:  "kata-qemu",
		poddInjectorImage:    "cubepod-podd:latest",
		defaultSandboxImage:  "ubuntu:24.04",
		transparentEgress:    true,
		forkMode:             "pvc-clone",
		e2bTemplateAPI:       true,
		templateBuildEnabled: false,
		imageBuilder:         NewFakeImageBuilder(),
		contextStore:         NewMemoryContextStore(),
		contextDir:           filepath.Join(os.TempDir(), "cubepod-context"),
		stateDir:             os.Getenv("CUBEPOD_STATE"),
		templateRepoPrefix:   "cubepod-templates",
		templateProbeTimeout: 120 * time.Second,
		templateObjects:      make(map[string]*v1alpha1.PodTemplate),
		templateIndex:        make(map[string]string),
		idempotency:          make(map[string]idempotencyRecord),
		buildingPids:         make(map[string]int),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.egressCA == nil {
		ca, err := egress.NewCA()
		if err != nil {
			slog.Warn("failed to initialize default egress CA", "err", err)
		} else {
			r.egressCA = ca
		}
	}
	if r.stateDir != "" {
		if mk, ok := r.kubeClient.(*MemoryKubeClient); ok {
			mk.LoadLocalTemplates(r.stateDir)
		}
		_ = r.loadLocalTemplates(context.Background())
	}
	_ = r.SyncTemplatesFromKube(context.Background())
	_ = r.EnsureSeededTemplates(context.Background())
	return r
}

// EgressCA returns the configured root CA used to mint per-sandbox mTLS client certificates.
func (r *Reconciler) EgressCA() *egress.CA {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.egressCA
}

// SetKubeClient updates the Kubernetes API client.
func (r *Reconciler) SetKubeClient(client KubeClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client != nil {
		r.kubeClient = client
	}
}

// KubeClient returns the currently configured Kubernetes client.
func (r *Reconciler) KubeClient() KubeClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.kubeClient
}

// Namespace returns the workload namespace used for Pods and PVCs.
func (r *Reconciler) Namespace() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namespace
}

// StorageClassName returns the default StorageClass for PVCs this reconciler creates.
func (r *Reconciler) StorageClassName() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.defaultStorageClass == "" {
		return "standard"
	}
	return r.defaultStorageClass
}

// VolumeMode returns the default PVC volumeMode ("Block" or "Filesystem") configured on the reconciler.
func (r *Reconciler) VolumeMode() string {
	if r == nil {
		return "Block"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.volumeMode == "" {
		return "Block"
	}
	return r.volumeMode
}

// ContextStore returns the currently configured build context and log store.
func (r *Reconciler) ContextStore() ContextStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.contextStore
}

// PingBuilder verifies connectivity to the configured ImageBuilder (BuildKit or Docker).
func (r *Reconciler) PingBuilder(ctx context.Context) error {
	r.mu.RLock()
	builder := r.imageBuilder
	r.mu.RUnlock()
	if builder == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return builder.Ping(ctx)
}

// SweepTemplateContexts removes expired template build contexts (24h TTL) and logs (7d TTL).
func (r *Reconciler) SweepTemplateContexts(now time.Time) error {
	r.mu.RLock()
	store := r.contextStore
	r.mu.RUnlock()
	if store == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return store.SweepExpired(now, 24*time.Hour, 7*24*time.Hour)
}

// RegisterTemplate saves or updates a reusable PodTemplateSpec.
func (r *Reconciler) RegisterTemplate(name string, spec v1alpha1.PodTemplateSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templates[name] = spec
	if r.templateObjects[name] == nil {
		tpl := v1alpha1.PodTemplate{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodTemplate",
			Metadata: v1alpha1.ObjectMeta{
				Name:      name,
				Namespace: r.namespace,
			},
			Spec: spec,
			Status: v1alpha1.PodTemplateStatus{
				TemplateID:   "tpl_" + Slugify(name),
				BuildStatus:  v1alpha1.BuildStatusReady,
				ReadyImage:   spec.Image,
				ReadyBuildID: "bld_reg",
			},
		}
		r.templateObjects[name] = &tpl
		_ = r.indexTemplateLocked(&tpl)
		if r.kubeClient != nil {
			_ = createPodTemplateWithStatus(context.Background(), r.kubeClient, r.namespace, &tpl)
		}
	} else {
		tpl := r.templateObjects[name]
		tpl.Spec = spec
		if tpl.Status.ReadyImage == "" {
			tpl.Status.ReadyImage = spec.Image
			tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
		}
		_ = r.indexTemplateLocked(tpl)
		if r.kubeClient != nil {
			_ = r.kubeClient.UpdatePodTemplateStatus(context.Background(), *tpl)
		}
	}
}

// E2BTemplateAPIEnabled reports whether the E2B template API feature gate is on.
func (r *Reconciler) E2BTemplateAPIEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.e2bTemplateAPI
}

// SetE2BTemplateAPI toggles the E2B template API feature gate.
func (r *Reconciler) SetE2BTemplateAPI(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.e2bTemplateAPI = enabled
}

// TemplateBuildEnabled reports whether Beta image builds are enabled.
func (r *Reconciler) TemplateBuildEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.templateBuildEnabled
}

// SetTemplateBuildEnabled toggles whether Beta image builds are enabled.
func (r *Reconciler) SetTemplateBuildEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.templateBuildEnabled = enabled
}

// SetImageBuilder updates the builder used for template image builds.
func (r *Reconciler) SetImageBuilder(builder ImageBuilder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if builder != nil {
		r.imageBuilder = builder
	}
}

// SetContextStore updates the store used for uploading context tars.
func (r *Reconciler) SetContextStore(store ContextStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if store != nil {
		r.contextStore = store
	}
}

// GetTemplate retrieves a registered PodTemplateSpec by name.
func (r *Reconciler) GetTemplate(name string) (v1alpha1.PodTemplateSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tpl, ok := r.templates[name]
	return tpl, ok
}

func (r *Reconciler) applyTemplateDefaults(tpl *v1alpha1.PodTemplateSpec) {
	if tpl == nil {
		return
	}
	if tpl.RuntimeClassName == "" && r.runtimeClassSet {
		tpl.RuntimeClassName = r.defaultRuntimeClass
	}
	if tpl.Storage.StorageClassName == "" && r.defaultStorageClass != "" {
		tpl.Storage.StorageClassName = r.defaultStorageClass
	}
	if tpl.Image == "" && r.defaultSandboxImage != "" {
		tpl.Image = r.defaultSandboxImage
	}
	if tpl.PoddImage == "" && r.poddInjectorImage != "" {
		tpl.PoddImage = r.poddInjectorImage
	}
	if tpl.Egress.Enabled {
		if tpl.Egress.Image == "" && r.defaultEgressImage != "" {
			tpl.Egress.Image = r.defaultEgressImage
		}
		if tpl.Egress.ExchangeURL == "" && r.defaultEgressExchangeURL != "" {
			tpl.Egress.ExchangeURL = r.defaultEgressExchangeURL
		}
	}
}

func (r *Reconciler) defaultTemplate(templateID string) v1alpha1.PodTemplateSpec {
	img := templateID
	if img == "" {
		img = r.defaultSandboxImage
	}
	if img == "" {
		img = "ubuntu:24.04"
	}

	rtClass := "kata-qemu"
	if r.runtimeClassSet {
		rtClass = r.defaultRuntimeClass
	} else if strings.EqualFold(r.volumeMode, "Filesystem") {
		rtClass = ""
	}

	poddImg := r.poddInjectorImage
	if poddImg == "" {
		poddImg = "cubepod-podd:latest"
	}

	sc := r.defaultStorageClass
	if sc == "" {
		sc = "standard"
	}

	var egressCfg v1alpha1.EgressConfig
	if r.defaultEgressEnabled {
		egressCfg = v1alpha1.EgressConfig{
			Enabled:     true,
			Image:       r.defaultEgressImage,
			ExchangeURL: r.defaultEgressExchangeURL,
		}
	}

	return v1alpha1.PodTemplateSpec{
		RuntimeClassName: rtClass,
		Image:            img,
		PoddImage:        poddImg,
		Egress:           egressCfg,
		Resources: v1alpha1.ResourceRequirements{
			CPURequest:    "1",
			CPULimit:      "2",
			MemoryRequest: "2Gi",
			MemoryLimit:   "4Gi",
		},
		Storage: v1alpha1.StorageSpec{
			StorageClassName: sc,
			RootfsSize:       "10Gi",
			WorkspaceSize:    "20Gi",
		},
	}
}

func defaultTemplate(templateID string) v1alpha1.PodTemplateSpec {
	return NewReconciler().defaultTemplate(templateID)
}

func (r *Reconciler) ensureReplenishChan() chan string {
	r.replenishInitOnce.Do(func() {
		if r.warmPoolReplenishCh == nil {
			r.warmPoolReplenishCh = make(chan string, 64)
		}
	})
	return r.warmPoolReplenishCh
}

// WarmPoolReplenishChan returns a read-only channel that receives pool names whenever
// a standby warm pod is claimed, enabling immediate asynchronous replenishment.
func (r *Reconciler) WarmPoolReplenishChan() <-chan string {
	return r.ensureReplenishChan()
}

func (r *Reconciler) notifyWarmPoolReplenish(poolName string) {
	if poolName == "" {
		return
	}
	ch := r.ensureReplenishChan()
	select {
	case ch <- poolName:
	default:
	}
}

func acquireKeyedLock(mu *sync.Mutex, table *map[string]*keyedMutex, key string) func() {
	mu.Lock()
	if *table == nil {
		*table = make(map[string]*keyedMutex)
	}
	entry := (*table)[key]
	if entry == nil {
		entry = &keyedMutex{}
		(*table)[key] = entry
	}
	entry.refs++
	mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		mu.Lock()
		entry.refs--
		if entry.refs == 0 && (*table)[key] == entry {
			delete(*table, key)
		}
		mu.Unlock()
	}
}

func tryAcquireKeyedLock(mu *sync.Mutex, table *map[string]*keyedMutex, key string) (func(), bool) {
	mu.Lock()
	if *table == nil {
		*table = make(map[string]*keyedMutex)
	}
	entry := (*table)[key]
	if entry == nil {
		entry = &keyedMutex{}
		(*table)[key] = entry
	}
	entry.refs++
	mu.Unlock()

	if !entry.mu.TryLock() {
		mu.Lock()
		entry.refs--
		if entry.refs == 0 && (*table)[key] == entry {
			delete(*table, key)
		}
		mu.Unlock()
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		mu.Lock()
		entry.refs--
		if entry.refs == 0 && (*table)[key] == entry {
			delete(*table, key)
		}
		mu.Unlock()
	}, true
}

func (r *Reconciler) acquireBoxLock(name string) func() {
	return acquireKeyedLock(&r.boxLocksMu, &r.boxLocks, name)
}

func (r *Reconciler) tryAcquireBoxLock(name string) (func(), bool) {
	return tryAcquireKeyedLock(&r.boxLocksMu, &r.boxLocks, name)
}

func (r *Reconciler) acquirePoolLock(poolName string) func() {
	return acquireKeyedLock(&r.poolLocksMu, &r.poolLocks, poolName)
}

func runParallel(fns ...func() error) error {
	var nonNil []func() error
	for _, fn := range fns {
		if fn != nil {
			nonNil = append(nonNil, fn)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]()
	}
	errs := make([]error, len(nonNil))
	var wg sync.WaitGroup
	wg.Add(len(nonNil))
	for i, fn := range nonNil {
		go func(idx int, f func() error) {
			defer wg.Done()
			errs[idx] = f()
		}(i, fn)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ReconcileWarmPool manages standby Kata MicroVM instances with pre-attached thin block PVCs.
func (r *Reconciler) ReconcileWarmPool(ctx context.Context, poolName string, spec v1alpha1.PodWarmPoolSpec) (*v1alpha1.PodWarmPoolStatus, error) {
	unlockPool := r.acquirePoolLock(poolName)
	defer unlockPool()

	if ctx == nil {
		ctx = context.Background()
	}

	type warmPoolSnapshot struct {
		tpl               v1alpha1.PodTemplateSpec
		currentAvail      int32
		need              int
		removedEntries    []*WarmPodEntry
		kc                KubeClient
		ns                string
		volumeMode        string
		transparentEgress bool
		pullSecrets       []string
		egressCA          *egress.CA
		upstreamCAPEM     []byte
		persisted         bool
	}

	allocateScaleUp := func(count int) ([]string, warmPoolSnapshot) {
		r.mu.Lock()
		defer r.mu.Unlock()

		pool, exists := r.warmPools[poolName]
		if !exists {
			pool = &warmPoolState{
				spec:      spec,
				entries:   make([]*WarmPodEntry, 0),
				persisted: false,
				createdAt: time.Now().UTC(),
			}
			r.warmPools[poolName] = pool
		}
		pool.spec = spec

		var tpl v1alpha1.PodTemplateSpec
		resolved, err := r.resolveTemplateLocked(spec.TemplateRef)
		if err == nil && resolved != nil {
			tpl = resolved.TemplateSpec
			tpl.Image = resolved.Image
			tpl.StartCmd = resolved.StartCmd
			tpl.ReadyCmd = resolved.ReadyCmd
			tpl.RunAsUser = resolved.RunAsUser
			tpl.RunAsGroup = resolved.RunAsGroup
			tpl.RunAsUsername = resolved.RunAsUsername
			if resolved.CPUCount > 0 {
				tpl.Resources.CPULimit = fmt.Sprintf("%d", resolved.CPUCount)
			}
			if resolved.MemoryMB > 0 {
				tpl.Resources.MemoryLimit = fmt.Sprintf("%dMi", resolved.MemoryMB)
			}
			if resolved.DiskSizeMB > 0 {
				tpl.Storage.RootfsSize = fmt.Sprintf("%dMi", resolved.DiskSizeMB)
			}
		} else {
			var ok bool
			tpl, ok = r.templates[spec.TemplateRef]
			if !ok {
				tpl = r.defaultTemplate(spec.TemplateRef)
			} else {
				r.applyTemplateDefaults(&tpl)
			}
		}
		r.applyTemplateDefaults(&tpl)

		if tpl.Egress.Enabled && r.egressCA == nil {
			if ca, caErr := egress.NewCA(); caErr == nil {
				r.egressCA = ca
			}
		}

		// Count existing available (unclaimed) replicas and advance warmCounter.
		var availableEntries []*WarmPodEntry
		for _, entry := range pool.entries {
			if !entry.Claimed {
				availableEntries = append(availableEntries, entry)
			}
			if idx := parseWarmIndex(entry.ID); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.PodName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.RootfsPVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.WorkspacePVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
		}
		for _, box := range r.boxes {
			if box == nil {
				continue
			}
			if idx := parseWarmIndex(box.PodName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(box.RootfsPVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(box.WorkspacePVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
		}

		currentAvail := int32(len(availableEntries))
		var entryIDs []string
		var removedEntries []*WarmPodEntry
		if count > 0 {
			for i := 0; i < count; i++ {
				r.warmCounter++
				for {
					entryID := fmt.Sprintf("%s-warm-%d", poolName, r.warmCounter)
					rootfsName := entryID + "-rootfs-pvc"
					wsName := entryID + "-workspace-pvc"
					conflict := false
					for _, b := range r.boxes {
						if b != nil && (b.RootfsPVCName == rootfsName || b.WorkspacePVCName == wsName) {
							conflict = true
							break
						}
					}
					if !conflict {
						for _, entry := range pool.entries {
							if entry != nil && (entry.ID == entryID || entry.RootfsPVCName == rootfsName || entry.WorkspacePVCName == wsName) {
								conflict = true
								break
							}
						}
					}
					if !conflict {
						for _, prev := range entryIDs {
							if prev == entryID {
								conflict = true
								break
							}
						}
					}
					if !conflict {
						entryIDs = append(entryIDs, entryID)
						break
					}
					r.warmCounter++
				}
			}
		} else if currentAvail < spec.Replicas {
			need := int(spec.Replicas - currentAvail)
			for i := 0; i < need; i++ {
				r.warmCounter++
				for {
					entryID := fmt.Sprintf("%s-warm-%d", poolName, r.warmCounter)
					rootfsName := entryID + "-rootfs-pvc"
					wsName := entryID + "-workspace-pvc"
					conflict := false
					for _, b := range r.boxes {
						if b != nil && (b.RootfsPVCName == rootfsName || b.WorkspacePVCName == wsName) {
							conflict = true
							break
						}
					}
					if !conflict {
						for _, entry := range pool.entries {
							if entry != nil && (entry.ID == entryID || entry.RootfsPVCName == rootfsName || entry.WorkspacePVCName == wsName) {
								conflict = true
								break
							}
						}
					}
					if !conflict {
						for _, prev := range entryIDs {
							if prev == entryID {
								conflict = true
								break
							}
						}
					}
					if !conflict {
						entryIDs = append(entryIDs, entryID)
						break
					}
					r.warmCounter++
				}
			}
		} else if currentAvail > spec.Replicas && len(availableEntries) > 0 && len(pool.entries) > 0 {
			toRemove := currentAvail - spec.Replicas
			var filtered []*WarmPodEntry
			var removedCount int32
			for _, entry := range pool.entries {
				if !entry.Claimed && removedCount < toRemove {
					removedCount++
					removedEntries = append(removedEntries, entry)
					continue
				}
				filtered = append(filtered, entry)
			}
			pool.entries = filtered
		}

		pullSecrets := append([]string{r.templatePullSecret}, r.defaultImagePullSecrets...)
		return entryIDs, warmPoolSnapshot{
			tpl:               tpl,
			currentAvail:      currentAvail,
			need:              len(entryIDs),
			removedEntries:    removedEntries,
			kc:                r.kubeClient,
			ns:                r.namespace,
			volumeMode:        r.volumeMode,
			transparentEgress: r.transparentEgress,
			pullSecrets:       pullSecrets,
			egressCA:          r.egressCA,
			upstreamCAPEM:     append([]byte(nil), r.egressUpstreamCAPEM...),
			persisted:         pool.persisted,
		}
	}

	provisionWarmEntry := func(entryID string, snap warmPoolSnapshot) (entry *WarmPodEntry, stopRetry bool) {
		warmBoxSpec := v1alpha1.PodBoxSpec{
			TemplateRef:      spec.TemplateRef,
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: 1,
			WarmPoolRef:      poolName,
		}

		rootfsPVC, wsPVC := BuildBlockPVCs(entryID, snap.tpl, warmBoxSpec, snap.volumeMode)
		rootfsPVC.Namespace = snap.ns
		wsPVC.Namespace = snap.ns

		var secErr error
		_ = runParallel(
			func() error {
				if snap.kc != nil {
					_ = snap.kc.EnsurePVC(ctx, rootfsPVC)
				}
				return nil
			},
			func() error {
				if snap.kc != nil {
					_ = snap.kc.EnsurePVC(ctx, wsPVC)
				}
				return nil
			},
			func() error {
				if snap.tpl.Egress.Enabled {
					secErr = ensureEgressSecretsWithClient(ctx, snap.kc, snap.ns, snap.egressCA, snap.upstreamCAPEM, entryID, warmBoxSpec, true)
				}
				return nil
			},
		)
		if secErr != nil {
			slog.Warn("failed to ensure egress secrets for warm pod", "pod", entryID, "err", secErr)
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			_ = runParallel(
				func() error {
					if snap.kc != nil {
						_ = snap.kc.DeletePVC(cleanupCtx, snap.ns, rootfsPVC.Name)
					}
					return nil
				},
				func() error {
					if snap.kc != nil {
						_ = snap.kc.DeletePVC(cleanupCtx, snap.ns, wsPVC.Name)
					}
					return nil
				},
				func() error {
					deleteEgressSecretsWithClient(cleanupCtx, snap.kc, snap.ns, entryID)
					return nil
				},
			)
			cancel()
			return nil, true
		}

		dummyStatus := v1alpha1.PodBoxStatus{
			PodName:          entryID,
			RootfsPVCName:    rootfsPVC.Name,
			WorkspacePVCName: wsPVC.Name,
			Phase:            v1alpha1.PodBoxPhaseRunning,
		}
		podManifest := BuildPodManifest(entryID, snap.tpl, warmBoxSpec, dummyStatus, PodBuildOptions{
			VolumeMode:        snap.volumeMode,
			ImagePullSecrets:  snap.pullSecrets,
			TransparentEgress: snap.transparentEgress,
		})
		podManifest.Namespace = snap.ns
		if podManifest.Labels == nil {
			podManifest.Labels = make(map[string]string)
		}
		podManifest.Labels["cubepod.io/warmpool"] = SanitizeLabelValue(poolName)
		podManifest.Labels["cubepod.io/template"] = SanitizeLabelValue(spec.TemplateRef)
		podManifest.Labels["cubepod.io/claimed"] = "false"

		is409 := false
		var podIP string
		var err error
		if snap.kc != nil {
			podIP, err = snap.kc.EnsurePod(ctx, podManifest)
		}
		if err != nil {
			if errors.Is(err, ErrPodAlreadyExists) || strings.Contains(strings.ToLower(err.Error()), "409") || strings.Contains(strings.ToLower(err.Error()), "already exists") || strings.Contains(strings.ToLower(err.Error()), "conflict") {
				is409 = true
			} else {
				slog.Warn("failed to ensure warm pod", "pod", dummyStatus.PodName, "err", err)
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				if delErr := snap.kc.DeletePod(cleanupCtx, snap.ns, dummyStatus.PodName); delErr != nil {
					slog.Warn("failed to clean up warm pod after EnsurePod error", "pod", dummyStatus.PodName, "err", delErr)
				}
				_ = runParallel(
					func() error {
						if delErr := snap.kc.DeletePVC(cleanupCtx, snap.ns, rootfsPVC.Name); delErr != nil {
							slog.Warn("failed to clean up warm rootfs PVC after EnsurePod error", "pvc", rootfsPVC.Name, "err", delErr)
						}
						return nil
					},
					func() error {
						if delErr := snap.kc.DeletePVC(cleanupCtx, snap.ns, wsPVC.Name); delErr != nil {
							slog.Warn("failed to clean up warm workspace PVC after EnsurePod error", "pvc", wsPVC.Name, "err", delErr)
						}
						return nil
					},
					func() error {
						deleteEgressSecretsWithClient(cleanupCtx, snap.kc, snap.ns, entryID)
						return nil
					},
				)
				stopPoolRetry := ctx.Err() != nil
				if !stopPoolRetry && snap.persisted {
					if _, getErr := snap.kc.GetPodWarmPool(cleanupCtx, snap.ns, poolName); getErr != nil && !errors.Is(getErr, ErrCRDNotInstalled) {
						errLower := strings.ToLower(getErr.Error())
						if strings.Contains(errLower, "not found") || strings.Contains(errLower, "404") {
							stopPoolRetry = true
						}
					}
				}
				cancel()
				return nil, stopPoolRetry
			}
		}
		if podIP != "" {
			dummyStatus.PodIP = podIP
		}

		var claimedBy string
		entryNotReady := podIP == ""
		if is409 && snap.kc != nil {
			existingPods, listErr := snap.kc.ListPods(ctx, snap.ns, "")
			if listErr != nil {
				slog.Warn("failed to list pods to verify existing warm pod", "pod", dummyStatus.PodName, "err", listErr)
				return nil, false
			}
			var foundPod *PodManifest
			for _, p := range existingPods {
				if p.Name == dummyStatus.PodName {
					foundPod = &p
					break
				}
			}
			if foundPod == nil {
				slog.Warn("existing pod not found in pod list", "pod", dummyStatus.PodName)
				return nil, false
			}
			if foundPod.Labels != nil && foundPod.Labels["cubepod.io/claimed"] == "true" {
				slog.Info("pod already exists and is claimed, skipping standby adoption", "pod", dummyStatus.PodName)
				return nil, false
			}
			if foundPod.PodIP != "" {
				podIP = foundPod.PodIP
				dummyStatus.PodIP = podIP
			}
			entryNotReady = foundPod.NotReady || podIP == ""
			if foundPod.Labels != nil {
				for k, v := range foundPod.Labels {
					podManifest.Labels[k] = v
				}
				claimedBy = foundPod.Labels["cubepod.io/claimed-by"]
			}
		}

		return &WarmPodEntry{
			ID:               entryID,
			PoolName:         poolName,
			TemplateRef:      spec.TemplateRef,
			PodName:          dummyStatus.PodName,
			PodIP:            podIP,
			NotReady:         entryNotReady,
			RootfsPVCName:    rootfsPVC.Name,
			WorkspacePVCName: wsPVC.Name,
			Image:            snap.tpl.Image,
			Claimed:          false,
			ClaimedBy:        claimedBy,
			CreatedAt:        time.Now().UTC(),
			PodManifest:      podManifest,
			RootfsPVC:        rootfsPVC,
			WorkspacePVC:     wsPVC,
		}, false
	}

	entryIDs, snap := allocateScaleUp(0)
	if len(entryIDs) > 0 {
		scaleStart := time.Now()
		maxRounds := 3
		for round := 0; round < maxRounds && len(entryIDs) > 0; round++ {
			results := make([]*WarmPodEntry, len(entryIDs))
			stops := make([]bool, len(entryIDs))
			var wg sync.WaitGroup
			wg.Add(len(entryIDs))
			for i, id := range entryIDs {
				go func(idx int, eid string) {
					defer wg.Done()
					results[idx], stops[idx] = provisionWarmEntry(eid, snap)
				}(i, id)
			}
			wg.Wait()

			var created []*WarmPodEntry
			var failedCount int
			stopAll := ctx.Err() != nil
			for i, res := range results {
				if res != nil {
					created = append(created, res)
				} else {
					failedCount++
					if stops[i] {
						stopAll = true
					}
				}
			}
			if len(created) > 0 {
				r.mu.Lock()
				if pool, ok := r.warmPools[poolName]; ok {
					pool.entries = append(pool.entries, created...)
				}
				r.mu.Unlock()
			}
			if failedCount == 0 || stopAll {
				break
			}
			entryIDs, snap = allocateScaleUp(failedCount)
		}
		r.phaseMetrics.observe("warmpool_scale_up", "total", time.Since(scaleStart))
	} else if len(snap.removedEntries) > 0 && snap.kc != nil {
		// Scale down excess unclaimed entries in parallel outside r.mu.
		var wg sync.WaitGroup
		wg.Add(len(snap.removedEntries))
		for _, entry := range snap.removedEntries {
			go func(e *WarmPodEntry) {
				defer wg.Done()
				_ = snap.kc.DeletePod(ctx, snap.ns, e.PodName)
				_ = runParallel(
					func() error { _ = snap.kc.DeletePVC(ctx, snap.ns, e.RootfsPVCName); return nil },
					func() error { _ = snap.kc.DeletePVC(ctx, snap.ns, e.WorkspacePVCName); return nil },
					func() error { deleteEgressSecretsWithClient(ctx, snap.kc, snap.ns, e.ID); return nil },
				)
			}(entry)
		}
		wg.Wait()
	}

	r.mu.RLock()
	pool := r.warmPools[poolName]
	var finalAvail int32
	var persisted bool
	if pool != nil {
		persisted = pool.persisted
		for _, entry := range pool.entries {
			if !entry.Claimed && !entry.NotReady && entry.PodIP != "" {
				finalAvail++
			}
		}
	}
	kc := r.kubeClient
	ns := r.namespace
	r.mu.RUnlock()

	st := &v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     finalAvail,
		AvailableReplicas: finalAvail,
	}

	// Persist the PodWarmPool CRD state to Kubernetes API outside r.mu.
	if kc != nil {
		if !persisted {
			cr := v1alpha1.PodWarmPool{
				APIVersion: "cubepod.io/v1alpha1",
				Kind:       "PodWarmPool",
				Metadata: v1alpha1.ObjectMeta{
					Name:      poolName,
					Namespace: ns,
				},
				Spec:   spec,
				Status: *st,
			}
			if err := kc.UpsertPodWarmPool(ctx, cr); err != nil {
				if errors.Is(err, ErrCRDNotInstalled) {
					slog.Debug("PodWarmPool CRD not installed on APIServer; skipping pool persistence", "pool", poolName)
				} else {
					slog.Warn("failed to upsert initial PodWarmPool CR", "pool", poolName, "err", err)
				}
			} else {
				r.mu.Lock()
				if p := r.warmPools[poolName]; p != nil {
					p.persisted = true
				}
				r.mu.Unlock()
			}
		} else {
			err := kc.UpdatePodWarmPoolStatus(ctx, ns, poolName, *st)
			if err != nil {
				if errors.Is(err, ErrCRDNotInstalled) {
					slog.Debug("PodWarmPool CRD not installed on APIServer; skipping status update", "pool", poolName)
				} else {
					errStr := strings.ToLower(err.Error())
					if strings.Contains(errStr, "not found") || strings.Contains(errStr, "404") {
						slog.Debug("PodWarmPool not found in Kubernetes; skipping status update and recreation", "pool", poolName, "err", err)
					} else {
						slog.Warn("failed to update PodWarmPool status", "pool", poolName, "err", err)
					}
				}
			}
		}
	}

	return st, nil
}

// ClaimWarmPod claims a standby warm pod from the given pool for a new PodBox without holding r.mu across K8s I/O.
func (r *Reconciler) ClaimWarmPod(ctx context.Context, poolName string, boxName string) (*WarmPodEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	tried := make(map[string]bool)
	for {
		r.mu.Lock()
		if r.FailWarmPoolClaim {
			r.mu.Unlock()
			return nil, fmt.Errorf("simulated warm pool claim failure")
		}
		pool, ok := r.warmPools[poolName]
		if !ok {
			r.mu.Unlock()
			return nil, fmt.Errorf("warm pool %q not found", poolName)
		}
		expectedImage := ""
		if resolved, err := r.resolveTemplateLocked(pool.spec.TemplateRef); err == nil && resolved != nil {
			expectedImage = resolved.Image
		}
		if expectedImage == "" {
			if t, ok := r.templates[pool.spec.TemplateRef]; ok {
				expectedImage = t.Image
			}
		}

		var candidate *WarmPodEntry
		for _, entry := range pool.entries {
			if !entry.Claimed && !entry.NotReady && entry.PodIP != "" && !tried[entry.PodName] {
				if entry.Image != "" && expectedImage != "" && entry.Image != expectedImage {
					continue
				}
				// Optimistically reserve entry under r.mu so concurrent ClaimWarmPod calls don't pick it.
				entry.Claimed = true
				entry.ClaimedBy = boxName
				tried[entry.PodName] = true
				candidate = entry
				break
			}
		}
		kc := r.kubeClient
		ns := r.namespace
		r.mu.Unlock()

		if candidate == nil {
			return nil, fmt.Errorf("warm pool %q has no available standby replicas", poolName)
		}

		if kc != nil {
			if err := kc.ClaimPod(ctx, ns, candidate.PodName, boxName); err != nil {
				r.mu.Lock()
				if errors.Is(err, ErrCASConflict) {
					slog.Warn("CAS conflict claiming standby pod, marked as claimed by other replica", "pod", candidate.PodName, "box", boxName, "err", err)
					candidate.Claimed = true
					candidate.ClaimedBy = "other-replica"
				} else {
					slog.Warn("transient/unknown error claiming standby pod, leaving unclaimed for retry", "pod", candidate.PodName, "box", boxName, "err", err)
					candidate.Claimed = false
					candidate.ClaimedBy = ""
				}
				r.mu.Unlock()
				continue
			}
		}

		r.mu.Lock()
		now := time.Now().UTC()
		candidate.Claimed = true
		candidate.ClaimedBy = boxName
		candidate.ClaimedAt = &now
		if candidate.PodManifest.Labels == nil {
			candidate.PodManifest.Labels = make(map[string]string)
		}
		candidate.PodManifest.Labels["cubepod.io/claimed"] = "true"
		candidate.PodManifest.Labels["cubepod.io/claimed-by"] = boxName
		cp := *candidate
		r.mu.Unlock()

		r.notifyWarmPoolReplenish(poolName)
		return &cp, nil
	}
}

// GetWarmPoolStatus returns the current ready and available count for a warm pool.
func (r *Reconciler) GetWarmPoolStatus(poolName string) (*v1alpha1.PodWarmPoolStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pool, ok := r.warmPools[poolName]
	if !ok {
		return nil, false
	}

	var avail int32
	for _, entry := range pool.entries {
		if !entry.Claimed && !entry.NotReady && entry.PodIP != "" {
			avail++
		}
	}

	return &v1alpha1.PodWarmPoolStatus{
		ReadyReplicas:     avail,
		AvailableReplicas: avail,
	}, true
}

const egressCertRotationMargin = 1 * time.Hour

// Reconcile drives a PodBox towards its DesiredState (Running or Paused),
// preserving both RootFS Upper PVC and Workspace PVC across pause/resume cycles,
// claiming from PodWarmPool if configured, and supporting CoW Fork cloning.
func (r *Reconciler) Reconcile(ctx context.Context, name string, spec v1alpha1.PodBoxSpec) (*v1alpha1.PodBoxStatus, error) {
	st, _, err := r.ReconcileWithTimings(ctx, name, spec)
	return st, err
}

// ReconcileWithTimings executes Reconcile without holding the global reconciler lock across
// Kubernetes network I/O and returns phase-level latency timings for observability.
func (r *Reconciler) ReconcileWithTimings(ctx context.Context, name string, spec v1alpha1.PodBoxSpec) (*v1alpha1.PodBoxStatus, ReconcilePhaseTimings, error) {
	totalStart := time.Now()
	var timings ReconcilePhaseTimings

	unlockBox := r.acquireBoxLock(name)
	defer unlockBox()

	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()

	r.mu.Lock()
	existingPtr, exists := r.boxes[name]
	var prevPhase v1alpha1.PodBoxPhase
	var localStatus v1alpha1.PodBoxStatus
	if exists && existingPtr != nil {
		localStatus = *existingPtr
		prevPhase = localStatus.Phase
		if localStatus.Phase == v1alpha1.PodBoxPhaseTerminated ||
			(localStatus.Phase == v1alpha1.PodBoxPhaseFailed && spec.RootfsGeneration <= localStatus.ObservedRootfsGen) {
			r.mu.Unlock()
			return &localStatus, timings, nil
		}
	}

	isFork := !exists && spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != ""
	var forkStart time.Time
	var forkMetricMode string
	forkResult := "error"
	if isFork {
		forkStart = time.Now()
		if r.forkMode == "snapshot" {
			forkMetricMode = "snapshot"
		} else {
			forkMetricMode = "pvc_clone"
		}
		defer func() {
			r.forkMetrics.observe(forkMetricMode, forkResult, time.Since(forkStart))
		}()
	}

	resolved, err := r.resolveTemplateLocked(spec.TemplateRef)
	if err != nil {
		r.mu.Unlock()
		return nil, timings, err
	}
	effectiveImage := resolved.Image
	if exists {
		if pinned := r.resolvedImages[name]; pinned != "" {
			effectiveImage = pinned
		}
	} else if effectiveImage != "" {
		r.resolvedImages[name] = effectiveImage
	}

	kc := r.kubeClient
	ns := r.namespace
	volumeMode := r.volumeMode
	forkMode := r.forkMode
	volumeSnapshotClass := r.volumeSnapshotClass
	pullSecrets := append([]string{r.templatePullSecret}, r.defaultImagePullSecrets...)
	transparentEgress := r.transparentEgress
	if r.egressCA == nil {
		if ca, caErr := egress.NewCA(); caErr == nil {
			r.egressCA = ca
		}
	}
	egressCA := r.egressCA
	upstreamCAPEM := append([]byte(nil), r.egressUpstreamCAPEM...)
	existingLease := r.volumeNFSLeases[name]
	if existingLease == "" && r.boxAnnotations[name] != nil {
		existingLease = r.boxAnnotations[name][VolumeNFSLeaseAnnotation]
	}
	existingRV := r.boxResourceVersions[name]
	r.mu.Unlock()

	if exists && kc != nil {
		if existingBox, getErr := kc.GetPodBox(ctx, ns, name); getErr == nil && existingBox != nil {
			r.mu.Lock()
			if existingBox.Metadata.ResourceVersion != "" {
				existingRV = existingBox.Metadata.ResourceVersion
				if r.boxResourceVersions == nil {
					r.boxResourceVersions = make(map[string]string)
				}
				r.boxResourceVersions[name] = existingRV
			}
			if existingBox.Metadata.Annotations != nil {
				r.mergeBoxAnnotationsLocked(name, existingBox.Metadata.Annotations)
				if pinned := existingBox.Metadata.Annotations["cubepod.io/resolved-image"]; pinned != "" && r.resolvedImages[name] == "" {
					r.resolvedImages[name] = pinned
					effectiveImage = pinned
				}
				if existingLease == "" {
					existingLease = existingBox.Metadata.Annotations[VolumeNFSLeaseAnnotation]
				}
			}
			r.mu.Unlock()
		}
	}

	tpl := resolved.TemplateSpec
	tpl.Image = effectiveImage
	tpl.StartCmd = resolved.StartCmd
	tpl.ReadyCmd = resolved.ReadyCmd
	tpl.RunAsUser = resolved.RunAsUser
	tpl.RunAsGroup = resolved.RunAsGroup
	tpl.RunAsUsername = resolved.RunAsUsername
	if resolved.CPUCount > 0 {
		tpl.Resources.CPULimit = fmt.Sprintf("%d", resolved.CPUCount)
	}
	if resolved.MemoryMB > 0 {
		tpl.Resources.MemoryLimit = fmt.Sprintf("%dMi", resolved.MemoryMB)
	}
	if resolved.DiskSizeMB > 0 {
		tpl.Storage.RootfsSize = fmt.Sprintf("%dMi", resolved.DiskSizeMB)
	}
	r.applyTemplateDefaults(&tpl)

	createdCold := false
	claimedFromWarmPool := false
	var createdForkSnapshots []string
	var coldEgressPrepared bool

	opName := "reconcile"
	if !exists {
		if spec.WarmPoolRef != "" && spec.ForkFrom == nil && spec.VolumeKey == "" && len(spec.VolumeMounts) == 0 && spec.NetworkPolicy == nil {
			opName = "warm_claim"
		} else {
			opName = "cold_start"
		}
	} else if spec.DesiredState == v1alpha1.DesiredStatePaused {
		opName = "pause"
	} else if prevPhase == v1alpha1.PodBoxPhasePaused {
		opName = "resume"
	}
	timings.Operation = opName

	prepareStart := time.Now()
	if !exists {
		// 1. Pre-Attached WarmPool Claim (when warmPoolRef is set, no ForkFrom,
		// no deterministic VolumeKey, no custom NetworkPolicy, and no volume mounts).
		if spec.WarmPoolRef != "" && spec.ForkFrom == nil && spec.VolumeKey == "" && len(spec.VolumeMounts) == 0 && spec.NetworkPolicy == nil {
			entry, err := r.ClaimWarmPod(ctx, spec.WarmPoolRef, name)
			if err != nil {
				return nil, timings, fmt.Errorf("failed to claim from warm pool %q for box %q: %w", spec.WarmPoolRef, name, err)
			}
			claimedFromWarmPool = true
			localStatus = v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseRunning,
				PodName:           entry.PodName,
				PodIP:             entry.PodIP,
				RootfsPVCName:     entry.RootfsPVCName,
				WorkspacePVCName:  entry.WorkspacePVCName,
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "WarmPoolClaimed",
				Message:           fmt.Sprintf("Claimed standby warm pod %s from pool %s", entry.ID, spec.WarmPoolRef),
			}
		} else if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
			// 2. CoW Fork provisioning from parent sandbox
			if err := ValidateVolumeMounts(spec.VolumeMounts); err != nil && spec.DesiredState != v1alpha1.DesiredStatePaused {
				return nil, timings, fmt.Errorf("invalid volume mounts: %w", err)
			}
			createdCold = true
			parentRootfsPVC, parentWorkspacePVC := r.resolveForkParentPVCs(ctx, spec.ForkFrom.PodBoxName)
			if parentRootfsPVC == "" {
				parentRootfsPVC = fmt.Sprintf("%s-rootfs-pvc", spec.ForkFrom.PodBoxName)
			}
			if parentWorkspacePVC == "" {
				parentWorkspacePVC = fmt.Sprintf("%s-workspace-pvc", spec.ForkFrom.PodBoxName)
			}
			rootfsName := fmt.Sprintf("%s-rootfs-pvc", name)
			if spec.RootfsGeneration > 1 {
				rootfsName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
			}
			localStatus = v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseProvisioning,
				PodName:           name,
				RootfsPVCName:     rootfsName,
				WorkspacePVCName:  WorkspacePVCName(name, spec),
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "ForkCloned",
				Message:           fmt.Sprintf("Forked from parent sandbox %s", spec.ForkFrom.PodBoxName),
			}
			var rootfsPVC, wsPVC BlockPVCManifest
			if forkMode == "snapshot" {
				if strings.EqualFold(volumeMode, "Filesystem") {
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("volumeSnapshot fork mode requires volumeMode=Block, not %q", volumeMode)
				}
				rootfsSnapName := forkSnapshotName(name, "-rootfs-snap")
				wsSnapName := forkSnapshotName(name, "-workspace-snap")
				var snapClassPtr *string
				if volumeSnapshotClass != "" {
					sc := volumeSnapshotClass
					snapClassPtr = &sc
				}
				snapLabels := map[string]string{
					"app.kubernetes.io/name": "cubepod",
					"pod.cubeplex.ai/box":    SanitizeLabelValue(name),
					"cubepod.io/forked-from": SanitizeLabelValue(spec.ForkFrom.PodBoxName),
				}
				rootfsSnap := VolumeSnapshot{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshot",
					Metadata: v1alpha1.ObjectMeta{
						Name:      rootfsSnapName,
						Namespace: ns,
						Labels:    snapLabels,
					},
					Spec: VolumeSnapshotSpec{
						VolumeSnapshotClassName: snapClassPtr,
						Source: VolumeSnapshotSource{
							PersistentVolumeClaimName: parentRootfsPVC,
						},
					},
				}
				wsSnap := VolumeSnapshot{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshot",
					Metadata: v1alpha1.ObjectMeta{
						Name:      wsSnapName,
						Namespace: ns,
						Labels:    snapLabels,
					},
					Spec: VolumeSnapshotSpec{
						VolumeSnapshotClassName: snapClassPtr,
						Source: VolumeSnapshotSource{
							PersistentVolumeClaimName: parentWorkspacePVC,
						},
					},
				}

				var snapRootfsErr, snapWSErr error
				_ = runParallel(
					func() error {
						snapRootfsErr = kc.EnsureVolumeSnapshot(ctx, rootfsSnap)
						return nil
					},
					func() error {
						snapWSErr = kc.EnsureVolumeSnapshot(ctx, wsSnap)
						return nil
					},
				)
				if snapRootfsErr == nil {
					createdForkSnapshots = append(createdForkSnapshots, rootfsSnapName)
				}
				if snapWSErr == nil {
					createdForkSnapshots = append(createdForkSnapshots, wsSnapName)
				}
				if snapRootfsErr != nil {
					cleanupForkSnapshotsWithClient(ctx, kc, ns, append(createdForkSnapshots, rootfsSnapName)...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("failed to create rootfs VolumeSnapshot %s: %w", rootfsSnapName, snapRootfsErr)
				}
				if snapWSErr != nil {
					cleanupForkSnapshotsWithClient(ctx, kc, ns, append(createdForkSnapshots, wsSnapName)...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("failed to create workspace VolumeSnapshot %s: %w", wsSnapName, snapWSErr)
				}

				var waitRootfsErr, waitWSErr error
				_ = runParallel(
					func() error {
						waitRootfsErr = kc.WaitVolumeSnapshotReady(ctx, ns, rootfsSnapName)
						return nil
					},
					func() error {
						waitWSErr = kc.WaitVolumeSnapshotReady(ctx, ns, wsSnapName)
						return nil
					},
				)
				if waitRootfsErr != nil {
					cleanupForkSnapshotsWithClient(ctx, kc, ns, createdForkSnapshots...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("rootfs VolumeSnapshot %s not ready: %w", rootfsSnapName, waitRootfsErr)
				}
				if waitWSErr != nil {
					cleanupForkSnapshotsWithClient(ctx, kc, ns, createdForkSnapshots...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("workspace VolumeSnapshot %s not ready: %w", wsSnapName, waitWSErr)
				}

				rootfsPVC, wsPVC = BuildBlockPVCsWithOptions(name, tpl, spec, BlockPVCOptions{
					VolumeMode:            volumeMode,
					RootfsSnapshotName:    rootfsSnapName,
					WorkspaceSnapshotName: wsSnapName,
				})
				rootfsPVC.Namespace = ns
				wsPVC.Namespace = ns

				var pvcRootfsErr, pvcWSErr, secErr error
				_ = runParallel(
					func() error {
						pvcRootfsErr = kc.EnsurePVC(ctx, rootfsPVC)
						return nil
					},
					func() error {
						pvcWSErr = kc.EnsurePVC(ctx, wsPVC)
						return nil
					},
					func() error {
						if tpl.Egress.Enabled && spec.DesiredState != v1alpha1.DesiredStatePaused {
							secErr = ensureEgressSecretsWithClient(ctx, kc, ns, egressCA, upstreamCAPEM, name, spec, true)
							if secErr == nil {
								coldEgressPrepared = true
							}
						}
						return nil
					},
				)
				if pvcRootfsErr != nil {
					if pvcWSErr == nil {
						_ = kc.DeletePVC(ctx, ns, wsPVC.Name)
					}
					if coldEgressPrepared {
						deleteEgressSecretsWithClient(ctx, kc, ns, name)
					}
					cleanupForkSnapshotsWithClient(ctx, kc, ns, createdForkSnapshots...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("failed to ensure forked rootfs PVC %s: %w", rootfsPVC.Name, pvcRootfsErr)
				}
				if pvcWSErr != nil {
					_ = kc.DeletePVC(ctx, ns, rootfsPVC.Name)
					if coldEgressPrepared {
						deleteEgressSecretsWithClient(ctx, kc, ns, name)
					}
					cleanupForkSnapshotsWithClient(ctx, kc, ns, createdForkSnapshots...)
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("failed to ensure forked workspace PVC %s: %w", wsPVC.Name, pvcWSErr)
				}
				if secErr != nil {
					r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
					return nil, timings, fmt.Errorf("failed to ensure egress secrets: %w", secErr)
				}
			} else {
				rootfsPVC, wsPVC = BuildBlockPVCs(name, tpl, spec, volumeMode, parentRootfsPVC, parentWorkspacePVC)
				rootfsPVC.Namespace = ns
				wsPVC.Namespace = ns
				var secErr error
				_ = runParallel(
					func() error {
						if kc != nil {
							_ = kc.EnsurePVC(ctx, rootfsPVC)
						}
						return nil
					},
					func() error {
						if kc != nil {
							_ = kc.EnsurePVC(ctx, wsPVC)
						}
						return nil
					},
					func() error {
						if tpl.Egress.Enabled && spec.DesiredState != v1alpha1.DesiredStatePaused {
							secErr = ensureEgressSecretsWithClient(ctx, kc, ns, egressCA, upstreamCAPEM, name, spec, true)
							if secErr == nil {
								coldEgressPrepared = true
							}
						}
						return nil
					},
				)
				if secErr != nil {
					r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
					return nil, timings, fmt.Errorf("failed to ensure egress secrets: %w", secErr)
				}
			}
		} else {
			// 3. Standard fresh sandbox provisioning
			if spec.DesiredState != v1alpha1.DesiredStatePaused {
				if err := ValidateVolumeMounts(spec.VolumeMounts); err != nil {
					r.mu.Lock()
					delete(r.resolvedImages, name)
					r.mu.Unlock()
					return nil, timings, fmt.Errorf("invalid volume mounts: %w", err)
				}
			}
			createdCold = true
			rootfsName := fmt.Sprintf("%s-rootfs-pvc", name)
			if spec.RootfsGeneration > 1 {
				rootfsName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
			}
			localStatus = v1alpha1.PodBoxStatus{
				Phase:             v1alpha1.PodBoxPhaseProvisioning,
				RootfsPVCName:     rootfsName,
				WorkspacePVCName:  WorkspacePVCName(name, spec),
				ObservedRootfsGen: spec.RootfsGeneration,
				LastActivityAt:    &now,
				Reason:            "Provisioned",
				Message:           "Provisioned fresh sandbox",
			}
			rootfsPVC, wsPVC := BuildBlockPVCs(name, tpl, spec, volumeMode)
			rootfsPVC.Namespace = ns
			wsPVC.Namespace = ns

			var secErr error
			_ = runParallel(
				func() error {
					if kc != nil {
						_ = kc.EnsurePVC(ctx, rootfsPVC)
					}
					return nil
				},
				func() error {
					if kc != nil && spec.WorkspaceBlockMount() == nil {
						_ = kc.EnsurePVC(ctx, wsPVC)
					}
					return nil
				},
				func() error {
					if tpl.Egress.Enabled && spec.DesiredState != v1alpha1.DesiredStatePaused {
						secErr = ensureEgressSecretsWithClient(ctx, kc, ns, egressCA, upstreamCAPEM, name, spec, true)
						if secErr == nil {
							coldEgressPrepared = true
						}
					}
					return nil
				},
			)
			if secErr != nil {
				r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
				return nil, timings, fmt.Errorf("failed to ensure egress secrets: %w", secErr)
			}
		}
	}

	// Handle independent RootFS reset while keeping Workspace PVC intact.
	didResetRootFS := false
	if spec.RootfsGeneration > localStatus.ObservedRootfsGen {
		didResetRootFS = true
		slog.Info("resetting RootFS Upper PVC while preserving Workspace PVC",
			"podBox", name,
			"oldGen", localStatus.ObservedRootfsGen,
			"newGen", spec.RootfsGeneration,
			"workspacePVC", localStatus.WorkspacePVCName,
		)
		oldRootfsPVC := localStatus.RootfsPVCName
		localStatus.ObservedRootfsGen = spec.RootfsGeneration
		localStatus.RootfsPVCName = fmt.Sprintf("%s-rootfs-gen%d-pvc", name, spec.RootfsGeneration)
		localStatus.Reason = "RootFSReset"
		localStatus.Message = fmt.Sprintf("RootFS reset to generation %d", spec.RootfsGeneration)

		// Delete old compute Pod first so it unmounts the old RootFS PVC and allows a new Pod to be created.
		if localStatus.PodName != "" {
			r.revokeVolumeNFSLease(ctx, name, "")
			if kc != nil {
				_ = kc.DeletePod(ctx, ns, localStatus.PodName)
			}
			localStatus.PodName = ""
			localStatus.PodIP = ""
		}

		// Delete old RootFS PVC and create new RootFS PVC in parallel.
		newRootfsPVC, _ := BuildBlockPVCs(name, tpl, spec, volumeMode)
		newRootfsPVC.Namespace = ns
		_ = runParallel(
			func() error {
				if oldRootfsPVC != "" && kc != nil {
					_ = kc.DeletePVC(ctx, ns, oldRootfsPVC)
				}
				return nil
			},
			func() error {
				if kc != nil {
					_ = kc.EnsurePVC(ctx, newRootfsPVC)
				}
				return nil
			},
		)
	}

	switch spec.DesiredState {
	case v1alpha1.DesiredStatePaused:
		r.revokeVolumeNFSLease(ctx, name, "")
		if localStatus.PodName != "" && kc != nil {
			delStart := time.Now()
			_ = kc.DeletePod(ctx, ns, localStatus.PodName)
			timings.DeletePodDur = time.Since(delStart)
			if localStatus.PodName != name {
				// Clean up claimed warm pod's one-time egress secrets; retain <name>-egress-* for fast resume.
				deleteEgressSecretsWithClient(ctx, kc, ns, localStatus.PodName)
			}
		}
		spec.Lifecycle.LeaseExpiresAt = nil
		localStatus.Phase = v1alpha1.PodBoxPhasePaused
		localStatus.PodName = ""
		localStatus.PodIP = ""
		localStatus.PausedAt = &now
	default:
		if !createdCold {
			if err := ValidateVolumeMounts(spec.VolumeMounts); err != nil {
				if didResetRootFS {
					r.persistFailedTransition(ctx, name, spec, &localStatus, localStatus.PodName, effectiveImage, now)
				}
				return nil, timings, fmt.Errorf("invalid volume mounts: %w", err)
			}
		}
		if claimedFromWarmPool && localStatus.PodName != "" && localStatus.PodIP != "" {
			localStatus.Phase = v1alpha1.PodBoxPhaseRunning
			localStatus.LastActivityAt = &now
			localStatus.PausedAt = nil
			timings.PrepareDur = time.Since(prepareStart)
			break
		}

		blockMount := spec.WorkspaceBlockMount()
		var secErr, fenceErr error
		_ = runParallel(
			func() error {
				if tpl.Egress.Enabled && !coldEgressPrepared {
					secErr = ensureEgressSecretsWithClient(ctx, kc, ns, egressCA, upstreamCAPEM, name, spec, false)
				}
				return nil
			},
			func() error {
				if blockMount != nil && kc != nil {
					fenceErr = verifyWorkspaceBlockLeaseWithClient(ctx, kc, ns, name, blockMount.PVCName)
				}
				return nil
			},
		)
		if secErr != nil {
			if createdCold {
				r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
			} else if didResetRootFS {
				r.persistFailedTransition(ctx, name, spec, &localStatus, localStatus.PodName, effectiveImage, now)
			}
			return nil, timings, fmt.Errorf("failed to ensure egress secrets: %w", secErr)
		}
		// CEP-0030 fencing runs before any CEP-0031 NFS lease is minted so a lost
		// block lease cannot leak an NFS lease or mark the box Running.
		if fenceErr != nil {
			localStatus.Phase = v1alpha1.PodBoxPhaseFailed
			localStatus.PodName = ""
			localStatus.PodIP = ""
			localStatus.Reason = ReasonWorkspaceVolumeLeaseLost
			localStatus.Message = fenceErr.Error()
			spec.Lifecycle.LeaseExpiresAt = nil

			r.mu.Lock()
			r.boxes[name] = &localStatus
			r.specs[name] = spec
			anns := r.podBoxAnnotationsLocked(name, effectiveImage)
			rv := r.boxResourceVersions[name]
			r.mu.Unlock()

			deleteEgressSecretsWithClient(ctx, kc, ns, name)
			if createdCold && localStatus.RootfsPVCName != "" && kc != nil {
				_ = kc.DeletePVC(ctx, ns, localStatus.RootfsPVCName)
			}
			if kc != nil {
				_ = kc.UpsertPodBox(ctx, v1alpha1.PodBox{
					APIVersion: "cubepod.io/v1alpha1",
					Kind:       "PodBox",
					Metadata: v1alpha1.ObjectMeta{
						Name:            name,
						Namespace:       ns,
						ResourceVersion: rv,
						Annotations:     anns,
					},
					Spec:   spec,
					Status: localStatus,
				})
			}
			return nil, timings, fenceErr
		}

		targetPodName := localStatus.PodName
		if targetPodName == "" {
			targetPodName = name
		}
		sharedMounts := spec.SharedVolumeMounts()
		var fc bool
		var fcErr error
		if len(sharedMounts) > 0 {
			fc, fcErr = r.usesFirecrackerRuntime(ctx, tpl)
			if fcErr != nil {
				if createdCold {
					r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
				} else if didResetRootFS {
					r.persistFailedTransition(ctx, name, spec, &localStatus, localStatus.PodName, effectiveImage, now)
				}
				return nil, timings, fcErr
			}
		}
		reusingPod := !createdCold && !didResetRootFS && strings.TrimSpace(localStatus.PodName) != "" && strings.TrimSpace(localStatus.PodIP) != ""
		if reusingPod && kc != nil {
			phase, err := kc.GetPodPhase(ctx, ns, localStatus.PodName)
			if err != nil && !errors.Is(err, ErrPodNotFound) {
				return nil, timings, fmt.Errorf("failed to inspect pod phase: %w", err)
			}
			if errors.Is(err, ErrPodNotFound) || phase == "Failed" || phase == "Succeeded" {
				r.revokeVolumeNFSLease(ctx, name, existingLease)
				existingLease = ""
				if localStatus.PodName != "" {
					cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
					_ = kc.DeletePod(cleanupCtx, ns, localStatus.PodName)
					cleanupCancel()
				}
				localStatus.PodIP = ""
				reusingPod = false
			}
		}
		var nfsOpts VolumeNFSOptions
		var leaseID string
		var nfsErr error
		if !reusingPod {
			nfsOpts, leaseID, nfsErr = r.prepareVolumeNFS(ctx, name, targetPodName, tpl, sharedMounts)
		}
		if nfsErr != nil {
			if createdCold {
				r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
			} else if didResetRootFS {
				r.persistFailedTransition(ctx, name, spec, &localStatus, localStatus.PodName, effectiveImage, now)
			}
			return nil, timings, nfsErr
		}
		if fc && len(sharedMounts) > 0 && leaseID == "" && !reusingPod {
			err := fmt.Errorf("Firecracker official volumeMounts require a volume NFS lease")
			if createdCold {
				r.rollbackColdCreate(ctx, name, spec, &localStatus, "")
			} else if didResetRootFS {
				r.persistFailedTransition(ctx, name, spec, &localStatus, localStatus.PodName, effectiveImage, now)
			}
			return nil, timings, err
		}
		timings.PrepareDur = time.Since(prepareStart)

		podConflictUnconfirmed := false
		if reusingPod {
			localStatus.Phase = v1alpha1.PodBoxPhaseRunning
			localStatus.LastActivityAt = &now
			localStatus.PausedAt = nil
		} else {
			if leaseID != "" {
				r.rememberVolumeNFSLease(name, leaseID)
			}
			localStatus.Phase = v1alpha1.PodBoxPhaseRunning
			if localStatus.PodName == "" {
				localStatus.PodName = name
			}
			podManifest := BuildPodManifest(name, tpl, spec, localStatus, PodBuildOptions{
				VolumeMode:        volumeMode,
				ImagePullSecrets:  pullSecrets,
				TransparentEgress: transparentEgress,
				VolumeNFS:         nfsOpts,
				IsFirecracker:     fc,
			})
			podManifest.Namespace = ns

			ensureStart := time.Now()
			podIP, err := r.ensurePodMaybeWithNFS(ctx, podManifest, leaseID, sharedMounts, name)
			timings.EnsurePodDur = time.Since(ensureStart)
			if err != nil {
				errStr := strings.ToLower(err.Error())
				if strings.Contains(errStr, "409") || strings.Contains(errStr, "already exists") || strings.Contains(errStr, "conflict") {
					if leaseID != "" {
						r.revokeVolumeNFSLeaseID(ctx, name, leaseID)
						if existingLease != "" {
							r.rememberVolumeNFSLease(name, existingLease)
						}
						leaseID = existingLease
					}
					err = nil
					podConflictUnconfirmed = podIP == ""
				} else {
					r.revokeVolumeNFSLease(ctx, name, leaseID)
					if !createdCold && podManifest.Name != "" && kc != nil {
						cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
						_ = kc.DeletePod(cleanupCtx, ns, podManifest.Name)
						cleanupCancel()
						localStatus.PodName = ""
						localStatus.PodIP = ""
					}
					if createdCold {
						r.rollbackColdCreate(ctx, name, spec, &localStatus, podManifest.Name)
					} else {
						r.persistFailedTransition(ctx, name, spec, &localStatus, "", effectiveImage, now)
					}
					return nil, timings, fmt.Errorf("failed to ensure Pod: %w", err)
				}
			}
			if podIP != "" {
				localStatus.PodIP = podIP
			}
			if podConflictUnconfirmed && localStatus.PodIP == "" {
				localStatus.Phase = v1alpha1.PodBoxPhaseProvisioning
			} else {
				localStatus.Phase = v1alpha1.PodBoxPhaseRunning
			}
			localStatus.PodName = podManifest.Name
			localStatus.LastActivityAt = &now
			localStatus.PausedAt = nil
		}
		localStatus.PausedAt = nil
		if len(createdForkSnapshots) > 0 && !podConflictUnconfirmed {
			cleanupForkSnapshotsWithClient(ctx, kc, ns, createdForkSnapshots...)
		}
	}

	cp := localStatus
	r.mu.Lock()
	r.boxes[name] = &cp
	r.specs[name] = spec
	anns := r.podBoxAnnotationsLocked(name, effectiveImage)
	if anns != nil {
		r.boxAnnotations[name] = maps.Clone(anns)
	}
	rv := r.boxResourceVersions[name]
	r.mu.Unlock()

	box := v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: rv,
			Annotations:     anns,
		},
		Spec:   spec,
		Status: cp,
	}

	upsertStart := time.Now()
	_ = runParallel(
		func() error {
			if kc != nil {
				_ = kc.UpsertPodBox(ctx, box)
			}
			return nil
		},
		func() error {
			r.recordPhaseTransition(ctx, name, prevPhase, cp)
			return nil
		},
	)
	timings.UpsertBoxDur = time.Since(upsertStart)
	timings.TotalDur = time.Since(totalStart)
	forkResult = "ok"

	if opName == "cold_start" || opName == "resume" || opName == "warm_claim" || opName == "pause" {
		if timings.PrepareDur > 0 {
			r.phaseMetrics.observe(opName, "pvc_egress_prepare", timings.PrepareDur)
		}
		if timings.EnsurePodDur > 0 {
			r.phaseMetrics.observe(opName, "ensure_pod", timings.EnsurePodDur)
		}
		if timings.DeletePodDur > 0 {
			r.phaseMetrics.observe(opName, "delete_pod", timings.DeletePodDur)
		}
		r.phaseMetrics.observe(opName, "podbox_upsert", timings.UpsertBoxDur)
		r.phaseMetrics.observe(opName, "total", timings.TotalDur)
	}

	return &cp, timings, nil
}

func (r *Reconciler) persistFailedTransition(
	ctx context.Context,
	name string,
	spec v1alpha1.PodBoxSpec,
	status *v1alpha1.PodBoxStatus,
	podName string,
	effectiveImage string,
	now time.Time,
) {
	// Persist in-progress state transitions (e.g. rootfsGeneration bump + new PVC)
	// even when ensuring egress secrets or waiting for the replacement Pod fails.
	status.Phase = v1alpha1.PodBoxPhaseProvisioning
	status.PodName = podName
	status.PodIP = ""
	status.LastActivityAt = &now
	cpErr := *status

	r.mu.Lock()
	r.boxes[name] = &cpErr
	r.specs[name] = spec
	annsErr := r.podBoxAnnotationsLocked(name, effectiveImage)
	if annsErr != nil {
		r.boxAnnotations[name] = maps.Clone(annsErr)
	}
	rv := r.boxResourceVersions[name]
	kc := r.kubeClient
	ns := r.namespace
	r.mu.Unlock()

	if kc == nil {
		return
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = kc.UpsertPodBox(persistCtx, v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: rv,
			Annotations:     annsErr,
		},
		Spec:   spec,
		Status: cpErr,
	})
}

func (r *Reconciler) mergeBoxAnnotationsLocked(name string, incoming map[string]string) {
	if len(incoming) == 0 {
		return
	}
	cur := r.boxAnnotations[name]
	if cur == nil {
		cur = make(map[string]string, len(incoming))
	}
	for k, v := range incoming {
		cur[k] = v
	}
	r.boxAnnotations[name] = cur
}

// SetBoxAnnotations merges annotations for PodBox name so subsequent Reconcile and Pause
// calls persist them on PodBox.Metadata.Annotations.
func (r *Reconciler) SetBoxAnnotations(name string, annotations map[string]string) {
	if name == "" || len(annotations) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mergeBoxAnnotationsLocked(name, annotations)
}

func (r *Reconciler) podBoxAnnotationsLocked(name, fallbackImage string) map[string]string {
	anns := make(map[string]string)
	for k, v := range r.boxAnnotations[name] {
		anns[k] = v
	}

	img := r.resolvedImages[name]
	if img == "" {
		img = fallbackImage
	}
	if img != "" {
		anns["cubepod.io/resolved-image"] = img
	}

	if spec, ok := r.specs[name]; ok {
		templateID := spec.TemplateRef
		if templateID == "" {
			templateID = "base"
		}
		anns["cubepod.io/template-id"] = templateID

		if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
			anns["cubepod.io/forked-from"] = spec.ForkFrom.PodBoxName
		} else {
			delete(anns, "cubepod.io/forked-from")
		}

		if spec.VolumeKey != "" {
			anns["cubepod.io/volume-key"] = spec.VolumeKey
		} else {
			delete(anns, "cubepod.io/volume-key")
		}

		gen := spec.RootfsGeneration
		if st, ok := r.boxes[name]; ok && st != nil && st.ObservedRootfsGen > gen {
			gen = st.ObservedRootfsGen
		}
		if gen <= 0 {
			gen = 1
		}
		anns["cubepod.io/rootfs-generation"] = strconv.FormatInt(gen, 10)
	}

	if len(anns) == 0 {
		return nil
	}
	return anns
}

func (r *Reconciler) resolveForkParentPVCs(ctx context.Context, parentName string) (rootfsPVC, workspacePVC string) {
	r.mu.RLock()
	if parentStatus, ok := r.boxes[parentName]; ok && parentStatus != nil {
		rootfsPVC = parentStatus.RootfsPVCName
		workspacePVC = parentStatus.WorkspacePVCName
		if rootfsPVC == "" && parentStatus.ObservedRootfsGen > 1 {
			rootfsPVC = fmt.Sprintf("%s-rootfs-gen%d-pvc", parentName, parentStatus.ObservedRootfsGen)
		}
	}
	kc := r.kubeClient
	ns := r.namespace
	r.mu.RUnlock()

	if (rootfsPVC == "" || workspacePVC == "") && kc != nil {
		if parentBox, err := kc.GetPodBox(ctx, ns, parentName); err == nil && parentBox != nil {
			if rootfsPVC == "" {
				rootfsPVC = parentBox.Status.RootfsPVCName
				if rootfsPVC == "" {
					gen := parentBox.Spec.RootfsGeneration
					if parentBox.Status.ObservedRootfsGen > gen {
						gen = parentBox.Status.ObservedRootfsGen
					}
					if gen > 1 {
						rootfsPVC = fmt.Sprintf("%s-rootfs-gen%d-pvc", parentName, gen)
					}
				}
			}
			if workspacePVC == "" {
				workspacePVC = parentBox.Status.WorkspacePVCName
			}
		}
	}
	return rootfsPVC, workspacePVC
}

func isSecretNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

func isSecretAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "409") || strings.Contains(msg, "conflict")
}

func ensureSecretWithClient(ctx context.Context, kc KubeClient, defaultNS string, secret KubeSecret, maybeExists bool) error {
	if kc == nil {
		return nil
	}
	ns := secret.Metadata.Namespace
	if ns == "" {
		ns = defaultNS
		secret.Metadata.Namespace = ns
	}
	if maybeExists {
		if delErr := kc.DeleteSecret(ctx, ns, secret.Metadata.Name); delErr != nil && !isSecretNotFoundError(delErr) {
			return delErr
		}
	}
	err := kc.CreateSecret(ctx, secret)
	if isSecretAlreadyExistsError(err) {
		if delErr := kc.DeleteSecret(ctx, ns, secret.Metadata.Name); delErr != nil && !isSecretNotFoundError(delErr) {
			return delErr
		}
		return kc.CreateSecret(ctx, secret)
	}
	return err
}

func generateMITMCAPEM(boxName string) (certPEM, keyPEM []byte, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate MITM CA key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate MITM CA serial: %w", err)
	}
	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("CubePod MITM CA (%s)", boxName),
			Organization: []string{"CubePod"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create MITM CA certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal MITM CA private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func serializeNetworkPolicy(policy *v1alpha1.NetworkPolicySpec) ([]byte, error) {
	if policy == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(policy)
}

func isEgressCertValid(certPEM []byte, ca *egress.CA, expectedCN string, margin time.Duration) bool {
	if len(certPEM) == 0 || ca == nil {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Add(margin).Before(cert.NotAfter) {
		return false
	}
	if expectedCN != "" && cert.Subject.CommonName != expectedCN {
		return false
	}
	if _, err := ca.VerifySandboxCert(certPEM); err != nil {
		return false
	}
	return true
}

func isMITMCACertValid(certPEM []byte, margin time.Duration) bool {
	if len(certPEM) == 0 {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	now := time.Now()
	return !now.Before(cert.NotBefore) && now.Add(margin).Before(cert.NotAfter)
}

func ensureEgressSecretsWithClient(
	ctx context.Context,
	kc KubeClient,
	ns string,
	egressCA *egress.CA,
	upstreamCAPEM []byte,
	boxName string,
	spec v1alpha1.PodBoxSpec,
	skipExistingCheck bool,
) error {
	if kc == nil {
		return nil
	}
	if egressCA == nil {
		ca, err := egress.NewCA()
		if err != nil {
			return fmt.Errorf("failed to initialize egress CA: %w", err)
		}
		egressCA = ca
	}

	policyJSON, err := serializeNetworkPolicy(spec.NetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to serialize network policy: %w", err)
	}

	mtlsSecretName := fmt.Sprintf("%s-egress-mtls", boxName)
	caSecretName := fmt.Sprintf("%s-egress-ca", boxName)
	labels := map[string]string{
		"app.kubernetes.io/name": "cubepod",
		"pod.cubeplex.ai/box":    boxName,
	}

	if !skipExistingCheck {
		var existingMtls, existingCA *KubeSecret
		var mtlsErr, caErr error
		_ = runParallel(
			func() error {
				existingMtls, mtlsErr = kc.GetSecret(ctx, ns, mtlsSecretName)
				return nil
			},
			func() error {
				existingCA, caErr = kc.GetSecret(ctx, ns, caSecretName)
				return nil
			},
		)
		if mtlsErr == nil && existingMtls != nil && caErr == nil && existingCA != nil {
			d := existingMtls.Data
			if len(d["tls.crt"]) > 0 && len(d["tls.key"]) > 0 && len(d["mitm-ca.crt"]) > 0 && len(d["mitm-ca.key"]) > 0 &&
				bytes.Equal(d["ca.crt"], egressCA.CACertPEM()) &&
				bytes.Equal(existingCA.Data["mitm-ca.crt"], d["mitm-ca.crt"]) &&
				bytes.Equal(d["upstream-ca.crt"], upstreamCAPEM) &&
				isEgressCertValid(d["tls.crt"], egressCA, boxName, egressCertRotationMargin) &&
				isMITMCACertValid(d["mitm-ca.crt"], egressCertRotationMargin) {
				if bytes.Equal(d["policy.json"], policyJSON) {
					return nil
				}
				updatedData := map[string][]byte{
					"tls.crt":     d["tls.crt"],
					"tls.key":     d["tls.key"],
					"ca.crt":      d["ca.crt"],
					"mitm-ca.crt": d["mitm-ca.crt"],
					"mitm-ca.key": d["mitm-ca.key"],
					"policy.json": policyJSON,
				}
				if len(upstreamCAPEM) > 0 {
					updatedData["upstream-ca.crt"] = upstreamCAPEM
				}
				updatedMtls := KubeSecret{
					APIVersion: "v1",
					Kind:       "Secret",
					Metadata: v1alpha1.ObjectMeta{
						Name:      mtlsSecretName,
						Namespace: ns,
						Labels:    labels,
					},
					Type: "Opaque",
					Data: updatedData,
				}
				return ensureSecretWithClient(ctx, kc, ns, updatedMtls, true)
			}
		}
	}

	tlsCertPEM, tlsKeyPEM, err := egressCA.MintSandboxCert(boxName, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("failed to mint sandbox mTLS cert: %w", err)
	}
	mitmCertPEM, mitmKeyPEM, err := generateMITMCAPEM(boxName)
	if err != nil {
		return err
	}

	mtlsData := map[string][]byte{
		"tls.crt":     tlsCertPEM,
		"tls.key":     tlsKeyPEM,
		"ca.crt":      egressCA.CACertPEM(),
		"mitm-ca.crt": mitmCertPEM,
		"mitm-ca.key": mitmKeyPEM,
		"policy.json": policyJSON,
	}
	if len(upstreamCAPEM) > 0 {
		mtlsData["upstream-ca.crt"] = upstreamCAPEM
	}
	mtlsSecret := KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: v1alpha1.ObjectMeta{
			Name:      mtlsSecretName,
			Namespace: ns,
			Labels:    labels,
		},
		Type: "Opaque",
		Data: mtlsData,
	}
	caSecret := KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: v1alpha1.ObjectMeta{
			Name:      caSecretName,
			Namespace: ns,
			Labels:    labels,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"mitm-ca.crt": mitmCertPEM,
		},
	}

	var mtlsWriteErr, caWriteErr error
	_ = runParallel(
		func() error {
			mtlsWriteErr = ensureSecretWithClient(ctx, kc, ns, mtlsSecret, !skipExistingCheck)
			return nil
		},
		func() error {
			caWriteErr = ensureSecretWithClient(ctx, kc, ns, caSecret, !skipExistingCheck)
			return nil
		},
	)
	if mtlsWriteErr != nil {
		return fmt.Errorf("failed to ensure egress mTLS secret %s: %w", mtlsSecretName, mtlsWriteErr)
	}
	if caWriteErr != nil {
		return fmt.Errorf("failed to ensure egress CA secret %s: %w", caSecretName, caWriteErr)
	}
	return nil
}

func (r *Reconciler) rollbackColdCreate(ctx context.Context, name string, spec v1alpha1.PodBoxSpec, status *v1alpha1.PodBoxStatus, podName string) {
	r.mu.RLock()
	kc := r.kubeClient
	ns := r.namespace
	r.mu.RUnlock()

	if kc != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		r.revokeVolumeNFSLease(cleanupCtx, name, "")

		targetPod := podName
		if targetPod == "" && status != nil {
			targetPod = status.PodName
		}
		if targetPod == "" {
			targetPod = name
		}
		_ = kc.DeletePod(cleanupCtx, ns, targetPod)

		_ = runParallel(
			func() error {
				if status != nil && status.RootfsPVCName != "" {
					_ = kc.DeletePVC(cleanupCtx, ns, status.RootfsPVCName)
				}
				return nil
			},
			func() error {
				// Preserve deterministic user workspace PVC and managed /volumes block PVCs across failures.
				if status != nil && spec.VolumeKey == "" && spec.WorkspaceBlockMount() == nil && status.WorkspacePVCName != "" {
					_ = kc.DeletePVC(cleanupCtx, ns, status.WorkspacePVCName)
				}
				return nil
			},
			func() error {
				deleteEgressSecretsWithClient(cleanupCtx, kc, ns, name)
				return nil
			},
			func() error {
				if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
					cleanupForkSnapshotsWithClient(cleanupCtx, kc, ns,
						forkSnapshotName(name, "-rootfs-snap"),
						forkSnapshotName(name, "-workspace-snap"),
					)
				}
				return nil
			},
			func() error {
				_ = kc.DeletePodBox(cleanupCtx, ns, name)
				return nil
			},
		)
	}

	r.mu.Lock()
	delete(r.boxes, name)
	delete(r.specs, name)
	delete(r.resolvedImages, name)
	delete(r.boxAnnotations, name)
	delete(r.boxResourceVersions, name)
	delete(r.volumeNFSLeases, name)
	r.mu.Unlock()
}

func forkSnapshotName(boxName, suffix string) string {
	name := boxName + suffix
	if len(name) <= 63 {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(boxName))
	hashTag := fmt.Sprintf("-%08x", h.Sum32())
	maxPrefix := 63 - len(suffix) - len(hashTag)
	if maxPrefix <= 0 {
		trimmed := "s" + hashTag + suffix
		if len(trimmed) > 63 {
			return trimmed[:63]
		}
		return trimmed
	}
	prefix := strings.TrimRight(boxName[:maxPrefix], "-")
	if prefix == "" {
		prefix = "snap"
	}
	return prefix + hashTag + suffix
}

func cleanupForkSnapshotsWithClient(ctx context.Context, kc KubeClient, ns string, snapNames ...string) {
	if kc == nil || len(snapNames) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	fns := make([]func() error, 0, len(snapNames))
	for _, snapName := range snapNames {
		if snapName == "" {
			continue
		}
		sn := snapName
		fns = append(fns, func() error {
			if err := kc.DeleteVolumeSnapshot(cleanupCtx, ns, sn); err != nil && !isSecretNotFoundError(err) {
				slog.Warn("failed to clean up temporary fork VolumeSnapshot", "snapshot", sn, "err", err)
			}
			return nil
		})
	}
	_ = runParallel(fns...)
}

func deleteEgressSecretsWithClient(ctx context.Context, kc KubeClient, ns, boxName string) {
	if kc == nil || boxName == "" {
		return
	}
	_ = runParallel(
		func() error {
			secretName := fmt.Sprintf("%s-egress-mtls", boxName)
			if err := kc.DeleteSecret(ctx, ns, secretName); err != nil && !isSecretNotFoundError(err) {
				slog.Warn("failed to delete egress secret", "secret", secretName, "err", err)
			}
			return nil
		},
		func() error {
			secretName := fmt.Sprintf("%s-egress-ca", boxName)
			if err := kc.DeleteSecret(ctx, ns, secretName); err != nil && !isSecretNotFoundError(err) {
				slog.Warn("failed to delete egress secret", "secret", secretName, "err", err)
			}
			return nil
		},
	)
}

// Pause pauses a running PodBox by updating its DesiredState to Paused and reconciling.
func (r *Reconciler) Pause(ctx context.Context, name string) (*v1alpha1.PodBoxStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	spec, ok := r.specs[name]
	statusVal, statusOk := r.boxes[name]
	var obsGen int64
	if statusOk && statusVal != nil {
		obsGen = statusVal.ObservedRootfsGen
	}
	r.mu.RUnlock()
	if !ok && !statusOk {
		return nil, fmt.Errorf("podbox %s not found", name)
	}
	if !ok && statusOk {
		spec = v1alpha1.PodBoxSpec{
			DesiredState:     v1alpha1.DesiredStatePaused,
			RootfsGeneration: obsGen,
		}
	} else {
		spec.DesiredState = v1alpha1.DesiredStatePaused
	}

	return r.Reconcile(ctx, name, spec)
}

// MarkFailed transitions a PodBox to Failed in both Reconciler memory and Kubernetes,
// clearing any active lease so Failed sandboxes never retain an active lease.
func (r *Reconciler) MarkFailed(ctx context.Context, name, reason, message string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	unlockBox := r.acquireBoxLock(name)
	defer unlockBox()

	r.mu.Lock()
	status, ok := r.boxes[name]
	spec, hasSpec := r.specs[name]
	kc := r.kubeClient
	ns := r.namespace

	if ok && status != nil {
		if status.Phase == v1alpha1.PodBoxPhaseTerminated {
			r.mu.Unlock()
			return nil
		}
		if hasSpec {
			spec.Lifecycle.LeaseExpiresAt = nil
			r.specs[name] = spec
		}
		prevPhase := status.Phase
		cp := *status
		cp.Phase = v1alpha1.PodBoxPhaseFailed
		if reason != "" {
			cp.Reason = reason
		}
		if message != "" {
			cp.Message = message
		}
		r.boxes[name] = &cp
		anns := r.podBoxAnnotationsLocked(name, "")
		rv := r.boxResourceVersions[name]
		r.mu.Unlock()

		r.recordPhaseTransition(ctx, name, prevPhase, cp)
		if kc != nil {
			box := v1alpha1.PodBox{
				APIVersion: "cubepod.io/v1alpha1",
				Kind:       "PodBox",
				Metadata: v1alpha1.ObjectMeta{
					Name:            name,
					Namespace:       ns,
					ResourceVersion: rv,
					Annotations:     anns,
				},
				Spec:   spec,
				Status: cp,
			}
			if err := kc.UpsertPodBox(ctx, box); err != nil {
				slog.Warn("failed to persist failed state to Kubernetes", "name", name, "err", err)
				return err
			}
		}
		return nil
	}

	if hasSpec {
		spec.Lifecycle.LeaseExpiresAt = nil
		r.specs[name] = spec
	}
	r.mu.Unlock()

	if kc == nil {
		return fmt.Errorf("podbox %s not found", name)
	}
	box, err := kc.GetPodBox(ctx, ns, name)
	if err != nil || box == nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("podbox %s not found", name)
	}
	if box.Status.Phase == v1alpha1.PodBoxPhaseTerminated {
		return nil
	}
	box.Spec.Lifecycle.LeaseExpiresAt = nil
	box.Status.Phase = v1alpha1.PodBoxPhaseFailed
	if reason != "" {
		box.Status.Reason = reason
	}
	if message != "" {
		box.Status.Message = message
	}
	// Use UpsertPodBox rather than UpdatePodBoxStatus so both Spec.Lifecycle.LeaseExpiresAt=nil
	// and Status.Phase=Failed are persisted to the Kubernetes APIServer.
	return kc.UpsertPodBox(ctx, *box)
}

// DeleteBox terminates a PodBox compute Pod and optionally deletes its underlying PVCs.
func (r *Reconciler) DeleteBox(ctx context.Context, name string, deleteVolumes bool) error {
	unlockBox := r.acquireBoxLock(name)
	defer unlockBox()

	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	kc := r.kubeClient
	ns := r.namespace
	hasLeaseClient := r.volumeNFS.LeaseClient != nil
	statusPtr, ok := r.boxes[name]
	var statusCopy v1alpha1.PodBoxStatus
	if ok && statusPtr != nil {
		statusCopy = *statusPtr
	}
	spec, hasSpec := r.specs[name]
	r.mu.RUnlock()

	if kc != nil && hasLeaseClient {
		if box, err := kc.GetPodBox(ctx, ns, name); err == nil && box != nil && box.Metadata.Annotations != nil {
			if id := box.Metadata.Annotations[VolumeNFSLeaseAnnotation]; id != "" {
				r.revokeVolumeNFSLease(ctx, name, id)
			}
		}
	}
	r.revokeVolumeNFSLease(ctx, name, "")

	var forkSnapshots []string
	if hasSpec && spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
		forkSnapshots = []string{
			forkSnapshotName(name, "-rootfs-snap"),
			forkSnapshotName(name, "-workspace-snap"),
		}
	}

	if !ok {
		_ = runParallel(
			func() error {
				if kc != nil {
					_ = kc.DeletePodBox(ctx, ns, name)
				}
				return nil
			},
			func() error {
				deleteEgressSecretsWithClient(ctx, kc, ns, name)
				return nil
			},
			func() error {
				if len(forkSnapshots) > 0 {
					cleanupForkSnapshotsWithClient(ctx, kc, ns, forkSnapshots...)
				}
				return nil
			},
		)
		r.mu.Lock()
		delete(r.boxAnnotations, name)
		delete(r.boxResourceVersions, name)
		r.mu.Unlock()
		return nil
	}

	if statusCopy.PodName != "" && kc != nil {
		_ = kc.DeletePod(ctx, ns, statusCopy.PodName)
	}

	isManagedBlock := (hasSpec && spec.WorkspaceBlockMount() != nil) || strings.HasPrefix(statusCopy.WorkspacePVCName, "vol-")
	pvcVerifyErr := false
	if !isManagedBlock && statusCopy.WorkspacePVCName != "" && kc != nil {
		pvc, err := kc.GetPVCManifest(ctx, ns, statusCopy.WorkspacePVCName)
		if err == nil && pvc != nil {
			if pvc.Labels[PVCLabelManagedBy] == PVCLabelManagedByVolumes {
				isManagedBlock = true
			}
		} else if err != nil && !errors.Is(err, ErrKubeNotFound) {
			pvcVerifyErr = true
			slog.Warn("skipping workspace PVC deletion: failed to verify managed-by label",
				"podBox", name,
				"pvc", statusCopy.WorkspacePVCName,
				"err", err,
			)
		}
	}

	_ = runParallel(
		func() error {
			if kc != nil {
				_ = kc.DeletePodBox(ctx, ns, name)
			}
			return nil
		},
		func() error {
			deleteEgressSecretsWithClient(ctx, kc, ns, name)
			return nil
		},
		func() error {
			if statusCopy.PodName != "" && statusCopy.PodName != name {
				deleteEgressSecretsWithClient(ctx, kc, ns, statusCopy.PodName)
			}
			return nil
		},
		func() error {
			if len(forkSnapshots) > 0 {
				cleanupForkSnapshotsWithClient(ctx, kc, ns, forkSnapshots...)
			}
			return nil
		},
		func() error {
			if kc != nil && statusCopy.RootfsPVCName != "" {
				_ = kc.DeletePVC(ctx, ns, statusCopy.RootfsPVCName)
			}
			return nil
		},
		func() error {
			if isManagedBlock {
				if statusCopy.WorkspacePVCName != "" {
					releaseWorkspaceBlockLeaseWithClient(ctx, kc, ns, name, statusCopy.WorkspacePVCName)
				}
			} else if (deleteVolumes || !hasSpec || spec.VolumeKey == "") && !pvcVerifyErr {
				if kc != nil && statusCopy.WorkspacePVCName != "" {
					_ = kc.DeletePVC(ctx, ns, statusCopy.WorkspacePVCName)
				}
			}
			return nil
		},
	)

	r.mu.Lock()
	delete(r.boxes, name)
	delete(r.specs, name)
	delete(r.resolvedImages, name)
	delete(r.boxAnnotations, name)
	delete(r.boxResourceVersions, name)
	delete(r.volumeNFSLeases, name)
	r.mu.Unlock()

	r.recordPodBoxEvent(ctx, name, PodBoxEventReasonDeleted, KubeEventTypeNormal, "PodBox deleted", time.Now().UTC())
	return nil
}

func parseWarmIndex(s string) int64 {
	idx := strings.Index(s, "-warm-")
	if idx == -1 {
		return 0
	}
	rem := s[idx+len("-warm-"):]
	var n int64
	for _, ch := range rem {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int64(ch-'0')
		} else {
			break
		}
	}
	return n
}

// SyncFromKube recovers PodBox and PodWarmPool states from the Kubernetes API into memory.
// It queries the APIServer for all PodBoxes and PodWarmPools in the reconciler's namespace,
// populates r.boxes, r.specs, and r.warmPools in memory, and returns the count of recovered PodBoxes.
func (r *Reconciler) SyncFromKube(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if r.kubeClient == nil {
		return 0, nil
	}

	boxes, err := r.kubeClient.ListPodBoxes(ctx, r.namespace)
	if err != nil {
		return 0, fmt.Errorf("failed to list PodBoxes from Kubernetes: %w", err)
	}

	var crdMissing bool
	pools, err := r.kubeClient.ListPodWarmPools(ctx, r.namespace)
	if err != nil {
		if errors.Is(err, ErrCRDNotInstalled) {
			crdMissing = true
			slog.Debug("PodWarmPool CRD not installed, skipping warm pool sync and cleanup")
		} else {
			return 0, fmt.Errorf("failed to list PodWarmPools from Kubernetes: %w", err)
		}
	}

	// Query pods for each warm pool in parallel.
	poolPods := make(map[string][]PodManifest, len(pools))
	if !crdMissing && len(pools) > 0 {
		var poolPodsMu sync.Mutex
		fns := make([]func() error, 0, len(pools))
		for _, p := range pools {
			pName := p.Metadata.Name
			if pName == "" {
				continue
			}
			poolName := pName
			fns = append(fns, func() error {
				pods, listErr := r.kubeClient.ListPods(ctx, r.namespace, "cubepod.io/warmpool="+SanitizeLabelValue(poolName))
				if listErr != nil {
					return fmt.Errorf("failed to list pods for warmpool %s: %w", poolName, listErr)
				}
				poolPodsMu.Lock()
				poolPods[poolName] = pods
				poolPodsMu.Unlock()
				return nil
			})
		}
		if err := runParallel(fns...); err != nil {
			return 0, err
		}
	}

	if err := r.SyncTemplatesFromKube(ctx); err != nil {
		return 0, fmt.Errorf("failed to sync PodTemplates from Kubernetes: %w", err)
	}

	r.mu.Lock()
	var staleEntriesToDelete []*WarmPodEntry

	recovered := 0
	for _, box := range boxes {
		name := box.Metadata.Name
		if name == "" {
			continue
		}
		st := box.Status
		r.boxes[name] = &st
		r.specs[name] = box.Spec
		if box.Metadata.ResourceVersion != "" {
			if r.boxResourceVersions == nil {
				r.boxResourceVersions = make(map[string]string)
			}
			r.boxResourceVersions[name] = box.Metadata.ResourceVersion
		}
		if box.Metadata.Annotations != nil {
			r.mergeBoxAnnotationsLocked(name, box.Metadata.Annotations)
			if img := box.Metadata.Annotations["cubepod.io/resolved-image"]; img != "" {
				r.resolvedImages[name] = img
			}
		}
		if r.volumeNFSLeases == nil {
			r.volumeNFSLeases = make(map[string]string)
		}
		leaseFromBox := ""
		if box.Metadata.Annotations != nil {
			leaseFromBox = box.Metadata.Annotations[VolumeNFSLeaseAnnotation]
		}
		if leaseFromBox != "" {
			r.volumeNFSLeases[name] = leaseFromBox
		} else {
			delete(r.volumeNFSLeases, name)
			if r.boxAnnotations[name] != nil {
				delete(r.boxAnnotations[name], VolumeNFSLeaseAnnotation)
			}
		}
		if idx := parseWarmIndex(box.Status.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.Status.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.Status.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		recovered++
	}

	// Sync PodWarmPools from Kubernetes
	activePools := make(map[string]bool)
	if !crdMissing {
		r.syncWarmPoolsStateLocked(pools, poolPods)
		for _, p := range pools {
			if p.Metadata.Name != "" {
				activePools[p.Metadata.Name] = true
			}
		}
		for name, pool := range r.warmPools {
			if !activePools[name] {
				if time.Since(pool.createdAt) > 10*time.Second {
					for _, entry := range pool.entries {
						if !entry.Claimed {
							staleEntriesToDelete = append(staleEntriesToDelete, entry)
						}
					}
					delete(r.warmPools, name)
				}
			}
		}
	}

	for _, pool := range r.warmPools {
		for _, entry := range pool.entries {
			if idx := parseWarmIndex(entry.ID); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.PodName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.RootfsPVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
			if idx := parseWarmIndex(entry.WorkspacePVCName); idx > r.warmCounter {
				r.warmCounter = idx
			}
		}
	}
	for _, box := range r.boxes {
		if box == nil {
			continue
		}
		if idx := parseWarmIndex(box.PodName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.RootfsPVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
		if idx := parseWarmIndex(box.WorkspacePVCName); idx > r.warmCounter {
			r.warmCounter = idx
		}
	}
	kc := r.kubeClient
	ns := r.namespace
	r.mu.Unlock()

	if kc != nil && len(staleEntriesToDelete) > 0 {
		for _, entry := range staleEntriesToDelete {
			_ = kc.DeletePod(ctx, ns, entry.PodName)
			_ = runParallel(
				func() error { _ = kc.DeletePVC(ctx, ns, entry.RootfsPVCName); return nil },
				func() error { _ = kc.DeletePVC(ctx, ns, entry.WorkspacePVCName); return nil },
			)
		}
	}

	return recovered, nil
}

func (r *Reconciler) syncWarmPoolsStateLocked(pools []v1alpha1.PodWarmPool, poolPods map[string][]PodManifest) {
	for _, p := range pools {
		pName := p.Metadata.Name
		if pName == "" {
			continue
		}
		pool, exists := r.warmPools[pName]
		if !exists {
			pool = &warmPoolState{
				spec:      p.Spec,
				entries:   make([]*WarmPodEntry, 0),
				persisted: true,
				createdAt: time.Now().UTC(),
			}
			r.warmPools[pName] = pool
		} else {
			pool.spec = p.Spec
			pool.persisted = true
		}

		pods, hasPods := poolPods[pName]
		if !hasPods {
			continue
		}
		existingEntries := make(map[string]*WarmPodEntry, len(pool.entries))
		for _, entry := range pool.entries {
			existingEntries[entry.PodName] = entry
		}

		var updatedEntries []*WarmPodEntry
		for _, pod := range pods {
			claimed := (pod.Labels != nil && pod.Labels["cubepod.io/claimed"] == "true")
			var claimedBy string
			if pod.Labels != nil {
				claimedBy = pod.Labels["cubepod.io/claimed-by"]
			}

			if entry, ok := existingEntries[pod.Name]; ok {
				entry.Claimed = claimed
				entry.ClaimedBy = claimedBy
				if pod.PodIP != "" {
					entry.PodIP = pod.PodIP
				}
				entry.NotReady = pod.NotReady || entry.PodIP == ""
				if entry.PodManifest.Labels == nil {
					entry.PodManifest.Labels = make(map[string]string)
				}
				for k, v := range pod.Labels {
					entry.PodManifest.Labels[k] = v
				}
				updatedEntries = append(updatedEntries, entry)
			} else {
				entryID := ""
				if pod.Labels != nil {
					entryID = pod.Labels["pod.cubeplex.ai/box"]
				}
				if entryID == "" {
					entryID = strings.TrimSuffix(pod.Name, "-kata-pod")
				}
				tplRef := ""
				if pod.Labels != nil {
					tplRef = pod.Labels["cubepod.io/template"]
				}
				if tplRef == "" {
					tplRef = p.Spec.TemplateRef
				}
				newEntry := &WarmPodEntry{
					ID:               entryID,
					PoolName:         pName,
					TemplateRef:      tplRef,
					PodName:          pod.Name,
					PodIP:            pod.PodIP,
					NotReady:         pod.NotReady || pod.PodIP == "",
					RootfsPVCName:    entryID + "-rootfs-pvc",
					WorkspacePVCName: entryID + "-workspace-pvc",
					Claimed:          claimed,
					ClaimedBy:        claimedBy,
					CreatedAt:        time.Now().UTC(),
					PodManifest:      pod,
				}
				updatedEntries = append(updatedEntries, newEntry)
			}

			if idx := parseWarmIndex(pod.Name); idx > r.warmCounter {
				r.warmCounter = idx
			}
		}
		pool.entries = updatedEntries
	}
}

// SyncWarmPoolsFromKube refreshes PodWarmPools (optionally filtered to templateRef) and their standby pods
// without listing all PodBoxes or PodTemplates, keeping cold-start warmpool discovery lightweight.
func (r *Reconciler) SyncWarmPoolsFromKube(ctx context.Context, templateRef string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	kc := r.kubeClient
	ns := r.namespace
	r.mu.RUnlock()
	if kc == nil {
		return nil
	}

	pools, err := kc.ListPodWarmPools(ctx, ns)
	if err != nil {
		if errors.Is(err, ErrCRDNotInstalled) {
			return nil
		}
		return fmt.Errorf("failed to list PodWarmPools from Kubernetes: %w", err)
	}

	poolPods := make(map[string][]PodManifest, len(pools))
	var poolPodsMu sync.Mutex
	var fns []func() error
	for _, p := range pools {
		pName := p.Metadata.Name
		if pName == "" {
			continue
		}
		if templateRef != "" && p.Spec.TemplateRef != templateRef {
			continue
		}
		poolName := pName
		fns = append(fns, func() error {
			pods, listErr := kc.ListPods(ctx, ns, "cubepod.io/warmpool="+SanitizeLabelValue(poolName))
			if listErr != nil {
				return fmt.Errorf("failed to list pods for warmpool %s: %w", poolName, listErr)
			}
			poolPodsMu.Lock()
			poolPods[poolName] = pods
			poolPodsMu.Unlock()
			return nil
		})
	}
	if err := runParallel(fns...); err != nil {
		return err
	}

	r.mu.Lock()
	r.syncWarmPoolsStateLocked(pools, poolPods)
	r.mu.Unlock()
	return nil
}

// FindAvailableWarmPool looks for a warm pool that matches the template and has available warm replicas.
func (r *Reconciler) FindAvailableWarmPool(templateRef string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for name, pool := range r.warmPools {
		if pool.spec.TemplateRef == templateRef {
			for _, entry := range pool.entries {
				if !entry.Claimed && !entry.NotReady && entry.PodIP != "" {
					return name
				}
			}
		}
	}
	return ""
}

// Delete tears down a PodBox, removing its compute Pod and dual Block PVCs.
func (r *Reconciler) Delete(ctx context.Context, name string) error {
	return r.DeleteBox(ctx, name, true)
}

// ReapIdle inspects all managed sandboxes and enforces idle auto-pause and paused TTL expiration.
// Running sandboxes are paused when LastActivityAt + IdleTimeoutSeconds <= now, provided
// LeaseExpiresAt is unset or already expired. Paused sandboxes are terminated when
// PausedAt + PausedTTLSeconds <= now.
func (r *Reconciler) ReapIdle(ctx context.Context, now time.Time) (paused []string, terminated []string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	type reapCandidate struct {
		name      string
		terminate bool
	}
	var candidates []reapCandidate

	r.mu.RLock()
	for name, status := range r.boxes {
		if status == nil {
			continue
		}
		spec := r.specs[name]
		if status.Phase == v1alpha1.PodBoxPhaseRunning {
			if spec.Lifecycle.IdleTimeoutSeconds > 0 && status.LastActivityAt != nil {
				idleDeadline := status.LastActivityAt.Add(time.Duration(spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
				if !now.Before(idleDeadline) {
					leaseActive := spec.Lifecycle.LeaseExpiresAt != nil && spec.Lifecycle.LeaseExpiresAt.After(now)
					if !leaseActive {
						candidates = append(candidates, reapCandidate{name: name, terminate: false})
					}
				}
			}
		} else if status.Phase == v1alpha1.PodBoxPhasePaused {
			if spec.Lifecycle.PausedTTLSeconds > 0 && status.PausedAt != nil {
				ttlDeadline := status.PausedAt.Add(time.Duration(spec.Lifecycle.PausedTTLSeconds) * time.Second)
				if !now.Before(ttlDeadline) {
					candidates = append(candidates, reapCandidate{name: name, terminate: true})
				}
			}
		}
	}
	r.mu.RUnlock()

	for _, cand := range candidates {
		unlockBox, locked := r.tryAcquireBoxLock(cand.name)
		if !locked {
			continue
		}

		r.mu.RLock()
		statusPtr, ok := r.boxes[cand.name]
		if !ok || statusPtr == nil {
			r.mu.RUnlock()
			unlockBox()
			continue
		}
		statusCopy := *statusPtr
		spec := r.specs[cand.name]
		kc := r.kubeClient
		ns := r.namespace
		rv := r.boxResourceVersions[cand.name]
		anns := r.podBoxAnnotationsLocked(cand.name, "")
		r.mu.RUnlock()

		if !cand.terminate {
			if statusCopy.Phase != v1alpha1.PodBoxPhaseRunning || spec.Lifecycle.IdleTimeoutSeconds <= 0 || statusCopy.LastActivityAt == nil {
				unlockBox()
				continue
			}
			idleDeadline := statusCopy.LastActivityAt.Add(time.Duration(spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
			leaseActive := spec.Lifecycle.LeaseExpiresAt != nil && spec.Lifecycle.LeaseExpiresAt.After(now)
			if now.Before(idleDeadline) || leaseActive {
				unlockBox()
				continue
			}

			specWithPaused := spec
			specWithPaused.DesiredState = v1alpha1.DesiredStatePaused

			statusWithPaused := statusCopy
			statusWithPaused.Phase = v1alpha1.PodBoxPhasePaused
			statusWithPaused.PodName = ""
			statusWithPaused.PodIP = ""
			statusWithPaused.PausedAt = &now
			statusWithPaused.Reason = "IdlePaused"
			statusWithPaused.Message = "Paused due to idle timeout"

			if kc != nil {
				if anns != nil {
					delete(anns, VolumeNFSLeaseAnnotation)
				}
				err := kc.UpsertPodBox(ctx, v1alpha1.PodBox{
					APIVersion: "cubepod.io/v1alpha1",
					Kind:       "PodBox",
					Metadata: v1alpha1.ObjectMeta{
						Name:            cand.name,
						Namespace:       ns,
						ResourceVersion: rv,
						Annotations:     anns,
					},
					Spec:   specWithPaused,
					Status: statusWithPaused,
				})
				if err != nil {
					slog.Warn("failed to persist paused state to Kubernetes; rolling back in-memory state", "name", cand.name, "err", err)
					unlockBox()
					continue
				}
				r.revokeVolumeNFSLease(ctx, cand.name, "")
				if statusCopy.PodName != "" {
					_ = kc.DeletePod(ctx, ns, statusCopy.PodName)
					if statusCopy.PodName != cand.name {
						deleteEgressSecretsWithClient(ctx, kc, ns, statusCopy.PodName)
					}
				}
			}

			r.mu.Lock()
			r.boxes[cand.name] = &statusWithPaused
			r.specs[cand.name] = specWithPaused
			r.mu.Unlock()

			paused = append(paused, cand.name)
			r.recordPodBoxEvent(ctx, cand.name, PodBoxEventReasonIdlePaused, KubeEventTypeNormal,
				"Paused due to idle timeout", now)
		} else {
			if statusCopy.Phase != v1alpha1.PodBoxPhasePaused || spec.Lifecycle.PausedTTLSeconds <= 0 || statusCopy.PausedAt == nil {
				unlockBox()
				continue
			}
			ttlDeadline := statusCopy.PausedAt.Add(time.Duration(spec.Lifecycle.PausedTTLSeconds) * time.Second)
			if now.Before(ttlDeadline) {
				unlockBox()
				continue
			}

			statusWithTerminated := statusCopy
			statusWithTerminated.Phase = v1alpha1.PodBoxPhaseTerminated
			statusWithTerminated.PodName = ""
			statusWithTerminated.PodIP = ""
			statusWithTerminated.Reason = "PausedTTLExpired"
			statusWithTerminated.Message = "Terminated due to paused TTL expiration"

			if kc != nil {
				err := kc.UpsertPodBox(ctx, v1alpha1.PodBox{
					APIVersion: "cubepod.io/v1alpha1",
					Kind:       "PodBox",
					Metadata: v1alpha1.ObjectMeta{
						Name:            cand.name,
						Namespace:       ns,
						ResourceVersion: rv,
						Annotations:     anns,
					},
					Spec:   spec,
					Status: statusWithTerminated,
				})
				if err != nil {
					slog.Warn("failed to persist terminated state to Kubernetes; rolling back in-memory state", "name", cand.name, "err", err)
					unlockBox()
					continue
				}
			}

			if statusCopy.PodName != "" && kc != nil {
				r.revokeVolumeNFSLease(ctx, cand.name, "")
				_ = kc.DeletePod(ctx, ns, statusCopy.PodName)
			}
			_ = runParallel(
				func() error {
					if blockMount := spec.WorkspaceBlockMount(); blockMount != nil {
						releaseWorkspaceBlockLeaseWithClient(ctx, kc, ns, cand.name, blockMount.PVCName)
					}
					return nil
				},
				func() error {
					deleteEgressSecretsWithClient(ctx, kc, ns, cand.name)
					return nil
				},
			)

			r.mu.Lock()
			r.boxes[cand.name] = &statusWithTerminated
			r.mu.Unlock()

			terminated = append(terminated, cand.name)
			r.recordPodBoxEvent(ctx, cand.name, PodBoxEventReasonPausedTTLExpired, KubeEventTypeNormal,
				"Terminated due to paused TTL expiration", now)
		}
		unlockBox()
	}

	slices.Sort(paused)
	slices.Sort(terminated)
	return paused, terminated
}

// ReconcileProvisioning advances PodBoxes stuck in Provisioning (e.g. after a transient
// EnsurePod readiness timeout) to Running once their backing Pod is Ready with a PodIP,
// and re-drives Provisioning PodBoxes whose backing Pod is missing.
func (r *Reconciler) ReconcileProvisioning(ctx context.Context, now time.Time) []string {
	r.mu.RLock()
	kc := r.kubeClient
	ns := r.namespace
	hasCandidates := false
	for name, status := range r.boxes {
		spec := r.specs[name]
		if status != nil && status.Phase == v1alpha1.PodBoxPhaseProvisioning && spec.DesiredState == v1alpha1.DesiredStateRunning {
			hasCandidates = true
			break
		}
	}
	r.mu.RUnlock()

	if kc == nil || !hasCandidates {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	listStartedAt := time.Now().UTC()
	pods, err := kc.ListPods(ctx, ns, "")
	if err != nil {
		slog.Warn("failed to list pods while reconciling provisioning sandboxes", "err", err)
		return nil
	}
	podsByName := make(map[string]PodManifest, len(pods))
	for _, p := range pods {
		podsByName[p.Name] = p
	}

	type missingPodCandidate struct {
		name string
		spec v1alpha1.PodBoxSpec
	}
	type readyPodCandidate struct {
		name string
	}
	var missingPodCandidates []missingPodCandidate
	var readyPodCandidates []readyPodCandidate
	var promoted []string

	r.mu.RLock()
	for name, status := range r.boxes {
		spec := r.specs[name]
		if status == nil || status.Phase != v1alpha1.PodBoxPhaseProvisioning || spec.DesiredState != v1alpha1.DesiredStateRunning {
			continue
		}
		if status.LastActivityAt != nil && status.LastActivityAt.After(listStartedAt) {
			continue
		}
		if status.PodName == "" {
			missingPodCandidates = append(missingPodCandidates, missingPodCandidate{name: name, spec: spec})
			continue
		}
		pod, ok := podsByName[status.PodName]
		if !ok {
			missingPodCandidates = append(missingPodCandidates, missingPodCandidate{name: name, spec: spec})
			continue
		}
		if pod.NotReady || pod.PodIP == "" {
			continue
		}
		readyPodCandidates = append(readyPodCandidates, readyPodCandidate{name: name})
	}
	r.mu.RUnlock()

	for _, cand := range readyPodCandidates {
		unlockBox, locked := r.tryAcquireBoxLock(cand.name)
		if !locked {
			continue
		}
		r.mu.RLock()
		statusPtr, ok := r.boxes[cand.name]
		if !ok || statusPtr == nil {
			r.mu.RUnlock()
			unlockBox()
			continue
		}
		spec := r.specs[cand.name]
		if statusPtr.Phase != v1alpha1.PodBoxPhaseProvisioning || spec.DesiredState != v1alpha1.DesiredStateRunning {
			r.mu.RUnlock()
			unlockBox()
			continue
		}
		pod, podOK := podsByName[statusPtr.PodName]
		if !podOK || pod.NotReady || pod.PodIP == "" {
			r.mu.RUnlock()
			unlockBox()
			continue
		}
		statusWithRunning := *statusPtr
		statusWithRunning.Phase = v1alpha1.PodBoxPhaseRunning
		statusWithRunning.PodIP = pod.PodIP
		statusWithRunning.PausedAt = nil
		statusWithRunning.LastActivityAt = &now
		if statusWithRunning.Reason == "" {
			statusWithRunning.Reason = "Provisioned"
		}
		if statusWithRunning.Message == "" {
			statusWithRunning.Message = "Sandbox pod is ready"
		}
		anns := r.podBoxAnnotationsLocked(cand.name, "")
		rv := r.boxResourceVersions[cand.name]
		r.mu.RUnlock()

		upsertErr := kc.UpsertPodBox(ctx, v1alpha1.PodBox{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Metadata: v1alpha1.ObjectMeta{
				Name:            cand.name,
				Namespace:       ns,
				ResourceVersion: rv,
				Annotations:     anns,
			},
			Spec:   spec,
			Status: statusWithRunning,
		})
		if upsertErr != nil {
			slog.Warn("failed to persist running state for provisioning sandbox", "name", cand.name, "err", upsertErr)
			unlockBox()
			continue
		}

		r.mu.Lock()
		r.boxes[cand.name] = &statusWithRunning
		r.mu.Unlock()

		r.recordPhaseTransition(ctx, cand.name, v1alpha1.PodBoxPhaseProvisioning, statusWithRunning)
		promoted = append(promoted, cand.name)
		unlockBox()
	}

	for _, cand := range missingPodCandidates {
		st, err := r.Reconcile(ctx, cand.name, cand.spec)
		if err != nil {
			slog.Warn("failed to re-reconcile provisioning sandbox with missing pod", "name", cand.name, "err", err)
			continue
		}
		if st != nil && st.Phase == v1alpha1.PodBoxPhaseRunning {
			promoted = append(promoted, cand.name)
		}
	}

	slices.Sort(promoted)
	return promoted
}

// ActiveSandboxesCount returns the number of Running sandboxes.
func (r *Reconciler) ActiveSandboxesCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, status := range r.boxes {
		if status.Phase == v1alpha1.PodBoxPhaseRunning {
			count++
		}
	}
	return count
}

// PausedSandboxesCount returns the number of Paused sandboxes.
func (r *Reconciler) PausedSandboxesCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, status := range r.boxes {
		if status.Phase == v1alpha1.PodBoxPhasePaused {
			count++
		}
	}
	return count
}

// WarmPoolReadyCount returns the total number of ready warm standby sandboxes across all pools.
func (r *Reconciler) WarmPoolReadyCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, pool := range r.warmPools {
		for _, entry := range pool.entries {
			if !entry.Claimed && !entry.NotReady && entry.PodIP != "" {
				total++
			}
		}
	}
	return total
}

// GetWarmPoolSpecs returns a snapshot of registered warm pool specs for background replenishment.
func (r *Reconciler) GetWarmPoolSpecs() map[string]v1alpha1.PodWarmPoolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make(map[string]v1alpha1.PodWarmPoolSpec, len(r.warmPools))
	for name, pool := range r.warmPools {
		specs[name] = pool.spec
	}
	return specs
}

// GetWarmPoolEntries returns a copy of the warm pod entries for a pool.
func (r *Reconciler) GetWarmPoolEntries(poolName string) []*WarmPodEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pool, ok := r.warmPools[poolName]
	if !ok {
		return nil
	}
	res := make([]*WarmPodEntry, len(pool.entries))
	copy(res, pool.entries)
	return res
}

// GetBox returns a copy of the observed status for a PodBox.
func (r *Reconciler) GetBox(name string) (*v1alpha1.PodBoxStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status, ok := r.boxes[name]
	if !ok {
		return nil, false
	}
	cp := *status
	return &cp, true
}

// GetBoxSpec returns a copy of the stored spec for a PodBox.
func (r *Reconciler) GetBoxSpec(name string) (v1alpha1.PodBoxSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[name]
	return spec, ok
}

// TouchActivity updates the LastActivityAt timestamp of a PodBox.
func (r *Reconciler) TouchActivity(name string, t time.Time) error {
	unlockBox := r.acquireBoxLock(name)
	defer unlockBox()

	r.mu.Lock()
	status, ok := r.boxes[name]
	if !ok || status == nil {
		r.mu.Unlock()
		return fmt.Errorf("podbox %q not found", name)
	}
	cp := *status
	cp.LastActivityAt = &t
	r.boxes[name] = &cp
	kc := r.kubeClient
	ns := r.namespace
	spec := r.specs[name]
	anns := r.podBoxAnnotationsLocked(name, "")
	rv := r.boxResourceVersions[name]
	r.mu.Unlock()

	if kc != nil {
		box := v1alpha1.PodBox{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Metadata: v1alpha1.ObjectMeta{
				Name:            name,
				Namespace:       ns,
				ResourceVersion: rv,
				Annotations:     anns,
			},
			Spec:   spec,
			Status: cp,
		}
		_ = kc.UpsertPodBox(context.Background(), box)
	}
	return nil
}

// SetPausedAt updates the PausedAt timestamp of a PodBox.
func (r *Reconciler) SetPausedAt(name string, t time.Time) error {
	unlockBox := r.acquireBoxLock(name)
	defer unlockBox()

	r.mu.Lock()
	status, ok := r.boxes[name]
	if !ok || status == nil {
		r.mu.Unlock()
		return fmt.Errorf("podbox %q not found", name)
	}
	cp := *status
	cp.PausedAt = &t
	r.boxes[name] = &cp
	kc := r.kubeClient
	ns := r.namespace
	spec := r.specs[name]
	anns := r.podBoxAnnotationsLocked(name, "")
	rv := r.boxResourceVersions[name]
	r.mu.Unlock()

	if kc != nil {
		box := v1alpha1.PodBox{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodBox",
			Metadata: v1alpha1.ObjectMeta{
				Name:            name,
				Namespace:       ns,
				ResourceVersion: rv,
				Annotations:     anns,
			},
			Spec:   spec,
			Status: cp,
		}
		_ = kc.UpsertPodBox(context.Background(), box)
	}
	return nil
}

// AcquireLease updates the LeaseExpiresAt timestamp of a PodBox.
func (r *Reconciler) AcquireLease(name string, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if box, ok := r.boxes[name]; ok && box != nil {
		if box.Phase == v1alpha1.PodBoxPhaseFailed || box.Phase == v1alpha1.PodBoxPhaseTerminated || box.Phase == v1alpha1.PodBoxPhasePaused {
			return fmt.Errorf("cannot acquire lease for podbox %q in phase %s", name, box.Phase)
		}
	}
	spec, ok := r.specs[name]
	if !ok {
		return fmt.Errorf("podbox %q not found", name)
	}
	spec.Lifecycle.LeaseExpiresAt = &expiresAt
	r.specs[name] = spec
	return nil
}

// SetPoolCreatedAt sets the createdAt timestamp of an in-memory warm pool (useful for testing stale list deletion).
func (r *Reconciler) SetPoolCreatedAt(name string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pool, ok := r.warmPools[name]; ok {
		pool.createdAt = t
	}
}

// WarmCounter returns the current warm replica counter.
func (r *Reconciler) WarmCounter() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.warmCounter
}

// verifyWorkspaceBlockLeaseWithClient checks that the managed block PVC exists and its
// cubepod.io/attached-sandbox annotation still matches boxName (controller-side fencing).
func verifyWorkspaceBlockLeaseWithClient(ctx context.Context, kc KubeClient, ns, boxName, pvcName string) error {
	if kc == nil || pvcName == "" {
		return nil
	}
	pvc, err := kc.GetPVCManifest(ctx, ns, pvcName)
	if err != nil {
		return fmt.Errorf("workspace block volume PVC %s unavailable: %w", pvcName, err)
	}
	holder := ""
	if pvc.Annotations != nil {
		holder = strings.TrimSpace(pvc.Annotations[PVCAnnotationAttachedSandbox])
	}
	if holder != boxName {
		return fmt.Errorf("workspace block volume PVC %s lease lost (expected %q, held by %q)", pvcName, boxName, holder)
	}
	return nil
}

// releaseWorkspaceBlockLeaseWithClient clears cubepod.io/attached-sandbox and cubepod.io/attached-at
// on pvcName only if the lease is still held by boxName.
func releaseWorkspaceBlockLeaseWithClient(ctx context.Context, kc KubeClient, ns, boxName, pvcName string) {
	if kc == nil || pvcName == "" || boxName == "" {
		return
	}
	pvc, err := kc.GetPVCManifest(ctx, ns, pvcName)
	if err != nil || pvc == nil || pvc.Annotations == nil {
		return
	}
	if strings.TrimSpace(pvc.Annotations[PVCAnnotationAttachedSandbox]) != boxName {
		return
	}
	_, _ = kc.UpdatePVCAnnotationsCAS(ctx, ns, pvcName, pvc.ResourceVersion, map[string]string{
		PVCAnnotationAttachedSandbox: "",
		PVCAnnotationAttachedAt:      "",
	})
}
