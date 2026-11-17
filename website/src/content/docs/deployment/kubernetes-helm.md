---
title: Kubernetes & Helm Deployment
description: Deploy the CubePod operator, gateway, egress proxy, CRDs, and optional rootless BuildKit builder on Kubernetes using Helm.
---

For Kubernetes deployments, CubePod is packaged as Helm chart `cubepod` (`version: 0.5.4`, `appVersion: "0.5.4"`) under `deploy/helm/cubepod/`. It installs the Custom Resource Definitions (`PodBox`, `PodTemplate`, `PodWarmPool`, `SandboxWebhook`), RBAC rules, workload namespace, Control Plane Gateway, Kubernetes Controller, and—when enabled—an isolated rootless BuildKit builder deployment.

For the exhaustive key-by-key schema generated directly from `deploy/helm/cubepod/values.yaml`, see the [Helm Values Reference](/reference/helm-values/) and [CRD Reference](/reference/crds/).

## Prerequisites

- Kubernetes cluster with `kubectl` and `helm` (`v3+`) configured.
- A [Kata Containers runtime and CSI block storage](/deployment/kata-and-block-storage/) provisioner (such as Longhorn V2 SPDK `longhorn-v2-block-thin` or OpenEBS LVM-LocalPV `cubepod-lvm-block`) if running in production microVM mode.

## Quick Install

Install directly from the published GHCR OCI registry into a control-plane namespace such as `cubepod-system` (no repository clone required):

```bash
helm install cubepod oci://ghcr.io/cubeplexai/charts/cubepod \
  --version 0.5.4 \
  --namespace cubepod-system \
  --create-namespace
```

Or install from a local repository checkout:

```bash
helm install cubepod ./deploy/helm/cubepod \
  --namespace cubepod-system \
  --create-namespace
```

Helm automatically installs the four CRDs in `deploy/helm/cubepod/crds/` on a fresh `helm install`:

- `cubepod.io_podboxes.yaml`
- `cubepod.io_podtemplates.yaml`
- `cubepod.io_podwarmpools.yaml`
- `cubepod.io_sandboxwebhooks.yaml`

:::note
Helm skips upgrading files inside `crds/` on subsequent `helm upgrade` runs. When upgrading CubePod, apply updated CRD manifests explicitly before running `helm upgrade`:
```bash
kubectl apply -f deploy/helm/cubepod/crds/
```
:::

## Deployed Resources Architecture

```mermaid
flowchart TB
  Gateway["Deployment: cubepod-gateway (default 2 replicas, :8080)"] -->|"/sandboxes, /commands, /files, /panel"| Workloads["Namespace: cubepod-workloads\nPodBox Kata Pods + PVCs"]
  Gateway -->|"Internal HTTP (:8081)"| Controller["Deployment: cubepod-controller (:8081)"]
  Controller -->|"CRD Reconcile (5s syncInterval)"| K8sAPI["Kubernetes API\nPodBox / PodTemplate / PodWarmPool"]
  Controller -->|"TCP :1234 (NetworkPolicy-restricted)"| BuildKit["Deployment: cubepod-buildkit\n(moby/buildkit:rootless, optional)"]
  BuildKit -->|"Persistent cache"| BKCache[("PVC: cubepod-buildkit-cache (20Gi)")]
  Controller -->|"Build context sync"| CtxVol[("PVC: cubepod-template-context (10Gi)")]
```

| Template File | Resources Created | Notes |
| :--- | :--- | :--- |
| `namespace.yaml` | `Namespace` (`controller.workloadNamespace`, default `cubepod-workloads`) | Labeled `cubepod.io/role: workloads` |
| `rbac.yaml` | `ServiceAccount`, `ClusterRole` (`<fullname>-controller`), `ClusterRoleBinding` | Grants CRUD on `pods`, `pods/exec`, `pods/log`, `persistentvolumeclaims`, `events`, `configmaps`, `secrets` and `cubepod.io`/`pod.cubeplex.ai` resources (`podboxes`, `podtemplates`, `podwarmpools`, `sandboxwebhooks`) + statuses; read access to `apps` `deployments`/`statefulsets` |
| `gateway-deployment.yaml` | `Deployment` (`gateway.replicaCount`, default `2`) | Serves `/health` liveness (`initialDelaySeconds: 5, periodSeconds: 10`) & readiness (`initialDelaySeconds: 2, periodSeconds: 5`); wires runtime/storage/egress flags, `--feature-gate`, `secrets.*`, `e2bVolumes.*`, `global.imagePullSecrets`, `egress.caSecretName`, and optional `gateway.persistence` |
| `pdb.yaml` *(Added in v0.6.0)* | `PodDisruptionBudget` (`<fullname>-gateway`) | Rendered when `gateway.podDisruptionBudget.enabled` is `true`, or `null` with `gateway.replicaCount > 1` (defaults to `minAvailable: 1`) |
| `controller-deployment.yaml` | `Deployment` (`controller.replicaCount`, forced to `1` when `templateBuild.enabled` is true) | Serves `/livez` and `/healthz`; reconciles on `--sync-interval` (`5s`); wires runtime/storage/egress flags, `--feature-gate`, `global.imagePullSecrets`, `egress.caSecretName`, and mounts `cubepod-template-context` PVC when `templateBuild.enabled` is true |
| `gateway-state-pvc.yaml` | `PersistentVolumeClaim` (`<fullname>-gateway-state`, optional) | Rendered when `gateway.persistence.enabled: true` and `existingClaim` is empty; persists Gateway `stateDir` (templates, file secrets, events/webhooks) |
| `secrets-key-secret.yaml` *(Added in v0.6.0)* | `Secret` (`<fullname>-secrets-key`, optional) | Rendered when `secrets.encryptionKey` is set inline without `secrets.existingSecret` or `gateway.secretsKeySecret` |
| `e2b-volumes-pvc.yaml` | `PersistentVolumeClaim` (`e2bVolumes.pvcName`, `ReadWriteMany`) | Rendered when `e2bVolumes.rwxStorageClassName` is non-empty; kept on uninstall via `helm.sh/resource-policy: keep` |
| `service.yaml` | `<fullname>-gateway` (`:8080`), `<fullname>-controller` (`:8081`) | Both default to `ClusterIP`; `gateway.service.nodePort` supported when switching to `NodePort` |
| `buildkit.yaml` | `Deployment`, `Service` (`:1234`), 2 `PVC`s, `NetworkPolicy` | Rendered only when `templateBuild.enabled: true` |

## Core Production Values

Override defaults in a custom `my-values.yaml` file rather than editing the chart directly:

```yaml
gateway:
  replicaCount: 2
  port: 8080
  panelSecret: "replace-with-random-hmac-secret"
  service:
    type: ClusterIP

runtime:
  runtimeClassName: kata-qemu
  defaultTemplate: cubeplex-ubuntu24-desktop
  poddInjectorImage: ghcr.io/cubeplexai/cubepod-podd:v0.5.4
  defaultSandboxImage: ghcr.io/cubeplexai/cubeplex-sandbox:v0.4.0

storage:
  storageClassName: longhorn-v2-block-thin
  volumeMode: Block
  rootfsSize: "10Gi"
  workspaceSize: "20Gi"

warmPool:
  enabled: true
  replicas: 3

egress:
  enabled: true
  image:
    registry: "ghcr.io"
    repository: "cubeplexai/cubepod-egress"
    tag: "v0.5.4"
  port: 18080
  exchangeUrl: "https://cubeplex-backend.cubeplex.svc:8443/api/v1/internal/egress/exchange"
```

Install or upgrade with your overrides:

```bash
helm upgrade --install cubepod ./deploy/helm/cubepod \
  --namespace cubepod-system \
  --create-namespace \
  -f my-values.yaml
```

### Key Runtime & Storage Defaults

- **Runtime injection (`runtime.poddInjectorImage`)**: Defaults to `ghcr.io/cubeplexai/cubepod-podd:v0.5.4`. Every sandbox pod uses an `initContainer` to copy `podd-init` and `podd` into an `emptyDir` mounted at `/opt/cubepod/bin` so user images remain clean.
- **Registry pull secrets (`global.imagePullSecrets`, `runtime.templatePullSecret`)**: Added in `v0.5.2`. Attaches private registry pull secrets to control-plane pods (`controller`, `gateway`, `buildkit`) and sandbox pods even when `templateBuild.enabled` is `false`.
- **Egress CA & state persistence (`egress.caSecretName`, `gateway.persistence`)**: Added in `v0.5.2`. Mounts a persistent root CA secret into both controller and gateway and optionally persists Gateway `stateDir` (`CUBEPOD_STATE`) for templates, E2B secrets, and webhooks.
- **Dual block PVCs (`storage.*`)**: Defaults to `storageClassName: longhorn-v2-block-thin`, `volumeMode: Block`, `rootfsSize: "10Gi"`, and `workspaceSize: "20Gi"`.
- **WarmPool (`warmPool.*`)**: Enabled by default with `3` pre-warmed replicas so `POST /sandboxes` can claim an already-running microVM sub-second.
- **Gateway PodDisruptionBudget (`gateway.podDisruptionBudget.*`)** *(Added in v0.6.0)*: When `gateway.podDisruptionBudget.enabled` is `null` (default) and `gateway.replicaCount > 1`, `pdb.yaml` automatically provisions a `PodDisruptionBudget` with `minAvailable: 1` (or `maxUnavailable` if set).
- **Zero-Trust Egress (`egress.*`)** *(Changed in v0.6.0)*: Enabled by default (`egress.enabled: true`, `egress.transparent: true`), injecting the `cubepod-egress` sidecar (`egress.image.*`) on port `18080` and swapping `cpodref_` placeholders against `egress.exchangeUrl`. Both `egress.transparent` and `featureGates.transparentEgress` must be `true` for `TransparentEgress` (`CUBEPOD_TRANSPARENT=1` + `NET_ADMIN`) to activate. See [Zero-Trust Egress](/concepts/zero-trust-egress/).
- **E2B Secrets HA Store (`secrets.*`)** *(Added in v0.6.0)*: Configures `CUBEPOD_SECRETS_BACKEND` (`"k8s"` default for multi-replica HA via `KubeSecretBackend`, `"file"` using `secrets.dir`, or `"disabled"`) and encryption key wiring (`secrets.existingSecret`, inline `secrets.encryptionKey`, or legacy fallback `gateway.secretsKeySecret`).
- **Unified Feature Gates (`featureGates.*`)** *(Changed in v0.6.0)*: Chart values under `featureGates` (`e2bTemplateAPI`, `transparentEgress`, `desktopStack`, `statelessGatewayHA`, `e2bFilesystemWatch`, `e2bSecretsAPI`, `sandboxEventsAndWebhooks`, `k8sE2BVolumeMounts`) are rendered into `--feature-gate=...` on both `cubepod-gateway` and `cubepod-controller`.
- **Shared E2B RWX Volumes (`e2bVolumes.*`)** *(Changed in v0.6.0)*: Setting `e2bVolumes.rwxStorageClassName` (for example `"longhorn-rwx"` or `"nfs-storage"`) automatically creates the `ReadWriteMany` PVC (`e2bVolumes.pvcName`), mounts it into `cubepod-gateway`, and **automatically enables `K8sE2BVolumeMounts`** even if `e2bVolumes.enabled` and `featureGates.k8sE2BVolumeMounts` remain `false`.

## Enabling In-Cluster Template Builds (`BuildKit`)

By default, both `featureGates.e2bTemplateAPI` and `templateBuild.enabled` are `false`. Enabling template builds deploys a locked-down `moby/buildkit:rootless` builder pod alongside the controller:

```yaml
featureGates:
  e2bTemplateAPI: true

templateBuild:
  enabled: true
  cacheSize: "20Gi"
  probeTimeout: "120s"
  buildkit:
    image: "moby/buildkit:rootless"
    address: "tcp://cubepod-buildkit.cubepod-system.svc:1234"
    nodeSelector: {}
    # Optional nodeSelector: { role: builder }
  registry:
    url: "ghcr.io/your-org"
    repositoryPrefix: "cubepod-templates"
    pushSecret: "cubepod-buildkit-push"
    pullSecret: "cubepod-buildkit-pull"
```

Operational constraints when `templateBuild.enabled: true`:

1. **Controller replica pinning**: `controller-deployment.yaml` forces controller replicas to `1` because `cubepod-template-context` (`10Gi`) is mounted `ReadWriteOnce` at `/var/lib/cubepod/context`.
2. **Rootless BuildKit security context**: `cubepod-buildkit` runs as `1000:1000` with `--oci-worker-no-process-sandbox`, `seccompProfile: Unconfined`, and `appArmorProfile: Unconfined` so nested `RUN` builds work without root inside the builder container.
3. **Ingress NetworkPolicy**: `cubepod-buildkit-netpol` restricts TCP port `1234` strictly to pods carrying `app.kubernetes.io/component: controller`.
4. **Image push/pull secrets**: Create the Kubernetes Docker registry secrets referenced by `templateBuild.registry.pushSecret` and `pullSecret` in your control-plane and workload namespaces before triggering builds.

See [Templates & Image Builds](/sdks/templates/) for the SDK and CLI workflow.

## Verifying the Deployment

Confirm pods, CRDs, and health probes are green:

```bash
kubectl -n cubepod-system get pods
kubectl get crds | grep cubepod.io
kubectl -n cubepod-system port-forward svc/cubepod-gateway 8080:8080
curl -fsS http://localhost:8080/health
```

## Next Steps

- Prepare worker nodes for Kata Containers and raw block storage in [Kata Containers & Block Storage](/deployment/kata-and-block-storage/).
- Inspect every Helm value in [Helm Values Reference](/reference/helm-values/) and CRD field in [CRD Reference](/reference/crds/).
