package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
)

const (
	// FeatureGateFirecrackerVolumeNFS is the CEP-0031 feature gate.
	FeatureGateFirecrackerVolumeNFS = "FirecrackerVolumeNFS"
	// VolumeNFSLeaseAnnotation stores the per-start NFS lease ID on a PodBox.
	VolumeNFSLeaseAnnotation = "cubepod.io/volume-nfs-lease"
)

// VolumeNFSLookup resolves the cubepod-volume-nfs Service ClusterIP and readiness.
type VolumeNFSLookup interface {
	GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error)
	ServiceHasReadyEndpoints(ctx context.Context, namespace, name string) (bool, error)
}

// VolumeNFSSettings configures Firecracker official-volume NFS transport.
type VolumeNFSSettings struct {
	Enabled     bool
	ServiceName string
	ServiceNS   string
	NFSPort     int
	LeaseClient *volumenfs.Client
	Lookup      VolumeNFSLookup
}

// WithVolumeNFS configures the Firecracker volume NFS backend used by the reconciler.
func WithVolumeNFS(settings VolumeNFSSettings) ReconcilerOption {
	return func(r *Reconciler) {
		r.volumeNFS = settings
		if r.volumeNFS.ServiceName == "" {
			r.volumeNFS.ServiceName = "cubepod-volume-nfs"
		}
		if r.volumeNFS.NFSPort <= 0 {
			r.volumeNFS.NFSPort = volumenfs.DefaultNFSPort
		}
		if r.volumeNFSLeases == nil {
			r.volumeNFSLeases = make(map[string]string)
		}
	}
}

// WithFirecrackerVolumeNFS toggles the FirecrackerVolumeNFS feature gate.
func WithFirecrackerVolumeNFS(enabled bool) ReconcilerOption {
	return func(r *Reconciler) {
		r.volumeNFS.Enabled = enabled
	}
}

func officialVolumeIDs(mounts []v1alpha1.VolumeMount) []string {
	out := make([]string, 0, len(mounts))
	seen := map[string]struct{}{}
	for _, m := range mounts {
		id := strings.TrimSpace(m.VolumeID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (r *Reconciler) usesFirecrackerRuntime(ctx context.Context, tpl v1alpha1.PodTemplateSpec) (bool, error) {
	name := strings.TrimSpace(tpl.RuntimeClassName)
	if IsFirecrackerRuntimeName(name) {
		return true, nil
	}
	if IsKnownNonFirecrackerRuntimeName(name) {
		return false, nil
	}
	// Custom RuntimeClass: resolve handler/labels. Fail closed on errors or a
	// missing Lookup so we never attach the Kata VolumeMount path to an
	// unrecognized Firecracker class.
	if r.volumeNFS.Lookup == nil {
		return false, fmt.Errorf("cannot resolve RuntimeClass %q to confirm it is not Firecracker", name)
	}
	rc, ok := r.volumeNFS.Lookup.(interface {
		GetRuntimeClass(ctx context.Context, name string) (handler string, labels map[string]string, err error)
	})
	if !ok {
		return false, fmt.Errorf("cannot resolve RuntimeClass %q to confirm it is not Firecracker", name)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handler, labels, err := rc.GetRuntimeClass(ctx, name)
	if err != nil {
		return false, fmt.Errorf("lookup RuntimeClass %q: %w", name, err)
	}
	return IsFirecrackerRuntime(name, handler, labels), nil
}

func (r *Reconciler) prepareVolumeNFS(ctx context.Context, boxName, podName string, tpl v1alpha1.PodTemplateSpec, mounts []v1alpha1.VolumeMount) (VolumeNFSOptions, string, error) {
	var zero VolumeNFSOptions
	if len(mounts) == 0 {
		return zero, "", nil
	}
	fc, err := r.usesFirecrackerRuntime(ctx, tpl)
	if err != nil {
		return zero, "", err
	}
	if !fc {
		return zero, "", nil
	}
	if !r.volumeNFS.Enabled {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts require the FirecrackerVolumeNFS feature gate")
	}
	if r.volumeNFS.LeaseClient == nil {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts require a configured cubepod-volume-nfs lease API")
	}
	if r.volumeNFS.Lookup == nil {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts require cubepod-volume-nfs Service discovery")
	}
	svcNS := r.volumeNFS.ServiceNS
	if svcNS == "" {
		svcNS = r.namespace
	}
	clusterIP, err := r.volumeNFS.Lookup.GetServiceClusterIP(ctx, svcNS, r.volumeNFS.ServiceName)
	if err != nil {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts: cubepod-volume-nfs Service not ready: %w", err)
	}
	if strings.TrimSpace(clusterIP) == "" || clusterIP == "None" {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts: cubepod-volume-nfs Service has no ClusterIP")
	}
	ready, err := r.volumeNFS.Lookup.ServiceHasReadyEndpoints(ctx, svcNS, r.volumeNFS.ServiceName)
	if err != nil {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts: cubepod-volume-nfs endpoints: %w", err)
	}
	if !ready {
		return zero, "", fmt.Errorf("Firecracker official volumeMounts: cubepod-volume-nfs Service has no ready endpoints")
	}

	leaseID, err := volumenfs.NewLeaseID()
	if err != nil {
		return zero, "", fmt.Errorf("generate volume NFS lease: %w", err)
	}
	ids := officialVolumeIDs(mounts)
	if _, err := r.volumeNFS.LeaseClient.Put(ctx, leaseID, volumenfs.LeaseRequest{
		VolumeIDs:  ids,
		PodName:    podName,
		PodBoxName: boxName,
	}); err != nil {
		return zero, "", fmt.Errorf("pre-register volume NFS lease: %w", err)
	}
	slog.Info("pre-registered volume NFS lease", "leaseID", leaseID, "pod", podName, "box", boxName, "volumeIDs", ids)
	return VolumeNFSOptions{
		ClusterIP: clusterIP,
		Port:      r.volumeNFS.NFSPort,
		LeaseID:   leaseID,
	}, leaseID, nil
}

func (r *Reconciler) bindVolumeNFSLease(ctx context.Context, leaseID, podIP string, mounts []v1alpha1.VolumeMount, podName, boxName string) error {
	if leaseID == "" || r.volumeNFS.LeaseClient == nil {
		return fmt.Errorf("volume NFS lease client is not configured")
	}
	if strings.TrimSpace(podIP) == "" {
		return fmt.Errorf("refusing to bind volume NFS lease %s with empty podIP", leaseID)
	}
	_, err := r.volumeNFS.LeaseClient.Put(ctx, leaseID, volumenfs.LeaseRequest{
		VolumeIDs:  officialVolumeIDs(mounts),
		PodName:    podName,
		PodBoxName: boxName,
		PodIP:      podIP,
	})
	if err != nil {
		return fmt.Errorf("bind volume NFS lease %s to podIP %s: %w", leaseID, podIP, err)
	}
	slog.Info("bound volume NFS lease", "leaseID", leaseID, "podIP", podIP, "pod", podName)
	return nil
}

func volumeNFSRevokeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, 10*time.Second)
}

func (r *Reconciler) revokeVolumeNFSLease(ctx context.Context, boxName, leaseID string) {
	ids := map[string]struct{}{}
	if strings.TrimSpace(leaseID) != "" {
		ids[leaseID] = struct{}{}
	}
	r.mu.Lock()
	if boxName != "" {
		if id := r.volumeNFSLeases[boxName]; id != "" {
			ids[id] = struct{}{}
		}
		if r.boxAnnotations[boxName] != nil {
			if id := r.boxAnnotations[boxName][VolumeNFSLeaseAnnotation]; id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	leaseClient := r.volumeNFS.LeaseClient
	if len(ids) == 0 || leaseClient == nil {
		if boxName != "" {
			delete(r.volumeNFSLeases, boxName)
		}
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	revokeCtx, cancel := volumeNFSRevokeContext(ctx)
	defer cancel()
	for id := range ids {
		if err := leaseClient.Delete(revokeCtx, id); err != nil {
			slog.Warn("failed to revoke volume NFS lease", "leaseID", id, "box", boxName, "err", err)
		} else {
			slog.Info("revoked volume NFS lease", "leaseID", id, "box", boxName)
		}
	}
	if boxName != "" {
		r.mu.Lock()
		if _, ok := ids[r.volumeNFSLeases[boxName]]; ok || r.volumeNFSLeases[boxName] == "" {
			delete(r.volumeNFSLeases, boxName)
		}
		if r.boxAnnotations[boxName] != nil {
			delete(r.boxAnnotations[boxName], VolumeNFSLeaseAnnotation)
		}
		r.mu.Unlock()
	}
}

// revokeVolumeNFSLeaseID revokes a single lease without clearing a different
// stored ID for the same box (used when a superseded start lease must be dropped).
func (r *Reconciler) revokeVolumeNFSLeaseID(ctx context.Context, boxName, leaseID string) {
	if strings.TrimSpace(leaseID) == "" {
		return
	}
	r.mu.RLock()
	leaseClient := r.volumeNFS.LeaseClient
	r.mu.RUnlock()
	if leaseClient == nil {
		return
	}
	revokeCtx, cancel := volumeNFSRevokeContext(ctx)
	defer cancel()
	if err := leaseClient.Delete(revokeCtx, leaseID); err != nil {
		slog.Warn("failed to revoke volume NFS lease", "leaseID", leaseID, "box", boxName, "err", err)
	} else {
		slog.Info("revoked volume NFS lease", "leaseID", leaseID, "box", boxName)
	}
	if boxName != "" {
		r.mu.Lock()
		if r.volumeNFSLeases[boxName] == leaseID {
			delete(r.volumeNFSLeases, boxName)
		}
		if r.boxAnnotations[boxName] != nil && r.boxAnnotations[boxName][VolumeNFSLeaseAnnotation] == leaseID {
			delete(r.boxAnnotations[boxName], VolumeNFSLeaseAnnotation)
		}
		r.mu.Unlock()
	}
}

func (r *Reconciler) rememberVolumeNFSLease(boxName, leaseID string) {
	if boxName == "" || leaseID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.volumeNFSLeases == nil {
		r.volumeNFSLeases = make(map[string]string)
	}
	r.volumeNFSLeases[boxName] = leaseID
	r.mergeBoxAnnotationsLocked(boxName, map[string]string{VolumeNFSLeaseAnnotation: leaseID})
}

func (r *Reconciler) ensurePodMaybeWithNFS(ctx context.Context, pod PodManifest, leaseID string, mounts []v1alpha1.VolumeMount, boxName string) (string, error) {
	if leaseID == "" {
		return r.kubeClient.EnsurePod(ctx, pod)
	}
	hooker, ok := r.kubeClient.(interface {
		EnsurePodWithHook(context.Context, PodManifest, PodIPHook) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("kube client cannot bind a volume NFS lease before Ready")
	}
	return hooker.EnsurePodWithHook(ctx, pod, func(ctx context.Context, _, _, podIP string) error {
		return r.bindVolumeNFSLease(ctx, leaseID, podIP, mounts, pod.Name, boxName)
	})
}

// RESTVolumeNFSLookup looks up Services and Endpoints through RESTKubeClient.
type RESTVolumeNFSLookup struct {
	Client *RESTKubeClient
}

func (l RESTVolumeNFSLookup) GetServiceClusterIP(ctx context.Context, namespace, name string) (string, error) {
	if l.Client == nil {
		return "", fmt.Errorf("kube client is not configured")
	}
	return l.Client.GetServiceClusterIP(ctx, namespace, name)
}

func (l RESTVolumeNFSLookup) ServiceHasReadyEndpoints(ctx context.Context, namespace, name string) (bool, error) {
	if l.Client == nil {
		return false, fmt.Errorf("kube client is not configured")
	}
	return l.Client.ServiceHasReadyEndpoints(ctx, namespace, name)
}

func (l RESTVolumeNFSLookup) GetRuntimeClass(ctx context.Context, name string) (string, map[string]string, error) {
	if l.Client == nil {
		return "", nil, fmt.Errorf("kube client is not configured")
	}
	return l.Client.GetRuntimeClass(ctx, name)
}

// FakeVolumeNFSLookup is an in-memory VolumeNFSLookup for tests.
type FakeVolumeNFSLookup struct {
	ClusterIP       string
	Ready           bool
	Err             error
	Handler         string
	Labels          map[string]string
	RuntimeClassErr error
}

func (f FakeVolumeNFSLookup) GetServiceClusterIP(context.Context, string, string) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	return f.ClusterIP, nil
}

func (f FakeVolumeNFSLookup) ServiceHasReadyEndpoints(context.Context, string, string) (bool, error) {
	if f.Err != nil {
		return false, f.Err
	}
	return f.Ready, nil
}

func (f FakeVolumeNFSLookup) GetRuntimeClass(context.Context, string) (string, map[string]string, error) {
	if f.RuntimeClassErr != nil {
		return "", nil, f.RuntimeClassErr
	}
	return f.Handler, f.Labels, nil
}

// decode helpers used by RESTKubeClient service lookup (shared JSON shapes).
func decodeServiceClusterIP(body []byte) (string, error) {
	var obj struct {
		Spec struct {
			ClusterIP string `json:"clusterIP"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", err
	}
	return obj.Spec.ClusterIP, nil
}

func decodeEndpointsReady(body []byte) bool {
	var obj struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
		} `json:"subsets"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	for _, s := range obj.Subsets {
		for _, a := range s.Addresses {
			if strings.TrimSpace(a.IP) != "" {
				return true
			}
		}
	}
	return false
}

func readAllBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b
}
