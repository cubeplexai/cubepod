#!/usr/bin/env bash
# Run only a newly owned Compose project. Never down/prune shared Docker state.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
HERE="$ROOT/test/compose-e2e"
export COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME:-cubepod-compose-e2e-$(date +%s)-$$}
[[ "$COMPOSE_PROJECT_NAME" =~ ^cubepod-compose-e2e-[a-z0-9-]+$ ]] || { echo 'Project must start with cubepod-compose-e2e-'; exit 2; }
export STATE="$ROOT/.compose-e2e/$COMPOSE_PROJECT_NAME"
export ARTIFACTS=${ARTIFACTS:-$STATE/artifacts}
mkdir -p "$STATE" "$ARTIFACTS"; chmod 700 "$STATE"
exec 9>"$STATE/run.lock"; flock -n 9 || { echo 'Project operation already active'; exit 1; }
MODE=${1:-run}
[[ "$MODE" == run || "$MODE" == down ]] || { echo 'Usage: run.sh [run|down]'; exit 2; }
if [[ "$MODE" == run ]]; then
 [[ ! -e "$STATE/owned" ]] || { echo 'Refusing to adopt an existing state directory'; exit 1; }
else
 [[ -f "$STATE/owned" && $(<"$STATE/owned") == "$COMPOSE_PROJECT_NAME" ]] || { echo 'No owned project'; exit 1; }
fi
export GOMAXPROCS=${GOMAXPROCS:-2} CARGO_BUILD_JOBS=${CARGO_BUILD_JOBS:-2}
if [[ -n ${COMPOSE_BIN:-} ]]; then COMPOSE=("$COMPOSE_BIN"); else COMPOSE=(docker compose); fi
# v2.39.4 supports !reset/!override used to remove the shipped fixed ports.
[[ $("${COMPOSE[@]}" version --short) == 2.39.4 ]] || { echo 'Compose v2.39.4 is required'; exit 1; }
d() { timeout 60s docker "$@"; }
c() { timeout 180s "${COMPOSE[@]}" --project-name "$COMPOSE_PROJECT_NAME" -f "$ROOT/deploy/docker-compose/docker-compose.yaml" -f "$HERE/compose.yaml" "$@"; }
if [[ "$MODE" == run ]]; then
for tool in docker go cargo python3 node npm timeout flock; do command -v "$tool" >/dev/null; done
d info >/dev/null
available=$(awk '/MemAvailable:/ {print $2}' /proc/meminfo)
(( available >= 1536 * 1024 )) || { echo 'Need 1.5 GiB available RAM; do not stop shared services'; exit 1; }
(( $(df -Pk "$ROOT" | awk 'NR==2 {print $4}') >= 10 * 1024 * 1024 )) || { echo 'Need 10 GiB free disk'; exit 1; }
# Exact name collisions are refused before acquiring ownership.
[[ -z $(d ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME") ]]
! d network inspect "${COMPOSE_PROJECT_NAME}_default" >/dev/null 2>&1
[[ -z $(d ps -aq --filter "name=^/${COMPOSE_PROJECT_NAME}-sb-") ]]
[[ -z $(d volume ls --format '{{.Name}}' | awk -v p="cubepod-vol-${COMPOSE_PROJECT_NAME}-" 'index($0,p)==1') ]]
for suffix in runtime-bin sandbox-data volumes; do
  ! d volume inspect "$COMPOSE_PROJECT_NAME-$suffix" >/dev/null 2>&1
done
fi
ACTIVE=0
export E2E_CONTROL_IMAGE="docker.io/cubepod-compose/control:$COMPOSE_PROJECT_NAME"
export E2E_SANDBOX_IMAGE="docker.io/cubepod-compose/sandbox:$COMPOSE_PROJECT_NAME"
export E2E_HOST_BIN=uninitialized E2E_HOST_DATA=uninitialized E2E_HOST_VOLUMES=uninitialized
sandbox_ids() { d ps -aq --filter "name=^/${COMPOSE_PROJECT_NAME}-sb-"; }
volume_names() { d volume ls --format '{{.Name}}' | awk -v p="cubepod-vol-${COMPOSE_PROJECT_NAME}-" 'index($0,p)==1'; }
collect() {
  c ps --all >"$ARTIFACTS/compose-ps.txt" 2>&1 || true
  c logs --no-color --timestamps >"$ARTIFACTS/compose.log" 2>&1 || true
  c config >"$ARTIFACTS/compose-config.yaml" 2>&1 || true
  while read -r id; do
    [[ -n "$id" ]] || continue
    d inspect "$id" >"$ARTIFACTS/sandbox-$id.json" 2>&1 || true
    d logs "$id" >"$ARTIFACTS/sandbox-$id.log" 2>&1 || true
  done < <(sandbox_ids)
}
finish() {
  rc=$?; trap - EXIT; set +e
  if [[ "$ACTIVE" == 1 ]]; then
    collect
    while read -r id; do [[ -z "$id" ]] || d rm -f "$id" || rc=1; done < <(sandbox_ids)
    c down --volumes --remove-orphans --timeout 15 >"$ARTIFACTS/cleanup.log" 2>&1 || rc=1
    while read -r name; do [[ -z "$name" ]] || d volume rm "$name" || rc=1; done < <(volume_names)
    for suffix in runtime-bin sandbox-data volumes; do
      name="$COMPOSE_PROJECT_NAME-$suffix"
      if [[ $(d volume inspect --format '{{index .Labels "io.cubepod.e2e"}}' "$name" 2>/dev/null) == "$COMPOSE_PROJECT_NAME" ]]; then
        d volume rm "$name" || rc=1
      fi
    done
    for image in "$E2E_CONTROL_IMAGE" "$E2E_SANDBOX_IMAGE"; do
      if d image inspect "$image" >/dev/null 2>&1; then
        d image rm "$image" >>"$ARTIFACTS/image-cleanup.log" 2>&1 || rc=1
      fi
    done
    if [[ -n $(sandbox_ids) || -n $(volume_names) || -n $(d ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME") ]]; then rc=1; fi
    [[ "$rc" != 0 ]] || rm -f "$STATE/owned"
  fi
  printf 'Exit: %s; project: %s; artifacts: %s\n' "$rc" "$COMPOSE_PROJECT_NAME" "$ARTIFACTS"
  exit "$rc"
}
ACTIVE=1; printf '%s\n' "$COMPOSE_PROJECT_NAME" >"$STATE/owned"
trap finish EXIT; trap 'exit 130' INT; trap 'exit 143' TERM
[[ "$MODE" != down ]] || exit 0
BUILD="$STATE/build"; mkdir -p "$BUILD"
(cd "$ROOT"
 for component in gateway controller egress; do
  timeout 600s env CGO_ENABLED=0 go build -p "$GOMAXPROCS" -trimpath -o "$BUILD/cubepod-$component" "./cmd/cubepod-$component"
 done
 timeout 600s cargo build --locked --release -p podd -p podd-init
 cp "${CARGO_TARGET_DIR:-target}/release/podd" "${CARGO_TARGET_DIR:-target}/release/podd-init" "$BUILD/")
cp "$ROOT/deploy/docker-compose/gateway-entrypoint.sh" "$BUILD/"
timeout 600s docker build -f "$HERE/Dockerfile" --build-arg UBUNTU_MIRROR="${UBUNTU_MIRROR:-}" -t "$E2E_CONTROL_IMAGE" "$BUILD"
timeout 600s docker build -f "$ROOT/test/kind-e2e/Dockerfile" --target sandbox --build-arg UBUNTU_MIRROR="${UBUNTU_MIRROR:-}" -t "$E2E_SANDBOX_IMAGE" "$BUILD"
for suffix in runtime-bin sandbox-data volumes; do
 d volume create --label "io.cubepod.e2e=$COMPOSE_PROJECT_NAME" "$COMPOSE_PROJECT_NAME-$suffix" >/dev/null
done
export E2E_HOST_BIN=$(d volume inspect --format '{{.Mountpoint}}' "$COMPOSE_PROJECT_NAME-runtime-bin")
export E2E_HOST_DATA=$(d volume inspect --format '{{.Mountpoint}}' "$COMPOSE_PROJECT_NAME-sandbox-data")
export E2E_HOST_VOLUMES=$(d volume inspect --format '{{.Mountpoint}}' "$COMPOSE_PROJECT_NAME-volumes")
d run --rm --name "$COMPOSE_PROJECT_NAME-bin-copy" --label "com.docker.compose.project=$COMPOSE_PROJECT_NAME" -v "$COMPOSE_PROJECT_NAME-runtime-bin:/out" "$E2E_CONTROL_IMAGE" cp /opt/cubepod/dist/podd /opt/cubepod/dist/podd-init /out/
c up --detach --no-build
port=$(c port cubepod-gateway 8080)
[[ "$port" =~ ^127\.0\.0\.1:[0-9]+$ ]] || { echo "Unexpected gateway bind: $port"; exit 1; }
export E2B_API_URL="http://$port"
python3 - <<'PY'
import os,time,urllib.request
url=os.environ['E2B_API_URL']+'/health'
end=time.monotonic()+90
while time.monotonic()<end:
 try:
  with urllib.request.urlopen(url,timeout=2) as r:
   if r.status==200:break
 except OSError:pass
 time.sleep(.2)
else:raise SystemExit('Gateway did not become healthy within 90 seconds')
PY
sdk=0
bash "$ROOT/test/kind-e2e/run-sdks.sh" || sdk=$?
# Destruction is part of the contract, not something cleanup can turn green.
[[ -z $(sandbox_ids) && -z $(volume_names) ]] || { echo 'SDK leaked sandbox containers/volumes'; exit 1; }
(( sdk == 0 ))
