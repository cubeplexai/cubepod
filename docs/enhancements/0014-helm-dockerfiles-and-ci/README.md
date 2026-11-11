# CEP-0014: Production Helm Chart, CRD Manifests, Multi-Component Dockerfiles, and GitHub Actions CI/CD

- **CEP Number**: `0014`
- **Status**: `implementable`
- **Target Version**: `v0.3.0`
- **Components**: `.github/workflows/{ci.yml,release.yml}`, `deploy/helm/cubepod/`, `deploy/docker-compose/docker-compose.yaml`, `images/{controller,gateway,egress,podd}/Dockerfile`

---

## 1. Summary

Establishes production packaging, Kubernetes deployment manifests, and automated GitHub Actions CI/CD pipelines for CubePod:
1. **Component Dockerfiles (`images/`)**:
   - `images/podd/Dockerfile`: Packages statically-linked `podd-init` and `podd` into `/opt/cubepod/dist/` for `initContainer` injection into `/opt/cubepod/bin`.
   - `images/controller/Dockerfile`: Packages `cubepod-controller`.
   - `images/gateway/Dockerfile`: Packages `cubepod-gateway`.
   - `images/egress/Dockerfile`: Packages `cubepod-egress`.
2. **Complete Helm Chart & Kubernetes CRDs (`deploy/helm/cubepod/`)**:
   - `crds/cubepod.io_podboxes.yaml`, `crds/cubepod.io_podtemplates.yaml`, `crds/cubepod.io_podwarmpools.yaml`
   - `templates/namespace.yaml`, `templates/rbac.yaml`, `templates/controller-deployment.yaml`, `templates/gateway-deployment.yaml`, `templates/service.yaml`
3. **GitHub Actions CI/CD (`.github/workflows/ci.yml` & `.github/workflows/release.yml`)**:
   - Runs Rust (`cargo fmt --check`, `cargo clippy --workspace --all-targets -- -D warnings`, `cargo test`), Go (`gofmt`, `go vet`, `go test -race`, `make test-e2e`), Python SDK tests, and Helm chart verification on every push and pull request.

---

## 2. Motivation

### Goals
- Enable one-command deployment of CubePod onto any Kubernetes cluster via `helm upgrade --install` or `kubectl apply`.
- Enforce strict CI gates on every PR and automate release artifact builds.

### Non-Goals
- External cloud Terraform modules.

---

## 3. Design Details

### 3.1 Dockerfiles (`images/{podd,controller,gateway,egress}/Dockerfile`)
- Support both multi-stage source builds and pre-built `bin/` binary packaging for fast local/cluster image builds.
- `images/podd/Dockerfile` copies `podd-init` and `podd` to `/opt/cubepod/dist/` and sets default command `sh -c "cp -f /opt/cubepod/dist/podd-init /opt/cubepod/dist/podd /opt/cubepod/bin/ && chmod 0755 /opt/cubepod/bin/podd-init /opt/cubepod/bin/podd"`.

### 3.2 Helm Chart (`deploy/helm/cubepod/`)
- `templates/rbac.yaml`: Creates `ServiceAccount`, `ClusterRole` (permissions for `pods`, `persistentvolumeclaims`, `podboxes.cubepod.io`, `podtemplates.cubepod.io`, `podwarmpools.cubepod.io`), and `ClusterRoleBinding`.
- `templates/controller-deployment.yaml`: Runs `cubepod-controller` with `:8081` `/healthz` liveness/readiness probes.
- `templates/gateway-deployment.yaml` & `templates/service.yaml`: Runs `cubepod-gateway` with `:8080` `/health` probes and configurable `ClusterIP` / `NodePort` Service.

### 3.3 GitHub Actions Workflows (`.github/workflows/ci.yml` & `.github/workflows/release.yml`)
- `ci.yml`: Jobs for `rust-ci`, `go-ci`, `python-sdk-ci`, and `e2e-suite`.
- `release.yml`: Triggered on `v*` tags to compile all 6 binaries (`podd-init`, `podd`, `cubepod-controller`, `cubepod-gateway`, `cubepod-egress`, `cubepod`) and attach them to the GitHub Release.

---

## 4. Test Plan
- Automated manifest & template validation test (`pkg/controller/helm_test.go` or `make lint-helm`) verifying YAML structure of all CRDs, Helm templates, and GitHub workflows.

---

## 5. Graduation Criteria & PRR
- All Dockerfiles build cleanly (`docker build`) and deploy on the Kubernetes test cluster.

---

## 6. Alternatives Considered
- **Kustomize-only deployment**: Helm provides cleaner parameterization across `Block` (Kata + Longhorn) vs. `Filesystem` (NFS / test cluster) environments.
