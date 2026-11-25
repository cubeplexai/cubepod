# CEP-0032 Statechart Verification Report (`PodBoxTieredSleep`)

## 1. Summary

- **Spec**: [`PodBoxTieredSleep.tla`](./PodBoxTieredSleep.tla) ([`PodBoxTieredSleep.cfg`](./PodBoxTieredSleep.cfg))
- **View**: [`PodBoxTieredSleep.view.ts`](./PodBoxTieredSleep.view.ts)
- **Counterexample Variant**: [`PodBoxTieredSleepNoEpoch.tla`](./PodBoxTieredSleepNoEpoch.tla) ([`PodBoxTieredSleepNoEpoch.cfg`](./PodBoxTieredSleepNoEpoch.cfg))
- **TLC Result**: Exhaustive BFS reached **20 distinct states** and **83 steps** (`12` projected chart states, `29` arrows).
- **Rules That Hold**: `TypeOK`, `TerminatedStaysTerminated`, `PodPresenceConsistent`, `FreezeConsistent`, `LeaseOnlyWhenRunning`, `PVCsRetainedDuringLifecycle`, `NoDeadlock` (**0 violations**).

## 2. Statechart Diagram

```mermaid
---
title: "PodBox Two-Tier Sleep Lifecycle (CEP-0032)"
---
stateDiagram-v2
  state "Legend" as legend {
    lg0: Init
    lg1: Active
    lg2: Transition
    lg3: LightSleep
    lg4: DeepSleep
    lg5: Terminal
  }
  class lg0 tan
  class lg1 teal
  class lg2 blue
  class lg3 purple
  class lg4 orange
  class lg5 pink
  s0: No Sandbox
  s1: Provisioning Pod
  s2: Running
  s3: L2 Deep Sleep (Pod Evicted)
  s4: Failed
  s5: L1 Freezing
  s6: L2 Scaling to Zero
  s7: Running (Lease Held)
  s8: L2 Deep Sleep
  s9: L1 Light Sleep
  s10: L1 Waking
  s11: Terminated
  [*] --> s0
  s0 --> s1: create pod and PVCs
  s0 --> s2: claim warm pod
  s1 --> s2: podd /health ready
  s1 --> s4: probe/boot fails
  s2 --> s3: pod evicted
  s2 --> s4: probe/boot fails
  s2 --> s5: idle timeout [no lease] or explicit light pause
  s2 --> s6: explicit deep pause
  s2 --> s7: acquire lease
  s3 --> s1: reconciler recreates pod or connect (L2 reboot)
  s4 --> s0: delete sandbox and PVCs
  s4 --> s1: reset rootfs
  s5 --> s6: freeze fails -> L2
  s5 --> s9: cgroup frozen
  s5 --> s10: concurrent connect
  s6 --> s8: pod deleted (PVCs kept)
  s7 --> s2: lease expires
  s7 --> s3: pod evicted
  s7 --> s4: probe/boot fails
  s7 --> s5: explicit light pause
  s7 --> s6: explicit deep pause
  s8 --> s1: connect (L2 reboot)
  s8 --> s11: L2 paused TTL expires
  s9 --> s8: pod evicted
  s9 --> s6: explicit deep pause or L1 TTL expires
  s9 --> s10: connect (L1 wake)
  s10 --> s1: thaw fails -> reboot
  s10 --> s2: cgroup thawed (<500ms)
  s11 --> s0: delete sandbox and PVCs
  class s0 tan
  class s1 blue
  class s2 teal
  class s3 orange
  class s4 pink
  class s5 blue
  class s6 blue
  class s7 teal
  class s8 orange
  class s9 purple
  class s10 blue
  class s11 pink
```

## 3. What the Model Includes & What the View Hides

- **Modeled in TLA+**:
  - Cold creation (`Provisioning -> Running`) and WarmPool claim (`Running`).
  - L1 Light Sleep (`Running -> PausingL1 -> LightPaused -> ResumingL1 -> Running`), including `ReapIdle` idle timeout vs. active `Lease` lock, freeze failure fallback to L2, thaw failure fallback to Pod reboot, and concurrent `connect` preempting `PausingL1`.
  - Async out-of-order `freeze` RPC fencing (`freezeInFlight` + epoch check).
  - L2 Deep Sleep (`PausingL2 -> DeepPaused`), triggered explicitly, via `LightSleepTTLSeconds` expiration (`ReapLightToDeep`), or via unexpected Pod eviction (`PodEvictedDuringCompute`).
  - Stale Gateway replica cache (`gwStaleLight`) across background L1->L2 demotion and `DeepPaused -> Terminated` TTL expiry (`StaleConnectRejectedOnTerminated`).
  - RootFS reset (`ResetRootFS`), `Failed`, `Terminated`, and `DeleteBox`.
- **Omitted from Model**: Wall-clock timestamp arithmetic (`IdleTimeoutSeconds`, `LightSleepTTLSeconds`, `PausedTTLSeconds` are abstracted as non-deterministic guard-checked timer events), exact integer `SleepEpoch` counter (abstracted as `freezeInFlight` epoch-fencing boolean), and `podd-agent` DaemonSet binary injection (orthogonal to PodBox lifecycle).
- **Hidden in View (`PodBoxTieredSleep.view.ts`)**: `DeleteBox` from non-terminal states (kept on `Failed` and `Terminated`), `ResetRootFS` from healthy states (kept on `Failed`), `BeginPauseDeep` from `Provisioning`, and `PodEvictedDuringCompute` from `PausingL1`/`ResumingL1` (`29` hidden steps total, `5` self-loop steps).

## 4. Counterexample Hazards Found During Verification

1. **`Provisioning -> PausingL2` Pod State Mismatch** (caught in initial run `out2`):
   - Path: `0. init` -> `1. CreateCold (Provisioning, podState="starting")` -> `2. BeginPauseDeep (PausingL2, podState="starting")`.
   - Fix: `PausingL2` may delete either a `"starting"` or `"ready"` Pod, and `ReconcileProvisioning` must verify `desiredState == Running` before promoting to `Running`.
2. **Out-of-Order Freeze/Unfreeze Race Without Epoch Fencing** (`PodBoxTieredSleepNoEpoch.tla`, `out-bug2`):
   - Rule broken: `FreezeConsistent` (`(phase = "Running") => ~workloadFrozen`).
   - Reader's rule: A `Running` sandbox must never have its guest workload cgroup frozen.
   - Shortest 5-step path:
     - `0`: `phase="none", workloadFrozen=FALSE`
     - `1. CreateWarm`: `phase="Running", workloadFrozen=FALSE`
     - `2. BeginIdleLight`: `phase="PausingL1", freezeInFlight=TRUE`
     - `3. PreemptPauseLightWithConnect`: `phase="ResumingL1", desiredState="Running", freezeInFlight=TRUE`
     - `4. CompleteWakeLight`: `phase="Running", workloadFrozen=FALSE, freezeInFlight=TRUE`
     - `5. LateFreezeWithoutEpoch`: `phase="Running", workloadFrozen=TRUE` (**VIOLATION**)
   - Fix applied to CEP-0032: Monotonic `PodBoxStatus.SleepEpoch` fencing on `POST /v1/lifecycle/freeze` and `POST /v1/lifecycle/unfreeze`.

## 5. Reproduce

```sh
TLA2TOOLS=/home/chris/.cache/statechart/tla2tools.jar JAVA=/usr/bin/java \
  statechart docs/enhancements/0032-tiered-sleep-and-init-free-podd/statechart/PodBoxTieredSleep.tla \
  --view docs/enhancements/0032-tiered-sleep-and-init-free-podd/statechart/PodBoxTieredSleep.view.ts \
  --out /tmp/cep0032-statechart-out
```
