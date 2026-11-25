---- MODULE PodBoxTieredSleepNoEpoch ----
EXTENDS PodBoxTieredSleep

\* Faulty variant: late freeze RPC overwrites cgroup.freeze=1 even after concurrent connect bumped phase to Running!
LateFreezeWithoutEpoch ==
    /\ freezeInFlight
    /\ phase \in {"ResumingL1", "Running"}
    /\ freezeInFlight' = FALSE
    /\ workloadFrozen' = TRUE
    /\ UNCHANGED <<phase, desiredState, podState, hasLease, pvcsIntact, gwStaleLight, wasTerminated>>

NextBuggy ==
    \/ Next
    \/ LateFreezeWithoutEpoch

====
