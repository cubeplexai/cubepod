package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

const (
	// SecretResourceLabel marks Kubernetes Secrets managed by the CubePod E2B secret store.
	SecretResourceLabel = "cubepod.io/resource"
	// SecretResourceValue is the label value for CubePod E2B secrets.
	SecretResourceValue = "e2b-secret"
	// SecretTenantLabel records a short tenant hash prefix on each Kubernetes Secret.
	SecretTenantLabel = "cubepod.io/tenant"
	// SecretNamePrefix is the Kubernetes Secret name prefix for E2B secrets.
	SecretNamePrefix = "cubepod-e2b-sec-"
)

type kubeSecretMeta struct {
	TenantHash string     `json:"tenantHash"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
}

// KubeSecretBackend persists E2B secrets as Kubernetes core/v1 Secrets for multi-replica HA gateways.
type KubeSecretBackend struct {
	client    controller.KubeClient
	namespace string
}

// NewKubeSecretBackend creates a Kubernetes Secret backend targeting the given namespace.
func NewKubeSecretBackend(client controller.KubeClient, namespace string) (*KubeSecretBackend, error) {
	if client == nil {
		return nil, errors.New("kube client is required")
	}
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = "cubepod-workloads"
	}
	return &KubeSecretBackend{
		client:    client,
		namespace: ns,
	}, nil
}

// KubeSecretName returns the RFC-1123 compliant Kubernetes Secret name for a secret ID.
func KubeSecretName(id string) string {
	clean := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "_", "-"))
	return SecretNamePrefix + clean
}

func tenantLabelPrefix(tenantHash string) string {
	h := strings.ToLower(strings.TrimSpace(tenantHash))
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// LoadAll lists all E2B secret objects from Kubernetes matching cubepod.io/resource=e2b-secret.
func (b *KubeSecretBackend) LoadAll(ctx context.Context) ([]StoredSecret, error) {
	secrets, err := b.client.ListSecrets(ctx, b.namespace, SecretResourceLabel+"="+SecretResourceValue)
	if err != nil {
		return nil, fmt.Errorf("list kube secrets: %w", err)
	}
	out := make([]StoredSecret, 0, len(secrets))
	for _, sec := range secrets {
		rawMeta, hasMeta := sec.Data["meta"]
		rawVal, hasVal := sec.Data["value"]
		if !hasMeta || !hasVal {
			slog.Warn("skipping malformed E2B kube secret missing meta or value", "name", sec.Metadata.Name)
			continue
		}
		var meta kubeSecretMeta
		if err := json.Unmarshal(rawMeta, &meta); err != nil {
			slog.Warn("skipping malformed E2B kube secret metadata", "name", sec.Metadata.Name, "err", err)
			continue
		}
		if meta.ID == "" || meta.TenantHash == "" || meta.Name == "" {
			slog.Warn("skipping incomplete E2B kube secret metadata", "name", sec.Metadata.Name)
			continue
		}
		out = append(out, StoredSecret{
			TenantHash: meta.TenantHash,
			ID:         meta.ID,
			Name:       meta.Name,
			Version:    meta.Version,
			CreatedAt:  meta.CreatedAt,
			UpdatedAt:  meta.UpdatedAt,
			Value:      string(rawVal),
		})
	}
	return out, nil
}

// Put writes or updates an E2B secret as a Kubernetes Secret via EnsureSecret.
func (b *KubeSecretBackend) Put(ctx context.Context, rec StoredSecret) error {
	metaBytes, err := json.Marshal(kubeSecretMeta{
		TenantHash: rec.TenantHash,
		ID:         rec.ID,
		Name:       rec.Name,
		Version:    rec.Version,
		CreatedAt:  rec.CreatedAt,
		UpdatedAt:  rec.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("marshal kube secret metadata: %w", err)
	}
	sec := controller.KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata: v1alpha1.ObjectMeta{
			Name:      KubeSecretName(rec.ID),
			Namespace: b.namespace,
			Labels: map[string]string{
				SecretResourceLabel: SecretResourceValue,
				SecretTenantLabel:   tenantLabelPrefix(rec.TenantHash),
			},
		},
		Data: map[string][]byte{
			"value": []byte(rec.Value),
			"meta":  metaBytes,
		},
	}
	return b.client.EnsureSecret(ctx, sec)
}

// Delete removes the Kubernetes Secret corresponding to secret ID.
func (b *KubeSecretBackend) Delete(ctx context.Context, id string) error {
	return b.client.DeleteSecret(ctx, b.namespace, KubeSecretName(id))
}
