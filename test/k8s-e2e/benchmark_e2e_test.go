package k8s_e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

func benchIterations() int {
	raw := strings.TrimSpace(os.Getenv("CUBEPOD_BENCH_ITERATIONS"))
	if raw == "" {
		return 3
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 3
	}
	return n
}

func buildArchiveSyncPayload() (map[string][]byte, int64) {
	files := make(map[string][]byte, 10)
	var total int64
	chunk := bytes.Repeat([]byte("cubepod-kata-benchmark-payload-line\n"), 180) // ~6.4 KiB per file
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("skill-%02d/handler.txt", i)
		data := append([]byte(fmt.Sprintf("# file %d\n", i)), chunk...)
		files[name] = data
		total += int64(len(data))
	}
	return files, total
}

// TestK8sPerformanceBenchmark runs an end-to-end CubePod latency & throughput suite
// against a live Kubernetes cluster (gated by CUBEPOD_K8S_BENCH=1).
// Set CUBEPOD_BENCH_OUT=/path/to/report.md to persist the Markdown report artifact.
func TestK8sPerformanceBenchmark(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_BENCH") != "1" {
		t.Skip("Skipping live Kubernetes performance benchmark: set CUBEPOD_K8S_BENCH=1 to run against live cluster")
	}

	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("set CUBEPOD_GATEWAY_URL, or expose svc/cubepod-gateway as NodePort or LoadBalancer")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("set CUBEPOD_SANDBOX_IMAGE, or pass -sandbox-image on the cubepod-gateway Deployment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("failed to initialize RESTKubeClient from environment/kubeconfig: %v", err)
	}

	if _, err := c.Health(ctx); err != nil {
		t.Fatalf("gateway health check failed at %s: %v", cfg.GatewayURL, err)
	}

	iters := benchIterations()
	env := map[string]string{
		"gatewayURL":   cfg.GatewayURL,
		"namespace":    cfg.Namespace,
		"sandboxImage": cfg.SandboxImage,
		"storageClass": cfg.StorageClass,
		"iterations":   strconv.Itoa(iters),
	}
	if rc := os.Getenv("CUBEPOD_BENCH_RUNTIME_CLASS"); rc != "" {
		env["runtimeClass"] = rc
	}
	collector := NewBenchmarkCollector("CubePod K8s Kata Performance Benchmark", env, DefaultCubePodSLOs())

	// Always emit the report on exit if any samples were collected.
	t.Cleanup(func() {
		report := collector.BuildReport()
		md := report.RenderMarkdown()
		t.Logf("\n%s", md)

		if outPath := strings.TrimSpace(os.Getenv("CUBEPOD_BENCH_OUT")); outPath != "" {
			if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
				t.Errorf("failed to create CUBEPOD_BENCH_OUT dir: %v", err)
			} else if err := os.WriteFile(outPath, []byte(md), 0644); err != nil {
				t.Errorf("failed to write CUBEPOD_BENCH_OUT %s: %v", outPath, err)
			} else {
				t.Logf("[Bench] Markdown report written to %s", outPath)
			}
		}

		if jsonPath := strings.TrimSpace(os.Getenv("CUBEPOD_BENCH_JSON_OUT")); jsonPath != "" {
			if raw, err := report.RenderJSON(); err != nil {
				t.Errorf("failed to render JSON benchmark report: %v", err)
			} else if err := os.MkdirAll(filepath.Dir(jsonPath), 0755); err != nil {
				t.Errorf("failed to create CUBEPOD_BENCH_JSON_OUT dir: %v", err)
			} else if err := os.WriteFile(jsonPath, raw, 0644); err != nil {
				t.Errorf("failed to write CUBEPOD_BENCH_JSON_OUT %s: %v", jsonPath, err)
			} else {
				t.Logf("[Bench] JSON report written to %s", jsonPath)
			}
		}
	})

	// 1. Cold Create
	t.Logf("[Bench] 1/7 Measuring Cold Create...")
	var sbCold *client.SandboxRecord
	if err := collector.Measure(OpColdCreate, 1, 0, nil, func() error {
		var createErr error
		sbCold, createErr = c.CreateSandbox(ctx, client.CreateSandboxRequest{
			TemplateID: cfg.SandboxImage,
			Metadata: map[string]string{
				"env":   "k8s-bench",
				"phase": "cold-create",
			},
		})
		return createErr
	}); err != nil {
		t.Fatalf("cold create failed: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = c.DeleteSandboxWithOptions(cleanupCtx, sbCold.SandboxID, true)
	}()

	podName := fmt.Sprintf("%s-kata-pod", sbCold.SandboxID)
	if podObj, err := kubeClient.GetPod(ctx, cfg.Namespace, podName); err == nil {
		if specMap, ok := podObj["spec"].(map[string]any); ok {
			if rc, _ := specMap["runtimeClassName"].(string); rc != "" {
				collector.SetEnvironment("runtimeClass", rc)
			}
		}
	}

	// 2. Foreground Exec RTT
	execIters := iters
	if execIters < 5 {
		execIters = 5
	}
	t.Logf("[Bench] 2/7 Measuring Foreground Exec RTT (%d iterations)...", execIters)
	for i := 1; i <= execIters; i++ {
		err := collector.Measure(OpExecForegroundRTT, i, 0, nil, func() error {
			res, err := c.RunCommand(ctx, sbCold.SandboxID, client.RunCommandRequest{
				Command: "echo bench-exec-ok",
			})
			if err != nil {
				return err
			}
			if res.ExitCode == nil || *res.ExitCode != 0 {
				return fmt.Errorf("unexpected exit code %v: %s", res.ExitCode, res.Output)
			}
			if !strings.Contains(res.Output, "bench-exec-ok") {
				return fmt.Errorf("unexpected output: %s", res.Output)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("foreground exec RTT iteration %d failed: %v", i, err)
		}
	}

	// 3. archive-sync
	syncFiles, syncBytes := buildArchiveSyncPayload()
	t.Logf("[Bench] 3/7 Measuring archive-sync (%d iterations, %d bytes/iter)...", iters, syncBytes)
	for i := 1; i <= iters; i++ {
		err := collector.Measure(OpArchiveSync, i, syncBytes, nil, func() error {
			return c.ArchiveSync(ctx, sbCold.SandboxID, "/workspace/bench-sync", syncFiles)
		})
		if err != nil {
			t.Fatalf("archive-sync iteration %d failed: %v", i, err)
		}
	}
	if got, err := c.ReadFile(ctx, sbCold.SandboxID, "/workspace/bench-sync/skill-00/handler.txt"); err != nil || !bytes.Equal(got, syncFiles["skill-00/handler.txt"]) {
		t.Fatalf("archive-sync verification failed: err=%v, len=%d", err, len(got))
	}

	// 4. Pause + Auto-Resume
	t.Logf("[Bench] 4/7 Measuring Pause & Auto-Resume...")
	wsMarkerPath := "/workspace/bench-persist.txt"
	wsMarkerData := []byte("cubepod-bench-workspace-marker")
	rootfsMarkerPath := "/var/tmp/bench-rootfs-persist.txt"
	rootfsMarkerData := []byte("cubepod-bench-rootfs-marker")
	if err := c.WriteFile(ctx, sbCold.SandboxID, wsMarkerPath, wsMarkerData, "0644"); err != nil {
		t.Fatalf("failed to write workspace marker: %v", err)
	}
	if err := c.WriteFile(ctx, sbCold.SandboxID, rootfsMarkerPath, rootfsMarkerData, "0644"); err != nil {
		t.Fatalf("failed to write rootfs marker: %v", err)
	}

	if err := collector.Measure(OpPause, 1, 0, nil, func() error {
		rec, err := c.PauseSandbox(ctx, sbCold.SandboxID)
		if err != nil {
			return err
		}
		if rec.State != "Paused" {
			return fmt.Errorf("expected Paused state, got %s", rec.State)
		}
		return nil
	}); err != nil {
		t.Fatalf("pause failed: %v", err)
	}

	// Ensure compute Pod is completely terminated before measuring Auto-Resume.
	podDeleted := false
	for i := 0; i < 60; i++ {
		_, err := kubeClient.GetPod(ctx, cfg.Namespace, podName)
		if err != nil && (strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")) {
			podDeleted = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !podDeleted {
		t.Fatalf("pod %s was not deleted after Pause", podName)
	}

	if err := collector.Measure(OpPauseAutoResume, 1, int64(len(wsMarkerData)), nil, func() error {
		got, err := c.ReadFile(ctx, sbCold.SandboxID, wsMarkerPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, wsMarkerData) {
			return fmt.Errorf("workspace marker mismatch after auto-resume: got %q", string(got))
		}
		return nil
	}); err != nil {
		t.Fatalf("pause -> auto-resume failed: %v", err)
	}

	// 5. reset-rootfs preserving /workspace
	t.Logf("[Bench] 5/7 Measuring reset-rootfs (preserving /workspace)...")
	if err := collector.Measure(OpResetRootFS, 1, 0, nil, func() error {
		rec, err := c.ResetRootFS(ctx, sbCold.SandboxID)
		if err != nil {
			return err
		}
		if rec.RootfsGeneration < 2 {
			return fmt.Errorf("expected rootfsGeneration >= 2, got %d", rec.RootfsGeneration)
		}
		return nil
	}); err != nil {
		t.Fatalf("reset-rootfs failed: %v", err)
	}
	if got, err := c.ReadFile(ctx, sbCold.SandboxID, wsMarkerPath); err != nil || !bytes.Equal(got, wsMarkerData) {
		t.Fatalf("workspace marker lost across reset-rootfs: err=%v, got=%q", err, string(got))
	}
	if _, err := c.ReadFile(ctx, sbCold.SandboxID, rootfsMarkerPath); err == nil {
		t.Fatalf("expected rootfs marker %s to be wiped after reset-rootfs", rootfsMarkerPath)
	}

	// 6. CoW Fork
	t.Logf("[Bench] 6/7 Measuring CoW Fork...")
	var sbForked *client.SandboxRecord
	if err := collector.Measure(OpCoWFork, 1, 0, nil, func() error {
		var forkErr error
		sbForked, forkErr = c.ForkSandbox(ctx, sbCold.SandboxID)
		return forkErr
	}); err != nil {
		t.Fatalf("fork failed: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = c.DeleteSandboxWithOptions(cleanupCtx, sbForked.SandboxID, true)
	}()
	if got, err := c.ReadFile(ctx, sbForked.SandboxID, wsMarkerPath); err != nil || !bytes.Equal(got, wsMarkerData) {
		t.Fatalf("forked sandbox missing workspace marker: err=%v, got=%q", err, string(got))
	}

	// 7. WarmPool Pre-Warm + Claim
	poolReplicas := int32(iters)
	if poolReplicas > 5 {
		poolReplicas = 5
	}
	poolName := fmt.Sprintf("bench-wp-%d", time.Now().Unix()%100000)
	t.Logf("[Bench] 7/7 Measuring WarmPool Pre-Warm (replicas=%d) & Claim...", poolReplicas)

	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = kubeClient.DeletePodWarmPool(cleanupCtx, cfg.Namespace, poolName)
	}()

	if err := collector.Measure(OpWarmPoolPrewarm, 1, 0, map[string]string{"replicas": strconv.Itoa(int(poolReplicas))}, func() error {
		if _, err := c.CreateWarmPool(ctx, poolName, cfg.SandboxImage, poolReplicas); err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			pools, err := c.ListWarmPools(ctx)
			if err == nil {
				for _, p := range pools {
					if p.Name == poolName && p.AvailableReplicas >= poolReplicas {
						return nil
					}
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		return fmt.Errorf("warmpool %s did not reach %d available replicas within 2m", poolName, poolReplicas)
	}); err != nil {
		t.Fatalf("warmpool pre-warm failed: %v", err)
	}

	for i := 1; i <= int(poolReplicas); i++ {
		var claimed *client.SandboxRecord
		err := collector.Measure(OpWarmPoolClaimE2E, i, 0, nil, func() error {
			var claimErr error
			claimed, claimErr = c.CreateSandbox(ctx, client.CreateSandboxRequest{
				TemplateID: cfg.SandboxImage,
				Metadata: map[string]string{
					"env":   "k8s-bench",
					"phase": "warmpool-claim",
				},
			})
			if claimErr != nil {
				return claimErr
			}
			if claimed.Reason != "WarmPoolClaimed" {
				return fmt.Errorf("expected Reason=WarmPoolClaimed, got %q", claimed.Reason)
			}
			return nil
		})
		if claimed != nil {
			sbID := claimed.SandboxID
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cleanupCancel()
				_ = c.DeleteSandboxWithOptions(cleanupCtx, sbID, true)
			}()
		}
		if err != nil {
			t.Fatalf("warmpool claim iteration %d failed: %v", i, err)
		}
	}

	if os.Getenv("CUBEPOD_BENCH_STRICT_SLO") == "1" {
		report := collector.BuildReport()
		if report.FailedCount > 0 {
			t.Fatalf("benchmark SLO strict check failed (%d operations in FAIL status)", report.FailedCount)
		}
	}
}
