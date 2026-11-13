package controller_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
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
		if !strings.Contains(df, "RUN export FOO='BAR'; export KEY='VALUE'; echo hello\n") {
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
		if !strings.Contains(df, "RUN export SECRET_VAR='supersecret'; setup.sh") {
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

	// 32. same-name rebuild reuses templateID, increments buildCount, keeps readyImage, rejects different primary name slug collision
	t.Run("same-name rebuild reuses templateID and rejects different primary name slug collision", func(t *testing.T) {
		ctx := context.Background()
		stateDir := t.TempDir()
		fakeBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
			controller.WithStateDir(stateDir),
		)

		resp1, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{
			Name:          "my_app",
			CPUCount:      2,
			MemoryMB:      1024,
			MinFreeDiskMB: 512, // below 10Gi minimum -> clamped to 10240
		}, "")
		if err != nil {
			t.Fatalf("CreateTemplate 1 failed: %v", err)
		}

		tpl1, ok := r.GetPodTemplate(resp1.TemplateID)
		if !ok {
			t.Fatalf("GetPodTemplate failed")
		}
		if tpl1.Spec.DiskSizeMB != 10240 || tpl1.Spec.Storage.RootfsSize != "10240Mi" || tpl1.Spec.Storage.WorkspaceSize != "20Gi" {
			t.Fatalf("unexpected sizing: disk=%d rootfs=%s ws=%s", tpl1.Spec.DiskSizeMB, tpl1.Spec.Storage.RootfsSize, tpl1.Spec.Storage.WorkspaceSize)
		}
		if tpl1.Spec.Resources.CPULimit != "2" || tpl1.Spec.Resources.MemoryLimit != "1024Mi" || tpl1.Spec.Resources.MemoryRequest != "1024Mi" {
			t.Fatalf("unexpected resources: %+v", tpl1.Spec.Resources)
		}
		if tpl1.Metadata.Annotations["cubepod.io/primary-name"] != "my_app" {
			t.Fatalf("expected primary-name annotation my_app, got %v", tpl1.Metadata.Annotations)
		}
		if tpl1.Status.BuildCount != 1 {
			t.Fatalf("expected BuildCount 1, got %d", tpl1.Status.BuildCount)
		}

		if err := r.StartBuild(ctx, resp1.TemplateID, resp1.BuildID, controller.StartBuildRequest{
			FromImage: "alpine:3.20",
		}); err != nil {
			t.Fatalf("StartBuild 1 failed: %v", err)
		}

		// Same-name rebuild while ready
		resp2, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{
			Name:     "my_app",
			CPUCount: 4,
			MemoryMB: 4096,
		}, "")
		if err != nil {
			t.Fatalf("CreateTemplate same-name rebuild failed: %v", err)
		}
		if resp2.TemplateID != resp1.TemplateID {
			t.Fatalf("expected reused templateID %s, got %s", resp1.TemplateID, resp2.TemplateID)
		}
		if resp2.BuildID == resp1.BuildID {
			t.Fatalf("expected fresh buildID, got same %s", resp2.BuildID)
		}
		tpl2, _ := r.GetPodTemplate(resp1.TemplateID)
		if tpl2.Status.BuildStatus != v1alpha1.BuildStatusWaiting {
			t.Fatalf("expected waiting status after rebuild CreateTemplate, got %s", tpl2.Status.BuildStatus)
		}
		if tpl2.Status.ReadyImage != "alpine:3.20" || tpl2.Status.ReadyBuildID != resp1.BuildID {
			t.Fatalf("expected ReadyImage/ReadyBuildID preserved, got %s / %s", tpl2.Status.ReadyImage, tpl2.Status.ReadyBuildID)
		}
		if tpl2.Status.BuildCount != 2 || len(tpl2.Status.Builds) != 2 {
			t.Fatalf("expected BuildCount=2 and 2 build records, got %d / %d", tpl2.Status.BuildCount, len(tpl2.Status.Builds))
		}

		// Different primary name that slugifies to same "my-app" must return 409 Conflict
		_, err = r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "my-app"}, "")
		if !errors.Is(err, controller.ErrTemplateConflict) {
			t.Fatalf("expected ErrTemplateConflict on different primary name collision, got %v", err)
		}

		// Reloading from stateDir recovers persisted template JSON
		rReloaded := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithStateDir(stateDir),
		)
		loaded, ok := rReloaded.GetPodTemplate(resp1.TemplateID)
		if !ok || loaded.Status.ReadyImage != "alpine:3.20" || loaded.Status.BuildCount != 2 {
			t.Fatalf("expected reloaded template from stateDir, ok=%v loaded=%+v", ok, loaded)
		}
	})

	// 33. RenderDockerfile secret safety, ordered env override, invalid env key, and POSIX quoting
	t.Run("RenderDockerfile secret safety ordered env override and quoting", func(t *testing.T) {
		_, _, err := controller.RenderDockerfile("alpine:latest", "bld_1", controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"echo cpodref_openai"}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "cpodref_") {
			t.Fatalf("expected 400 error on cpodref_ in step arg, got %v", err)
		}

		_, _, err = controller.RenderDockerfile("alpine:latest", "bld_1", controller.StartBuildRequest{
			FromImageRegistry: &controller.RegistryAuth{Username: "u", Password: "top-secret-password"},
			Steps: []controller.BuildStep{
				{Type: "RUN", Args: []string{"echo top-secret-password"}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "400:") {
			t.Fatalf("expected 400 error when step arg leaks registry password, got %v", err)
		}

		_, _, err = controller.RenderDockerfile("alpine:latest", "bld_1", controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ENV", Args: []string{"1BAD", "val"}},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "400:") {
			t.Fatalf("expected 400 error on invalid env key, got %v", err)
		}

		df, _, err := controller.RenderDockerfile("alpine:latest", "bld_1", controller.StartBuildRequest{
			Steps: []controller.BuildStep{
				{Type: "ENV", Args: []string{"A", "old", "B", "it's"}},
				{Type: "ENV", Args: []string{"A", "new"}},
				{Type: "RUN", Args: []string{"env"}},
			},
		})
		if err != nil {
			t.Fatalf("RenderDockerfile failed: %v", err)
		}
		want := `RUN export A='new'; export B='it'"'"'s'; env`
		if !strings.Contains(df, want) {
			t.Fatalf("expected %q in dockerfile:\n%s", want, df)
		}
	})

	// 34. fromTemplate 404, 409 unready, inheritance, and len(steps)==0 && readyCmd!="" requires templateBuild
	t.Run("fromTemplate 404/409 inheritance and readyCmd gating", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		rDisabled := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		respDis, _ := rDisabled.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "gate-check"}, "")
		err := rDisabled.StartBuild(ctx, respDis.TemplateID, respDis.BuildID, controller.StartBuildRequest{
			FromImage: "alpine:latest",
			ReadyCmd:  "true",
		})
		if !errors.Is(err, controller.ErrTemplateBuildDisabled) {
			t.Fatalf("expected ErrTemplateBuildDisabled when steps empty but readyCmd non-empty, got %v", err)
		}

		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)

		// Missing parent -> 404
		childResp, _ := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "child-missing"}, "")
		err = r.StartBuild(ctx, childResp.TemplateID, childResp.BuildID, controller.StartBuildRequest{
			FromTemplate: "does-not-exist",
		})
		if !errors.Is(err, controller.ErrTemplateNotFound) {
			t.Fatalf("expected ErrTemplateNotFound, got %v", err)
		}

		// Unready parent -> 409
		unreadyParent, _ := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "unready-parent"}, "")
		childUnready, _ := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "child-unready"}, "")
		err = r.StartBuild(ctx, childUnready.TemplateID, childUnready.BuildID, controller.StartBuildRequest{
			FromTemplate: unreadyParent.TemplateID,
		})
		if !errors.Is(err, controller.ErrTemplateNotReady) {
			t.Fatalf("expected ErrTemplateNotReady, got %v", err)
		}

		// Ready parent with USER + StartCmd + ReadyCmd -> child inherits when unset
		parentResp, _ := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "parent-ready"}, "")
		err = r.StartBuild(ctx, parentResp.TemplateID, parentResp.BuildID, controller.StartBuildRequest{
			FromImage: "ubuntu:24.04",
			StartCmd:  "sleep infinity",
			ReadyCmd:  "true",
			Steps:     []controller.BuildStep{{Type: "USER", Args: []string{"1001:1002"}}},
		})
		if err != nil {
			t.Fatalf("parent StartBuild failed: %v", err)
		}

		childInherit, _ := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "child-inherit"}, "")
		err = r.StartBuild(ctx, childInherit.TemplateID, childInherit.BuildID, controller.StartBuildRequest{
			FromTemplate: parentResp.TemplateID,
		})
		if err != nil {
			t.Fatalf("child StartBuild failed: %v", err)
		}
		cTpl, _ := r.GetPodTemplate(childInherit.TemplateID)
		if cTpl.Spec.StartCmd != "sleep infinity" || cTpl.Spec.ReadyCmd != "true" ||
			cTpl.Spec.RunAsUser == nil || *cTpl.Spec.RunAsUser != 1001 ||
			cTpl.Spec.RunAsGroup == nil || *cTpl.Spec.RunAsGroup != 1002 {
			t.Fatalf("expected child to inherit parent runtime fields, got %+v", cTpl.Spec)
		}
	})

	// 35. StartBuildAsync, ephemeral secret cleanup, metrics, RecordTemplateSpawn, Paused->Running pinned image, DeleteTemplate & WarmPool retirement
	t.Run("async build ephemeral secret metrics spawn tracking resume pin and delete rules", func(t *testing.T) {
		ctx := context.Background()
		stateDir := t.TempDir()
		fakeBuilder := controller.NewFakeImageBuilder()
		buildStarted := make(chan struct{})
		releaseBuild := make(chan struct{})
		var sawSecretDuringBuild bool

		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
			controller.WithStateDir(stateDir),
			controller.WithTemplateRegistry("registry.example.com"),
			controller.WithTemplateRepoPrefix("org/templates"),
			controller.WithTemplatePullSecret("cubepod-template-pull"),
		)

		fakeBuilder.BuildHook = func(opts controller.BuildOptions) error {
			secName := fmt.Sprintf("cubepod-template-src-%s", opts.BuildID)
			if sec, secErr := r.KubeClient().GetSecret(ctx, "cubepod-workloads", secName); secErr == nil && sec != nil {
				sawSecretDuringBuild = true
			}
			close(buildStarted)
			<-releaseBuild
			return nil
		}

		resp, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "async-app"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate failed: %v", err)
		}

		err = r.StartBuildAsync(ctx, resp.TemplateID, resp.BuildID, controller.StartBuildRequest{
			FromImage:         "private.reg.io/base:1.0",
			FromImageRegistry: &controller.RegistryAuth{Username: "bot", Password: "pw"},
			Steps:             []controller.BuildStep{{Type: "RUN", Args: []string{"echo hi"}}},
		})
		if err != nil {
			t.Fatalf("StartBuildAsync failed: %v", err)
		}

		<-buildStarted

		// Cannot delete while building
		if delErr := r.DeleteTemplate(ctx, resp.TemplateID, true); !errors.Is(delErr, controller.ErrTemplateConflict) {
			t.Fatalf("expected 409 deleting building template, got %v", delErr)
		}

		close(releaseBuild)

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			tpl, ok := r.GetPodTemplate(resp.TemplateID)
			if ok && tpl.Status.BuildStatus == v1alpha1.BuildStatusReady {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}

		tpl, _ := r.GetPodTemplate(resp.TemplateID)
		if tpl.Status.BuildStatus != v1alpha1.BuildStatusReady {
			t.Fatalf("expected ready status, got %s (%s)", tpl.Status.BuildStatus, tpl.Status.Reason)
		}
		wantTag := fmt.Sprintf("registry.example.com/org/templates/async-app:%s", resp.BuildID)
		if tpl.Status.ReadyImage != wantTag {
			t.Fatalf("expected tag %s, got %s", wantTag, tpl.Status.ReadyImage)
		}
		if !sawSecretDuringBuild {
			t.Fatalf("expected ephemeral secret cubepod-template-src-%s to exist during Build", resp.BuildID)
		}
		if _, secErr := r.KubeClient().GetSecret(ctx, "cubepod-workloads", "cubepod-template-src-"+resp.BuildID); secErr == nil {
			t.Fatalf("expected ephemeral source secret to be deleted after build")
		}
		if metrics := r.TemplateBuildMetrics(); metrics.TemplateBuildsTotal["ready"] < 1 || metrics.TemplateBuildDurationCount < 1 {
			t.Fatalf("expected build metrics recorded, got %+v", metrics)
		}

		// RecordTemplateSpawn increments SpawnCount & LastSpawnedAt
		r.RecordTemplateSpawn(ctx, "async-app")
		tplAfterSpawn, _ := r.GetPodTemplate(resp.TemplateID)
		if tplAfterSpawn.Status.SpawnCount != 1 || tplAfterSpawn.Status.LastSpawnedAt == "" {
			t.Fatalf("expected SpawnCount=1 and LastSpawnedAt set, got %+v", tplAfterSpawn.Status)
		}

		// Reconcile sandbox & check templatePullSecret attached (and never cubepod-template-src-*)
		_, err = r.Reconcile(ctx, "box-pinned", v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStateRunning,
		})
		if err != nil {
			t.Fatalf("Reconcile box-pinned failed: %v", err)
		}
		podManifest := r.KubeClient().(*controller.MemoryKubeClient).Pods["cubepod-workloads/box-pinned-kata-pod"]
		if !slices.Contains(podManifest.ImagePullSecrets, "cubepod-template-pull") {
			t.Fatalf("expected cubepod-template-pull secret on sandbox pod, got %v", podManifest.ImagePullSecrets)
		}
		for _, sec := range podManifest.ImagePullSecrets {
			if strings.HasPrefix(sec, "cubepod-template-src-") {
				t.Fatalf("unexpected ephemeral source secret leaked into sandbox pod: %v", podManifest.ImagePullSecrets)
			}
		}

		// Pause box, rebuild template, resume box -> resolved-image must stay pinned to v1 tag!
		if _, err := r.Pause(ctx, "box-pinned"); err != nil {
			t.Fatalf("Pause failed: %v", err)
		}
		fakeBuilder.BuildHook = nil
		resp2, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "async-app"}, "")
		if err != nil {
			t.Fatalf("rebuild CreateTemplate failed: %v", err)
		}
		if err := r.StartBuild(ctx, resp2.TemplateID, resp2.BuildID, controller.StartBuildRequest{
			FromImage: "alpine:3.20",
		}); err != nil {
			t.Fatalf("rebuild StartBuild failed: %v", err)
		}
		if _, err := r.Reconcile(ctx, "box-pinned", v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStateRunning,
		}); err != nil {
			t.Fatalf("Resume failed: %v", err)
		}
		resumedBox, _ := r.KubeClient().GetPodBox(ctx, "cubepod-workloads", "box-pinned")
		if resumedBox.Metadata.Annotations["cubepod.io/resolved-image"] != wantTag {
			t.Fatalf("expected resumed PodBox to stay pinned to %s, got %s", wantTag, resumedBox.Metadata.Annotations["cubepod.io/resolved-image"])
		}

		// DeleteTemplate blocked while box is Running, allowed once box is Terminated
		if delErr := r.DeleteTemplate(ctx, resp.TemplateID, false); !errors.Is(delErr, controller.ErrTemplateConflict) {
			t.Fatalf("expected ErrTemplateConflict while box is Running, got %v", delErr)
		}
		// Expire box via ReapIdle to mark Terminated
		_ = r.SetPausedAt("box-pinned", time.Now().Add(-2*time.Hour))
		_, _ = r.Pause(ctx, "box-pinned")
		_ = r.SetPausedAt("box-pinned", time.Now().Add(-2*time.Hour))
		_, err = r.Reconcile(ctx, "box-pinned", v1alpha1.PodBoxSpec{
			TemplateRef:  resp.TemplateID,
			DesiredState: v1alpha1.DesiredStatePaused,
			Lifecycle:    v1alpha1.LifecyclePolicy{PausedTTLSeconds: 60},
		})
		if err != nil {
			t.Fatalf("Reconcile paused failed: %v", err)
		}
		_ = r.SetPausedAt("box-pinned", time.Now().Add(-2*time.Hour))
		_, term := r.ReapIdle(ctx, time.Now())
		if len(term) != 1 || term[0] != "box-pinned" {
			t.Fatalf("expected box-pinned terminated, got %v", term)
		}

		if err := r.DeleteTemplate(ctx, resp.TemplateID, false); err != nil {
			t.Fatalf("expected DeleteTemplate to succeed when referencing PodBox is Terminated, got %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(stateDir, "templates", resp.TemplateID+".json")); !os.IsNotExist(statErr) {
			t.Fatalf("expected local template file removed after DeleteTemplate, statErr=%v", statErr)
		}
	})

	// 36. ContextStore 512Mi limit, symlink/hardlink/fifo rejection, ReadLogs & SweepExpired
	t.Run("ContextStore size limit link traversal fifo ReadLogs and SweepExpired", func(t *testing.T) {
		ctxDir := t.TempDir()
		store := controller.NewMemoryContextStore(ctxDir)
		store.SetMaxBytes(1024)

		// Exceeds configured maxBytes -> 413 ErrContextTooLarge
		var bigBuf bytes.Buffer
		tw := tar.NewWriter(&bigBuf)
		payload := bytes.Repeat([]byte("a"), 1500)
		_ = tw.WriteHeader(&tar.Header{Name: "big.txt", Mode: 0644, Size: int64(len(payload))})
		_, _ = tw.Write(payload)
		_ = tw.Close()
		if err := store.Store(strings.Repeat("1", 64), &bigBuf); !errors.Is(err, controller.ErrContextTooLarge) {
			t.Fatalf("expected ErrContextTooLarge, got %v", err)
		}
		store.SetMaxBytes(1024 * 1024)

		// Symlink escaping context prefix -> rejected
		var symBuf bytes.Buffer
		twSym := tar.NewWriter(&symBuf)
		_ = twSym.WriteHeader(&tar.Header{Name: "sub/escape", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"})
		_ = twSym.Close()
		if err := store.Store(strings.Repeat("2", 64), &symBuf); !errors.Is(err, controller.ErrPathTraversal) {
			t.Fatalf("expected ErrPathTraversal on escaping symlink, got %v", err)
		}

		// Hardlink escaping context prefix -> rejected
		var linkBuf bytes.Buffer
		twLink := tar.NewWriter(&linkBuf)
		_ = twLink.WriteHeader(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../secret"})
		_ = twLink.Close()
		if err := store.Store(strings.Repeat("3", 64), &linkBuf); !errors.Is(err, controller.ErrPathTraversal) {
			t.Fatalf("expected ErrPathTraversal on escaping hardlink, got %v", err)
		}

		// FIFO / device node -> rejected
		var fifoBuf bytes.Buffer
		twFifo := tar.NewWriter(&fifoBuf)
		_ = twFifo.WriteHeader(&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0644})
		_ = twFifo.Close()
		if err := store.Store(strings.Repeat("4", 64), &fifoBuf); !errors.Is(err, controller.ErrInvalidContextTar) {
			t.Fatalf("expected ErrInvalidContextTar on FIFO entry, got %v", err)
		}

		// AppendLog + ReadLogs filtering & pagination
		_ = store.AppendLog("bld_logs1", []string{"starting build", "warning: deprecated flag", "error: step failed"})
		errsOnly, err := store.ReadLogs("bld_logs1", 0, 10, "error")
		if err != nil || len(errsOnly) != 1 || errsOnly[0].Level != "error" {
			t.Fatalf("expected 1 error log entry, got %+v (err=%v)", errsOnly, err)
		}
		paged, err := store.ReadLogs("bld_logs1", 1, 1, "")
		if err != nil || len(paged) != 1 || !strings.Contains(paged[0].Message, "warning") {
			t.Fatalf("expected 2nd entry on offset=1 limit=1, got %+v (err=%v)", paged, err)
		}

		// Valid context tar + SweepExpired
		var validBuf bytes.Buffer
		twVal := tar.NewWriter(&validBuf)
		_ = twVal.WriteHeader(&tar.Header{Name: "ok.txt", Mode: 0644, Size: 2})
		_, _ = twVal.Write([]byte("ok"))
		_ = twVal.Close()
		validHash := strings.Repeat("5", 64)
		if err := store.Store(validHash, &validBuf); err != nil {
			t.Fatalf("Store valid tar failed: %v", err)
		}
		if err := store.SweepExpired(time.Now().Add(2*time.Hour), 1*time.Hour, 1*time.Hour); err != nil {
			t.Fatalf("SweepExpired failed: %v", err)
		}
		if store.IsPresent(validHash) {
			t.Fatalf("expected expired tar to be swept")
		}
		if afterLogs, _ := store.ReadLogs("bld_logs1", 0, 10, ""); len(afterLogs) != 0 {
			t.Fatalf("expected expired logs to be swept, got %+v", afterLogs)
		}
	})

	// 37. Secret CRUD (Memory + REST httptest) & BuildKitBuilder TagExists + Probe
	t.Run("Secret CRUD REST httptest and BuildKitBuilder TagExists and Probe", func(t *testing.T) {
		ctx := context.Background()

		// Registry mock with Bearer token exchange + V2 manifest HEAD
		var tokenHits atomic.Int32
		regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/token":
				tokenHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-abc"})
			case "/v2/org/my-tpl/manifests/bld_123":
				if r.Header.Get("Authorization") != "Bearer tok-abc" {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="http://%s/token",service="reg",scope="repository:org/my-tpl:pull"`, r.Host))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer regSrv.Close()

		// Mock Kubernetes APIServer for Secrets and Probe Pods
		secrets := make(map[string]controller.KubeSecret)
		var createdPodJSON map[string]any
		var deletedProbePod atomic.Bool
		var probePollCount atomic.Int32

		kubeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/cubepod-workloads/secrets":
				var sec controller.KubeSecret
				_ = json.NewDecoder(r.Body).Decode(&sec)
				secrets[sec.Metadata.Name] = sec
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(sec)
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/cubepod-workloads/secrets/"):
				name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/cubepod-workloads/secrets/")
				sec, ok := secrets[name]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(sec)
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/cubepod-workloads/secrets/"):
				name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/cubepod-workloads/secrets/")
				delete(secrets, name)
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/cubepod-workloads/pods":
				_ = json.NewDecoder(r.Body).Decode(&createdPodJSON)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(createdPodJSON)
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/cubepod-workloads/pods/cubepod-probe-bld-123":
				phase := "Running"
				if probePollCount.Add(1) >= 2 {
					phase = "Succeeded"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"metadata": map[string]any{"name": "cubepod-probe-bld-123"},
					"status":   map[string]any{"phase": phase},
				})
			case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/cubepod-workloads/pods/cubepod-probe-bld-123":
				deletedProbePod.Store(true)
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer kubeSrv.Close()

		restKC := controller.NewRESTKubeClient(kubeSrv.URL, "", "cubepod-workloads", kubeSrv.Client())
		err := restKC.CreateSecret(ctx, controller.KubeSecret{
			APIVersion: "v1",
			Kind:       "Secret",
			Metadata:   v1alpha1.ObjectMeta{Name: "push-sec", Namespace: "cubepod-workloads"},
			Type:       "kubernetes.io/dockerconfigjson",
			StringData: map[string]string{
				".dockerconfigjson": fmt.Sprintf(`{"auths":{%q:{"username":"u","password":"p"}}}`, strings.TrimPrefix(regSrv.URL, "http://")),
			},
		})
		if err != nil {
			t.Fatalf("REST CreateSecret failed: %v", err)
		}

		bk := controller.NewBuildKitBuilder("tcp://127.0.0.1:1234",
			controller.WithBuildKitKubeClient(restKC, "cubepod-workloads"),
			controller.WithBuildKitPushSecret("push-sec"),
			controller.WithBuildKitHTTPClient(regSrv.Client()),
			controller.WithBuildKitInsecureRegistry(true),
		)

		regHost := strings.TrimPrefix(regSrv.URL, "http://")
		exists, err := bk.TagExists(ctx, regHost+"/org/my-tpl:bld_123")
		if err != nil || !exists {
			t.Fatalf("expected TagExists=true, got exists=%v err=%v", exists, err)
		}
		if tokenHits.Load() == 0 {
			t.Fatalf("expected Bearer token exchange during TagExists")
		}
		missing, err := bk.TagExists(ctx, regHost+"/org/my-tpl:missing")
		if err != nil || missing {
			t.Fatalf("expected TagExists=false on missing tag, got %v err=%v", missing, err)
		}

		uid := int64(1001)
		err = bk.Probe(ctx, regHost+"/org/my-tpl:bld_123", controller.ProbeOptions{
			BuildID:        "bld_123",
			Namespace:      "cubepod-workloads",
			StartCmd:       "python3 -m http.server 8080",
			ReadyCmd:       "true",
			RunAsUser:      &uid,
			Timeout:        10 * time.Second,
			PullSecretName: "push-sec",
		})
		if err != nil {
			t.Fatalf("BuildKit Probe failed: %v", err)
		}
		if !deletedProbePod.Load() {
			t.Fatalf("expected probe pod to be deleted in defer")
		}
		specMap := createdPodJSON["spec"].(map[string]any)
		if rc, hasRC := specMap["runtimeClassName"]; hasRC && rc != "" {
			t.Fatalf("expected probe pod without kata runtimeClassName, got %v", rc)
		}
		if specMap["restartPolicy"] != "Never" {
			t.Fatalf("expected probe pod restartPolicy=Never, got %v", specMap["restartPolicy"])
		}

		if delErr := restKC.DeleteSecret(ctx, "cubepod-workloads", "push-sec"); delErr != nil {
			t.Fatalf("REST DeleteSecret failed: %v", delErr)
		}
	})

	// 38. ContextStore(), PingBuilder, SweepTemplateContexts, AssignTemplateTags & DeleteTemplateTags
	t.Run("ContextStore PingBuilder SweepTemplateContexts AssignTemplateTags DeleteTemplateTags", func(t *testing.T) {
		ctx := context.Background()
		fakeBuilder := controller.NewFakeImageBuilder()
		ctxDir := t.TempDir()
		store := controller.NewMemoryContextStore(ctxDir)

		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
			controller.WithContextStore(store),
		)

		if r.ContextStore() != store {
			t.Fatalf("expected ContextStore() to return configured store")
		}
		if err := r.PingBuilder(ctx); err != nil {
			t.Fatalf("PingBuilder failed: %v", err)
		}
		if fakeBuilder.PingCalls != 1 {
			t.Fatalf("expected 1 Ping call, got %d", fakeBuilder.PingCalls)
		}
		fakeBuilder.PingErr = errors.New("builder down")
		if err := r.PingBuilder(ctx); err == nil {
			t.Fatalf("expected PingBuilder error")
		}

		// SweepTemplateContexts removes >24h tar contexts and >7d logs
		var tarBuf bytes.Buffer
		tw := tar.NewWriter(&tarBuf)
		_ = tw.WriteHeader(&tar.Header{Name: "f.txt", Mode: 0644, Size: 1})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		h := strings.Repeat("9", 64)
		if err := store.Store(h, &tarBuf); err != nil {
			t.Fatalf("store.Store failed: %v", err)
		}
		_ = store.AppendLog("bld_sweep", []string{"old log"})
		if err := r.SweepTemplateContexts(time.Now().Add(48 * time.Hour)); err != nil {
			t.Fatalf("SweepTemplateContexts failed: %v", err)
		}
		if store.IsPresent(h) {
			t.Fatalf("expected 48h sweep to expire context tar (>24h TTL)")
		}
		if entries, _ := store.ReadLogs("bld_sweep", 0, 10, ""); len(entries) != 1 {
			t.Fatalf("expected logs to survive 48h sweep (<7d TTL), got %d", len(entries))
		}
		if err := r.SweepTemplateContexts(time.Now().Add(8 * 24 * time.Hour)); err != nil {
			t.Fatalf("SweepTemplateContexts 8d failed: %v", err)
		}
		if entries, _ := store.ReadLogs("bld_sweep", 0, 10, ""); len(entries) != 0 {
			t.Fatalf("expected logs to expire at 8d (>7d TTL), got %d", len(entries))
		}

		// AssignTemplateTags & DeleteTemplateTags
		respA, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "tag-alpha"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate tag-alpha failed: %v", err)
		}
		respB, err := r.CreateTemplate(ctx, controller.CreateTemplateRequest{Name: "tag-beta"}, "")
		if err != nil {
			t.Fatalf("CreateTemplate tag-beta failed: %v", err)
		}

		updated, err := r.AssignTemplateTags(ctx, respA.TemplateID, []string{"v1", "tag-alpha:prod"})
		if err != nil {
			t.Fatalf("AssignTemplateTags failed: %v", err)
		}
		if !slices.Contains(updated.Spec.Names, "tag-alpha:v1") || !slices.Contains(updated.Spec.Names, "tag-alpha:prod") {
			t.Fatalf("expected tags in Spec.Names, got %v", updated.Spec.Names)
		}
		if _, ok := r.GetPodTemplate("tag-alpha:v1"); !ok {
			t.Fatalf("expected GetPodTemplate(\"tag-alpha:v1\") to resolve")
		}

		// Tag conflict with another template -> 409
		if _, err := r.AssignTemplateTags(ctx, respB.TemplateID, []string{"tag-alpha:v1"}); !errors.Is(err, controller.ErrTemplateConflict) {
			t.Fatalf("expected ErrTemplateConflict on claimed tag, got %v", err)
		}

		// Delete tags preserves primary name "tag-alpha"
		afterDel, err := r.DeleteTemplateTags(ctx, respA.TemplateID, []string{"v1", "tag-alpha"})
		if err != nil {
			t.Fatalf("DeleteTemplateTags failed: %v", err)
		}
		if slices.Contains(afterDel.Spec.Names, "tag-alpha:v1") {
			t.Fatalf("expected tag-alpha:v1 removed, got %v", afterDel.Spec.Names)
		}
		if !slices.Contains(afterDel.Spec.Names, "tag-alpha") || !slices.Contains(afterDel.Spec.Names, "tag-alpha:prod") {
			t.Fatalf("expected primary name tag-alpha and tag-alpha:prod preserved, got %v", afterDel.Spec.Names)
		}
		if _, ok := r.GetPodTemplate("tag-alpha:v1"); ok {
			t.Fatalf("expected tag-alpha:v1 to no longer resolve after DeleteTemplateTags")
		}
	})
}
