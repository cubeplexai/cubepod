# Real kind E2E baseline

Added after v0.6.0 (unreleased). This is independent of the process-backed
`test/e2b-compat` harness: the real Helm gateway/controller create real Kubernetes
sandbox Pods, inject the locally built Rust daemons, and mount real PVCs. Both
clients use the official `e2b==2.64.0` SDK, not CubePod's own SDK or mocks.

## Requirements

- Linux amd64 or arm64, Docker Engine reachable by the current user. An explicit
  `DOCKER_HOST` is respected; the runner never changes Docker contexts/services.
- At least **3 GiB available RAM**, preferably 6 GiB and 15 GiB free disk. Builds
  default to two compiler jobs and finish before Kubernetes starts. Allow up to
  40 minutes for first downloads/builds. Docker must support privileged kind
  nodes. No KVM, Kata, OpenEBS, Longhorn, or pre-existing cluster is needed.
- Go 1.27.3, Rust/Cargo 1.100.0, Python 3.12+ with venv/pip, Node 24.22.1/npm,
  curl, tar, GNU timeout/coreutils. CI pins these toolchain versions. Cargo.lock,
  go.sum, package-lock.json and the fully pinned Python requirements lock inputs.
- kind v0.32.0, kubectl v1.35.5, Helm v3.19.0. Optional checksum-verified installer:

  ```bash
  bash test/kind-e2e/install-tools.sh
  export PATH="$PWD/.kind-e2e/tools:$PATH"
  ```

Kube-proxy uses native nftables (Linux kernel nftables support is required),
avoiding legacy iptables statistic-extension dependencies.

Kubernetes is pinned by digest in `versions.env`; the Ubuntu base is also pinned
by digest. Ubuntu package security updates are deliberately not frozen to an
archive snapshot, so generated image layers are not promised bit-for-bit stable.
Network access is required for tool/package/image downloads on the first run.

## Single-command run

```bash
make test-kind-e2e
```

The command builds images **from this checkout**, creates a uniquely named
single-node cluster, loads images, installs the Helm chart with live Pod routing,
registers the prebuilt test template, waits with deadlines, runs both SDK suites,
exports diagnostics, and deletes its cluster on success **or failure**. A skip,
missing report, fewer than five scenarios per language, or failed assertion fails
the check. Python failure does not suppress execution of TypeScript.

Each invocation uses a private `.kind-e2e/<cluster>/kubeconfig`, unique image tags,
a single test namespace `cubepod-kind` **inside its own cluster**, and an OS-assigned
loopback port. The default kubeconfig/current context and existing clusters are
never read or rewritten. An existing cluster name is refused, not adopted.
State, credentials, dependencies and reports are gitignored.

To keep a local development environment:

```bash
CLUSTER_NAME=cubepod-my-e2e KEEP_CLUSTER=1 make test-kind-e2e
export KUBECONFIG="$PWD/.kind-e2e/cubepod-my-e2e/kubeconfig"
kubectl --context kind-cubepod-my-e2e -n cubepod-kind port-forward service/cubepod-gateway 18883:8080 --address 127.0.0.1
# In another shell, rerun both suites (KEEP_CLUSTER is required again):
CLUSTER_NAME=cubepod-my-e2e KEEP_CLUSTER=1 bash test/kind-e2e/run.sh test
# Delete only this runner-owned environment:
CLUSTER_NAME=cubepod-my-e2e bash test/kind-e2e/run.sh down
```

Automatic test port-forward processes stop on exit. `KEEP_CLUSTER=1` retains the
control plane, test template, storage fixture and network target for reuse.
Temporary sandbox PodBoxes/Pods/PVCs are removed after diagnostics even on failure.
The runner-owned kubeconfig contains local cluster credentials: **do not upload
or commit it**. `ARTIFACTS=/absolute/path` can select a diagnostics directory.
Native binaries, compiler caches, dependency installs and Docker image/build
caches remain for faster subsequent builds; `down` removes the cluster, not
shared Docker images, volumes or caches. Remove only the relevant ignored state
directory after `down` if no longer needed. Never use Docker system prune.

## Coverage and boundaries

Each SDK runs five real scenarios (ten total), based on the existing official SDK
compatibility suites but with no direct-gateway fallback:

| Scenario | Assertions |
| --- | --- |
| Lifecycle | create, connect, sandbox hostname identity, stdout/stderr, exit 42, info, kill |
| Files | mkdir, write/read/list, watch sees a real write |
| Processes | background PID and kill, PTY create/resize/input/output/exit |
| Network | sandbox HTTP request to another Pod over cluster DNS; HTTP server in sandbox reached through gateway using SDK `getHost` |
| Volumes | official Volume creation, mount `/data`, read/write from the sandbox, destruction/recreation of sandbox with the same volume preserves content; Volume deletion |

The default kind `standard` local-path provisioner backs rootfs/workspace
Filesystem PVCs. The shared E2B volume fixture is a **single-node hostPath PV**
inside the disposable kind node, exposed as a shared filesystem claim to gateway
and sandboxes. It needs no host storage installation. This tests mount/persistence
wiring, **not production multi-node RWX CSI semantics**, thin block snapshots,
cloning or storage durability across kind-cluster deletion.

The baseline explicitly disables the zero-trust egress sidecar/transparent
interception and desktop stack. It validates outbound connectivity to an in-cluster
HTTP target (outside the sandbox), **not public Internet connectivity**, firewall
policy, mTLS/secret substitution or HTTP/2 MITM. Kata/Firecracker/KVM, rootfs
pivot/block mode, desktop, code-interpreter notebooks, pause/resume, template
builds, warm pools, metrics, async SDKs, webhooks and production isolation/security
are outside this baseline. Passing this suite does not validate those features.
There are no silent skips for any of the ten promised scenarios.

## CI, diagnostics and troubleshooting

`.github/workflows/kind-e2e.yml` runs the same command for PRs into main and
`workflow_dispatch`, with read-only repository permissions, a 40-minute job bound,
per-PR cancellation and unique cluster/artifact names. Failure diagnostics and
reports are uploaded even when the suite fails; no secret kubeconfig is uploaded.

Artifacts include Python/TypeScript JUnit XML, Python test output, Node stderr,
runner output (CI), gateway port-forward output, Pod/PVC/PodBox/PodTemplate status,
events, Pod/PVC descriptions, current/previous component and sandbox logs, and
kind node logs. Collection itself is bounded. On abrupt runner loss, Actions
cannot guarantee artifacts; normal failure and cancellation run cleanup.

- Prerequisite/memory failure: fix the prerequisite or use a larger dedicated
  worker; do not restart or delete other bots' services.
- Pending PVC: inspect `events.txt` and `describe-pvcs.txt`; `standard` should be
  provided by kind and the E2B shared claim bound to `cubepod-kind-volumes`.
- Sandbox startup/SDK errors: inspect `python.log`, `typescript.xml`, component
  logs and `describe-pods.txt`. Do not switch live routing off or use mocks to
  make these tests pass.
- Product regressions stay red. Record the exact failing scenario and logs;
  unsupported capabilities must be documented rather than silently skipped.

Harness fail-closed contract checks (not E2E coverage):

```bash
python3 -m unittest discover -s test/kind-e2e -p test_harness.py
```

### Container-mode volume regression fixed with this baseline

The first real run exposed `podd-init` attempting an in-container bind mount in
no-pivot mode, which correctly lacks `CAP_SYS_ADMIN`. The controller now asks
kubelet to mount the same PVC/subPath at its final path as well as its staging
path. Only in no-pivot mode, the initializer verifies identical device/inode and
initializes ownership without issuing `mount(2)`. No extra privileges are granted.
Kata/pivot-mode bind mounts and their nosuid/nodev remount path are unchanged.
The container baseline inherits Kubernetes/runtime volume-mount security semantics;
it does not claim the pivot-mode remount hardening or VM isolation.
