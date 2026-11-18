# Real kind + Kata FC / CLH E2E

Added after v0.6.0. These are independent, **fail-closed** VMM lanes, not aliases
for the non-KVM runc baseline or Compose deployment mode:

```sh
make test-kind-fc-e2e
make test-kind-clh-e2e
```

`.github/workflows/kata-e2e.yml` exposes separately named Firecracker and Cloud
Hypervisor checks, `fail-fast: false`, independent hosted Ubuntu x64 VMs, unique
cluster names and artifact bundles. One runtime failure does not cancel the
other, runc, or Compose. Both PR and manual-dispatch events are supported.
GitHub nested virtualization remains experimental, without a stability SLA.

## Requirements and version pins

Linux x86_64; readable/writable KVM from a Docker `--device /dev/kvm` container,
virtualization CPU extensions, TUN/TAP, at least **6 GiB available RAM and 20 GiB
free disk**, plus the pinned kind baseline toolchain. See `versions.env`:

- Kata **4.4.0 Go shim** and its SHA256-verified official static bundle;
- bundled FC **1.17.0**, CLH **54.1**, Linux guest **6.18.62** and guest image;
- FC only: SHA256-verified full containerd **2.3.1** inside the dedicated node
  (kind's same-version build omits devmapper), host thin-pool kernel support;
- block CSI hostpath **1.18.0**, registrar **2.18.0**, provisioner **6.4.0**.

The legacy Go shim is supported but deprecated upstream; it is pinned here
because FC is a Go-only Kata profile. This does not assert runtime-rs coverage.
The bundled root image is a partitioned EROFS disk: native probes boot **vda1**.
CLH requires explicit `image_type=raw`; FC's automatic root override is disabled
in the native probe. No host artifact installation or unchecked latest downloads.

## What must really execute

1. Open KVM, validate API 12, create VM/vCPU and execute a bounded x86 instruction
   probe. This alone is explicitly **not** a Linux guest success.
2. Boot the selected VMM with its actual pinned Linux kernel/root disk in a
   no-network, KVM-device-only disposable container. Execute guest shell `printf`
   and require both the kernel banner and the **assembled** output token; echoed
   input cannot satisfy it. Stop the owned VMM process on all outcomes.
3. Create a new kind node with the read-only Kata bundle, configure only its
   containerd runtime handler/shim, and boot a real Kata smoke Pod. Archive its
   guest kernel, exact RuntimeClass and actual selected VMM executable/argv.
4. Install a disposable single-node block CSI driver. FC additionally gets a
   uniquely named thin-pool backed only by newly allocated loop files, never an
   existing disk. Import its sandbox/podd/pause images into the real devmapper
   snapshotter, not overlayfs masquerading as FC.
5. Deploy real CubePod using Block rootfs/workspace PVCs and run the **same full
   Python and TypeScript SDK contract** as runc/Compose: lifecycle/commands,
   files/mkdir/list/watch, background/PTY, outbound HTTP/exposed port and mounted
   volume persistence. Each SDK checks the real guest kernel; a Pod watch must
   record all 12 sandbox Pods using the requested Kata class. Missing, skipped,
   partial or fallback execution fails, never `continue-on-error`.

The existing E2B shared-store fixture is a real filesystem volume, separate from
Block rootfs/workspace. **FC has no virtiofs.** If current CubePod's filesystem
E2B staging path cannot work with FC, the full volume test must remain a visible
failure/blocker until a genuine supported block-backed content-store path is
implemented. This harness does not pretend a guest probe or successful smoke
Pod proves the full product works, nor bypass/skip the failing volume assertion.

## Isolation, evidence and cleanup

Only newly owned `cubepod-*` clusters are accepted. Use independent KUBECONFIGs;
never modify default contexts, shared Docker/Kata/containerd/k3s/CSI configuration,
shared sysctls, physical disks, existing thin-pools or networks. Loading
`dm_thin_pool` occurs **only in the explicitly disposable FC Actions VM**; the
local script fails if host support is missing and never loads host modules.
Node package/configuration changes and daemon restarts occur inside the verified
owned kind node only. Avoid raising shared inotify limits to fit another cluster;
report an exhausted shared-host quota instead.

Before deleting a node, stop its workloads, unmount its mount namespace, check
the unique pool UUID and each thin device's parent, then detach loops only after
kernel backing-inode association to files under the owned directory is verified.
Never `losetup -D`, `dmsetup remove_all`, force-remove a pool or prune Docker.
If safe device cleanup fails, retain the owned node and markers for recovery
rather than losing the namespace/backing files. Retry with the same VMM/name:

```sh
KATA_VMM=fc CLUSTER_NAME=cubepod-EXACT-NAME bash test/kind-e2e/run.sh down
```

CPU/KVM checks, native guest console, node runtime config, actual VMM proof,
Pod watch, containerd/Kata journal, CSI logs, Pods/PVCs/events/describe and both
SDK JUnit reports live in `.kind-e2e/CLUSTER/artifacts/` or `ARTIFACTS`.
All builds/setup/readiness/probes/tests/cleanup are bounded. Test-created Kata
artifact volumes/tool-image tags are removed after the owning node is gone;
normal compiler/Docker caches may remain. `KATA_DOWNLOAD_CACHE` can explicitly
reuse a local artifact cache; every use rechecks its pinned SHA256.

## Other formal runtimes

The repository also ships **kata-qemu**. Neither FC nor CLH results imply QEMU
coverage. A follow-up QEMU lane must use its actual QEMU VMM/guest preflight,
required vhost-vsock transport, matching RuntimeClass and the same unskipped SDK
suite. It is deliberately not labeled covered by this FC/CLH-focused change.
Production RWX CSI, multi-node, zero-trust egress, desktop and performance SLAs
are outside this single-node integration harness.


## Current integration evidence and unresolved FC dependency

On 2026-11-18, hosted `ubuntu-24.04` booted native FC and CLH Linux guests.
CLH additionally passed all five official Python and all five official
TypeScript scenarios, with zero skips, actual VMM process proof and twelve
RuntimeClass-matching Pods (run `44248528618`). That run remained red due strict
CSI cleanup; a successful test report is not a successful cleanup check.

FC currently remains red before the SDK suite: containerd's devmapper plugin
creates thin mappings, but their `/dev/mapper/OWNED-pool-snap-N` device entries
are absent in kind's private `/dev` without udev (run `44249767997`). The owned
pool itself, CRI and explicit transfer unpack configuration are working; the
owned thin mappings/pool/loops were cleaned. Do not fix this by mounting the
host's entire `/dev` read-write, running broad host udev rules, skipping volumes
or accepting a native guest as full E2E. A node-scoped, ownership-checked device
publication mechanism (including every child snapshot, not just the pool) is
required before claiming FC Kata/SDK coverage. This PR now implements a
synchronous node-only `dmsetup create` wrapper, matching containerd 2.3.1's actual
CLI calls. It validates both requested and actual kernel thin parents, own pool
UUID and name, then publishes only that owned device before returning. It does
not start udev, mount host /dev or change a host service. Ten safety/contract unit
tests pass; **actual FC revalidation is still required**. The full lane deliberately
fails visibly until real integration succeeds. Native Linux guest success is not the missing
Kata/SDK proof.


### Test CSI reclaim policy (not production reclaim coverage)

The block fixture uses `Retain`: captured runs with the pinned example
hostpath driver show `DeleteVolume` losing its volume record before kubelet's
asynchronous `NodeUnpublishVolume`; that RPC then returns `NotFound` for every
published block bind. Waiting longer or stopping containers does not repair
that lost metadata. Evidence is retained in runs 44250515533 and 44251855668.

Retain is a real supported StorageClass profile, not a mocked volume or skipped
SDK assertion. All official mounted-volume persistence tests remain identical.
The harness must prove every CSI block bind is unpublished, then stop only owned
node processes, verify owned backing-inode associations and actual loop detach,
and delete only the owned node/artifact volume, removing backing files and PV
records. Uncertain cleanup remains red. **Automatic CSI `DeleteVolume` reclaim
is not covered by this profile**; the initial failure is documented rather than
claimed fixed in the external driver. See the driver's state/unpublish behavior
in https://github.com/kubernetes-csi/csi-driver-host-path/issues/217 and
https://github.com/kubernetes-csi/csi-driver-host-path/issues/457.
