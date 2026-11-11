package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"text/template"
	"time"
)

// TestK8sBlockPublish provisions one volumeMode Block PVC on the cluster's
// storage class and checks that a Pod can read the published device.
// Longhorn V2 additionally requires the device, or its device-mapper slave,
// to be an NVMe namespace. Set CUBEPOD_STORAGE_CLASS to point at another class.
func TestK8sBlockPublish(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_BLOCK") != "1" && os.Getenv("CUBEPOD_K8S_LONGHORN_V2") != "1" {
		t.Skip("Skipping block publish E2E: set CUBEPOD_K8S_BLOCK=1")
	}

	cfg := loadClusterConfig(t)
	if cfg.StorageClass == "" {
		t.Fatal("set CUBEPOD_STORAGE_CLASS, or pass -storage-class on the cubepod-gateway Deployment")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("set CUBEPOD_SANDBOX_IMAGE to an image that contains bash, blockdev, and stat")
	}
	ns := cfg.Namespace
	image := cfg.SandboxImage
	scName := cfg.StorageClass
	name := fmt.Sprintf("block-%d", time.Now().Unix())

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	sc := kubectlJSON(ctx, t, "get", "sc", scName, "-o", "json")
	provisioner, _ := sc["provisioner"].(string)
	params, _ := sc["parameters"].(map[string]any)
	expectNVMe := provisioner == "driver.longhorn.io" && params["dataEngine"] == "v2"

	var manifestBuf bytes.Buffer
	tmpl := template.Must(template.New("probe").Parse(longhornProbeManifest))
	if err := tmpl.Execute(&manifestBuf, map[string]string{
		"Name":       name,
		"Namespace":  ns,
		"Class":      scName,
		"Image":      image,
		"PullSecret": os.Getenv("CUBEPOD_IMAGE_PULL_SECRET"),
		"NodeName":   os.Getenv("CUBEPOD_SCHEDULE_NODE"),
	}); err != nil {
		t.Fatalf("render probe manifest: %v", err)
	}
	manifest := manifestBuf.String()

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		_ = kubectlRun(cctx, "delete", "pod", name, "-n", ns, "--ignore-not-found", "--wait=false")
		_ = kubectlRun(cctx, "delete", "pvc", name, "-n", ns, "--ignore-not-found", "--wait=false")
	})

	if err := kubectlApply(ctx, manifest); err != nil {
		t.Fatalf("apply block probe: %v", err)
	}

	deadline := time.Now().Add(6 * time.Minute)
	var phase string
	for time.Now().Before(deadline) {
		pvc := kubectlJSON(ctx, t, "get", "pvc", name, "-n", ns, "-o", "json")
		status, _ := pvc["status"].(map[string]any)
		phase, _ = status["phase"].(string)
		if phase == "Bound" {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if phase != "Bound" {
		t.Fatalf("pvc %s phase = %s, want Bound", name, phase)
	}

	pvc := kubectlJSON(ctx, t, "get", "pvc", name, "-n", ns, "-o", "json")
	spec, _ := pvc["spec"].(map[string]any)
	if spec["volumeMode"] != "Block" || spec["storageClassName"] != scName {
		t.Fatalf("pvc spec = %#v", spec)
	}
	volumeName, _ := spec["volumeName"].(string)
	if volumeName == "" {
		t.Fatalf("bound pvc has empty volumeName")
	}
	pv := kubectlJSON(ctx, t, "get", "pv", volumeName, "-o", "json")
	pvSpec, _ := pv["spec"].(map[string]any)
	csi, _ := pvSpec["csi"].(map[string]any)
	if driver, _ := csi["driver"].(string); driver != provisioner {
		t.Fatalf("pv csi driver = %v, storage class provisioner = %s", csi["driver"], provisioner)
	}
	handle, _ := csi["volumeHandle"].(string)
	if expectNVMe {
		if handle == "" {
			t.Fatalf("pv volumeHandle is empty")
		}
		vol := kubectlJSON(ctx, t, "get", "volumes.longhorn.io", handle, "-n", cfg.LonghornNamespace, "-o", "json")
		volSpec, _ := vol["spec"].(map[string]any)
		if volSpec["dataEngine"] != "v2" {
			t.Fatalf("longhorn volume dataEngine = %v, want v2", volSpec["dataEngine"])
		}
	}

	podPhase := ""
	var logs string
	for time.Now().Before(deadline) {
		pod := kubectlJSON(ctx, t, "get", "pod", name, "-n", ns, "-o", "json")
		status, _ := pod["status"].(map[string]any)
		podPhase, _ = status["phase"].(string)
		if podPhase == "Succeeded" || podPhase == "Failed" {
			out, err := kubectlOutput(ctx, "logs", "pod/"+name, "-n", ns)
			if err == nil {
				logs = out
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	if podPhase != "Succeeded" {
		t.Fatalf("probe pod phase = %s, logs:\n%s", podPhase, logs)
	}
	if !strings.Contains(logs, "OK size=") {
		t.Fatalf("probe pod logs missing a readable block device:\n%s", logs)
	}
	if expectNVMe && !strings.Contains(logs, "nvme") {
		t.Fatalf("Longhorn V2 device is not NVMe-backed:\n%s", logs)
	}
	t.Logf("block device: %s", strings.TrimSpace(logs))
}

const longhornProbeManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: {{.Class}}
  volumeMode: Block
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  restartPolicy: Never
{{if .PullSecret}}
  imagePullSecrets:
    - name: {{.PullSecret}}
{{end}}
{{if .NodeName}}
  nodeSelector:
    kubernetes.io/hostname: {{.NodeName}}
{{end}}
  containers:
    - name: check
      image: {{.Image}}
      imagePullPolicy: IfNotPresent
      securityContext:
        privileged: true
      command: ["bash", "-c"]
      args:
        - |
          set -eu
          test -b /dev/cubepod-longhorn
          size=$(blockdev --getsize64 /dev/cubepod-longhorn)
          test "$size" -gt 0
          dd if=/dev/cubepod-longhorn of=/dev/null bs=4096 count=1 status=none
          mm=$(stat -c '%t %T' /dev/cubepod-longhorn)
          maj=$((16#${mm%% *}))
          min=$((16#${mm##* }))
          found=""
          for d in /sys/class/block/*; do
            if [ "$(cat "$d/dev" 2>/dev/null || true)" = "${maj}:${min}" ]; then
              found=$(basename "$d")
              break
            fi
          done
          test -n "$found"
          subsys=$(readlink -f "/sys/class/block/${found}/device/subsystem" || true)
          slave_subsys=""
          if [ -d "/sys/class/block/${found}/slaves" ]; then
            for s in /sys/class/block/${found}/slaves/*; do
              [ -e "$s" ] || continue
              ss=$(readlink -f "$s/device/subsystem" || true)
              slave_subsys="${slave_subsys} $(basename "$s"):${ss}"
            done
          fi
          echo "OK size=${size} block=${found} subsys=${subsys} slave_subsys=${slave_subsys}"
      volumeDevices:
        - name: data
          devicePath: /dev/cubepod-longhorn
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: {{.Name}}
`

func TestBlockProbeManifest(t *testing.T) {
	tmpl := template.Must(template.New("probe").Parse(longhornProbeManifest))
	var plain bytes.Buffer
	if err := tmpl.Execute(&plain, map[string]string{
		"Name": "block-1", "Namespace": "work", "Class": "local-block", "Image": "example.com/sandbox:1",
	}); err != nil {
		t.Fatal(err)
	}
	got := plain.String()
	if strings.Contains(got, "nodeSelector") || strings.Contains(got, "imagePullSecrets") {
		t.Fatalf("empty optional fields leaked into the manifest:\n%s", got)
	}
	if !strings.Contains(got, "storageClassName: local-block") || !strings.Contains(got, "image: example.com/sandbox:1") {
		t.Fatalf("manifest missing class or image:\n%s", got)
	}
	var pinned bytes.Buffer
	if err := tmpl.Execute(&pinned, map[string]string{
		"Name": "block-1", "Namespace": "work", "Class": "local-block", "Image": "example.com/sandbox:1",
		"PullSecret": "regcred", "NodeName": "worker-a",
	}); err != nil {
		t.Fatal(err)
	}
	got = pinned.String()
	if !strings.Contains(got, "name: regcred") || !strings.Contains(got, "kubernetes.io/hostname: worker-a") {
		t.Fatalf("optional fields missing:\n%s", got)
	}
}

func kubectlApply(ctx context.Context, manifest string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func kubectlRun(ctx context.Context, args ...string) error {
	_, err := kubectlOutput(ctx, args...)
	return err
}

func kubectlOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func kubectlJSON(ctx context.Context, t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := kubectlOutput(ctx, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("decode kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return obj
}
