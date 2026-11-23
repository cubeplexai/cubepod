# Experimental Kata Firecracker NFSv3 guest kernel

Added after v0.7.1 (unreleased). This is **kernel infrastructure**, not proof that
CEP-0031 passes on a Kata Firecracker pod. Production defaults and #120/#122 skips
are unchanged. No kernel has been promoted by this change.

## Contract

- Kata source and stock amd64 bundle: **4.4.0**; Linux source: **6.18.62**.
- Base: the actual bundle's `config-6.18.62-211` (SHA256
  `0127c096083a883f9701a44249cb343f1c33f137d05f6e6184d1b3b833f26de6`).
- Overlay: `nfs.config`; built-in `NFS_FS`, `NFS_V3`, `SUNRPC`; Kconfig selects
  `LOCKD` and other dependencies. Modules remain disabled. `nolock` does not mean
  `CONFIG_LOCKD=n`. No NFSv2, NFSv4, root-NFS or NFS server is requested.
- Release: **6.18.62-cubepod-nfs1**. A version change requires changing the build,
  archive checksums, consumer's supported release and guest assertions together.
- Source archives, stock config, compiler base image and yq are checksum-pinned.
  The Kata GCC 11/Ubuntu 22.04 recipe and package inventory are recorded. Apt
  package resolution is not snapshot-pinned: do **not** promise bit-identical
  binaries across different package repository states. Published binary SHA256
  and OCI digest are the deployment identity, not the version label alone.

## Build and real guest preflight

```bash
bash test/guest-kernel/build.sh "$PWD/kernel-artifacts"
tools=$(cat kernel-artifacts/tools-image.txt)
timeout 100s docker run --rm -i --network none --device /dev/kvm \
  --memory=512m --cpus=1 -v "$PWD/kernel-artifacts:/art:ro" \
  "$tools" python3 - /art < test/guest-kernel/preflight.py
```

Needs ordinary Docker, curl, tar, zstd, ~20 GiB disk and KVM. `build.sh` refuses an
existing output directory; `KERNEL_DOWNLOAD_CACHE` can reuse checksum-verified
archives. Compilation runs without network access in a container with no devices
or host mounts; docker cp also supports a daemon with a different filesystem.
The **preflight mount** above requires the usual same-host Docker bind semantics;
for a remote daemon, copy the artifacts and script into an owned container instead.
On a shared workstation use the explicitly allocated Docker endpoint and KVM
group session. Never load host modules or change shared Kata/containerd services.

The preflight runs a real Firecracker process, checks exact guest release and an
actual `nfs` filesystem entry. It does not prove MOUNT RPC, pod source-IP identity,
helper-free `podd-init` mount or SDK persistence. Those still require the complete
isolated CEP-0031 implementation gate.

## CI and publication

Manually run **Experimental Firecracker guest kernel** (`guest-kernel.yml`).
`publish=false` is the default. An explicit `publish=true` publishes only after
the real KVM preflight passes, using the workflow token with `packages:write`.
The candidate is named `ghcr.io/<repository-owner>/cubepod-fc-kernel` and tagged
with version, source commit and run/attempt. The artifact is marked experimental,
not volume-validated. No package visibility or repository settings are changed.
Org package permissions, Actions approval, and workflow availability on the
default branch may need maintainer action; do not bypass them or merge for this.

The evidence artifact contains `pin.json` with an **OCI sha256 digest**, exact
release, kernel SHA256 and config SHA256, together with source/config/guest logs.
No `latest` or mutable version tag is accepted by deployment. Download and review
the pin from the intended successful workflow run; retain it in deployment
configuration. An authorized GHCR login may be needed for a private package.

## Opt-in deployment and isolated e2e

```bash
# Copies files only, never runs the OCI image or changes a shared service.
bash deploy/k8s/kata/guest-kernel/fetch.sh /absolute/path/pin.json /new/private/kernel-directory
# Isolated FC e2e selects the exact artifact in its private configuration-e2e.toml.
KATA_VMM=fc KATA_FC_KERNEL_LOCK=/absolute/path/pin.json \
  CLUSTER_NAME=cubepod-my-private-fc bash test/kind-e2e/run.sh
```

The consumer rejects mutable references, unsupported releases, missing/tampered
kernel/config data, manifest mismatch and modular NFS. The e2e runner places
`vmlinux-cubepod-custom` in its owned Kata volume, never `/opt/kata` on the host;
smoke and SDK kernel checks use the pinned exact release. CLH/runc behavior is
unchanged. An FC lock supplied to CLH is rejected. No NFS gate is enabled here.
For an independently managed deployment, select the fetched `vmlinux` using
`[hypervisor.firecracker].kernel` in a dedicated Kata configuration; pin the same
rootfs/runtime compatibility version and roll new guests deliberately. Existing
VMs/snapshots must not silently switch kernels. Do not deploy this experimental
candidate as a working production volume solution before the full gate passes.
