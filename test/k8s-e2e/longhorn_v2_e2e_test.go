package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/controller"
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

type kataVirtioBlkGuestReport struct {
	GuestKernel      string
	HostKernel       string
	RuntimeClass     string
	ExecUIDGID       string
	WorkspaceUIDGID  string
	RootMount        string
	WorkspaceMount   string
	VirtioBlkDrivers string
	RootfsBlock      string
	WorkspaceBlock   string
	HypervisorHint   string
}

const kataVirtioBlkGuestProbeScript = `bash -c '
set -eu
guest_kernel=$(uname -r)
exec_uid_gid="$(id -u):$(id -g)"
ws_uid_gid=$(stat -c "%u:%g" /workspace)
root_mount=$(awk "\$2 == \"/\" {print \$1 \"|\" \$2 \"|\" \$3 \"|\" \$4}" /proc/mounts | tail -n1)
ws_mount=$(awk "\$2 == \"/workspace\" {print \$1 \"|\" \$2 \"|\" \$3 \"|\" \$4}" /proc/mounts | tail -n1)

virtio_devs=""
for drv_dir in /sys/bus/virtio/drivers/virtio_blk /sys/bus/virtio/drivers/virtio_scsi; do
  [ -d "$drv_dir" ] || continue
  drv_name=$(basename "$drv_dir")
  for v in "$drv_dir"/virtio*; do
    [ -e "$v" ] || continue
    vname=$(basename "$v")
    blk=""
    if [ -d "$v/block" ]; then
      for b in "$v/block/"*; do
        [ -e "$b" ] || continue
        blk=$(basename "$b")
      done
    fi
    if [ -n "$virtio_devs" ]; then virtio_devs="${virtio_devs},"; fi
    if [ -n "$blk" ]; then
      virtio_devs="${virtio_devs}${drv_name}:${vname}:${blk}"
    else
      virtio_devs="${virtio_devs}${drv_name}:${vname}"
    fi
  done
done

resolve_dev() {
  p="$1"
  if [ ! -b "$p" ]; then
    echo "missing"
    return
  fi
  mm=$(stat -c "%t %T" "$p")
  maj=$((16#${mm%% *}))
  min=$((16#${mm##* }))
  bname=""
  drv=""
  syspath=""
  for d in /sys/class/block/*; do
    if [ "$(cat "$d/dev" 2>/dev/null || true)" = "${maj}:${min}" ]; then
      bname=$(basename "$d")
      syspath=$(readlink -f "$d" 2>/dev/null || true)
      drv=$(basename "$(readlink -f "$d/device/driver" 2>/dev/null || true)" 2>/dev/null || true)
      break
    fi
  done
  echo "${bname:-unknown}(${maj}:${min},driver=${drv:-unknown},syspath=${syspath:-unknown})"
}

rootfs_blk=$(resolve_dev /dev/cubepod-rootfs)
ws_blk=$(resolve_dev /dev/cubepod-workspace)
dmi_product=$(cat /sys/devices/virtual/dmi/id/product_name 2>/dev/null || true)
if [ -n "$dmi_product" ]; then
  hypervisor_hint="$dmi_product"
elif grep -q -i "hypervisor" /proc/cpuinfo 2>/dev/null; then
  hypervisor_hint="cpuinfo:hypervisor"
else
  hypervisor_hint="virtio-vm"
fi

printf "GUEST_KERNEL=%s\n" "$guest_kernel"
printf "EXEC_UID_GID=%s\n" "$exec_uid_gid"
printf "WS_UID_GID=%s\n" "$ws_uid_gid"
printf "ROOT_MOUNT=%s\n" "$root_mount"
printf "WS_MOUNT=%s\n" "$ws_mount"
printf "VIRTIO_BLK_DRIVERS=%s\n" "$virtio_devs"
printf "ROOTFS_BLK=%s\n" "$rootfs_blk"
printf "WORKSPACE_BLK=%s\n" "$ws_blk"
printf "HYPERVISOR_HINT=%s\n" "$hypervisor_hint"
'`

func parseKataVirtioBlkProbeOutput(raw string) kataVirtioBlkGuestReport {
	var rep kataVirtioBlkGuestReport
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "GUEST_KERNEL":
			rep.GuestKernel = val
		case "EXEC_UID_GID":
			rep.ExecUIDGID = val
		case "WS_UID_GID":
			rep.WorkspaceUIDGID = val
		case "ROOT_MOUNT":
			rep.RootMount = val
		case "WS_MOUNT":
			rep.WorkspaceMount = val
		case "VIRTIO_BLK_DRIVERS":
			rep.VirtioBlkDrivers = val
		case "ROOTFS_BLK":
			rep.RootfsBlock = val
		case "WORKSPACE_BLK":
			rep.WorkspaceBlock = val
		case "HYPERVISOR_HINT":
			rep.HypervisorHint = val
		}
	}
	return rep
}

func isVirtioBackedBlockEntry(entry string) bool {
	if strings.Contains(entry, "driver=virtio_blk") && strings.HasPrefix(entry, "vd") {
		return true
	}
	// Kata hypervisors configured with virtio-scsi expose host Block PVCs as sdX
	// under a /sys/devices/.../virtioN/host.../block/sdX topology.
	if strings.Contains(entry, "driver=sd") && strings.Contains(entry, "/virtio") && strings.HasPrefix(entry, "sd") {
		return true
	}
	return false
}

func validateKataVirtioBlkGuestReport(rep kataVirtioBlkGuestReport) error {
	if strings.TrimSpace(rep.GuestKernel) == "" {
		return fmt.Errorf("empty guest kernel (uname -r)")
	}
	if rep.HostKernel != "" && rep.GuestKernel == rep.HostKernel && rep.VirtioBlkDrivers == "" {
		return fmt.Errorf("guest kernel %q matches host kernel without virtio isolation", rep.GuestKernel)
	}
	if rep.WorkspaceUIDGID != "1000:1000" {
		return fmt.Errorf("/workspace ownership = %q, want 1000:1000", rep.WorkspaceUIDGID)
	}
	if rep.ExecUIDGID != "1000:1000" {
		return fmt.Errorf("default command uid:gid = %q, want 1000:1000", rep.ExecUIDGID)
	}

	rootParts := strings.Split(rep.RootMount, "|")
	if len(rootParts) < 4 || rootParts[1] != "/" || rootParts[2] != "overlay" {
		return fmt.Errorf("root /proc/mounts entry = %q, want overlay on /", rep.RootMount)
	}
	if !strings.Contains(rootParts[3], "upperdir=") || !strings.Contains(rootParts[3], "workdir=") {
		return fmt.Errorf("overlay mount options missing upperdir/workdir: %q", rootParts[3])
	}

	wsParts := strings.Split(rep.WorkspaceMount, "|")
	if len(wsParts) < 4 || wsParts[1] != "/workspace" || wsParts[2] != "ext4" {
		return fmt.Errorf("/workspace /proc/mounts entry = %q, want ext4 on /workspace", rep.WorkspaceMount)
	}

	if rep.VirtioBlkDrivers == "" {
		return fmt.Errorf("/sys/bus/virtio/drivers/{virtio_blk,virtio_scsi} reported no bound virtio block devices")
	}

	if !isVirtioBackedBlockEntry(rep.RootfsBlock) {
		return fmt.Errorf("/dev/cubepod-rootfs backing = %q, want virtio-backed block device (virtio_blk vd* or virtio_scsi /virtio*/.../sd*)", rep.RootfsBlock)
	}
	if !isVirtioBackedBlockEntry(rep.WorkspaceBlock) {
		return fmt.Errorf("/dev/cubepod-workspace backing = %q, want virtio-backed block device (virtio_blk vd* or virtio_scsi /virtio*/.../sd*)", rep.WorkspaceBlock)
	}
	return nil
}

// TestK8sKataVirtioBlkGuestVerification creates one ephemeral sandbox on the live
// cluster via cfg.GatewayURL and verifies inside the Kata MicroVM guest:
//   - isolated Kata guest kernel (uname -r vs host node kernelVersion),
//   - virtio_blk driver bindings (/sys/bus/virtio/drivers/virtio_blk) backing both
//     /dev/cubepod-rootfs (OverlayFS upperdir) and /dev/cubepod-workspace (ext4),
//   - /workspace ext4 mount and 1000:1000 (cubeplex) ownership,
//   - clean ephemeral sandbox and PVC deletion on exit.
func TestK8sKataVirtioBlkGuestVerification(t *testing.T) {
	cfg := requireLiveE2EConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	kubeClient, err := controller.NewRESTKubeClientFromEnv()
	if err != nil {
		t.Fatalf("NewRESTKubeClientFromEnv: %v", err)
	}

	templateID := cfg.SandboxImage
	if overridePodd := discoverCachedPoddInjectorImage(ctx, cfg); overridePodd != "" {
		tplName := fmt.Sprintf("kata-virtioblk-tpl-%d", time.Now().Unix())
		rtClass := discoverControllerFlag(ctx, cfg, "-runtime-class")
		if rtClass == "" {
			rtClass = "kata-clh"
		}
		ephemeralTpl := v1alpha1.PodTemplate{
			APIVersion: "cubepod.io/v1alpha1",
			Kind:       "PodTemplate",
			Metadata: v1alpha1.ObjectMeta{
				Name:      tplName,
				Namespace: cfg.Namespace,
			},
			Spec: v1alpha1.PodTemplateSpec{
				RuntimeClassName: rtClass,
				Image:            cfg.SandboxImage,
				PoddImage:        overridePodd,
				Storage: v1alpha1.StorageSpec{
					StorageClassName: cfg.StorageClass,
					RootfsSize:       "10Gi",
					WorkspaceSize:    "20Gi",
				},
			},
		}
		if tplErr := kubeClient.CreatePodTemplate(ctx, ephemeralTpl); tplErr == nil {
			t.Logf("[Kata-VirtioBlk] Registered ephemeral PodTemplate %s with cached poddImage=%s", tplName, overridePodd)
			defer func() {
				cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer ccancel()
				_ = kubeClient.DeletePodTemplate(cctx, cfg.Namespace, tplName)
			}()
			// Allow gateway background SyncPodBoxesOnce (5s interval) to pick up the PodTemplate CR.
			time.Sleep(6 * time.Second)
			templateID = tplName
		}
	}

	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: templateID,
		Metadata: map[string]string{
			"env":   "k8s-e2e",
			"suite": "kata-virtio-blk-guest",
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	sandboxID := sb.SandboxID
	t.Logf("[Kata-VirtioBlk] Created ephemeral sandbox: id=%s state=%s", sandboxID, sb.State)

	deletedCleanly := false
	defer func() {
		if deletedCleanly {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if delErr := c.DeleteSandboxWithOptions(cleanupCtx, sandboxID, true); delErr != nil {
			t.Logf("[Kata-VirtioBlk] defer cleanup DeleteSandboxWithOptions(%s): %v", sandboxID, delErr)
		}
	}()

	podObj, err := kubeClient.GetPod(ctx, cfg.Namespace, sandboxID)
	if err != nil {
		t.Fatalf("GetPod %s/%s: %v", cfg.Namespace, sandboxID, err)
	}
	specMap, _ := podObj["spec"].(map[string]any)
	runtimeClass, _ := specMap["runtimeClassName"].(string)
	nodeName, _ := specMap["nodeName"].(string)
	if !strings.HasPrefix(runtimeClass, "kata") {
		t.Fatalf("expected Kata runtimeClassName on Pod %s, got %q", sandboxID, runtimeClass)
	}

	hostKernel := ""
	if nodeName != "" {
		nodeObj := kubectlJSON(ctx, t, "get", "node", nodeName, "-o", "json")
		if nStatus, ok := nodeObj["status"].(map[string]any); ok {
			if nInfo, ok := nStatus["nodeInfo"].(map[string]any); ok {
				hostKernel, _ = nInfo["kernelVersion"].(string)
			}
		}
	}

	for _, pvcSuffix := range []string{"-rootfs-pvc", "-workspace-pvc"} {
		pvcName := sandboxID + pvcSuffix
		pvcObj, pvcErr := kubeClient.GetPVC(ctx, cfg.Namespace, pvcName)
		if pvcErr != nil {
			t.Fatalf("GetPVC %s: %v", pvcName, pvcErr)
		}
		pvcSpec, _ := pvcObj["spec"].(map[string]any)
		if pvcSpec["volumeMode"] != "Block" {
			t.Fatalf("PVC %s volumeMode = %v, want Block", pvcName, pvcSpec["volumeMode"])
		}
		if cfg.StorageClass != "" && pvcSpec["storageClassName"] != cfg.StorageClass {
			t.Fatalf("PVC %s storageClassName = %v, want %s", pvcName, pvcSpec["storageClassName"], cfg.StorageClass)
		}
	}

	// On cold boot, Pod phase becomes Running when PID 1 (podd-init) starts inside Kata;
	// podd (:49983) begins listening right after podd-init finishes formatting both block PVCs and pivot_root.
	var initStateBytes []byte
	readyDeadline := time.Now().Add(60 * time.Second)
	for {
		initStateBytes, err = c.ReadFile(ctx, sandboxID, "/run/cubepod/init-state.json")
		if err == nil {
			break
		}
		if time.Now().After(readyDeadline) || (!strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "connection refused")) {
			t.Fatalf("ReadFile /run/cubepod/init-state.json: %v", err)
		}
		time.Sleep(1 * time.Second)
	}
	var initState InitState
	if err := json.Unmarshal(initStateBytes, &initState); err != nil {
		t.Fatalf("Unmarshal init-state.json: %v (raw=%s)", err, string(initStateBytes))
	}
	if !initState.Pivoted {
		t.Fatalf("expected initState.Pivoted=true, got %+v", initState)
	}

	res, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
		Command: kataVirtioBlkGuestProbeScript,
	})
	if err != nil {
		t.Fatalf("in-guest virtio-blk probe command failed: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("in-guest virtio-blk probe exitCode=%v output:\n%s", res.ExitCode, res.Output)
	}

	rep := parseKataVirtioBlkProbeOutput(res.Output)
	rep.HostKernel = hostKernel
	rep.RuntimeClass = runtimeClass

	if err := validateKataVirtioBlkGuestReport(rep); err != nil {
		t.Fatalf("Kata virtio-blk guest verification failed: %v\nraw output:\n%s", err, res.Output)
	}

	t.Logf("[Kata-VirtioBlk] Verified isolated VM kernel: runtimeClass=%s node=%s hostKernel=%s guestKernel=%s hypervisor=%s",
		rep.RuntimeClass, nodeName, rep.HostKernel, rep.GuestKernel, rep.HypervisorHint)
	t.Logf("[Kata-VirtioBlk] Verified virtio-blk & mounts: virtio_blk=[%s] rootfs=%s workspace=%s rootMount=%s wsMount=%s uid_gid=%s initDurationMs=%d",
		rep.VirtioBlkDrivers, rep.RootfsBlock, rep.WorkspaceBlock, rep.RootMount, rep.WorkspaceMount, rep.WorkspaceUIDGID, initState.DurationMs)

	// Explicitly delete ephemeral sandbox + block PVCs and confirm cleanup.
	if err := c.DeleteSandboxWithOptions(ctx, sandboxID, true); err != nil {
		t.Fatalf("DeleteSandboxWithOptions(%s, true): %v", sandboxID, err)
	}
	deletedCleanly = true

	for i := 0; i < 45; i++ {
		_, podErr := kubeClient.GetPod(ctx, cfg.Namespace, sandboxID)
		_, wsErr := kubeClient.GetPVC(ctx, cfg.Namespace, sandboxID+"-workspace-pvc")
		if podErr != nil && wsErr != nil {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("[Kata-VirtioBlk] Ephemeral sandbox %s and dual Block PVCs cleaned up", sandboxID)
}

// discoverCachedPoddInjectorImage checks CUBEPOD_PODD_IMAGE or inspects an already-Running
// sandbox Pod in cfg.Namespace when the gateway's configured -podd-image differs from a
// known-good cached podd-installer image on the node.
func discoverCachedPoddInjectorImage(ctx context.Context, cfg clusterConfig) string {
	if envImg := strings.TrimSpace(os.Getenv("CUBEPOD_PODD_IMAGE")); envImg != "" {
		return envImg
	}
	configuredPodd := discoverControllerFlag(ctx, cfg, "-podd-image")
	raw, err := kubectlOutput(ctx, "get", "pods", "-n", cfg.Namespace, "-o", "json")
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Spec struct {
				InitContainers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return ""
	}
	for _, pod := range list.Items {
		if pod.Status.Phase != "Running" {
			continue
		}
		for _, ic := range pod.Spec.InitContainers {
			if ic.Name == "podd-installer" && ic.Image != "" && ic.Image != configuredPodd {
				return ic.Image
			}
		}
	}
	return ""
}

func TestKataVirtioBlkGuestReportValidation(t *testing.T) {
	sample := strings.Join([]string{
		"GUEST_KERNEL=6.12.47",
		"EXEC_UID_GID=1000:1000",
		"WS_UID_GID=1000:1000",
		"ROOT_MOUNT=overlay|/|overlay|rw,relatime,lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work",
		"WS_MOUNT=/dev/cubepod-workspace|/workspace|ext4|rw,relatime",
		"VIRTIO_BLK_DRIVERS=virtio_blk:virtio1:vda,virtio_blk:virtio2:vdb",
		"ROOTFS_BLK=vda(253:0,driver=virtio_blk,syspath=/sys/devices/pci0000:00/0000:00:04.0/virtio1/block/vda)",
		"WORKSPACE_BLK=vdb(253:16,driver=virtio_blk,syspath=/sys/devices/pci0000:00/0000:00:05.0/virtio2/block/vdb)",
		"HYPERVISOR_HINT=Cloud Hypervisor",
	}, "\n")
	rep := parseKataVirtioBlkProbeOutput(sample)
	rep.HostKernel = "7.0.0-38-generic"
	rep.RuntimeClass = "kata-clh"
	if err := validateKataVirtioBlkGuestReport(rep); err != nil {
		t.Fatalf("expected valid virtio_blk guest report, got error: %v", err)
	}

	scsiSample := strings.Join([]string{
		"GUEST_KERNEL=6.18.56",
		"EXEC_UID_GID=1000:1000",
		"WS_UID_GID=1000:1000",
		"ROOT_MOUNT=overlay|/|overlay|rw,relatime,lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work,uuid=on",
		"WS_MOUNT=/dev/cubepod-workspace|/workspace|ext4|rw,noatime",
		"VIRTIO_BLK_DRIVERS=virtio_scsi:virtio1",
		"ROOTFS_BLK=sda(8:0,driver=sd,syspath=/sys/devices/pci0000:00/0000:00:02.0/virtio1/host0/target0:0:0/0:0:0:0/block/sda)",
		"WORKSPACE_BLK=sdb(8:16,driver=sd,syspath=/sys/devices/pci0000:00/0000:00:02.0/virtio1/host0/target0:0:1/0:0:1:0/block/sdb)",
		"HYPERVISOR_HINT=cpuinfo:hypervisor",
	}, "\n")
	scsiRep := parseKataVirtioBlkProbeOutput(scsiSample)
	scsiRep.HostKernel = "7.0.0-38-generic"
	scsiRep.RuntimeClass = "kata-clh"
	if err := validateKataVirtioBlkGuestReport(scsiRep); err != nil {
		t.Fatalf("expected valid virtio_scsi guest report, got error: %v", err)
	}

	badOwner := rep
	badOwner.WorkspaceUIDGID = "0:0"
	if err := validateKataVirtioBlkGuestReport(badOwner); err == nil {
		t.Fatal("expected non-1000:1000 /workspace ownership to fail validation")
	}

	badDriver := rep
	badDriver.WorkspaceBlock = "dm-0(252:0,driver=unknown,syspath=/sys/devices/virtual/block/dm-0)"
	if err := validateKataVirtioBlkGuestReport(badDriver); err == nil {
		t.Fatal("expected host dm-0 non-virtio workspace block device to fail validation")
	}
}

func TestLonghornV2VolumeSnapshotClassManifest(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "deploy", "k8s", "longhorn-v2", "volumesnapshotclass.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	text := string(raw)
	for _, want := range []string{
		"apiVersion: snapshot.storage.k8s.io/v1",
		"kind: VolumeSnapshotClass",
		"name: longhorn-v2-snapclass",
		"driver: driver.longhorn.io",
		"deletionPolicy: Delete",
		"type: snap",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %q:\n%s", manifestPath, want, text)
		}
	}
}
