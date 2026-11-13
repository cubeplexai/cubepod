package k8s_e2e_test

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestBenchmarkPercentileCalculation(t *testing.T) {
	collector := NewBenchmarkCollector("unit-percentile", nil, map[string]SLOTarget{})

	// Record 20 unsorted samples with latencies 10ms..200ms (step 10ms).
	// 1 MiB transferred in each sample.
	const oneMiB = 1024 * 1024
	durations := []float64{
		140, 10, 200, 30, 90, 50, 120, 70, 160, 20,
		180, 40, 110, 80, 150, 60, 190, 100, 130, 170,
	}
	for i, d := range durations {
		collector.Record(BenchmarkSample{
			Operation:  OpArchiveSync,
			Iteration:  i + 1,
			DurationMs: d,
			Bytes:      oneMiB,
			Success:    true,
		})
	}

	report := collector.BuildReport()
	if len(report.Summaries) != 1 {
		t.Fatalf("expected 1 operation summary, got %d", len(report.Summaries))
	}
	s := report.Summaries[0]
	if s.Count != 20 || s.SuccessCount != 20 || s.FailureCount != 0 {
		t.Fatalf("unexpected counts: %+v", s)
	}
	if !almostEqual(s.MinMs, 10, 0.001) {
		t.Fatalf("min = %.2f, want 10", s.MinMs)
	}
	if !almostEqual(s.P50Ms, 100, 0.001) {
		t.Fatalf("p50 = %.2f, want 100", s.P50Ms)
	}
	if !almostEqual(s.P95Ms, 190, 0.001) {
		t.Fatalf("p95 = %.2f, want 190", s.P95Ms)
	}
	if !almostEqual(s.MaxMs, 200, 0.001) {
		t.Fatalf("max = %.2f, want 200", s.MaxMs)
	}
	// Mean of 10..200 step 10 is 105.0.
	if !almostEqual(s.MeanMs, 105, 0.001) {
		t.Fatalf("mean = %.2f, want 105", s.MeanMs)
	}
	// Total 20 MiB over 2100ms (2.1s) ~= 9.5238 MiB/s.
	wantThroughput := 20.0 / 2.1
	if !almostEqual(s.ThroughputMBps, wantThroughput, 0.01) {
		t.Fatalf("throughput = %.4f MiB/s, want %.4f", s.ThroughputMBps, wantThroughput)
	}

	// Single-sample edge case.
	single := computeOperationStats("single", []BenchmarkSample{
		{Operation: "single", DurationMs: 42.5, Success: true},
	}, nil)
	if single.MinMs != 42.5 || single.P50Ms != 42.5 || single.P95Ms != 42.5 || single.MaxMs != 42.5 || single.MeanMs != 42.5 {
		t.Fatalf("unexpected single-sample stats: %+v", single)
	}
}

func TestBenchmarkSLOPassWarnFailEvaluation(t *testing.T) {
	slos := DefaultCubePodSLOs()
	if slos[OpWarmPoolClaimController].TargetMs != 50 {
		t.Fatalf("expected warmpool_claim_controller target 50ms, got %v", slos[OpWarmPoolClaimController].TargetMs)
	}
	if slos[OpWarmPoolClaimE2E].TargetMs != 200 {
		t.Fatalf("expected warmpool_claim_e2e target 200ms, got %v", slos[OpWarmPoolClaimE2E].TargetMs)
	}
	if slos[OpPauseAutoResume].TargetMs != 1500 {
		t.Fatalf("expected pause_auto_resume target 1500ms, got %v", slos[OpPauseAutoResume].TargetMs)
	}
	if slos[OpCoWFork].TargetMs != 1500 {
		t.Fatalf("expected cow_fork target 1500ms, got %v", slos[OpCoWFork].TargetMs)
	}
	if slos[OpArchiveSync].TargetMs != 100 {
		t.Fatalf("expected archive_sync target 100ms, got %v", slos[OpArchiveSync].TargetMs)
	}

	collector := NewBenchmarkCollector("unit-slo", nil, slos)

	// 1. PASS: p95 <= 200ms for warmpool_claim_e2e.
	for i, d := range []float64{110, 130, 150, 175, 190} {
		collector.Record(BenchmarkSample{Operation: OpWarmPoolClaimE2E, Iteration: i + 1, DurationMs: d, Success: true})
	}

	// 2. WARN: p50 <= 100ms (80ms) while p95 > 100ms (180ms) for archive_sync.
	for i, d := range []float64{60, 70, 80, 95, 180} {
		collector.Record(BenchmarkSample{Operation: OpArchiveSync, Iteration: i + 1, DurationMs: d, Success: true})
	}

	// 3. FAIL: both p50 and p95 exceed TargetMs and WarnMs for cow_fork (target 1500ms, warn 2500ms).
	for i, d := range []float64{2600, 2900, 3200} {
		collector.Record(BenchmarkSample{Operation: OpCoWFork, Iteration: i + 1, DurationMs: d, Success: true})
	}

	// 4. INFO: no SLO target defined for cold_create.
	collector.Record(BenchmarkSample{Operation: OpColdCreate, Iteration: 1, DurationMs: 4200, Success: true})

	// 5. FAIL: all samples failed for pause_auto_resume.
	collector.Record(BenchmarkSample{Operation: OpPauseAutoResume, Iteration: 1, DurationMs: 500, Success: false, Error: "timeout"})

	report := collector.BuildReport()
	if report.PassedCount != 1 || report.WarnCount != 1 || report.FailedCount != 2 || report.InfoCount != 1 {
		t.Fatalf("unexpected SLO counts: pass=%d warn=%d fail=%d info=%d",
			report.PassedCount, report.WarnCount, report.FailedCount, report.InfoCount)
	}

	statusByOp := make(map[string]string, len(report.Summaries))
	for _, s := range report.Summaries {
		statusByOp[s.Operation] = s.Status
	}
	if statusByOp[OpWarmPoolClaimE2E] != SLOStatusPass {
		t.Fatalf("warmpool_claim_e2e status = %s, want PASS", statusByOp[OpWarmPoolClaimE2E])
	}
	if statusByOp[OpArchiveSync] != SLOStatusWarn {
		t.Fatalf("archive_sync status = %s, want WARN", statusByOp[OpArchiveSync])
	}
	if statusByOp[OpCoWFork] != SLOStatusFail {
		t.Fatalf("cow_fork status = %s, want FAIL", statusByOp[OpCoWFork])
	}
	if statusByOp[OpColdCreate] != SLOStatusInfo {
		t.Fatalf("cold_create status = %s, want INFO", statusByOp[OpColdCreate])
	}
	if statusByOp[OpPauseAutoResume] != SLOStatusFail {
		t.Fatalf("pause_auto_resume status = %s, want FAIL", statusByOp[OpPauseAutoResume])
	}
}

func TestBenchmarkReportRendering(t *testing.T) {
	collector := NewBenchmarkCollector("CubePod Kata MicroVM Benchmark", map[string]string{
		"runtimeClass": "kata-qemu",
		"storageClass": "longhorn-v2-block-thin",
	}, DefaultCubePodSLOs())

	collector.Record(BenchmarkSample{
		Operation:  OpWarmPoolClaimE2E,
		Iteration:  1,
		DurationMs: 120.5,
		Success:    true,
	})
	collector.Record(BenchmarkSample{
		Operation:  OpArchiveSync,
		Iteration:  1,
		DurationMs: 45.0,
		Bytes:      512 * 1024,
		Success:    true,
	})

	report := collector.BuildReport()

	rawJSON, err := report.RenderJSON()
	if err != nil {
		t.Fatalf("RenderJSON failed: %v", err)
	}
	var decoded BenchmarkSuiteReport
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("RenderJSON produced invalid JSON: %v\n%s", err, string(rawJSON))
	}
	if decoded.SuiteName != "CubePod Kata MicroVM Benchmark" {
		t.Fatalf("decoded SuiteName = %q", decoded.SuiteName)
	}
	if decoded.Environment["runtimeClass"] != "kata-qemu" {
		t.Fatalf("decoded runtimeClass = %q", decoded.Environment["runtimeClass"])
	}
	if len(decoded.Summaries) != 2 || len(decoded.Samples) != 2 {
		t.Fatalf("decoded counts summaries=%d samples=%d", len(decoded.Summaries), len(decoded.Samples))
	}

	md := report.RenderMarkdown()
	for _, substr := range []string{
		"# CubePod Kata MicroVM Benchmark",
		"runtimeClass",
		"kata-qemu",
		"longhorn-v2-block-thin",
		"| Operation | Samples (OK/Total) | Min (ms) | p50 (ms) | p95 (ms) | Max (ms) | Mean (ms) | Throughput | SLO Target | Status |",
		"| `warmpool_claim_e2e` | 1/1 | 120.50 |",
		"| `archive_sync` | 1/1 | 45.00 |",
		"< 200 ms",
		"< 100 ms",
		"**PASS**",
		"MiB/s",
	} {
		if !strings.Contains(md, substr) {
			t.Fatalf("Markdown output missing %q:\n%s", substr, md)
		}
	}
}

func TestConfigureContainerdKataScriptDryRun(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "deploy", "k8s", "kata", "configure-containerd-kata.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("script not found at %s: %v", scriptPath, err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("script %s is not executable (mode %v)", scriptPath, info.Mode())
	}

	tmpDir := t.TempDir()

	// 1. Dry-run on a containerd config.toml with kata-qemu missing the required flags.
	containerdCfg := filepath.Join(tmpDir, "config.toml")
	initialContainerd := `version = 2

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
  runtime_type = "io.containerd.kata.v2"
  privileged_without_host_devices = false
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"
`
	if err := os.WriteFile(containerdCfg, []byte(initialContainerd), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath, "--dry-run", "--config-path", containerdCfg)
	outBytes, err := cmd.CombinedOutput()
	out := string(outBytes)
	if err != nil {
		t.Fatalf("configure-containerd-kata.sh --dry-run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"/dev/kvm",
		"privileged_without_host_devices = true",
		"privileged_without_host_devices_all_devices_allowed = true",
		"DRY-RUN",
		"files_modified=0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}

	// Verify --dry-run did not mutate the config file on disk.
	afterDryRun, err := os.ReadFile(containerdCfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDryRun) != initialContainerd {
		t.Fatalf("dry-run modified %s on disk:\n%s", containerdCfg, string(afterDryRun))
	}

	// 2. Dry-run on a Kata configuration-qemu.toml.
	kataCfg := filepath.Join(tmpDir, "configuration-qemu.toml")
	initialKata := `[hypervisor.qemu]
path = "/opt/kata/bin/qemu-system-x86_64"

[runtime]
enable_debug = false
`
	if err := os.WriteFile(kataCfg, []byte(initialKata), 0644); err != nil {
		t.Fatal(err)
	}
	cmdKata := exec.Command("bash", scriptPath, "--dry-run", "--config-path", kataCfg)
	kataOutBytes, err := cmdKata.CombinedOutput()
	kataOut := string(kataOutBytes)
	if err != nil {
		t.Fatalf("configure-containerd-kata.sh --dry-run on Kata TOML failed: %v\n%s", err, kataOut)
	}
	if !strings.Contains(kataOut, "privileged_without_host_devices = true") ||
		!strings.Contains(kataOut, "privileged_without_host_devices_all_devices_allowed = true") {
		t.Fatalf("Kata TOML dry-run missing expected keys:\n%s", kataOut)
	}

	// 3. Apply mode on temp containerd config + verify idempotency on second run.
	cmdApply := exec.Command("bash", scriptPath, "--allow-missing-kvm", "--config-path", containerdCfg)
	if applyOut, err := cmdApply.CombinedOutput(); err != nil {
		t.Fatalf("apply to temp config failed: %v\n%s", err, string(applyOut))
	}
	updated, err := os.ReadFile(containerdCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "privileged_without_host_devices = true") ||
		!strings.Contains(string(updated), "privileged_without_host_devices_all_devices_allowed = true") ||
		strings.Contains(string(updated), "privileged_without_host_devices = false") {
		t.Fatalf("applied config did not contain expected flags:\n%s", string(updated))
	}

	// Second run must report already configured with no modifications.
	cmdSecond := exec.Command("bash", scriptPath, "--allow-missing-kvm", "--config-path", containerdCfg)
	secondOutBytes, err := cmdSecond.CombinedOutput()
	secondOut := string(secondOutBytes)
	if err != nil {
		t.Fatalf("second idempotent run failed: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "changes_applied=0") {
		t.Fatalf("expected changes_applied=0 on second run, got:\n%s", secondOut)
	}
}
