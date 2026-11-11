#!/bin/bash
# Install Longhorn v1.8.2 V2 on the labeled storage node and register the AIO disk.
# Does not restart kubelet unless CUBEPOD_RESTART_KUBELET=1.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
if [[ -f "$ROOT/../env.local" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT/../env.local"
  set +a
fi
NODE="${CUBEPOD_LONGHORN_NODE:-}"
NS="${CUBEPOD_LONGHORN_NAMESPACE:-longhorn-system}"
SYSTEM_NS="${CUBEPOD_SYSTEM_NAMESPACE:-cubepod-system}"
GATEWAY_NAME="${CUBEPOD_GATEWAY_NAME:-cubepod-gateway}"
IMG="${CUBEPOD_LONGHORN_IMG:-/storage/cubepod/longhorn-v2.img}"
DEV="${CUBEPOD_LONGHORN_DEV:-/dev/cubepod/longhorn-v2}"
SIZE="${CUBEPOD_LONGHORN_SIZE:-80G}"
PAGES="${CUBEPOD_HUGEPAGES:-1024}"
PULL="${CUBEPOD_IMAGE_PULL_SECRET:-}"
PULL_BLOCK=""
if [[ -n "$PULL" ]]; then
  PULL_BLOCK="$(printf 'imagePullSecrets:\n        - name: %s' "$PULL")"
fi
if [[ -z "$NODE" ]]; then
  echo "Set CUBEPOD_LONGHORN_NODE to the worker that should host the V2 disk." >&2
  kubectl get nodes
  exit 2
fi
PREP_IMAGE="${CUBEPOD_PREP_IMAGE:-${CUBEPOD_SANDBOX_IMAGE:-}}"
if [[ -z "$PREP_IMAGE" ]]; then
  while IFS= read -r arg; do
    case "$arg" in
      -sandbox-image=*|--sandbox-image=*) PREP_IMAGE="${arg#*=}" ;;
    esac
  done < <(kubectl -n "$SYSTEM_NS" get deploy "$GATEWAY_NAME" -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null || true)
fi
if [[ -z "$PREP_IMAGE" ]]; then
  echo "Set CUBEPOD_PREP_IMAGE to an image that contains bash and nsenter." >&2
  exit 2
fi

render_manifest() {
  local text
  text="$(cat "$1")"
  text="${text//__CUBEPOD_PREP_IMAGE__/${PREP_IMAGE}}"
  text="${text//__CUBEPOD_LONGHORN_IMG__/${IMG}}"
  text="${text//__CUBEPOD_LONGHORN_DEV__/${DEV}}"
  text="${text//__CUBEPOD_LONGHORN_SIZE__/${SIZE}}"
  text="${text//__CUBEPOD_HUGEPAGES__/${PAGES}}"
  text="${text//__CUBEPOD_PULL_SECRETS__/${PULL_BLOCK}}"
  printf '%s' "$text"
}

kubectl label node "$NODE" cubepod.io/longhorn-v2=true --overwrite

kubectl -n kube-system delete job cubepod-longhorn-v2-prep --ignore-not-found
kubectl -n kube-system create configmap cubepod-longhorn-v2-prep \
  --from-file=prep-node.sh="$ROOT/prep-node.sh" \
  --dry-run=client -o yaml | kubectl apply -f -
render_manifest "$ROOT/node-prep-job.yaml" | kubectl apply -f -
kubectl -n kube-system wait --for=condition=complete job/cubepod-longhorn-v2-prep --timeout=180s
kubectl -n kube-system logs job/cubepod-longhorn-v2-prep

hp="$(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.hugepages-2Mi}')"
echo "allocatable hugepages-2Mi=${hp:-0}"
if [[ -z "$hp" || "$hp" == "0" ]]; then
  if [[ "${CUBEPOD_RESTART_KUBELET:-0}" != "1" ]]; then
    echo "kubelet on $NODE does not advertise hugepages yet." >&2
    echo "Re-run with CUBEPOD_RESTART_KUBELET=1 to restart kubelet on that node." >&2
    echo "That restart bounces every Pod scheduled on $NODE." >&2
    exit 2
  fi
  echo "restarting kubelet on $NODE"
  kubectl -n kube-system delete pod cubepod-restart-kubelet --ignore-not-found --wait=false
  restart_pull=""
  if [[ -n "$PULL" ]]; then
    restart_pull="$(printf '  imagePullSecrets:\n    - name: %s' "$PULL")"
  fi
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: cubepod-restart-kubelet
  namespace: kube-system
spec:
${restart_pull}
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  hostPID: true
  containers:
    - name: restart
      image: ${PREP_IMAGE}
      imagePullPolicy: IfNotPresent
      securityContext:
        privileged: true
        runAsUser: 0
      command: ["bash", "-c", "nsenter -t 1 -m -u -i -n -- systemctl restart kubelet"]
EOF
  for _ in $(seq 1 90); do
    hp="$(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.hugepages-2Mi}' 2>/dev/null || true)"
    if [[ -n "$hp" && "$hp" != "0" ]]; then
      break
    fi
    sleep 2
  done
  if [[ -z "$hp" || "$hp" == "0" ]]; then
    echo "kubelet on $NODE still does not advertise hugepages-2Mi" >&2
    exit 1
  fi
fi
echo "allocatable hugepages-2Mi=$hp"

helm repo add longhorn https://charts.longhorn.io
helm repo update longhorn
helm upgrade --install longhorn longhorn/longhorn \
  --namespace "$NS" \
  --create-namespace \
  --version 1.8.2 \
  --values "$ROOT/values.yaml" \
  --timeout 15m

kubectl apply -f "$ROOT/storageclass.yaml"

echo "waiting for Longhorn node object $NODE"
for _ in $(seq 1 60); do
  if kubectl -n "$NS" get "nodes.longhorn.io/$NODE" >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
kubectl -n "$NS" get "nodes.longhorn.io/$NODE" >/dev/null

kubectl -n "$NS" patch "nodes.longhorn.io/$NODE" --type merge --patch-file /dev/stdin <<EOF
spec:
  disks:
    v2-aio:
      allowScheduling: true
      diskType: block
      path: "${DEV}"
      storageReserved: 0
      tags:
        - cubepod-v2
EOF

echo "Longhorn V2 install submitted on node ${NODE}."
echo "  disk ${DEV}  image ${IMG}  size ${SIZE}"
echo "Confirm the disk is schedulable with:"
echo "  kubectl -n $NS get nodes.longhorn.io $NODE -o yaml"
echo "Then run: make test-k8s-block"
