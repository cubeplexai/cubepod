# Real Compose E2E

Added after v0.6.0. This is a **deployment-mode** check, not a Kubernetes
RuntimeClass or a VMM. It complements the kind runc baseline and the separately
planned Kata Firecracker / Cloud Hypervisor checks. The repository also ships
`kata-qemu`; it is not implicitly covered by either FC or CLH.

```sh
make test-compose-e2e
```

Requirements: Linux x86_64, Docker Engine, **Compose v2.39.4**, Go 1.27.3,
Rust 1.100.0, Python 3.12.16 with venv, Node 24.22.1/npm, GNU timeout/flock;
at least 1.5 GiB *available* RAM (4 GiB total recommended) and 10 GiB free disk.
Use `COMPOSE_BIN=/absolute/path/docker-compose` for the standalone pinned binary.
`GOMAXPROCS=1 CARGO_BUILD_JOBS=1` reduces compiler peaks on shared machines.
The workflow installs checksum-verified Compose without changing shared Docker
configuration. It uses independent concurrency and does not cancel kind checks.

The runner overlays `deploy/docker-compose/docker-compose.yaml`, preserving its
real gateway/controller/egress topology. It supplies the actual Docker backend
selector (`CUBEPOD_RUNTIME=docker`), disables Kubernetes Pod-IP routing, supplies
daemon-visible bind paths, and builds **this checkout's** binaries and images.
The old `CUBEPOD_RUNTIME_MODE=docker-loopback` setting alone is not a backend
selector in the current gateway. A healthy HTTP endpoint is not accepted as E2E.

The **exact same**, unmodified `test/kind-e2e/test_sdk.py` and `sdk.test.mjs`
execute through the shared `run-sdks.sh`: real create/connect/exit/destroy,
files/mkdir/list/watch, background commands/PTY, outbound Docker-network HTTP,
exposed sandbox port, and mounted E2B-volume persistence across sandbox deletion.
The network fixture has the same logical DNS alias as the kind fixture.
Both SDKs run even if the first fails. Missing/empty/skipped reports fail.

Each invocation allocates its own project, ephemeral loopback gateway port,
network, runtime-bin/data/state volumes, sandbox prefix and E2B-volume prefix.
Existing projects/resources are refused. Runtime binaries are **injected**, not
baked into user images; workspace and RootFS remain independently bound.
Filesystem-backed unprivileged Docker containers use podd-init's no-pivot mode,
without `--privileged`, CAP_SYS_ADMIN, or disabling injection/storage. This is
not a raw-block/OverlayFS/Kata or zero-trust-egress test.

Reports, resolved Compose config, component logs and surviving failed-sandbox
inspect/logs are saved under `.compose-e2e/PROJECT/artifacts` (or `ARTIFACTS`).
Setup/build/readiness/tests/cleanup have bounded timeouts. The trap removes only
this invocation's resources and image tags; it never prunes Docker or stops
other stacks. A failed cleanup remains a failure. Retry owned cleanup explicitly:

```sh
COMPOSE_PROJECT_NAME=cubepod-compose-e2e-EXACT-PROJECT bash test/compose-e2e/run.sh down
```

The isolated kind environment from the baseline is not changed or removed.
