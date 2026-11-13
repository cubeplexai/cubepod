package controller_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/runtime"
)

func TestCEP0019_TemplateCatalogAndBuild(t *testing.T) {
	// 1. fromImage empty steps ready does not start a builder
	t.Run("fromImage empty steps ready does not start a builder", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)
		req := controller.CreateTemplateRequest{
			Name: "empty-steps-tpl",
		}
		resp, err := r.CreateTemplate(ctx, req, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		buildReq := controller.StartBuildRequest{
			FromImage: "custom-base:latest",
			Steps:     nil, // empty steps!
		}
		err = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, buildReq)
		if err != nil {
			t.Fatalf("StartBuild failed: %v", err)
		}
		if len(fakeBuilder.BuildCalls) != 0 {
			t.Fatalf("expected builder not to be started, but got %d calls", len(fakeBuilder.BuildCalls))
		}
		resolved, err := r.ResolveTemplate(resp.TemplateID)
		if err != nil {
			t.Fatalf("ResolveTemplate failed: %v", err)
		}
		if resolved.Image != "custom-base:latest" {
			t.Fatalf("expected image custom-base:latest, got %s", resolved.Image)
		}
	})

	// 2. gate off pre-CEP resolution
	t.Run("gate off pre-CEP resolution", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(false))
		for _, alias := range []string{"", "base", "code-interpreter", "code-interpreter-v1", "python"} {
			res, err := r.ResolveTemplate(alias)
			if err != nil {
				t.Fatalf("ResolveTemplate(%q) failed: %v", alias, err)
			}
			if res.Image != "cubeplex-ubuntu24-desktop:latest" {
				t.Fatalf("expected cubeplex-ubuntu24-desktop:latest for alias %q, got %s", alias, res.Image)
			}
		}
		// Any other string uses defaultTemplate
		res, err := r.ResolveTemplate("alpine:3.19")
		if err != nil {
			t.Fatalf("ResolveTemplate(alpine:3.19) failed: %v", err)
		}
		if res.Image != "alpine:3.19" {
			t.Fatalf("expected alpine:3.19, got %s", res.Image)
		}
	})

	// 3. seeded aliases base code-interpreter code-interpreter-v1 python
	t.Run("seeded aliases base code-interpreter code-interpreter-v1 python", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		for _, alias := range []string{"base", "code-interpreter", "code-interpreter-v1", "python"} {
			res, err := r.ResolveTemplate(alias)
			if err != nil {
				t.Fatalf("ResolveTemplate(%q) failed: %v", alias, err)
			}
			if res.Image != "cubeplex-ubuntu24-desktop:latest" {
				t.Fatalf("expected cubeplex-ubuntu24-desktop:latest, got %s", res.Image)
			}
			if res.CPUCount != 2 || res.MemoryMB != 2048 {
				t.Fatalf("expected CPU 2 Mem 2048, got %d / %d", res.CPUCount, res.MemoryMB)
			}
		}
	})

	// 4. sandbox create uses cpuCount memoryMB diskSizeMB
	t.Run("sandbox create uses cpuCount memoryMB diskSizeMB", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{
			Name:          "custom-sized",
			CPUCount:      4,
			MemoryMB:      8192,
			MinFreeDiskMB: 20480,
		}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		err = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "custom-sized:1.0",
			StartCmd:  "python3 app.py",
			ReadyCmd:  "curl -f http://localhost:8080/health",
		})
		if err != nil {
			t.Fatalf("StartBuild failed: %v", err)
		}

		res, err := r.ResolveTemplate(resp.TemplateID)
		if err != nil {
			t.Fatalf("ResolveTemplate failed: %v", err)
		}
		if res.CPUCount != 4 || res.MemoryMB != 8192 || res.DiskSizeMB != 20480 {
			t.Fatalf("expected 4/8192/20480, got %d/%d/%d", res.CPUCount, res.MemoryMB, res.DiskSizeMB)
		}
		if res.StartCmd != "python3 app.py" || res.ReadyCmd != "curl -f http://localhost:8080/health" {
			t.Fatalf("unexpected startCmd / readyCmd")
		}

		// Reconcile a PodBox using this template
		boxSpec := v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStateRunning,
		}
		status, err := r.Reconcile(ctx, "sized-box", boxSpec)
		if err != nil {
			t.Fatalf("Reconcile failed: %v", err)
		}
		// Check annotation cubepod.io/resolved-image on PodBox
		client := r.KubeClient()
		podBox, err := client.GetPodBox(ctx, "cubepod-workloads", "sized-box")
		if err != nil {
			t.Fatalf("GetPodBox failed: %v", err)
		}
		if podBox.Metadata.Annotations["cubepod.io/resolved-image"] != "custom-sized:1.0" {
			t.Fatalf("expected annotation cubepod.io/resolved-image=custom-sized:1.0, got %s",
				podBox.Metadata.Annotations["cubepod.io/resolved-image"])
		}
		// Check pod manifest env var CUBEPOD_START_CMD
		memClient := client.(*controller.MemoryKubeClient)
		pod, ok := memClient.Pods["cubepod-workloads/"+status.PodName]
		if !ok {
			t.Fatalf("pod not found in memory client: %s", status.PodName)
		}
		var foundStartCmd bool
		for _, env := range pod.Containers[0].Env {
			if env.Name == "CUBEPOD_START_CMD" && env.Value == "python3 app.py" {
				foundStartCmd = true
			}
		}
		if !foundStartCmd {
			t.Fatalf("expected CUBEPOD_START_CMD in container env, got %v", pod.Containers[0].Env)
		}
	})

	// 5. placeholder cubepod.io/unbuilt:waiting is 409
	t.Run("placeholder cubepod.io/unbuilt:waiting is 409", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		_, err := r.ResolveTemplate("cubepod.io/unbuilt:waiting")
		if err == nil || !errors.Is(err, controller.ErrTemplateNotReady) {
			t.Fatalf("expected ErrTemplateNotReady (409) for placeholder, got %v", err)
		}
	})

	// 6. ghcr.io/org/app:1 still falls back when the gate is on
	t.Run("ghcr.io/org/app:1 still falls back when the gate is on", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		res, err := r.ResolveTemplate("ghcr.io/org/app:1")
		if err != nil {
			t.Fatalf("ResolveTemplate failed: %v", err)
		}
		if res.Image != "ghcr.io/org/app:1" {
			t.Fatalf("expected image ghcr.io/org/app:1, got %s", res.Image)
		}
	})

	// 7. delete referenced by PodBox or WarmPool returns 409
	t.Run("delete referenced by PodBox or WarmPool returns 409", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "ref-test-tpl"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		_ = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{FromImage: "busybox:latest"})

		// Reference by PodBox
		_, err = r.Reconcile(ctx, "ref-box", v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStateRunning,
		})
		if err != nil {
			t.Fatalf("Reconcile failed: %v", err)
		}

		err = r.DeleteTemplate(ctx, resp.TemplateID, false)
		if err == nil || !errors.Is(err, controller.ErrTemplateConflict) {
			t.Fatalf("expected ErrTemplateConflict (409), got %v", err)
		}
	})

	// 8. force delete removes the template and leaves volumes
	t.Run("force delete removes the template and leaves volumes", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "force-del-tpl"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		_ = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{FromImage: "busybox:latest"})

		// Reference by PodBox
		status, err := r.Reconcile(ctx, "force-box", v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStateRunning,
		})
		if err != nil {
			t.Fatalf("Reconcile failed: %v", err)
		}

		// Force delete
		err = r.DeleteTemplate(ctx, resp.TemplateID, true)
		if err != nil {
			t.Fatalf("force DeleteTemplate failed: %v", err)
		}

		// Volumes must still exist
		memClient := r.KubeClient().(*controller.MemoryKubeClient)
		if _, ok := memClient.PVCs["cubepod-workloads/"+status.RootfsPVCName]; !ok {
			t.Fatalf("expected rootfs PVC to remain in memory client: %s", status.RootfsPVCName)
		}
		if _, ok := memClient.PVCs["cubepod-workloads/"+status.WorkspacePVCName]; !ok {
			t.Fatalf("expected workspace PVC to remain in memory client: %s", status.WorkspacePVCName)
		}
	})

	// 9. Dockerfile FROM RUN COPY WORKDIR USER ENV
	t.Run("Dockerfile FROM RUN COPY WORKDIR USER ENV", func(t *testing.T) {
		filesHash := strings.Repeat("a", 64)
		req := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ENV", Args: []string{"FOO", "BAR", "KEY", "VALUE"}},
				{Type: "WORKDIR", Args: []string{"/app"}},
				{Type: "USER", Args: []string{"1001:1001"}},
				{Type: "COPY", Args: []string{"src.txt", "/app/dest.txt"}, FilesHash: filesHash},
				{Type: "RUN", Args: []string{"echo hello"}},
			},
		}
		df, meta, err := controller.RenderDockerfile("alpine:latest", "bld_12345678", req)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if !strings.Contains(df, "FROM alpine:latest\n") {
			t.Fatalf("missing FROM alpine:latest in: %s", df)
		}
		if !strings.Contains(df, "WORKDIR /app\n") {
			t.Fatalf("missing WORKDIR /app in: %s", df)
		}
		if !strings.Contains(df, "USER 1001:1001\n") {
			t.Fatalf("missing USER 1001:1001 in: %s", df)
		}
		if !strings.Contains(df, fmt.Sprintf("COPY %s/src.txt /app/dest.txt\n", filesHash)) {
			t.Fatalf("missing COPY in: %s", df)
		}
		if !strings.Contains(df, "RUN FOO=BAR KEY=VALUE echo hello\n") {
			t.Fatalf("missing RUN with accumulated env in: %s", df)
		}
		if meta.RunAsUser == nil || *meta.RunAsUser != 1001 {
			t.Fatalf("expected RunAsUser 1001, got %v", meta.RunAsUser)
		}
		if meta.RunAsGroup == nil || *meta.RunAsGroup != 1001 {
			t.Fatalf("expected RunAsGroup 1001, got %v", meta.RunAsGroup)
		}
	})

	// 10. ENV does not emit an ENV instruction
	t.Run("ENV does not emit an ENV instruction", func(t *testing.T) {
		req := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ENV", Args: []string{"SECRET_VAR", "supersecret"}},
				{Type: "RUN", Args: []string{"setup.sh"}},
			},
		}
		df, _, err := controller.RenderDockerfile("debian:12", "bld_12345678", req)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		for _, line := range strings.Split(df, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "ENV ") {
				t.Fatalf("found forbidden ENV instruction in Dockerfile: %s", line)
			}
		}
		if !strings.Contains(df, "RUN SECRET_VAR=supersecret setup.sh") {
			t.Fatalf("expected RUN with prefixed env, got: %s", df)
		}
	})

	// 11. COPY chown chmod
	t.Run("COPY chown chmod", func(t *testing.T) {
		filesHash := strings.Repeat("b", 64)
		req := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{
					Type:      "COPY",
					Args:      []string{"app.py", "/app/app.py", "1000:1000", "0755"},
					FilesHash: filesHash,
				},
			},
		}
		df, _, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		expected := fmt.Sprintf("COPY --chown=1000:1000 --chmod=0755 %s/app.py /app/app.py\n", filesHash)
		if !strings.Contains(df, expected) {
			t.Fatalf("expected %q, got Dockerfile:\n%s", expected, df)
		}
	})

	// 12. RUN user wrap does not change runAsUser
	t.Run("RUN user wrap does not change runAsUser", func(t *testing.T) {
		req := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"1000"}},
				{Type: "RUN", Args: []string{"npm install", "root"}},
			},
		}
		df, meta, err := controller.RenderDockerfile("node:20", "bld_12345678", req)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if !strings.Contains(df, "USER root\nRUN npm install\nUSER 1000\n") {
			t.Fatalf("expected wrapped RUN with restoring USER 1000, got:\n%s", df)
		}
		if meta.RunAsUser == nil || *meta.RunAsUser != 1000 {
			t.Fatalf("expected meta.RunAsUser to remain 1000, got %v", meta.RunAsUser)
		}
	})

	// 13. USER 0 sets runAsUser
	t.Run("USER 0 sets runAsUser", func(t *testing.T) {
		// USER 0
		req0 := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"0"}},
			},
		}
		_, meta0, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req0)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if meta0.RunAsUser == nil || *meta0.RunAsUser != 0 {
			t.Fatalf("expected meta.RunAsUser to be &0, got %v", meta0.RunAsUser)
		}
		if meta0.RunAsUsername != "" {
			t.Fatalf("expected nil/empty username for USER 0, got %q", meta0.RunAsUsername)
		}

		// USER 0abc sets RunAsUsername and leaves both pointers nil
		req0abc := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"0abc"}},
			},
		}
		_, meta0abc, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req0abc)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if meta0abc.RunAsUsername != "0abc" || meta0abc.RunAsUser != nil || meta0abc.RunAsGroup != nil {
			t.Fatalf("expected 0abc to set username only and nil pointers, got user=%v group=%v username=%q",
				meta0abc.RunAsUser, meta0abc.RunAsGroup, meta0abc.RunAsUsername)
		}

		// USER root sets RunAsUsername and leaves both pointers nil
		reqRoot := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"root"}},
			},
		}
		_, metaRoot, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", reqRoot)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if metaRoot.RunAsUsername != "root" || metaRoot.RunAsUser != nil || metaRoot.RunAsGroup != nil {
			t.Fatalf("expected root to set username only and nil pointers, got user=%v group=%v username=%q",
				metaRoot.RunAsUser, metaRoot.RunAsGroup, metaRoot.RunAsUsername)
		}

		// USER 1000:1000 sets both pointers
		req1000 := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"1000:1000"}},
			},
		}
		_, meta1000, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req1000)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if meta1000.RunAsUser == nil || *meta1000.RunAsUser != 1000 || meta1000.RunAsGroup == nil || *meta1000.RunAsGroup != 1000 {
			t.Fatalf("expected 1000:1000 to set both pointers, got user=%v group=%v",
				meta1000.RunAsUser, meta1000.RunAsGroup)
		}
		if meta1000.RunAsUsername != "" {
			t.Fatalf("expected empty username for 1000:1000, got %q", meta1000.RunAsUsername)
		}

		// USER 1000:1000x sets RunAsUsername only
		req1000x := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"1000:1000x"}},
			},
		}
		_, meta1000x, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req1000x)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if meta1000x.RunAsUsername != "1000:1000x" || meta1000x.RunAsUser != nil || meta1000x.RunAsGroup != nil {
			t.Fatalf("expected 1000:1000x to set username only and nil pointers, got user=%v group=%v username=%q",
				meta1000x.RunAsUser, meta1000x.RunAsGroup, meta1000x.RunAsUsername)
		}

		// Verify BuildKataPod sets CUBEPOD_RUN_AS_USER: "0"
		zero := int64(0)
		tpl := v1alpha1.PodTemplateSpec{
			RunAsUser: &zero,
		}
		pod := controller.BuildKataPod("test-box", tpl, v1alpha1.PodBoxSpec{}, v1alpha1.PodBoxStatus{})
		var foundEnv bool
		for _, env := range pod.Containers[0].Env {
			if env.Name == "CUBEPOD_RUN_AS_USER" && env.Value == "0" {
				foundEnv = true
			}
		}
		if !foundEnv {
			t.Fatalf("expected CUBEPOD_RUN_AS_USER=0 in pod env, got: %v", pod.Containers[0].Env)
		}
	})

	// 14. omitted USER leaves pointers nil
	t.Run("omitted USER leaves pointers nil", func(t *testing.T) {
		req := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "WORKDIR", Args: []string{"/app"}},
			},
		}
		_, meta, err := controller.RenderDockerfile("ubuntu:24.04", "bld_12345678", req)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if meta.RunAsUser != nil {
			t.Fatalf("expected nil RunAsUser, got %v", meta.RunAsUser)
		}
		if meta.RunAsGroup != nil {
			t.Fatalf("expected nil RunAsGroup, got %v", meta.RunAsGroup)
		}

		tpl := v1alpha1.PodTemplateSpec{}
		pod := controller.BuildKataPod("test-box-nil", tpl, v1alpha1.PodBoxSpec{}, v1alpha1.PodBoxStatus{})
		for _, env := range pod.Containers[0].Env {
			if env.Name == "CUBEPOD_RUN_AS_USER" || env.Name == "CUBEPOD_RUN_AS_GROUP" || env.Name == "CUBEPOD_RUN_AS_USERNAME" {
				t.Fatalf("unexpected user env var %s in pod", env.Name)
			}
		}
	})

	// 15. uploaded reported as building
	t.Run("uploaded reported as building", func(t *testing.T) {
		if got := controller.WireBuildStatus(v1alpha1.BuildStatusUploaded); got != v1alpha1.BuildStatusBuilding {
			t.Fatalf("expected uploaded -> building, got %s", got)
		}
		if got := controller.WireBuildStatus(v1alpha1.BuildStatusBuilding); got != v1alpha1.BuildStatusBuilding {
			t.Fatalf("expected building -> building, got %s", got)
		}
		if got := controller.WireBuildStatus(v1alpha1.BuildStatusReady); got != v1alpha1.BuildStatusReady {
			t.Fatalf("expected ready -> ready, got %s", got)
		}
		if got := controller.WireBuildStatus(v1alpha1.BuildStatusError); got != v1alpha1.BuildStatusError {
			t.Fatalf("expected error -> error, got %s", got)
		}
		if got := controller.WireBuildStatus(v1alpha1.BuildStatusWaiting); got != v1alpha1.BuildStatusWaiting {
			t.Fatalf("expected waiting -> waiting, got %s", got)
		}
	})

	// 16. failed rebuild keeps readyImage
	t.Run("failed rebuild keeps readyImage", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "rebuild-fail"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		// Initial build succeeds
		err = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "initial-good:1.0",
		})
		if err != nil {
			t.Fatalf("initial StartBuild failed: %v", err)
		}

		res, _ := r.ResolveTemplate(resp.TemplateID)
		if res.Image != "initial-good:1.0" {
			t.Fatalf("expected initial-good:1.0, got %s", res.Image)
		}

		// Next build fails
		fakeBuilder.BuildErr = errors.New("simulated build failure")
		err = r.StartBuild(ctx, resp.TemplateID, "bld_newbuild", controller.StartBuildRequest{
			FromImage: "initial-good:1.0",
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"failing_command"}},
			},
		})
		if err == nil {
			t.Fatalf("expected StartBuild to fail")
		}

		// Re-resolve: must still use initial-good:1.0
		resAfter, err := r.ResolveTemplate(resp.TemplateID)
		if err != nil {
			t.Fatalf("ResolveTemplate failed: %v", err)
		}
		if resAfter.Image != "initial-good:1.0" {
			t.Fatalf("expected readyImage to stay initial-good:1.0, got %s", resAfter.Image)
		}
	})

	// 17. helm Deployment moby/buildkit:rootless and no per-build Job
	t.Run("helm Deployment moby/buildkit:rootless and no per-build Job", func(t *testing.T) {
		helmPath, err := exec.LookPath("helm")
		if err != nil {
			t.Skip("helm not found in PATH")
		}
		chartPath := "../../deploy/helm/cubepod"
		if _, err := os.Stat(chartPath); err != nil {
			chartPath = "deploy/helm/cubepod"
		}

		// 1. With templateBuild.enabled=true
		outTrue, err := exec.Command(helmPath, "template", chartPath, "--set", "templateBuild.enabled=true").CombinedOutput()
		if err != nil {
			t.Fatalf("helm template enabled=true failed: %v, out: %s", err, string(outTrue))
		}
		manifestStr := string(outTrue)
		if !strings.Contains(manifestStr, "moby/buildkit:rootless") {
			t.Fatalf("expected moby/buildkit:rootless in manifests")
		}
		if !strings.Contains(manifestStr, "cubepod-buildkit") {
			t.Fatalf("expected cubepod-buildkit deployment")
		}
		if strings.Contains(manifestStr, "kind: Job") {
			t.Fatalf("manifest must not contain per-build Job")
		}

		// Assert that --oci-worker-gc-keepstorage= is followed by digits only
		reg := regexp.MustCompile(`--oci-worker-gc-keepstorage=([^\s"]+)`)
		matches := reg.FindStringSubmatch(manifestStr)
		if len(matches) < 2 {
			t.Fatalf("missing --oci-worker-gc-keepstorage in manifests: %s", manifestStr)
		}
		val := matches[1]
		digitReg := regexp.MustCompile(`^[0-9]+$`)
		if !digitReg.MatchString(val) {
			t.Fatalf("expected --oci-worker-gc-keepstorage= to be digits only, got %q", val)
		}

		// Assert cache PVC request is still 20Gi
		if !strings.Contains(manifestStr, "storage: 20Gi") {
			t.Fatalf("expected cache PVC request to be 20Gi in manifests")
		}

		// 2. With templateBuild.enabled=false
		outFalse, err := exec.Command(helmPath, "template", chartPath, "--set", "templateBuild.enabled=false").CombinedOutput()
		if err != nil {
			t.Fatalf("helm template enabled=false failed: %v, out: %s", err, string(outFalse))
		}
		if strings.Contains(string(outFalse), "cubepod-buildkit") {
			t.Fatalf("cubepod-buildkit should not be rendered when templateBuild.enabled=false")
		}
	})

	// 18. rendered Dockerfile does not contain podd-init
	t.Run("rendered Dockerfile does not contain podd-init", func(t *testing.T) {
		// Attempting to inject podd-init returns 400
		reqBad := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"curl -O /opt/podd-init"}},
			},
		}
		_, _, err := controller.RenderDockerfile("alpine:latest", "bld_12345678", reqBad)
		if err == nil {
			t.Fatalf("expected error for Dockerfile containing podd-init")
		}

		// Reject COPY whose source base name is podd
		reqCopyPodd := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "COPY", Args: []string{"bin/podd", "/opt/podd"}},
			},
		}
		_, _, err = controller.RenderDockerfile("alpine:latest", "bld_12345678", reqCopyPodd)
		if err == nil {
			t.Fatalf("expected error for COPY whose source base name is podd")
		}

		// Reject ADD whose source base name is podd-init
		reqAddPoddInit := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ADD", Args: []string{"podd-init", "/opt/podd-init"}},
			},
		}
		_, _, err = controller.RenderDockerfile("alpine:latest", "bld_12345678", reqAddPoddInit)
		if err == nil {
			t.Fatalf("expected error for ADD whose source base name is podd-init")
		}

		// A RUN whose command is "echo podd" must render!
		reqEchoPodd := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"echo podd"}},
			},
		}
		dfPodd, _, err := controller.RenderDockerfile("alpine:latest", "bld_12345678", reqEchoPodd)
		if err != nil {
			t.Fatalf("expected RUN echo podd to render successfully, got err: %v", err)
		}
		if !strings.Contains(dfPodd, "RUN echo podd\n") {
			t.Fatalf("expected RUN echo podd in Dockerfile, got: %s", dfPodd)
		}

		// Valid build steps do not contain podd-init
		reqGood := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "WORKDIR", Args: []string{"/app"}},
				{Type: "RUN", Args: []string{"echo normal-build"}},
			},
		}
		df, _, err := controller.RenderDockerfile("alpine:latest", "bld_12345678", reqGood)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		if strings.Contains(df, "podd-init") {
			t.Fatalf("rendered Dockerfile contains podd-init: %s", df)
		}
	})

	// 19. local tag cubepod-templates/<slug>:<buildID>
	t.Run("local tag cubepod-templates/<slug>:<buildID>", func(t *testing.T) {
		tag := runtime.LocalTemplateTag("python-app", "bld_aabbccdd")
		if tag != "cubepod-templates/python-app:bld_aabbccdd" {
			t.Fatalf("expected cubepod-templates/python-app:bld_aabbccdd, got %s", tag)
		}
	})

	// 20. workspace PVC unchanged when rootfs image changes
	t.Run("workspace PVC unchanged when rootfs image changes", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "pvc-persist-test"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		_ = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "image-v1:latest",
		})

		boxSpec := v1alpha1.PodBoxSpec{
			TemplateRef:      resp.TemplateID,
			DesiredState:     v1alpha1.DesiredStateRunning,
			RootfsGeneration: 1,
		}
		status1, err := r.Reconcile(ctx, "persistent-box", boxSpec)
		if err != nil {
			t.Fatalf("Reconcile 1 failed: %v", err)
		}
		initialWsPVC := status1.WorkspacePVCName
		initialRootfsPVC := status1.RootfsPVCName

		// Rebuild template with a new image (image-v2)
		err = r.StartBuild(ctx, resp.TemplateID, "bld_v2", controller.StartBuildRequest{
			FromImage: "image-v2:latest",
		})
		if err != nil {
			t.Fatalf("StartBuild v2 failed: %v", err)
		}

		// Sandbox reconcile again
		status2, err := r.Reconcile(ctx, "persistent-box", boxSpec)
		if err != nil {
			t.Fatalf("Reconcile 2 failed: %v", err)
		}

		if status2.WorkspacePVCName != initialWsPVC {
			t.Fatalf("expected workspace PVC %s to remain unchanged, got %s", initialWsPVC, status2.WorkspacePVCName)
		}
		// Existing PodBox rootfs PVC is preserved for existing sandbox
		if status2.RootfsPVCName != initialRootfsPVC {
			t.Fatalf("expected rootfs PVC %s to remain unchanged for running box, got %s", initialRootfsPVC, status2.RootfsPVCName)
		}
	})

	// 21. gzip body stored and a different body is 409
	t.Run("gzip body stored and a different body is 409", func(t *testing.T) {
		store := controller.NewMemoryContextStore()
		hash := strings.Repeat("c", 64)

		createTar := func(content string) []byte {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			hdr := &tar.Header{
				Name: "file.txt",
				Mode: 0644,
				Size: int64(len(content)),
			}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write([]byte(content))
			_ = tw.Close()
			return buf.Bytes()
		}

		gzipBytes := func(data []byte) []byte {
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			_, _ = gw.Write(data)
			_ = gw.Close()
			return buf.Bytes()
		}

		tar1 := createTar("hello world")
		gz1 := gzipBytes(tar1)

		// Store gzip body
		err := store.Store(hash, bytes.NewReader(gz1))
		if err != nil {
			t.Fatalf("Store gzipped tar failed: %v", err)
		}
		if !store.IsPresent(hash) {
			t.Fatalf("expected store.IsPresent(%s) to be true", hash)
		}

		// Storing identical bytes succeeds
		err = store.Store(hash, bytes.NewReader(gz1))
		if err != nil {
			t.Fatalf("Store identical body failed: %v", err)
		}

		// Storing different body for same hash returns 409
		tar2 := createTar("different content")
		gz2 := gzipBytes(tar2)
		err = store.Store(hash, bytes.NewReader(gz2))
		if err == nil || !errors.Is(err, controller.ErrTarMismatch) {
			t.Fatalf("expected ErrTarMismatch (409) for different body, got %v", err)
		}
	})

	// 22. ARG step is 400 and the only ARG line is CUBEPOD_CACHEBUST
	t.Run("ARG step is 400 and the only ARG line is CUBEPOD_CACHEBUST", func(t *testing.T) {
		// 1. Incoming step type ARG is 400
		reqArg := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ARG", Args: []string{"FOO=bar"}},
			},
		}
		_, _, err := controller.RenderDockerfile("alpine:latest", "bld_12345678", reqArg)
		if err == nil {
			t.Fatalf("expected 400 error for incoming step type ARG")
		}

		// 2. Step with Force: true inserts ARG CUBEPOD_CACHEBUST=<buildID>
		buildID := "bld_testcachebust"
		reqForce := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"echo cached"}},
				{Type: "RUN", Args: []string{"echo busted"}, Force: true},
			},
		}
		df, _, err := controller.RenderDockerfile("alpine:latest", buildID, reqForce)
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		expectedArg := fmt.Sprintf("ARG CUBEPOD_CACHEBUST=%s", buildID)
		if !strings.Contains(df, expectedArg) {
			t.Fatalf("expected %q in Dockerfile:\n%s", expectedArg, df)
		}

		// Assert that the only ARG lines in Dockerfile are CUBEPOD_CACHEBUST
		for _, line := range strings.Split(df, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "ARG ") {
				if !strings.HasPrefix(trimmed, "ARG CUBEPOD_CACHEBUST=") {
					t.Fatalf("unexpected ARG line in Dockerfile: %s", trimmed)
				}
			}
		}
	})

	// 23. Log scrubber removes a fixture password
	t.Run("log scrubber removes a fixture password", func(t *testing.T) {
		pass := "super-secret-fixture-pass-123"
		lines := []string{
			"Connecting to registry...",
			fmt.Sprintf("Auth header: Bearer %s", pass),
			"Pushing layer sha256:12345",
		}
		tmpDir := t.TempDir()
		buildID := "bld_scrubtest"
		err := controller.WriteScrubbedLog(tmpDir, buildID, lines, []string{pass})
		if err != nil {
			t.Fatalf("WriteScrubbedLog failed: %v", err)
		}
		logFile := filepath.Join(tmpDir, "logs", buildID+".log")
		content, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		contentStr := string(content)
		if strings.Contains(contentStr, pass) {
			t.Fatalf("log contains unscrubbed password: %s", contentStr)
		}
		if !strings.Contains(contentStr, "[REDACTED]") {
			t.Fatalf("expected [REDACTED] in scrubbed log: %s", contentStr)
		}
	})

	// 24. Idempotency replay returns same templateID and buildID and mismatch returns 409
	t.Run("idempotency replay returns same templateID and buildID and mismatch returns 409", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		req1 := controller.CreateTemplateRequest{Name: "idemp-tpl", CPUCount: 2}
		resp1, err := r.CreateTemplate(ctx, req1, "key-123")
		if err != nil {
			t.Fatalf("CreateTemplate 1 failed: %v", err)
		}

		// Replay with same key and same body
		resp2, err := r.CreateTemplate(ctx, req1, "key-123")
		if err != nil {
			t.Fatalf("CreateTemplate replay failed: %v", err)
		}
		if resp2.TemplateID != resp1.TemplateID || resp2.BuildID != resp1.BuildID {
			t.Fatalf("expected same templateID/buildID, got %v vs %v", resp2, resp1)
		}

		// Replay with same key and different body
		req2 := controller.CreateTemplateRequest{Name: "idemp-tpl", CPUCount: 4}
		_, err = r.CreateTemplate(ctx, req2, "key-123")
		if err == nil || !errors.Is(err, controller.ErrIdempotencyMismatch) {
			t.Fatalf("expected ErrIdempotencyMismatch (409), got %v", err)
		}
	})

	// 25. COPY or WORKDIR of /workspace returns 400
	t.Run("COPY or WORKDIR of /workspace returns 400", func(t *testing.T) {
		reqWorkdir := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "WORKDIR", Args: []string{"/workspace"}},
			},
		}
		if _, _, err := controller.RenderDockerfile("alpine:latest", "bld_1", reqWorkdir); err == nil {
			t.Fatalf("expected 400 for WORKDIR /workspace")
		}
		reqWorkdirSub := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "WORKDIR", Args: []string{"/workspace/subdir"}},
			},
		}
		if _, _, err := controller.RenderDockerfile("alpine:latest", "bld_1", reqWorkdirSub); err == nil {
			t.Fatalf("expected 400 for WORKDIR /workspace/subdir")
		}
		reqCopy := controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "COPY", Args: []string{"file.txt", "/workspace/file.txt"}},
			},
		}
		if _, _, err := controller.RenderDockerfile("alpine:latest", "bld_1", reqCopy); err == nil {
			t.Fatalf("expected 400 for COPY dest in /workspace")
		}
	})

	// 26. Warm pool retirement and ClaimWarmPod does not return old image
	t.Run("readyImage change deletes unclaimed warm entries and ClaimWarmPod does not return old image", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "warm-retire-tpl"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		_ = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "base-warm:v1",
		})

		poolSpec := v1alpha1.PodWarmPoolSpec{
			TemplateRef: resp.TemplateID,
			Replicas:    2,
		}
		_, err = r.ReconcileWarmPool(ctx, "test-pool", poolSpec)
		if err != nil {
			t.Fatalf("ReconcileWarmPool failed: %v", err)
		}

		// Claim one pod for box-1
		boxSpec := v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			WarmPoolRef:  "test-pool",
			DesiredState: v1alpha1.DesiredStateRunning,
		}
		boxStatus, err := r.Reconcile(ctx, "box-1", boxSpec)
		if err != nil {
			t.Fatalf("Reconcile box-1 failed: %v", err)
		}
		claimedPodName := boxStatus.PodName

		// Template image changes to base-warm:v2
		err = r.StartBuild(ctx, resp.TemplateID, "bld_v2", controller.StartBuildRequest{
			FromImage: "base-warm:v2",
		})
		if err != nil {
			t.Fatalf("StartBuild v2 failed: %v", err)
		}

		// Claimed PodBox is intact
		memClient := r.KubeClient().(*controller.MemoryKubeClient)
		if _, ok := memClient.Pods["cubepod-workloads/"+claimedPodName]; !ok {
			t.Fatalf("expected claimed Pod to remain in memory client: %s", claimedPodName)
		}

		// Old unclaimed entry was retired; ClaimWarmPod will not return an entry stamped with the previous image
		claim2, err := r.ClaimWarmPod(ctx, "test-pool", "box-2")
		if err == nil && claim2 != nil && claim2.Image == "base-warm:v1" {
			t.Fatalf("expected ClaimWarmPod to not return entry with old image base-warm:v1, got %v", claim2)
		}

		// Re-reconciling warm pool provisions new replicas stamped with base-warm:v2
		_, err = r.ReconcileWarmPool(ctx, "test-pool", poolSpec)
		if err != nil {
			t.Fatalf("ReconcileWarmPool after update failed: %v", err)
		}
		claim3, err := r.ClaimWarmPod(ctx, "test-pool", "box-3")
		if err != nil {
			t.Fatalf("ClaimWarmPod for new replica failed: %v", err)
		}
		if claim3.Image != "base-warm:v2" {
			t.Fatalf("expected newly claimed pod to have image base-warm:v2, got %s", claim3.Image)
		}
	})

	// 27. Non-empty steps while template builds are disabled returns ErrTemplateBuildDisabled
	t.Run("non-empty steps while template builds are disabled returns ErrTemplateBuildDisabled", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(false),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "disabled-tpl"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}
		err = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "alpine:latest",
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"echo test"}},
			},
		})
		if !errors.Is(err, controller.ErrTemplateBuildDisabled) {
			t.Fatalf("expected ErrTemplateBuildDisabled, got %v", err)
		}
	})

	// 28. In-flight build on same template returns 409
	t.Run("second build on same template while latest is building returns 409", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		fakeBuilder.BuildHook = func(_ controller.BuildOptions) error {
			return nil
		}
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "conflict-tpl"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}

		// Set status to building
		tpl, _ := r.GetPodTemplate(resp.TemplateID)
		tpl.Status.BuildStatus = v1alpha1.BuildStatusBuilding
		_ = r.KubeClient().UpdatePodTemplateStatus(ctx, *tpl)

		// Calling StartBuild with a different buildID while status is building returns 409
		err = r.StartBuild(ctx, resp.TemplateID, "bld_other", controller.StartBuildRequest{
			FromImage: "alpine:latest",
			Steps:     []controller.BuildStep{{Type: "RUN", Args: []string{"echo test"}}},
		})
		if err == nil || !errors.Is(err, controller.ErrTemplateConflict) {
			t.Fatalf("expected ErrTemplateConflict (409), got %v", err)
		}
	})

	// 29. RecoverInFlightBuilds tag present and probe success ends ready; tag absent ends with reason
	t.Run("RecoverInFlightBuilds tag present and probe success ends ready; tag absent ends with reason", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)

		// 1. Tag present and probe success ends ready
		resp1, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "recover-success"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate 1 failed: %v", err)
		}
		tpl1, _ := r.GetPodTemplate(resp1.TemplateID)
		tpl1.Status.BuildStatus = v1alpha1.BuildStatusBuilding
		tpl1.Status.LatestBuildID = "bld_succ1"
		tpl1.Spec.ReadyCmd = "true"
		_ = r.KubeClient().UpdatePodTemplateStatus(ctx, *tpl1)
		_ = r.EnsureSeededTemplates(ctx)

		tag1 := fmt.Sprintf("cubepod-templates/%s:%s", tpl1.Metadata.Name, "bld_succ1")
		fakeBuilder.ExistingTags[tag1] = true

		r.RecoverInFlightBuilds(ctx)

		recovered1, ok := r.GetPodTemplate(resp1.TemplateID)
		if !ok {
			t.Fatalf("GetPodTemplate 1 failed")
		}
		if recovered1.Status.BuildStatus != v1alpha1.BuildStatusReady {
			t.Fatalf("expected ready status when tag exists and probe succeeds, got %s", recovered1.Status.BuildStatus)
		}
		if recovered1.Status.ReadyImage != tag1 {
			t.Fatalf("expected readyImage %s, got %s", tag1, recovered1.Status.ReadyImage)
		}
		if recovered1.Spec.Image != tag1 {
			t.Fatalf("expected spec.image %s, got %s", tag1, recovered1.Spec.Image)
		}

		// 2. Tag absent ends with reason "builder disappeared" and previous readyImage unchanged
		resp2, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "recover-absent"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate 2 failed: %v", err)
		}
		// Set initial ready image
		err = r.StartBuild(ctx, resp2.TemplateID, resp2.BuildID, controller.StartBuildRequest{
			FromImage: "initial-stable:1.0",
		})
		if err != nil {
			t.Fatalf("StartBuild initial failed: %v", err)
		}

		tpl2, _ := r.GetPodTemplate(resp2.TemplateID)
		tpl2.Status.BuildStatus = v1alpha1.BuildStatusBuilding
		tpl2.Status.LatestBuildID = "bld_absent"
		_ = r.KubeClient().UpdatePodTemplateStatus(ctx, *tpl2)
		_ = r.EnsureSeededTemplates(ctx)

		r.RecoverInFlightBuilds(ctx)

		recovered2, ok := r.GetPodTemplate(resp2.TemplateID)
		if !ok {
			t.Fatalf("GetPodTemplate 2 failed")
		}
		if recovered2.Status.BuildStatus != v1alpha1.BuildStatusError {
			t.Fatalf("expected error status when tag absent, got %s", recovered2.Status.BuildStatus)
		}
		if recovered2.Status.Reason != "builder disappeared" {
			t.Fatalf("expected reason 'builder disappeared', got %s", recovered2.Status.Reason)
		}
		if recovered2.Status.ReadyImage != "initial-stable:1.0" {
			t.Fatalf("expected previous readyImage initial-stable:1.0 unchanged, got %s", recovered2.Status.ReadyImage)
		}
	})

	// 30. status subresource does not persist spec
	t.Run("status subresource does not persist spec", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "subresource-test"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}

		// 1. Empty steps build with startCmd "sleep 1"
		err = r.StartBuild(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage: "custom-img:1.0",
			StartCmd:  "sleep 1",
		})
		if err != nil {
			t.Fatalf("StartBuild 1 failed: %v", err)
		}

		// Reload object from KubeClient
		obj1, err := r.KubeClient().GetPodTemplate(ctx, "cubepod-workloads", "subresource-test")
		if err != nil {
			t.Fatalf("GetPodTemplate 1 failed: %v", err)
		}
		if obj1.Spec.Image != "custom-img:1.0" {
			t.Fatalf("expected spec.image to be persisted as custom-img:1.0, got %s", obj1.Spec.Image)
		}
		if obj1.Spec.StartCmd != "sleep 1" {
			t.Fatalf("expected spec.startCmd to be persisted as 'sleep 1', got %s", obj1.Spec.StartCmd)
		}

		// 2. Second build whose USER step is "0"
		err = r.StartBuild(ctx, resp.TemplateID, "bld_second", controller.StartBuildRequest{
			FromImage: "custom-img:1.0",
			Steps: []controller.BuildStep{
				{Type: "USER", Args: []string{"0"}},
			},
		})
		if err != nil {
			t.Fatalf("StartBuild 2 failed: %v", err)
		}

		// Reload object from KubeClient
		obj2, err := r.KubeClient().GetPodTemplate(ctx, "cubepod-workloads", "subresource-test")
		if err != nil {
			t.Fatalf("GetPodTemplate 2 failed: %v", err)
		}
		if obj2.Spec.RunAsUser == nil || *obj2.Spec.RunAsUser != 0 {
			t.Fatalf("expected spec.runAsUser 0, got %v", obj2.Spec.RunAsUser)
		}
		if obj2.Spec.Image == "" || obj2.Spec.Image == "cubepod.io/unbuilt:waiting" {
			t.Fatalf("expected spec.image to be updated to target tag, got %s", obj2.Spec.Image)
		}
	})

	// 31. mark public and get by alias
	t.Run("mark public and get by alias", func(t *testing.T) {
		ctx := context.Background()
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))

		// 1. GetPodTemplate by alias "python" returns the seeded cubeplex-ubuntu24-desktop object
		tpl, ok := r.GetPodTemplate("python")
		if !ok || tpl == nil {
			t.Fatalf("expected GetPodTemplate(\"python\") to find template")
		}
		if tpl.Metadata.Name != "cubeplex-ubuntu24-desktop" {
			t.Fatalf("expected cubeplex-ubuntu24-desktop, got %s", tpl.Metadata.Name)
		}

		// 2. SetTemplatePublic to true
		err := r.SetTemplatePublic(ctx, "python", true)
		if err != nil {
			t.Fatalf("SetTemplatePublic true failed: %v", err)
		}

		// In-memory object has public=true
		tplPublic, ok := r.GetPodTemplate("python")
		if !ok || !tplPublic.Spec.Public || !tplPublic.Status.Public {
			t.Fatalf("expected in-memory template public true, got spec=%v status=%v",
				tplPublic.Spec.Public, tplPublic.Status.Public)
		}

		// Persisted object in KubeClient has public=true
		kubeObj, err := r.KubeClient().GetPodTemplate(ctx, "cubepod-workloads", "cubeplex-ubuntu24-desktop")
		if err != nil {
			t.Fatalf("GetPodTemplate from KubeClient failed: %v", err)
		}
		if !kubeObj.Spec.Public || !kubeObj.Status.Public {
			t.Fatalf("expected persisted template public true, got spec=%v status=%v",
				kubeObj.Spec.Public, kubeObj.Status.Public)
		}

		// 3. SetTemplatePublic back to false
		err = r.SetTemplatePublic(ctx, "python", false)
		if err != nil {
			t.Fatalf("SetTemplatePublic false failed: %v", err)
		}
		tplPrivate, _ := r.GetPodTemplate("python")
		if tplPrivate.Spec.Public || tplPrivate.Status.Public {
			t.Fatalf("expected template public false, got spec=%v status=%v",
				tplPrivate.Spec.Public, tplPrivate.Status.Public)
		}
	})
}
