---
title: Reproducible kind E2E
description: Run the real CubePod Helm deployment and official E2B Python and TypeScript SDKs in an isolated local Kubernetes cluster.
---

Added after v0.6.0 (unreleased).

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

Added after v0.6.0. The runc baseline stays non-KVM. Two additional independently
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
must show the requested runtime for all created sandboxes; no runc fallback or
skipped tests count as success. Matrix `fail-fast` is disabled.

FC also needs a devmapper snapshotter/thin-pool. The harness uses only its newly
owned loop files and checked pool UUID; it never formats an existing host disk.
The local scripts do not load host modules or tune shared sysctls. Configuration
and containerd restarts occur only inside the new kind node. Before deletion,
backing-file inode checks protect unrelated loops; unsafe cleanup failure retains
the owned node for recovery rather than discarding device ownership state.

A native guest or smoke-Pod pass is **not** full SDK E2E. In particular FC has no
virtiofs, while CubePod's current E2B shared content store uses filesystem staging.
If unsupported, its real volume test must fail visibly pending a genuine block
solution; it is never skipped or replaced with a mock. `kata-qemu` is another
formal runtime, planned for its own VMM/transport/full-SDK lane and not implicitly
covered here. Runtime and guest diagnostics join the ordinary kind test artifacts.
