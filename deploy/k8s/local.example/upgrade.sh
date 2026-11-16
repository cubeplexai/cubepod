#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
LOCAL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RELEASE="${CUBEPOD_HELM_RELEASE:-cubepod}"
SYS_NS="${CUBEPOD_SYSTEM_NAMESPACE:-cubepod-system}"
WORKLOAD_NS="${CUBEPOD_WORKLOADS_NAMESPACE:-cubepod-workloads}"

echo "==> 1. Applying CRDs from deploy/helm/cubepod/crds/"
kubectl apply -f "${ROOT_DIR}/deploy/helm/cubepod/crds/"

echo "==> 2. Ensuring Helm ownership on existing workload namespace (${WORKLOAD_NS})"
if kubectl get namespace "${WORKLOAD_NS}" >/dev/null 2>&1; then
  kubectl label namespace "${WORKLOAD_NS}" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate namespace "${WORKLOAD_NS}" \
    meta.helm.sh/release-name="${RELEASE}" \
    meta.helm.sh/release-namespace="${SYS_NS}" \
    --overwrite
fi

echo "==> 3. Running helm upgrade --install"
helm upgrade --install "${RELEASE}" "${ROOT_DIR}/deploy/helm/cubepod" \
  --namespace "${SYS_NS}" \
  --create-namespace \
  -f "${LOCAL_DIR}/values.yaml"

echo "==> 4. Waiting for rollout"
kubectl -n "${SYS_NS}" rollout status deployment/"${RELEASE}-controller" --timeout=180s
kubectl -n "${SYS_NS}" rollout status deployment/"${RELEASE}-gateway" --timeout=180s
