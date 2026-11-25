export const title = "PodBox Two-Tier Sleep Lifecycle (CEP-0032)";

export const colors = {
  Init: "tan",
  Active: "teal",
  Transition: "blue",
  LightSleep: "purple",
  DeepSleep: "orange",
  Terminal: "pink",
};

export const group = (v) => {
  switch (v.phase) {
    case "none":
      return "Init";
    case "Running":
      return "Active";
    case "Provisioning":
    case "PausingL1":
    case "ResumingL1":
    case "PausingL2":
      return "Transition";
    case "LightPaused":
      return "LightSleep";
    case "DeepPaused":
      return "DeepSleep";
    case "Failed":
    case "Terminated":
      return "Terminal";
    default:
      return "Transition";
  }
};

export const state = (v) => {
  switch (v.phase) {
    case "none":
      return "No Sandbox";
    case "Provisioning":
      return "Provisioning Pod";
    case "Running":
      return v.hasLease ? "Running (Lease Held)" : "Running";
    case "PausingL1":
      return "L1 Freezing";
    case "LightPaused":
      return "L1 Light Sleep";
    case "ResumingL1":
      return "L1 Waking";
    case "PausingL2":
      return "L2 Scaling to Zero";
    case "DeepPaused":
      return v.desiredState === "Running" ? "L2 Deep Sleep (Pod Evicted)" : "L2 Deep Sleep";
    case "Failed":
      return "Failed";
    case "Terminated":
      return "Terminated";
    default:
      return v.phase;
  }
};

export const step = (action, from, to) => {
  // Collapse DeleteBox from every non-init state (except Terminated/Failed so terminal states keep an exit arrow)
  if (action === "DeleteBox" && from.phase !== "Terminated" && from.phase !== "Failed") {
    return null;
  }
  // Collapse ResetRootFS from active/sleeping states (keep from Failed to show recovery)
  if (action === "ResetRootFS" && from.phase !== "Failed") {
    return null;
  }
  // Collapse PodEvictedDuringCompute from transient states (keep from Running & LightPaused)
  if (action === "PodEvictedDuringCompute" && (from.phase === "PausingL1" || from.phase === "ResumingL1")) {
    return null;
  }
  // Collapse BeginPauseDeep from Provisioning/LightPaused? Keep LightPaused -> PausingL2!
  if (action === "BeginPauseDeep" && from.phase === "Provisioning") {
    return null;
  }

  switch (action) {
    case "CreateWarm":
      return "claim warm pod";
    case "CreateCold":
      return "create pod and PVCs";
    case "PodReady":
      return "podd /health ready";
    case "ProvisionFail":
      return "probe/boot fails";
    case "AcquireLease":
      return "acquire lease";
    case "ExpireLease":
      return "lease expires";
    case "BeginPauseLight":
      return "explicit light pause";
    case "BeginIdleLight":
      return "idle timeout [no lease]";
    case "CompletePauseLight":
      return "cgroup frozen";
    case "FallbackPauseLightToDeep":
      return "freeze fails -> L2";
    case "PreemptPauseLightWithConnect":
      return "concurrent connect";
    case "LateFreezeRejectedByEpoch":
      return "stale freeze rejected";
    case "BeginWakeLight":
      return "connect (L1 wake)";
    case "CompleteWakeLight":
      return "cgroup thawed (<500ms)";
    case "WakeLightFallbackReboot":
      return "thaw fails -> reboot";
    case "BeginPauseDeep":
      return "explicit deep pause";
    case "ReapLightToDeep":
      return "L1 TTL expires";
    case "CompletePauseDeep":
      return "pod deleted (PVCs kept)";
    case "PodEvictedDuringCompute":
      return "pod evicted";
    case "WakeFromDeep":
      return "connect (L2 reboot)";
    case "RecoverEvictedPod":
      return "reconciler recreates pod";
    case "ResetRootFS":
      return "reset rootfs";
    case "ReapDeepToTerminated":
      return "L2 paused TTL expires";
    case "StaleConnectRejectedOnTerminated":
      return "stale connect -> 409";
    case "TerminatedStutter":
      return "stays terminated";
    case "DeleteBox":
      return "delete sandbox and PVCs";
    default:
      return action;
  }
};
