---- MODULE PodBoxTieredSleep ----
EXTENDS Naturals

VARIABLES
    phase,          \* "none", "Provisioning", "Running", "PausingL1", "LightPaused", "ResumingL1", "PausingL2", "DeepPaused", "Failed", "Terminated"
    desiredState,   \* "none", "Running", "LightPaused", "DeepPaused"
    podState,       \* "absent", "starting", "ready"
    workloadFrozen, \* BOOLEAN: TRUE iff guest workload cgroup is frozen (cgroup.freeze=1)
    freezeInFlight, \* BOOLEAN: TRUE while an L1 freeze RPC for the current/prior epoch is in flight
    hasLease,       \* BOOLEAN: active lease lock preventing idle L1 pause
    pvcsIntact,     \* BOOLEAN: RootFS & Workspace PVCs exist
    gwStaleLight,   \* BOOLEAN: TRUE when a Gateway replica cache still sees LightPaused after background L1->L2/Terminated
    wasTerminated   \* BOOLEAN: ghost variable tracking irreversible Terminated state

vars == <<phase, desiredState, podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

Init ==
    /\ phase = "none"
    /\ desiredState = "none"
    /\ podState = "absent"
    /\ workloadFrozen = FALSE
    /\ freezeInFlight = FALSE
    /\ hasLease = FALSE
    /\ pvcsIntact = FALSE
    /\ gwStaleLight = FALSE
    /\ wasTerminated = FALSE

\* 1. Create Sandbox (WarmPool claim -> Running, or Cold -> Provisioning)
CreateWarm ==
    /\ phase = "none"
    /\ phase' = "Running"
    /\ desiredState' = "Running"
    /\ podState' = "ready"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ pvcsIntact' = TRUE
    /\ gwStaleLight' = FALSE
    /\ wasTerminated' = FALSE

CreateCold ==
    /\ phase = "none"
    /\ phase' = "Provisioning"
    /\ desiredState' = "Running"
    /\ podState' = "starting"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ pvcsIntact' = TRUE
    /\ gwStaleLight' = FALSE
    /\ wasTerminated' = FALSE

\* 2. Provisioning completes or fails
PodReady ==
    /\ phase = "Provisioning"
    /\ desiredState = "Running"
    /\ phase' = "Running"
    /\ podState' = "ready"
    /\ UNCHANGED <<desiredState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

ProvisionFail ==
    /\ phase \in {"Provisioning", "Running"}
    /\ phase' = "Failed"
    /\ podState' = "absent"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<desiredState, pvcsIntact, wasTerminated>>

\* 3. Active Lease lifecycle (only in Running)
AcquireLease ==
    /\ phase = "Running"
    /\ ~hasLease
    /\ hasLease' = TRUE
    /\ UNCHANGED <<phase, desiredState, podState, workloadFrozen, freezeInFlight, pvcsIntact, gwStaleLight, wasTerminated>>

ExpireLease ==
    /\ hasLease
    /\ hasLease' = FALSE
    /\ UNCHANGED <<phase, desiredState, podState, workloadFrozen, freezeInFlight, pvcsIntact, gwStaleLight, wasTerminated>>

\* 4. L1 Light Sleep initiation (explicit pause?tier=light or ReapIdle timeout)
BeginPauseLight ==
    /\ phase = "Running"
    /\ ~freezeInFlight
    /\ phase' = "PausingL1"
    /\ desiredState' = "LightPaused"
    /\ freezeInFlight' = TRUE
    /\ hasLease' = FALSE
    /\ UNCHANGED <<podState, workloadFrozen, pvcsIntact, gwStaleLight, wasTerminated>>

BeginIdleLight ==
    /\ phase = "Running"
    /\ ~hasLease
    /\ ~freezeInFlight
    /\ phase' = "PausingL1"
    /\ desiredState' = "LightPaused"
    /\ freezeInFlight' = TRUE
    /\ UNCHANGED <<podState, workloadFrozen, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* L1 cgroup freeze completes (valid epoch)
CompletePauseLight ==
    /\ phase = "PausingL1"
    /\ desiredState = "LightPaused"
    /\ freezeInFlight
    /\ phase' = "LightPaused"
    /\ workloadFrozen' = TRUE
    /\ freezeInFlight' = FALSE
    /\ UNCHANGED <<desiredState, podState, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* L1 cgroup freeze fails (e.g. podd unreachable) -> fallback to L2 Deep Pause
FallbackPauseLightToDeep ==
    /\ phase = "PausingL1"
    /\ phase' = "PausingL2"
    /\ desiredState' = "DeepPaused"
    /\ freezeInFlight' = FALSE
    /\ UNCHANGED <<podState, workloadFrozen, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* Concurrent connect arrives while PausingL1 is in flight -> preempt to ResumingL1 (epoch bumped)
PreemptPauseLightWithConnect ==
    /\ phase = "PausingL1"
    /\ phase' = "ResumingL1"
    /\ desiredState' = "Running"
    /\ UNCHANGED <<podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* Stale freeze RPC from prior epoch arrives at podd after preemption/wake -> rejected by epoch fence!
LateFreezeRejectedByEpoch ==
    /\ freezeInFlight
    /\ phase \in {"ResumingL1", "Running"}
    /\ freezeInFlight' = FALSE
    /\ UNCHANGED <<phase, desiredState, podState, workloadFrozen, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* 5. L1 Wake (LightPaused -> ResumingL1 -> Running)
BeginWakeLight ==
    /\ phase = "LightPaused"
    /\ phase' = "ResumingL1"
    /\ desiredState' = "Running"
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, wasTerminated>>

CompleteWakeLight ==
    /\ phase = "ResumingL1"
    /\ desiredState = "Running"
    /\ phase' = "Running"
    /\ workloadFrozen' = FALSE
    /\ UNCHANGED <<desiredState, podState, freezeInFlight, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* L1 Wake fails (podd unresponsive during thaw) -> delete broken pod and reboot via Provisioning
WakeLightFallbackReboot ==
    /\ phase = "ResumingL1"
    /\ phase' = "Provisioning"
    /\ desiredState' = "Running"
    /\ podState' = "starting"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ UNCHANGED <<hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* 6. L2 Deep Pause initiation (explicit deep pause from Provisioning/Running/LightPaused)
BeginPauseDeep ==
    /\ phase \in {"Provisioning", "Running", "LightPaused"}
    /\ phase' = "PausingL2"
    /\ desiredState' = "DeepPaused"
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ gwStaleLight' = (phase = "LightPaused")
    /\ UNCHANGED <<podState, workloadFrozen, pvcsIntact, wasTerminated>>

\* Controller ReapIdle demotes LightPaused -> PausingL2 (blocked if ResumingL1)
ReapLightToDeep ==
    /\ phase = "LightPaused"
    /\ desiredState = "LightPaused"
    /\ phase' = "PausingL2"
    /\ desiredState' = "DeepPaused"
    /\ gwStaleLight' = TRUE
    /\ UNCHANGED <<podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, wasTerminated>>

\* Pod deletion completes for L2 Deep Sleep
CompletePauseDeep ==
    /\ phase = "PausingL2"
    /\ phase' = "DeepPaused"
    /\ podState' = "absent"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ UNCHANGED <<desiredState, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

\* Pod evicted/killed unexpectedly while compute Pod exists -> gracefully demote to DeepPaused (PVCs intact)
PodEvictedDuringCompute ==
    /\ phase \in {"Running", "PausingL1", "LightPaused", "ResumingL1"}
    /\ phase' = "DeepPaused"
    /\ desiredState' = IF desiredState = "Running" THEN "Running" ELSE "DeepPaused"
    /\ podState' = "absent"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ gwStaleLight' = (phase = "LightPaused" \/ gwStaleLight)
    /\ UNCHANGED <<pvcsIntact, wasTerminated>>

\* 7. L2 Wake (DeepPaused -> Provisioning), including when Gateway had stale L1 cache
WakeFromDeep ==
    /\ phase = "DeepPaused"
    /\ phase' = "Provisioning"
    /\ desiredState' = "Running"
    /\ podState' = "starting"
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<workloadFrozen, freezeInFlight, hasLease, pvcsIntact, wasTerminated>>

\* Reconcile recovers a DeepPaused sandbox whose Pod was evicted while desiredState=Running
RecoverEvictedPod ==
    /\ phase = "DeepPaused"
    /\ desiredState = "Running"
    /\ phase' = "Provisioning"
    /\ podState' = "starting"
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<desiredState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, wasTerminated>>

\* 8. RootFS Reset (recovers Failed or resets active/paused sandbox, keeping Workspace PVC)
ResetRootFS ==
    /\ phase \in {"Running", "LightPaused", "DeepPaused", "Failed"}
    /\ phase' = "Provisioning"
    /\ desiredState' = "Running"
    /\ podState' = "starting"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<pvcsIntact, wasTerminated>>

\* 9. L2 PausedTTL expiration -> Terminated
ReapDeepToTerminated ==
    /\ phase = "DeepPaused"
    /\ desiredState = "DeepPaused"
    /\ phase' = "Terminated"
    /\ wasTerminated' = TRUE
    /\ UNCHANGED <<desiredState, podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, gwStaleLight>>

\* Stale Gateway connect against Terminated must reject (409 Conflict) and refresh cache
StaleConnectRejectedOnTerminated ==
    /\ phase = "Terminated"
    /\ gwStaleLight
    /\ gwStaleLight' = FALSE
    /\ UNCHANGED <<phase, desiredState, podState, workloadFrozen, freezeInFlight, hasLease, pvcsIntact, wasTerminated>>

\* Terminal state stutter (valid end state for NoDeadlock)
TerminatedStutter ==
    /\ phase = "Terminated"
    /\ ~gwStaleLight
    /\ UNCHANGED vars

\* 10. Explicit Sandbox Delete
DeleteBox ==
    /\ phase /= "none"
    /\ phase' = "none"
    /\ desiredState' = "none"
    /\ podState' = "absent"
    /\ workloadFrozen' = FALSE
    /\ freezeInFlight' = FALSE
    /\ hasLease' = FALSE
    /\ pvcsIntact' = FALSE
    /\ gwStaleLight' = FALSE
    /\ wasTerminated' = FALSE

Next ==
    \/ CreateWarm
    \/ CreateCold
    \/ PodReady
    \/ ProvisionFail
    \/ AcquireLease
    \/ ExpireLease
    \/ BeginPauseLight
    \/ BeginIdleLight
    \/ CompletePauseLight
    \/ FallbackPauseLightToDeep
    \/ PreemptPauseLightWithConnect
    \/ LateFreezeRejectedByEpoch
    \/ BeginWakeLight
    \/ CompleteWakeLight
    \/ WakeLightFallbackReboot
    \/ BeginPauseDeep
    \/ ReapLightToDeep
    \/ CompletePauseDeep
    \/ PodEvictedDuringCompute
    \/ WakeFromDeep
    \/ RecoverEvictedPod
    \/ ResetRootFS
    \/ ReapDeepToTerminated
    \/ StaleConnectRejectedOnTerminated
    \/ TerminatedStutter
    \/ DeleteBox

TypeOK ==
    /\ phase \in {"none", "Provisioning", "Running", "PausingL1", "LightPaused", "ResumingL1", "PausingL2", "DeepPaused", "Failed", "Terminated"}
    /\ desiredState \in {"none", "Running", "LightPaused", "DeepPaused"}
    /\ podState \in {"absent", "starting", "ready"}
    /\ workloadFrozen \in BOOLEAN
    /\ freezeInFlight \in BOOLEAN
    /\ hasLease \in BOOLEAN
    /\ pvcsIntact \in BOOLEAN
    /\ gwStaleLight \in BOOLEAN
    /\ wasTerminated \in BOOLEAN

TerminatedStaysTerminated ==
    wasTerminated => (phase \in {"Terminated", "none"})

PodPresenceConsistent ==
    /\ (phase \in {"none", "DeepPaused", "Failed", "Terminated"}) => (podState = "absent")
    /\ (phase = "Provisioning") => (podState = "starting")
    /\ (phase \in {"Running", "PausingL1", "LightPaused", "ResumingL1"}) => (podState = "ready")
    /\ (phase = "PausingL2") => (podState \in {"starting", "ready"})

FreezeConsistent ==
    /\ (phase = "LightPaused") => workloadFrozen
    /\ (phase = "Running") => ~workloadFrozen
    /\ (podState = "absent") => ~workloadFrozen

LeaseOnlyWhenRunning ==
    hasLease => (phase = "Running")

PVCsRetainedDuringLifecycle ==
    (phase \in {"Provisioning", "Running", "PausingL1", "LightPaused", "ResumingL1", "PausingL2", "DeepPaused", "Failed", "Terminated"}) => pvcsIntact

====
