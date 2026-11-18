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
