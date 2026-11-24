#!/usr/bin/env bash
# Dedicated cluster only: never consumes or changes the caller's kubecontext.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
HERE="$ROOT/test/kind-e2e"
source "$HERE/versions.env"
export KIND_EXPERIMENTAL_PROVIDER=docker
CLUSTER_NAME=${CLUSTER_NAME:-cubepod-e2e-$(date +%s)-$$}
[[ "$CLUSTER_NAME" =~ ^cubepod-[a-z0-9-]+$ ]] || { echo 'CLUSTER_NAME must start with cubepod-'; exit 2; }
STATE="$ROOT/.kind-e2e/$CLUSTER_NAME"
# TEST-ONLY (CEP-0031 gate, do not merge): enable FirecrackerVolumeNFS only in a Kata FC run.
CEP0031_FC_NFS_GATE=${CEP0031_FC_NFS_GATE:-0}
if [[ $CEP0031_FC_NFS_GATE == 1 && ${KATA_VMM:-} != fc ]]; then echo 'CEP0031_FC_NFS_GATE requires KATA_VMM=fc'; exit 2; fi
ARTIFACTS=${ARTIFACTS:-$STATE/artifacts}
export KUBECONFIG="$STATE/kubeconfig"
export GOMAXPROCS=${GOMAXPROCS:-2} CARGO_BUILD_JOBS=${CARGO_BUILD_JOBS:-2}
mkdir -p "$STATE" "$ARTIFACTS"
chmod 700 "$STATE"
# Never let test/down/build invocations race over the same owned state.
exec 9>"$STATE/run.lock"
flock -n 9 || { echo "Another operation owns $STATE" >&2; exit 1; }
BUILD="$STATE/build"
NS=cubepod-kind
PF_PID=""
ACTIVE_OWNER=0
owned() { [[ -f "$STATE/owned" && $(<"$STATE/owned") == "$CLUSTER_NAME" ]]; }
k() { kubectl --context "kind-$CLUSTER_NAME" --request-timeout=20s "$@"; }
collect() {
  owned || return 0
  k get nodes -o wide >"$ARTIFACTS/nodes.txt" 2>&1 || true
  k get pods,pvc,podboxes.cubepod.io,podtemplates.cubepod.io -A -o wide >"$ARTIFACTS/resources.txt" 2>&1 || true
  k get events -A --sort-by=.metadata.creationTimestamp >"$ARTIFACTS/events.txt" 2>&1 || true
  k describe pods -A >"$ARTIFACTS/describe-pods.txt" 2>&1 || true
  k describe pvc -A >"$ARTIFACTS/describe-pvcs.txt" 2>&1 || true
  while read -r pod; do
    [[ -n "$pod" ]] || continue
    k logs -n "$NS" "$pod" --all-containers --prefix --tail=1000 >"$ARTIFACTS/${pod//\//-}.log" 2>&1 || true
    k logs -n "$NS" "$pod" --all-containers --previous --prefix --tail=1000 >"$ARTIFACTS/${pod//\//-}-previous.log" 2>&1 || true
  done < <(k get pods -n "$NS" -o name 2>/dev/null)
  timeout 90s kind export logs --name "$CLUSTER_NAME" "$ARTIFACTS/kind" || true
}
finish() {
  local rc=$?
  trap - EXIT
  set +e
  if [[ $ACTIVE_OWNER != 1 ]]; then
    if [[ -n ${KATA_VMM:-} ]]; then kata_cleanup || rc=1; fi
    echo "No cluster acquired by this invocation; artifacts: $ARTIFACTS"
    exit "$rc"
  fi
  if [[ -z ${KATA_VMM:-} || ! -f "$STATE/kata-diagnostics-collected" ]]; then
    collect
    if [[ -n ${KATA_VMM:-} ]]; then kata_collect; touch "$STATE/kata-diagnostics-collected"; fi
  fi
  if [[ -n ${KATA_VMM:-} ]]; then
    if ! kata_teardown_node; then
      echo "Kata device cleanup failed; retaining owned node and state for safe recovery" >&2
      KEEP_CLUSTER=1; rc=1
    fi
  fi
  # Keep the development control plane, not leaked sandboxes from failed creates.
  if owned && [[ ${KEEP_CLUSTER:-0} == 1 && -z ${KATA_VMM:-} ]]; then
    k delete podboxes.cubepod.io --all -n "$NS" --ignore-not-found --timeout=30s || { [[ $rc != 0 ]] || rc=1; }
    k delete pods,pvc -n "$NS" -l pod.cubeplex.ai/box --ignore-not-found --timeout=45s || { [[ $rc != 0 ]] || rc=1; }
  fi
  [[ -z "$PF_PID" ]] || { kill "$PF_PID" 2>/dev/null; wait "$PF_PID" 2>/dev/null; }
  if owned && [[ ${KEEP_CLUSTER:-0} != 1 ]]; then
    if timeout 120s kind delete cluster --name "$CLUSTER_NAME"; then
      rm -f "$STATE/owned" "$KUBECONFIG"
    else
      echo "Cluster deletion failed; preserving ownership marker for retry" >&2
      [[ $rc != 0 ]] || rc=1
    fi
  fi
  if [[ -n ${KATA_VMM:-} ]]; then kata_cleanup || rc=1; fi
  echo "Exit: $rc; artifacts: $ARTIFACTS"
  if owned; then echo "Retained cluster: $CLUSTER_NAME; KUBECONFIG=$KUBECONFIG"; fi
  exit "$rc"
}
preflight() {
  for tool in kind kubectl helm docker go cargo python3 npm node timeout flock; do
    command -v "$tool" >/dev/null || { echo "Missing prerequisite: $tool" >&2; return 1; }
  done
  [[ $(kind version) == "kind $KIND_VERSION "* ]] || { echo "Expected kind $KIND_VERSION"; return 1; }
  [[ $(uname -s) == Linux ]] || { echo 'Native builds require Linux (or run in a Linux VM).'; return 1; }
  docker info >/dev/null
  # Build before creating Kubernetes so compiler and cluster peaks do not overlap.
  local available
  available=$(awk '/MemAvailable:/ { print $2 }' /proc/meminfo)
  (( available >= 3 * 1024 * 1024 )) || { echo 'Need at least 3 GiB available RAM; do not stop shared services to make room.'; return 1; }
  local disk
  disk=$(df -Pk "$ROOT" | awk 'NR == 2 {print $4}')
  (( disk >= 15 * 1024 * 1024 )) || { echo 'Need at least 15 GiB free disk for builds/images'; return 1; }
  python3 -m unittest discover -s "$HERE" -p test_harness.py
}
build_images() {
  mkdir -p "$BUILD"
  (cd "$ROOT"; timeout 600s env CGO_ENABLED=0 go build -p "$GOMAXPROCS" -trimpath -o "$BUILD/cubepod-gateway" ./cmd/cubepod-gateway
    timeout 600s env CGO_ENABLED=0 go build -p "$GOMAXPROCS" -trimpath -o "$BUILD/cubepod-controller" ./cmd/cubepod-controller
    if [[ $CEP0031_FC_NFS_GATE == 1 ]]; then timeout 600s env CGO_ENABLED=0 go build -p "$GOMAXPROCS" -trimpath -o "$BUILD/cubepod-volume-nfs" ./cmd/cubepod-volume-nfs; fi
    timeout 600s cargo build --locked --release -p podd -p podd-init
    cp "${CARGO_TARGET_DIR:-target}/release/podd" "${CARGO_TARGET_DIR:-target}/release/podd-init" "$BUILD/")
  local components=(gateway controller injector sandbox)
  [[ $CEP0031_FC_NFS_GATE != 1 ]] || components+=(volume-nfs)
  for component in "${components[@]}"; do
    local image=$component
    [[ "$component" != injector ]] || image=podd
    local target=$component
    if [[ "$component" == sandbox && -n ${KATA_VMM:-} ]]; then target=sandbox-block; fi
    timeout 600s docker build -f "$HERE/Dockerfile" --target "$target" --build-arg UBUNTU_MIRROR="${UBUNTU_MIRROR:-}" -t "docker.io/cubepod-kind/$image:$CLUSTER_NAME" "$BUILD"
  done
}
ensure_new_cluster() {
  if kind get clusters 2>/dev/null | awk -v name="$CLUSTER_NAME" '$0 == name { found=1 } END { exit !found }'; then
    echo "Refusing to adopt existing cluster $CLUSTER_NAME" >&2; return 1
  fi
}
create() {
  ensure_new_cluster
  if [[ -n ${KATA_VMM:-} ]]; then kata_prepare; fi
  # Mark ownership before create so even partial failures are collected/cleaned.
  ACTIVE_OWNER=1
  printf '%s\n' "$CLUSTER_NAME" >"$STATE/owned"
  cat >"$STATE/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "127.0.0.1"
  kubeProxyMode: "nftables"
nodes:
- role: control-plane
EOF
  if [[ -n ${KATA_VMM:-} ]]; then kata_kind_config; fi
  timeout 300s kind create cluster --name "$CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --config "$STATE/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 180s --retain
  chmod 600 "$KUBECONFIG"
  timeout 300s kind load docker-image --name "$CLUSTER_NAME" docker.io/cubepod-kind/{gateway,controller,podd,sandbox}:"$CLUSTER_NAME"
  if [[ $CEP0031_FC_NFS_GATE == 1 ]]; then timeout 300s kind load docker-image --name "$CLUSTER_NAME" "docker.io/cubepod-kind/volume-nfs:$CLUSTER_NAME"; fi
  if [[ -n ${KATA_VMM:-} ]]; then kata_configure; fi
  k create namespace "$NS"
  k label namespace "$NS" app.kubernetes.io/managed-by=Helm
  k annotate namespace "$NS" meta.helm.sh/release-name=cubepod meta.helm.sh/release-namespace="$NS"
  k apply -f "$HERE/storage.yaml"
  local runtime_args=()
  if [[ -n ${KATA_VMM:-} ]]; then runtime_args=(--set "runtime.runtimeClassName=kata-$KATA_VMM-e2e" --set storage.volumeMode=Block --set storage.storageClassName=cubepod-block); fi
  if [[ $CEP0031_FC_NFS_GATE == 1 ]]; then runtime_args+=(--set featureGates.firecrackerVolumeNFS=true --set volumes.nfs.image.registry=docker.io --set volumes.nfs.image.repository=cubepod-kind/volume-nfs --set "volumes.nfs.image.tag=$CLUSTER_NAME" --set volumes.nfs.image.pullPolicy=Never); fi
  timeout 240s helm upgrade --install cubepod "$ROOT/deploy/helm/cubepod" --namespace "$NS" --kube-context "kind-$CLUSTER_NAME" -f "$HERE/values.yaml" \
    --set gateway.image.tag="$CLUSTER_NAME" --set controller.image.tag="$CLUSTER_NAME" \
    --set runtime.poddInjectorImage="docker.io/cubepod-kind/podd:$CLUSTER_NAME" \
    --set runtime.defaultSandboxImage="docker.io/cubepod-kind/sandbox:$CLUSTER_NAME" "${runtime_args[@]}" --wait --timeout 180s
  local runtime=none storage=standard
  if [[ -n ${KATA_VMM:-} ]]; then runtime="kata-$KATA_VMM-e2e"; storage=cubepod-block; fi
  sed -e "s/:e2e/:$CLUSTER_NAME/g" -e "s/runtimeClassName: none/runtimeClassName: $runtime/" -e "s/storageClassName: standard/storageClassName: $storage/" "$HERE/template.yaml" | k apply -f -
  sed "s/:e2e/:$CLUSTER_NAME/g" "$HERE/network.yaml" | k apply -f -
  # Prebuilt local test image: declare it ready, without invoking template builds.
  k patch podtemplates.cubepod.io kind-e2e -n "$NS" --subresource=status --type=merge -p "{\"status\":{\"buildStatus\":\"ready\",\"readyImage\":\"docker.io/cubepod-kind/sandbox:$CLUSTER_NAME\",\"readyBuildID\":\"kind-local\",\"templateID\":\"kind-e2e\"}}"
  k rollout status deployment/network-target -n "$NS" --timeout=120s
}
test_sdks() {
  owned || { echo 'Missing ownership marker; use run first.'; return 1; }
  # Port 0 allocates an available loopback-only port atomically.
  kubectl --context "kind-$CLUSTER_NAME" --request-timeout=20s port-forward -n "$NS" service/cubepod-gateway :8080 --address 127.0.0.1 >"$ARTIFACTS/port-forward.log" 2>&1 &
  PF_PID=$!
  local port=""
  for ((i=0; i<100; i++)); do
    port=$(awk '/Forwarding from 127.0.0.1:/ { split($3,a,":"); print a[2]; exit }' "$ARTIFACTS/port-forward.log")
    [[ -z "$port" ]] || break
    kill -0 "$PF_PID" || { echo 'Port forward exited'; return 1; }
    sleep 0.1
  done
  [[ -n "$port" ]] || { echo 'Port forward timed out'; return 1; }
  export E2B_API_URL="http://127.0.0.1:$port" E2B_SANDBOX_URL="http://127.0.0.1:$port" E2B_VOLUME_API_URL="http://127.0.0.1:$port" E2B_API_KEY=cpod_kind_test_only
  local sdk=0
  if [[ -n ${KATA_VMM:-} ]]; then kata_watch_sdks; fi
  STATE="$STATE" ARTIFACTS="$ARTIFACTS" bash "$HERE/run-sdks.sh" || sdk=$?
  if [[ -n ${KATA_VMM:-} ]]; then kata_verify_sdks || sdk=1; fi
  # Never turn a missing report, unlabeled skip, or unexpectedly empty suite into green.
  local deleted=0
  k wait --for=delete pods -n "$NS" -l pod.cubeplex.ai/box --timeout=60s >"$ARTIFACTS/sandbox-cleanup.txt" 2>&1 || deleted=$?
  echo "SDK exit code=$sdk; sandbox deletion=$deleted"
  python3 "$HERE/check_reports.py" "$ARTIFACTS/python.xml" "$ARTIFACTS/typescript.xml" || return 1
  if [[ $CEP0031_FC_NFS_GATE == 1 ]]; then
    k get deployment cubepod-volume-nfs -n "$NS" -o yaml >"$ARTIFACTS/cep0031-volume-nfs-deployment.yaml" || return 1
    k logs -n "$NS" deployment/cubepod-volume-nfs --all-containers --tail=-1 >"$ARTIFACTS/cep0031-volume-nfs.log" || return 1
    k get nodes -o json >"$ARTIFACTS/cep0031-nodes.json" || return 1
    python3 "$ROOT/test/kata-e2e/verify_cep0031_gate.py" "$ARTIFACTS" | tee "$ARTIFACTS/cep0031-gate.json" || return 1
  fi
  (( sdk == 0 && deleted == 0 ))
}
if [[ -f "$STATE/kata-owned" ]]; then
  stored_vmm=$(<"$STATE/kata-profile")
  [[ "$stored_vmm" == fc || "$stored_vmm" == clh ]] || { echo 'Invalid owned Kata profile'; exit 2; }
  [[ -z ${KATA_VMM:-} || "$KATA_VMM" == "$stored_vmm" ]] || { echo 'Owned runtime/profile mismatch'; exit 2; }
  KATA_VMM=$stored_vmm
fi
if [[ -n ${KATA_VMM:-} ]]; then
  export KATA_VMM
  source "$ROOT/test/kata-e2e/hooks.sh"
fi
case "${1:-run}" in
  run) preflight; ensure_new_cluster; trap finish EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; build_images; create; test_sdks ;;
  test) preflight; owned || { echo 'No owned cluster; use run first.'; exit 1; }; ACTIVE_OWNER=1; if [[ -n ${KATA_VMM:-} ]]; then KATA_ACTIVE=1; fi; trap finish EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; test_sdks ;;
  down) if owned; then ACTIVE_OWNER=1
    elif [[ -n ${KATA_VMM:-} && -f "$STATE/kata-owned" && $(<"$STATE/kata-owned") == "$CLUSTER_NAME" ]]; then ACTIVE_OWNER=0
    else echo 'No owned cluster or Kata setup at this state path'; exit 1; fi; if [[ -n ${KATA_VMM:-} ]]; then KATA_ACTIVE=1; fi; trap finish EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; KEEP_CLUSTER=0 ;;
  *) echo 'Usage: run.sh [run|test|down]'; exit 2 ;;
esac
