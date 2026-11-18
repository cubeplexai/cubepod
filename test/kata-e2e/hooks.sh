# Sourced by the isolated kind runner, never run against an arbitrary kubeconfig.
source "$ROOT/test/kata-e2e/versions.env"
KATA_HERE="$ROOT/test/kata-e2e"
KATA_WATCH_PID=""
KATA_ACTIVE=0
kata_node() {
  owned || { echo 'No owned cluster'; return 1; }
  [[ $(docker inspect --format '{{index .Config.Labels "io.x-k8s.kind.cluster"}}' "$CLUSTER_NAME-control-plane") == "$CLUSTER_NAME" ]] || return 1
  timeout 180s docker exec "$CLUSTER_NAME-control-plane" "$@"
}
kata_prepare() {
  python3 -m unittest discover -s "$KATA_HERE" -p test_harness.py
  [[ "$KATA_VMM" == fc || "$KATA_VMM" == clh ]] || return 1
  [[ ! -e "$STATE/kata-owned" ]] || { echo 'Refusing existing Kata state: use down first'; return 1; }
  [[ $(uname -m) == x86_64 && $(uname -r) != 6.18.62 ]] || { echo 'Need x64 host and distinguishable guest kernel'; return 1; }
  [[ ${KEEP_CLUSTER:-0} != 1 ]] || { echo 'Kata lanes currently require owned cleanup, not KEEP_CLUSTER'; return 1; }
  { uname -a; cat /proc/cpuinfo; ls -l /dev/kvm; } >"$ARTIFACTS/host-cpu-kvm.txt" 2>&1
  (( $(awk '/MemAvailable:/ {print $2}' /proc/meminfo) >= 6 * 1024 * 1024 )) || { echo 'Kata needs 6 GiB available RAM; preserve shared services'; return 1; }
  (( $(df -Pk "$ROOT" | awk 'NR==2 {print $4}') >= 20 * 1024 * 1024 )) || { echo 'Kata needs 20 GiB free disk'; return 1; }
  KATA_VOLUME="$CLUSTER_NAME-kata"
  KATA_TOOLS="cubepod-kata-e2e-tools:$CLUSTER_NAME"
  ! timeout 30s docker volume inspect "$KATA_VOLUME" >/dev/null 2>&1 || { echo 'Refusing existing Kata volume'; return 1; }
  ! timeout 30s docker image inspect "$KATA_TOOLS" >/dev/null 2>&1 || { echo 'Refusing existing tools tag'; return 1; }
  printf '%s\n' "$CLUSTER_NAME" >"$STATE/kata-owned"
  printf '%s\n' "$KATA_VMM" >"$STATE/kata-profile"
  KATA_ACTIVE=1
  local cache=${KATA_DOWNLOAD_CACHE:-$STATE/downloads}
  mkdir -p "$cache"
  local bundle="$cache/kata-go-static-$KATA_VERSION-amd64.tar.zst"
  if [[ ! -f "$bundle" ]]; then
    timeout 620s curl -fsSL --retry 2 --max-time 600 "https://github.com/kata-containers/kata-containers/releases/download/$KATA_VERSION/kata-go-static-$KATA_VERSION-amd64.tar.zst" -o "$bundle"
  fi
  echo "$KATA_SHA256  $bundle" | sha256sum -c -
  timeout 300s docker build -f "$KATA_HERE/Tools.Dockerfile" -t "$KATA_TOOLS" "$KATA_HERE"
  timeout 30s docker volume create --label "io.cubepod.e2e=$CLUSTER_NAME" "$KATA_VOLUME" >/dev/null
  timeout 180s docker run --rm -i --name "$CLUSTER_NAME-extract" --label "io.cubepod.e2e=$CLUSTER_NAME" -v "$KATA_VOLUME:/opt/kata" "$KATA_TOOLS" tar --zstd -xf - -C / <"$bundle"
  timeout 90s docker run --rm -i --name "$CLUSTER_NAME-kvm" --label "io.cubepod.e2e=$CLUSTER_NAME" --memory=128m --cpus=1 --network none --device /dev/kvm "$KATA_TOOLS" python3 - <"$KATA_HERE/kvm_preflight.py" >"$ARTIFACTS/kvm-preflight.log" 2>&1
  timeout 90s docker run --rm -i --name "$CLUSTER_NAME-guest" --label "io.cubepod.e2e=$CLUSTER_NAME" --memory=512m --cpus=1 --network none --device /dev/kvm -v "$KATA_VOLUME:/opt/kata:ro" "$KATA_TOOLS" python3 - "$KATA_VMM" <"$KATA_HERE/guest_preflight.py" >"$ARTIFACTS/host-guest.log" 2>&1
  KATA_MOUNT=$(timeout 30s docker volume inspect --format '{{.Mountpoint}}' "$KATA_VOLUME")
  [[ "$KATA_MOUNT" == /* && "$KATA_MOUNT" != *$'\n'* ]] || return 1
  if [[ "$KATA_VMM" == fc ]]; then
    local archive="$cache/containerd-$CONTAINERD_VERSION-linux-amd64.tar.gz"
    if [[ ! -f "$archive" ]]; then
      timeout 150s curl -fsSL --retry 2 --max-time 120 "https://github.com/containerd/containerd/releases/download/v$CONTAINERD_VERSION/containerd-$CONTAINERD_VERSION-linux-amd64.tar.gz" -o "$archive"
    fi
    echo "$CONTAINERD_SHA256  $archive" | sha256sum -c -
    mkdir -p "$STATE/containerd"
    tar -xzf "$archive" -C "$STATE/containerd" bin/containerd
  fi
}
kata_kind_config() {
  cat >>"$STATE/kind.yaml" <<YAML
  extraMounts:
  - hostPath: "$KATA_MOUNT"
    containerPath: /opt/kata
    readOnly: true
YAML
}
kata_configure() {
  kata_node bash -ec 'sed -i "s|http://deb.debian.org|https://deb.debian.org|g" /etc/apt/sources.list.d/debian.sources; apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends python3 dmsetup e2fsprogs thin-provisioning-tools' >"$ARTIFACTS/node-tools.log" 2>&1
  kata_node test -r /dev/kvm
  kata_node test -w /dev/kvm
  if [[ "$KATA_VMM" == fc ]]; then
    # kind intentionally compiles its containerd without devmapper. Replace ONLY
    # this owned node's daemon with the same-version official full build.
    kata_node mkdir -p /usr/local/libexec/cubepod-kata-e2e
    timeout 30s docker cp "$KATA_HERE/dmsetup_node.py" "$CLUSTER_NAME-control-plane:/usr/local/libexec/cubepod-kata-e2e/dmsetup_node.py"
    timeout 30s docker cp "$STATE/containerd/bin/containerd" "$CLUSTER_NAME-control-plane:/usr/local/bin/containerd-e2e"
    kata_node bash -ec 'mkdir -p /etc/systemd/system/containerd.service.d; printf "[Service]\nExecStart=\nExecStart=/usr/local/bin/containerd-e2e\nEnvironment=PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin\n" >/etc/systemd/system/containerd.service.d/e2e.conf; systemctl daemon-reload'
  fi
  timeout 180s docker exec -i "$CLUSTER_NAME-control-plane" python3 - "$KATA_VMM" "$CLUSTER_NAME" <"$KATA_HERE/configure_node.py" >"$ARTIFACTS/configure-node.log" 2>&1
  kata_node systemctl restart containerd
  k wait --for=condition=Ready node --all --timeout=120s
  kata_node ctr plugins ls >"$ARTIFACTS/containerd-plugins.txt"
  if [[ "$KATA_VMM" == fc ]]; then
    awk '$1=="io.containerd.snapshotter.v1" && $2=="devmapper" && $4=="ok" {ok=1} END {exit !ok}' "$ARTIFACTS/containerd-plugins.txt"
  fi
  if [[ "$KATA_VMM" == fc ]]; then
    kata_node ctr -n k8s.io images export /tmp/cubepod-kata-images.tar "docker.io/cubepod-kind/sandbox:$CLUSTER_NAME" "docker.io/cubepod-kind/podd:$CLUSTER_NAME" registry.k8s.io/pause:3.10
    kata_node ctr -n k8s.io images import --snapshotter devmapper /tmp/cubepod-kata-images.tar
    kata_node rm /tmp/cubepod-kata-images.tar
  fi
  cat <<YAML | k apply -f -
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: kata-$KATA_VMM-e2e}
handler: kata-$KATA_VMM-e2e
YAML
  # First prove this node's runtime actually boots a Linux guest, independently
  # of CubePod and CSI, then run the complete product suite below.
  cat <<YAML | k apply -f -
apiVersion: v1
kind: Pod
metadata: {name: kata-smoke, namespace: default}
spec:
  runtimeClassName: kata-$KATA_VMM-e2e
  automountServiceAccountToken: false
  restartPolicy: Never
  containers:
  - name: guest
    image: docker.io/cubepod-kind/sandbox:$CLUSTER_NAME
    imagePullPolicy: Never
    command: [bash, -ec, 'uname -r; echo KATA_GUEST_READY; sleep 180']
YAML
  k wait --for=condition=Ready pod/kata-smoke --timeout=180s
  # Pod Ready can precede asynchronous shim/CRI stdout delivery. Wait for the
  # SAME strict positive proof, never rerun the guest or accept an empty log.
  local deadline=$((SECONDS + 30)) guest_proven=0
  while (( SECONDS < deadline )); do
    if timeout 3s kubectl --context "kind-$CLUSTER_NAME" --request-timeout=2s logs kata-smoke >"$ARTIFACTS/kata-smoke.log" &&
       python3 "$KATA_HERE/verify_guest_log.py" "$ARTIFACTS/kata-smoke.log"; then guest_proven=1; break; fi
    sleep 0.2
  done
  (( guest_proven == 1 )) || { echo 'Timed out waiting for actual Kata guest kernel and output'; return 1; }
  kata_node python3 -c 'import os,sys,json; target={"fc":"firecracker","clh":"cloud-hypervisor"}[sys.argv[1]]; found=[]
for p in os.listdir("/proc"):
 if not p.isdigit(): continue
 try:
  if os.path.basename(os.readlink("/proc/"+p+"/exe"))==target:found.append({"pid":p,"argv":open("/proc/"+p+"/cmdline","rb").read().decode().replace("\0"," ")})
 except (FileNotFoundError,PermissionError,ProcessLookupError):pass
print(json.dumps(found,indent=2));assert found,"No actual selected VMM process"' "$KATA_VMM" >"$ARTIFACTS/actual-vmm.json"
  k get pod kata-smoke -o yaml >"$ARTIFACTS/kata-smoke.yaml"
  k delete pod kata-smoke --wait=true --timeout=60s
  sed "s/CLUSTER_PLACEHOLDER/$CLUSTER_NAME/g" "$KATA_HERE/csi.yaml" | k apply -f -
  k rollout status statefulset/hostpath -n cubepod-csi --timeout=240s
}
kata_watch_sdks() {
  export E2E_GUEST_KERNEL=6.18.62
  timeout 1300s kubectl --context "kind-$CLUSTER_NAME" --request-timeout=0 get pods -n "$NS" -l pod.cubeplex.ai/box --watch --output-watch-events -o json >"$ARTIFACTS/sdk-pods-watch.json" 2>"$ARTIFACTS/sdk-pods-watch.stderr" &
  KATA_WATCH_PID=$!
}
kata_stop_watch() {
  if [[ -n "$KATA_WATCH_PID" ]]; then kill "$KATA_WATCH_PID" 2>/dev/null || true; wait "$KATA_WATCH_PID" 2>/dev/null || true; KATA_WATCH_PID=""; fi
}
kata_verify_sdks() {
  kata_stop_watch
  python3 "$KATA_HERE/verify_pods.py" "$ARTIFACTS/sdk-pods-watch.json" "kata-$KATA_VMM-e2e"
}
kata_collect() {
  kata_stop_watch
  k get runtimeclass,pv,storageclass -o yaml >"$ARTIFACTS/kata-storage.yaml" 2>&1 || true
  k logs -n cubepod-csi statefulset/hostpath --all-containers=true --tail=1000 >"$ARTIFACTS/csi.log" 2>&1 || true
  kata_node journalctl -u containerd --no-pager -n 3000 >"$ARTIFACTS/containerd-kata.log" 2>&1 || true
  kata_node bash -ec 'cat /etc/containerd/config.toml; cat /etc/kata-containers/configuration-e2e.toml; ls -l /dev/kvm; cat /proc/cpuinfo' >"$ARTIFACTS/runtime-config-cpu.txt" 2>&1 || true
  if [[ "$KATA_VMM" == fc ]]; then kata_node cat "/var/lib/cubepod-kata-e2e/$CLUSTER_NAME/device-publication.log" >"$ARTIFACTS/device-publication.log" 2>&1 || true; fi
  kata_node bash -ec 'ctr -n k8s.io tasks list; findmnt -rn -o TARGET,SOURCE,FSTYPE; losetup --list' >"$ARTIFACTS/tasks-mounts-loops.txt" 2>&1 || true
  kata_node bash -ec 'ls -R /run/vc /var/log/kata-containers 2>/dev/null || true' >"$ARTIFACTS/guest-log-paths.txt" 2>&1 || true
}
kata_teardown_node() {
  [[ -f "$STATE/kata-owned" ]] || return 0
  # Requires live owned node: never lose the namespace holding owned loop files.
  kata_node true || return 1
  if kata_node test ! -d "/var/lib/cubepod-kata-e2e/$CLUSTER_NAME"; then return 0; fi
  timeout 180s docker exec -i "$CLUSTER_NAME-control-plane" python3 - "$CLUSTER_NAME" <"$KATA_HERE/cleanup_node.py" >>"$ARTIFACTS/kata-node-cleanup.log" 2>&1
}
kata_cleanup() {
  kata_stop_watch
  [[ "$KATA_ACTIVE" == 1 ]] || return 0
  [[ -f "$STATE/kata-owned" && $(<"$STATE/kata-owned") == "$CLUSTER_NAME" ]] || return 0
  # Never release artifacts from underneath a retained node.
  if timeout 30s docker inspect "$CLUSTER_NAME-control-plane" >/dev/null 2>&1; then
    echo 'Owned node retained: keeping Kata volume for safe cleanup'; return 1
  fi
  timeout 20s docker info >/dev/null || return 1
  local id
  while read -r id; do [[ -z "$id" ]] || timeout 30s docker rm -f "$id" || return 1; done < <(docker ps -aq --filter "label=io.cubepod.e2e=$CLUSTER_NAME")
  local volume="$CLUSTER_NAME-kata"
  if [[ $(docker volume inspect --format '{{index .Labels "io.cubepod.e2e"}}' "$volume" 2>/dev/null) == "$CLUSTER_NAME" ]]; then timeout 30s docker volume rm "$volume" || return 1; fi
  if docker image inspect "cubepod-kata-e2e-tools:$CLUSTER_NAME" >/dev/null 2>&1; then timeout 30s docker image rm "cubepod-kata-e2e-tools:$CLUSTER_NAME" || return 1; fi
  [[ -z $(docker ps -aq --filter "label=io.cubepod.e2e=$CLUSTER_NAME") ]] || return 1
  ! docker volume inspect "$volume" >/dev/null 2>&1 || return 1
  rm -f "$STATE/kata-owned" "$STATE/kata-profile"
}
