---
title: Reproducible kind E2E
description: Run the real CubePod Helm deployment and official E2B Python and TypeScript SDKs in an isolated local Kubernetes cluster.
---

Added in v0.7.0.

From a source checkout on Linux with Docker, Go, Rust, Python and Node installed:

```bash
bash test/kind-e2e/install-tools.sh
export PATH="$PWD/.kind-e2e/tools:$PATH"
make test-kind-e2e
```

Allow at least 3 GiB available memory (6 GiB recommended) and 15 GiB free disk.
The runner builds the checkout, creates a dedicated single-node kind cluster,
loads images, installs CubePod with Helm and live Pod routing, runs both official
E2B SDK suites, collects diagnostics and cleans up. It never switches the default
kubecontext or changes existing clusters/services. No KVM is required.

The ten strict scenarios cover lifecycle/connect/commands, files and watch,
background commands/PTYs, outbound cluster connectivity and exposed ports, and
volume mounts persisting across sandbox recreation. The shared volume fixture is
node-local storage inside kind, not a production multi-node RWX CSI validation.
The no-pivot volume path uses kubelet-provided mounts without granting sandbox
`CAP_SYS_ADMIN`; pivot-mode bind/remount behavior is unchanged.
Kata/Firecracker, block-mode rootfs pivot, desktop and zero-trust egress are not
validated. No skips are treated as passing coverage.

For pinned prerequisites, reusable development clusters, exact coverage boundaries
and failure-artifact troubleshooting, see the
[runner documentation](https://github.com/cubeplexai/cubepod/blob/main/test/kind-e2e/README.md).
The `Kind E2E` GitHub Actions workflow runs the same command on PRs and supports
manual dispatch, with bounded execution and downloadable reports and diagnostics.

## Independent Kata Firecracker and Cloud Hypervisor lanes

Added in v0.7.0. The runc baseline stays non-KVM. Two additional independently
reported checks run the same complete official dual-SDK scenarios with real Kata:

```sh
make test-kind-fc-e2e
make test-kind-clh-e2e
```

Allow at least 6 GiB **available** RAM and 20 GiB free disk on an x86_64 Linux KVM
host. The lanes pin Kata 4.4.0, bundled FC 1.17.0 / CLH 54.1 and Linux 6.18.62.
They first execute KVM VM/vCPU checks and boot an actual Linux guest with each
VMM, then boot a real Kata Pod before deploying CubePod with block rootfs and
workspace CSI volumes. Both SDKs assert the guest kernel, and Kubernetes evidence
must show the requested runtime for all created sandboxes; no runc fallback
counts as success. Cloud Hypervisor rejects every skip. On Firecracker only,
skips that cite [issue #120](https://github.com/cubeplexai/cubepod/issues/120) or
[issue #122](https://github.com/cubeplexai/cubepod/issues/122) are allowed until
those issues are fixed; any other skip or failure still fails the check. Matrix
`fail-fast` is disabled.

FC also needs a devmapper snapshotter/thin-pool. The harness uses only its newly
owned loop files and checked pool UUID; it never formats an existing host disk.
The local scripts do not load host modules or tune shared sysctls. Configuration
and containerd restarts occur only inside the new kind node. Before deletion,
backing-file inode checks protect unrelated loops; unsafe cleanup failure retains
the owned node for recovery rather than discarding device ownership state.

A native guest or smoke-Pod pass is **not** full SDK E2E. In particular FC has no
virtiofs, while CubePod's current E2B shared content store uses filesystem staging.
The official volume tests are temporarily skipped on Firecracker only, citing
[issue #122](https://github.com/cubeplexai/cubepod/issues/122), and must be
re-enabled when that issue is fixed; they are not replaced with a mock.
Sandbox-create failures caused by [issue #120](https://github.com/cubeplexai/cubepod/issues/120)
are handled the same way. `kata-qemu` is another
formal runtime, planned for its own VMM/transport/full-SDK lane and not implicitly
covered here. Runtime and guest diagnostics join the ordinary kind test artifacts.

### Experimental pinned Firecracker guest kernel

Added in v0.8.0. The optional `KATA_FC_KERNEL_LOCK` input selects a
custom kernel **only in the isolated Firecracker lane's private Kata config**:

```bash
KATA_VMM=fc KATA_FC_KERNEL_LOCK=/absolute/path/to/pin.json \
  CLUSTER_NAME=cubepod-private-fc bash test/kind-e2e/run.sh
```

The **Experimental Firecracker guest kernel** workflow builds Kata 4.4.0's
Linux 6.18.62 configuration with built-in NFSv3 and requires a real KVM guest
preflight. Publication is opt-in. Its evidence artifact supplies `pin.json` with
an immutable GHCR digest, exact kernel release and binary/config checksums;
mutable tags and checksum mismatches are rejected. Private packages require an
authorized registry login. The helper at
`deploy/k8s/kata/guest-kernel/fetch.sh` only fetches verified files; it never runs
the artifact image or modifies host Kata/containerd services.

Changed in v0.8.0: the CI **Kata Firecracker** lane uses the committed
`deploy/k8s/kata/guest-kernel/fc-nfs1.pin.json` (release `6.18.62-cubepod-nfs1`,
published by workflow run 45314457814 and pinned by OCI digest). Only that lane's
private `configuration-e2e.toml` selects it; the Cloud Hypervisor lane and default
deployments keep Kata's stock kernel.

Changed in v0.8.0: `FirecrackerVolumeNFS` is on by default and every
kind lane deploys a checkout-built `cubepod-volume-nfs`. The CEP-0031 gate (helper-free
`mount(2)`, no `nfs-common`/`mount.nfs`, NFS source IP equal to the sandbox Pod IP,
data surviving destroy/recreate, with both official SDKs) passed on this kernel in
draft PR #157. On Kata Firecracker the official SDK volume persistence tests stay
skipped for [issue #120](https://github.com/cubeplexai/cubepod/issues/120) (Kata 4.4.0
virtio-mmio hotplug `/dev/vdb` EIO race, waiting on upstream). The opt-in gate re-enables
them fail-closed and runs `test/kata-e2e/verify_cep0031_gate.py`:

```bash
KATA_VMM=fc CEP0031_FC_NFS_GATE=1 KATA_FC_KERNEL_LOCK=/absolute/path/to/pin.json \
  CLUSTER_NAME=cubepod-private-fc bash test/kind-e2e/run.sh
```

In CI it is off by default; dispatch **Kata E2E** with `cep0031_gate=true`. See
`test/guest-kernel/README.md` in the source tree for the full version contract,
checksums, build commands and limitations.
