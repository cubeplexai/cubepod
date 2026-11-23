package controller

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
)

const (
	workspacePVCSuffix     = "-workspace-pvc"
	maxWorkspacePVCNameLen = 63
	maxWorkspacePrefixLen  = maxWorkspacePVCNameLen - len(workspacePVCSuffix) // 49
	workspaceHashHexLen    = 8
)

// SanitizeVolumeKey normalizes a user-supplied volumeKey into a valid DNS-1123
// label prefix so appending "-workspace-pvc" stays within 63 characters.
func SanitizeVolumeKey(volumeKey string) string {
	lower := strings.ToLower(volumeKey)
	var b strings.Builder
	b.Grow(len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	prefix := strings.Trim(b.String(), "-")

	sum := sha256.Sum256([]byte(volumeKey))
	hashSuffix := fmt.Sprintf("%x", sum[:workspaceHashHexLen/2])

	if prefix == "" {
		return "vol-" + hashSuffix
	}
	if len(prefix) > maxWorkspacePrefixLen {
		keepLen := maxWorkspacePrefixLen - 1 - workspaceHashHexLen // 40
		truncated := strings.TrimRight(prefix[:keepLen], "-")
		if truncated == "" {
			return "vol-" + hashSuffix
		}
		return truncated + "-" + hashSuffix
	}
	return prefix
}

func isLabelAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isLabelAllowedChar(b byte) bool {
	return isLabelAlnum(b) || b == '-' || b == '_' || b == '.'
}

// SanitizeLabelValue normalizes v to a valid Kubernetes label value (max 63 chars,
// [A-Za-z0-9-_.] interior, alphanumeric start and end).
func SanitizeLabelValue(v string) string {
	if v == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		if isLabelAllowedChar(c) {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	start := 0
	for start < len(s) && !isLabelAlnum(s[start]) {
		start++
	}
	end := len(s)
	for end > start && !isLabelAlnum(s[end-1]) {
		end--
	}
	s = s[start:end]
	if len(s) > 63 {
		s = s[:63]
		for len(s) > 0 && !isLabelAlnum(s[len(s)-1]) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// WorkspacePVCName returns the workspace PVC name for a PodBox.
// Precedence:
// 1. Managed block volume mount at /workspace (spec.VolumeMounts with type: "block")
// 2. Legacy deterministic spec.VolumeKey
// 3. Ephemeral per-sandbox <podBoxName>-workspace-pvc
func WorkspacePVCName(podBoxName string, spec v1alpha1.PodBoxSpec) string {
	if m := spec.WorkspaceBlockMount(); m != nil && strings.TrimSpace(m.PVCName) != "" {
		return strings.TrimSpace(m.PVCName)
	}
	if spec.VolumeKey != "" {
		return SanitizeVolumeKey(spec.VolumeKey) + workspacePVCSuffix
	}
	return fmt.Sprintf("%s-workspace-pvc", podBoxName)
}

// BlockPVCManifest represents the Kubernetes PersistentVolumeClaim specification
// for a raw block volume (volumeMode: Block).
type BlockPVCManifest struct {
	Name               string            `json:"name"`
	Namespace          string            `json:"namespace,omitempty"`
	ResourceVersion    string            `json:"resourceVersion,omitempty"`
	StorageClassName   string            `json:"storageClassName"`
	VolumeMode         string            `json:"volumeMode"`
	Size               string            `json:"size"`
	AccessModes        []string          `json:"accessModes"`
	DataSourcePVC      string            `json:"dataSourcePVC,omitempty"`
	DataSourceSnapshot string            `json:"dataSourceSnapshot,omitempty"`
	DataSourceKind     string            `json:"dataSourceKind,omitempty"`
	DataSourceAPIGroup string            `json:"dataSourceAPIGroup,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	Annotations        map[string]string `json:"annotations,omitempty"`
}

// BlockPVCOptions configures PVC synthesis for BuildBlockPVCsWithOptions.
type BlockPVCOptions struct {
	VolumeMode            string
	ParentRootfsPVC       string
	ParentWorkspacePVC    string
	RootfsSnapshotName    string
	WorkspaceSnapshotName string
}

// VolumeMount represents a file system volume mount inside a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// VolumeDevice represents a raw block device mapped into a container.
type VolumeDevice struct {
	Name       string `json:"name"`
	DevicePath string `json:"devicePath"`
}

// EnvVar represents an environment variable name-value pair.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// HTTPGetAction describes an HTTP GET probe target.
type HTTPGetAction struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

// ProbeSpec describes a container liveness or readiness probe.
type ProbeSpec struct {
	HTTPGet             *HTTPGetAction `json:"httpGet,omitempty"`
	InitialDelaySeconds int32          `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32          `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int32          `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    int32          `json:"successThreshold,omitempty"`
	FailureThreshold    int32          `json:"failureThreshold,omitempty"`
}

// CapabilitiesSpec specifies POSIX capabilities to add or drop for a container.
type CapabilitiesSpec struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

// SecurityContextSpec holds container-level security attributes.
type SecurityContextSpec struct {
	Privileged   *bool             `json:"privileged,omitempty"`
	RunAsUser    *int64            `json:"runAsUser,omitempty"`
	RunAsGroup   *int64            `json:"runAsGroup,omitempty"`
	Capabilities *CapabilitiesSpec `json:"capabilities,omitempty"`
}

// ContainerManifest represents a container specification within a Pod.
type ContainerManifest struct {
	Name            string                        `json:"name"`
	Image           string                        `json:"image"`
	ImagePullPolicy string                        `json:"imagePullPolicy,omitempty"`
	Command         []string                      `json:"command,omitempty"`
	Args            []string                      `json:"args,omitempty"`
	VolumeMounts    []VolumeMount                 `json:"volumeMounts,omitempty"`
	VolumeDevices   []VolumeDevice                `json:"volumeDevices,omitempty"`
	Resources       v1alpha1.ResourceRequirements `json:"resources,omitempty"`
	Env             []EnvVar                      `json:"env,omitempty"`
	Privileged      bool                          `json:"privileged,omitempty"`
	RunAsUser       *int64                        `json:"runAsUser,omitempty"`
	RunAsGroup      *int64                        `json:"runAsGroup,omitempty"`
	SecurityContext *SecurityContextSpec          `json:"securityContext,omitempty"`
	ReadinessProbe  *ProbeSpec                    `json:"readinessProbe,omitempty"`
}

// VolumeManifest represents a Pod volume definition.
type VolumeManifest struct {
	Name                  string `json:"name"`
	EmptyDir              bool   `json:"emptyDir,omitempty"`
	PersistentVolumeClaim string `json:"persistentVolumeClaim,omitempty"`
	SecretName            string `json:"secretName,omitempty"`
}

// PodManifest represents the Kubernetes Pod specification for Kata MicroVMs.
type PodManifest struct {
	Name             string              `json:"name"`
	Namespace        string              `json:"namespace,omitempty"`
	RuntimeClassName string              `json:"runtimeClassName"`
	RestartPolicy    string              `json:"restartPolicy,omitempty"`
	Labels           map[string]string   `json:"labels,omitempty"`
	InitContainers   []ContainerManifest `json:"initContainers"`
	Containers       []ContainerManifest `json:"containers"`
	Volumes          []VolumeManifest    `json:"volumes"`
	ImagePullSecrets []string            `json:"imagePullSecrets,omitempty"`
	PodIP            string              `json:"podIP,omitempty"`
	NotReady         bool                `json:"notReady,omitempty"`
}

// BuildBlockPVCs generates volumeMode: Block or Filesystem PVC specifications for RootFS Upper and Workspace.
// When ForkFrom is specified in spec, DataSourcePVC is populated pointing to the parent sandbox's PVCs (in Block mode).
// When volumeMode is "Filesystem", DataSourcePVC cloning is skipped so NFS provisioners do not reject the PVC.
func BuildBlockPVCs(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, volumeMode ...string) (rootfsPVC, workspacePVC BlockPVCManifest) {
	opts := BlockPVCOptions{}
	if len(volumeMode) > 0 {
		opts.VolumeMode = volumeMode[0]
	}
	if len(volumeMode) > 1 {
		opts.ParentRootfsPVC = volumeMode[1]
	}
	if len(volumeMode) > 2 {
		opts.ParentWorkspacePVC = volumeMode[2]
	}
	return BuildBlockPVCsWithOptions(boxName, tpl, spec, opts)
}

// BuildBlockPVCsWithOptions generates RootFS Upper and Workspace PVC manifests with explicit clone or snapshot options.
func BuildBlockPVCsWithOptions(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, opts BlockPVCOptions) (rootfsPVC, workspacePVC BlockPVCManifest) {
	vm := opts.VolumeMode
	if vm == "" {
		vm = "Block"
	}

	storageClass := tpl.Storage.StorageClassName
	if storageClass == "" {
		storageClass = "standard"
	}

	rootfsSize := tpl.Storage.RootfsSize
	if rootfsSize == "" {
		rootfsSize = "10Gi"
	}

	workspaceSize := tpl.Storage.WorkspaceSize
	if workspaceSize == "" {
		workspaceSize = "20Gi"
	}

	rootfsName := fmt.Sprintf("%s-rootfs-pvc", boxName)
	if spec.RootfsGeneration > 1 {
		rootfsName = fmt.Sprintf("%s-rootfs-gen%d-pvc", boxName, spec.RootfsGeneration)
	}
	workspaceName := WorkspacePVCName(boxName, spec)

	pvcVolMode := "Block"
	if strings.EqualFold(vm, "Filesystem") {
		pvcVolMode = "Filesystem"
	}

	rootfsPVC = BlockPVCManifest{
		Name:             rootfsName,
		StorageClassName: storageClass,
		VolumeMode:       pvcVolMode,
		Size:             rootfsSize,
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"app.kubernetes.io/name":   "cubepod",
			"pod.cubeplex.ai/box":      boxName,
			"pod.cubeplex.ai/vol-type": "rootfs",
		},
	}

	workspacePVC = BlockPVCManifest{
		Name:             workspaceName,
		StorageClassName: storageClass,
		VolumeMode:       pvcVolMode,
		Size:             workspaceSize,
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"app.kubernetes.io/name":   "cubepod",
			"pod.cubeplex.ai/box":      boxName,
			"pod.cubeplex.ai/vol-type": "workspace",
		},
	}

	if !strings.EqualFold(vm, "Filesystem") && (opts.RootfsSnapshotName != "" || opts.WorkspaceSnapshotName != "") {
		if opts.RootfsSnapshotName != "" {
			rootfsPVC.DataSourceSnapshot = opts.RootfsSnapshotName
			rootfsPVC.DataSourceKind = "VolumeSnapshot"
			rootfsPVC.DataSourceAPIGroup = "snapshot.storage.k8s.io"
		}
		if opts.WorkspaceSnapshotName != "" {
			workspacePVC.DataSourceSnapshot = opts.WorkspaceSnapshotName
			workspacePVC.DataSourceKind = "VolumeSnapshot"
			workspacePVC.DataSourceAPIGroup = "snapshot.storage.k8s.io"
		}
	} else if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
		if !strings.EqualFold(vm, "Filesystem") {
			rootfsSource := fmt.Sprintf("%s-rootfs-pvc", spec.ForkFrom.PodBoxName)
			if opts.ParentRootfsPVC != "" {
				rootfsSource = opts.ParentRootfsPVC
			}
			wsSource := fmt.Sprintf("%s-workspace-pvc", spec.ForkFrom.PodBoxName)
			if opts.ParentWorkspacePVC != "" {
				wsSource = opts.ParentWorkspacePVC
			}
			rootfsPVC.DataSourcePVC = rootfsSource
			workspacePVC.DataSourcePVC = wsSource
		}
	}

	return rootfsPVC, workspacePVC
}

// VolumePVCName returns the filesystem claim for a shared volume: vol-<id>.
// It is never the workspace PVC.
func VolumePVCName(volumeID string) (string, bool) {
	if len(volumeID) != 32 {
		return "", false
	}
	for i := 0; i < len(volumeID); i++ {
		c := volumeID[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return "vol-" + volumeID, true
}

func validVolumeSubPath(subPath string) bool {
	if subPath == "" || strings.HasPrefix(subPath, "/") || strings.Contains(subPath, "\\") {
		return false
	}
	for _, seg := range strings.Split(subPath, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ValidateVolumeMounts verifies that all volume mounts
// have a valid volume ID, matching PVC name (or a non-empty shared PVC name
// when subPath is set, or a non-empty block PVC name for type: "block"),
// valid container path, and no duplicate mount paths.
func ValidateVolumeMounts(mounts []v1alpha1.VolumeMount) error {
	seenPath := map[string]struct{}{}
	blockCount := 0
	for _, m := range mounts {
		pvc, ok := VolumePVCName(m.VolumeID)
		if !ok {
			return fmt.Errorf("invalid volumeID %q", m.VolumeID)
		}
		if m.IsBlock() {
			if strings.TrimSpace(m.PVCName) == "" {
				return fmt.Errorf("missing pvcName for block volume %s", m.VolumeID)
			}
			if m.Path != "/workspace" {
				return fmt.Errorf("block volume mount path must be /workspace, got %q", m.Path)
			}
			blockCount++
			if blockCount > 1 {
				return fmt.Errorf("at most one block volume mount is allowed per sandbox")
			}
		} else {
			if m.SubPath != "" {
				if !validVolumeSubPath(m.SubPath) {
					return fmt.Errorf("invalid volume subPath %q", m.SubPath)
				}
				if strings.TrimSpace(m.PVCName) == "" {
					return fmt.Errorf("missing pvcName for volume %s with subPath", m.VolumeID)
				}
			} else if m.PVCName != "" && m.PVCName != pvc {
				return fmt.Errorf("mismatched pvcName %q for volume %s", m.PVCName, m.VolumeID)
			}
			if !v1alpha1.ValidVolumeContainerPath(m.Path) {
				return fmt.Errorf("invalid volume mount path %q", m.Path)
			}
		}
		if _, dup := seenPath[m.Path]; dup {
			return fmt.Errorf("duplicate volume mount path %q", m.Path)
		}
		seenPath[m.Path] = struct{}{}
	}
	return nil
}

type volumeMountEnvEntry struct {
	VolumeID    string `json:"volume_id"`
	StagingPath string `json:"staging_path,omitempty"`
	TargetPath  string `json:"target_path"`
	Transport   string `json:"transport,omitempty"`
	NFSTarget   string `json:"nfs_target,omitempty"`
}

// VolumeNFSOptions describes the Firecracker NFS transport injected into a Pod.
type VolumeNFSOptions struct {
	ClusterIP string
	Port      int
	LeaseID   string
}

// Ready reports whether the NFS target can be written into CUBEPOD_VOLUME_MOUNTS.
func (o VolumeNFSOptions) Ready() bool {
	return strings.TrimSpace(o.LeaseID) != "" && strings.TrimSpace(o.ClusterIP) != ""
}

// applyVolumeMounts stages each shared volume at
// /run/cubepod/volumes/<volumeID>, deduplicates shared PVCs in pod.Volumes,
// and passes the staging->target mapping to podd-init via CUBEPOD_VOLUME_MOUNTS.
// When mountDirect is true (Filesystem volume mode with an empty runtime class),
// the kubelet also mounts each volume at its final path so standard containers
// do not need CAP_SYS_ADMIN or a pivot_root to expose the volume.
// When nfs.Ready() is true, official volumes are not attached as Kata VolumeMounts
// (that path is a one-way copy on Firecracker) and CUBEPOD_VOLUME_MOUNTS entries
// use transport "nfs".
func applyVolumeMounts(volumes []VolumeManifest, main ContainerManifest, mounts []v1alpha1.VolumeMount, mountDirect bool, nfs VolumeNFSOptions) ([]VolumeManifest, ContainerManifest) {
	if len(mounts) == 0 {
		return volumes, main
	}
	if err := ValidateVolumeMounts(mounts); err != nil {
		return volumes, main
	}
	if nfs.Ready() {
		port := nfs.Port
		if port <= 0 {
			port = volumenfs.DefaultNFSPort
		}
		envEntries := make([]volumeMountEnvEntry, 0, len(mounts))
		for _, m := range mounts {
			envEntries = append(envEntries, volumeMountEnvEntry{
				VolumeID:   m.VolumeID,
				TargetPath: m.Path,
				Transport:  "nfs",
				NFSTarget:  volumenfs.NFSTarget(nfs.ClusterIP, nfs.LeaseID, m.VolumeID),
			})
		}
		if raw, err := json.Marshal(envEntries); err == nil {
			main.Env = append(main.Env,
				EnvVar{Name: "CUBEPOD_VOLUME_MOUNTS", Value: string(raw)},
				EnvVar{Name: "CUBEPOD_VOLUME_NFS_PORT", Value: strconv.Itoa(port)},
			)
		}
		return volumes, main
	}
	seenPVC := map[string]struct{}{}
	envEntries := make([]volumeMountEnvEntry, 0, len(mounts))
	for _, m := range mounts {
		pvc := m.PVCName
		if pvc == "" {
			pvc, _ = VolumePVCName(m.VolumeID)
		}
		if _, ok := seenPVC[pvc]; !ok {
			seenPVC[pvc] = struct{}{}
			volumes = append(volumes, VolumeManifest{
				Name:                  pvc,
				PersistentVolumeClaim: pvc,
			})
		}
		stagingPath := "/run/cubepod/volumes/" + m.VolumeID
		main.VolumeMounts = append(main.VolumeMounts, VolumeMount{
			Name:      pvc,
			MountPath: stagingPath,
			SubPath:   m.SubPath,
		})
		if mountDirect {
			main.VolumeMounts = append(main.VolumeMounts, VolumeMount{
				Name:      pvc,
				MountPath: m.Path,
				SubPath:   m.SubPath,
			})
		}
		envEntries = append(envEntries, volumeMountEnvEntry{
			VolumeID:    m.VolumeID,
			StagingPath: stagingPath,
			TargetPath:  m.Path,
		})
	}
	if raw, err := json.Marshal(envEntries); err == nil {
		main.Env = append(main.Env, EnvVar{
			Name:  "CUBEPOD_VOLUME_MOUNTS",
			Value: string(raw),
		})
	}
	return volumes, main
}

// PodBuildOptions configures optional pod synthesis settings for BuildPodManifest.
type PodBuildOptions struct {
	VolumeMode        string
	ImagePullSecrets  []string
	TransparentEgress bool
	VolumeNFS         VolumeNFSOptions
	IsFirecracker     bool
}

// BuildKataPod synthesizes the complete Kubernetes Pod manifest for running a Kata or container sandbox.
// In volumeMode: Block, it maps raw block devices /dev/cubepod-rootfs and /dev/cubepod-workspace.
// In volumeMode: Filesystem, it mounts rootfs-pvc at /mnt/cubepod/rootfs and workspace-pvc at /workspace
// via ContainerManifest.VolumeMounts instead of VolumeDevices, and adds CUBEPOD_SKIP_PIVOT=1 when RuntimeClassName == "".
func BuildKataPod(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, status v1alpha1.PodBoxStatus, volumeMode ...string) PodManifest {
	opts := PodBuildOptions{
		TransparentEgress: true,
	}
	if len(volumeMode) > 0 {
		opts.VolumeMode = volumeMode[0]
	}
	if len(volumeMode) > 1 {
		opts.ImagePullSecrets = volumeMode[1:]
	}
	return BuildPodManifest(boxName, tpl, spec, status, opts)
}

// BuildPodManifest synthesizes the complete Kubernetes Pod manifest for running a Kata or container sandbox
// with explicit PodBuildOptions (such as TransparentEgress interception).
func BuildPodManifest(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, status v1alpha1.PodBoxStatus, opts PodBuildOptions) PodManifest {
	vm := opts.VolumeMode
	if vm == "" {
		vm = "Block"
	}

	podName := status.PodName
	if podName == "" {
		podName = boxName
	}

	rtClass := tpl.RuntimeClassName
	if strings.EqualFold(rtClass, "none") || rtClass == "-" {
		rtClass = ""
	} else if rtClass == "" && !strings.EqualFold(vm, "Filesystem") {
		rtClass = "kata-qemu"
	}

	poddImg := tpl.PoddImage
	if poddImg == "" {
		poddImg = "cubepod-podd:latest"
	}

	mainImg := tpl.Image
	if mainImg == "" {
		mainImg = "ubuntu:24.04"
	}

	rootfsPVC := status.RootfsPVCName
	if rootfsPVC == "" {
		if spec.RootfsGeneration > 1 {
			rootfsPVC = fmt.Sprintf("%s-rootfs-gen%d-pvc", boxName, spec.RootfsGeneration)
		} else {
			rootfsPVC = fmt.Sprintf("%s-rootfs-pvc", boxName)
		}
	}

	workspacePVC := status.WorkspacePVCName
	if workspacePVC == "" {
		workspacePVC = WorkspacePVCName(boxName, spec)
	}

	volumes := []VolumeManifest{
		{
			Name:     "cubepod-bin",
			EmptyDir: true,
		},
		{
			Name:                  "rootfs-volume",
			PersistentVolumeClaim: rootfsPVC,
		},
		{
			Name:                  "workspace-volume",
			PersistentVolumeClaim: workspacePVC,
		},
	}

	initContainers := []ContainerManifest{
		{
			Name:            "podd-installer",
			Image:           poddImg,
			ImagePullPolicy: "IfNotPresent",
			Command: []string{
				"/bin/sh",
				"-c",
				"cp -f /opt/cubepod/dist/podd-init /opt/cubepod/dist/podd /opt/cubepod/bin/ 2>/dev/null || cp -f /podd-init /podd /opt/cubepod/bin/ 2>/dev/null || cp -f /usr/local/bin/podd* /opt/cubepod/bin/ 2>/dev/null || true; chmod 0755 /opt/cubepod/bin/podd-init /opt/cubepod/bin/podd 2>/dev/null || true",
			},
			VolumeMounts: []VolumeMount{
				{
					Name:      "cubepod-bin",
					MountPath: "/opt/cubepod/bin",
				},
			},
		},
	}

	mainContainer := ContainerManifest{
		Name:            "sandbox",
		Image:           mainImg,
		ImagePullPolicy: "IfNotPresent",
		Command: []string{
			"/opt/cubepod/bin/podd-init",
			"/opt/cubepod/bin/podd",
		},
		VolumeMounts: []VolumeMount{
			{
				Name:      "cubepod-bin",
				MountPath: "/opt/cubepod/bin",
			},
		},
		Resources: tpl.Resources,
	}

	if strings.EqualFold(vm, "Filesystem") {
		mainContainer.VolumeMounts = append(mainContainer.VolumeMounts,
			VolumeMount{
				Name:      "rootfs-volume",
				MountPath: "/mnt/cubepod/rootfs",
			},
			VolumeMount{
				Name:      "workspace-volume",
				MountPath: "/workspace",
			},
		)
		if rtClass == "" {
			mainContainer.Env = append(mainContainer.Env, EnvVar{
				Name:  "CUBEPOD_SKIP_PIVOT",
				Value: "1",
			})
		}
	} else {
		mainContainer.Privileged = true
		mainContainer.VolumeDevices = []VolumeDevice{
			{
				Name:       "rootfs-volume",
				DevicePath: "/dev/cubepod-rootfs",
			},
			{
				Name:       "workspace-volume",
				DevicePath: "/dev/cubepod-workspace",
			},
		}
	}

	if spec.NetworkPolicy != nil {
		for _, b := range spec.NetworkPolicy.SecretBindings {
			mainContainer.Env = append(mainContainer.Env, EnvVar{
				Name:  b.EnvName,
				Value: b.Placeholder,
			})
		}
	}

	if tpl.StartCmd != "" {
		mainContainer.Env = append(mainContainer.Env, EnvVar{
			Name:  "CUBEPOD_START_CMD",
			Value: tpl.StartCmd,
		})
	}
	if tpl.RunAsUser != nil {
		mainContainer.Env = append(mainContainer.Env, EnvVar{
			Name:  "CUBEPOD_RUN_AS_USER",
			Value: fmt.Sprintf("%d", *tpl.RunAsUser),
		})
	}
	if tpl.RunAsGroup != nil {
		mainContainer.Env = append(mainContainer.Env, EnvVar{
			Name:  "CUBEPOD_RUN_AS_GROUP",
			Value: fmt.Sprintf("%d", *tpl.RunAsGroup),
		})
	}
	if tpl.RunAsUsername != "" {
		mainContainer.Env = append(mainContainer.Env, EnvVar{
			Name:  "CUBEPOD_RUN_AS_USERNAME",
			Value: tpl.RunAsUsername,
		})
	}

	// Kubelet probes podd /health on :49983 so Pod Ready reflects completed
	// block formatting and pivot_root (preventing premature WarmPool claims).
	mainContainer.ReadinessProbe = &ProbeSpec{
		HTTPGet: &HTTPGetAction{
			Path: "/health",
			Port: 49983,
		},
		InitialDelaySeconds: 1,
		PeriodSeconds:       1,
		TimeoutSeconds:      1,
		FailureThreshold:    60,
	}

	// Official E2B volumes are filesystem mounts in addition to rootfs and
	// workspace. They stay VolumeMounts when the workspace is a block device.
	// Standard containers do not pivot_root and lack CAP_SYS_ADMIN, so the
	// kubelet also mounts each volume at its final path. podd-init recognizes
	// the identical mount and only initializes ownership. Kata/block-mode
	// workloads keep the staging + in-guest bind-mount path.
	mountDirect := strings.EqualFold(vm, "Filesystem") && rtClass == ""
	// Firecracker must never attach official shared volumes as Kata VolumeMounts.
	// When NFS options are incomplete the mounts are omitted; the controller
	// is responsible for failing closed before EnsurePod. Block workspace
	// mounts (CEP-0030) are excluded by SharedVolumeMounts and never use NFS.
	sharedMounts := spec.SharedVolumeMounts()
	if (opts.IsFirecracker || IsFirecrackerRuntimeName(rtClass)) && !opts.VolumeNFS.Ready() && len(sharedMounts) > 0 {
		// Omit official-volume VolumeMounts to avoid the one-way copy path (#122).
	} else {
		volumes, mainContainer = applyVolumeMounts(volumes, mainContainer, sharedMounts, mountDirect, opts.VolumeNFS)
	}

	var containers []ContainerManifest

	if tpl.Egress.Enabled {
		volumes = append(volumes,
			VolumeManifest{
				Name:       "egress-mtls",
				SecretName: fmt.Sprintf("%s-egress-mtls", boxName),
			},
			VolumeManifest{
				Name:       "egress-ca",
				SecretName: fmt.Sprintf("%s-egress-ca", boxName),
			},
		)

		mainContainer.VolumeMounts = append(mainContainer.VolumeMounts, VolumeMount{
			Name:      "egress-ca",
			MountPath: "/etc/cubepod/ca",
			ReadOnly:  true,
		})
		mainContainer.Env = append(mainContainer.Env,
			EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: "/etc/cubepod/ca/mitm-ca.crt"},
			EnvVar{Name: "CUBEPOD_EXTRA_CA_CERT", Value: "/etc/cubepod/ca/mitm-ca.crt"},
			EnvVar{Name: "SSL_CERT_FILE", Value: "/etc/ssl/certs/ca-certificates.crt"},
			EnvVar{Name: "REQUESTS_CA_BUNDLE", Value: "/etc/ssl/certs/ca-certificates.crt"},
			EnvVar{Name: "CURL_CA_BUNDLE", Value: "/etc/ssl/certs/ca-certificates.crt"},
			EnvVar{Name: "HTTP_PROXY", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "HTTPS_PROXY", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
			EnvVar{Name: "http_proxy", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "https_proxy", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "no_proxy", Value: "127.0.0.1,localhost"},
		)

		egressImg := tpl.Egress.Image
		if egressImg == "" {
			egressImg = "cubepod-egress:latest"
		}

		egressEnv := []EnvVar{
			{Name: "EXCHANGE_URL", Value: tpl.Egress.ExchangeURL},
			{Name: "SANDBOX_ID", Value: boxName},
			{Name: "CUBEPOD_EXCHANGE_URL", Value: tpl.Egress.ExchangeURL},
			{Name: "CUBEPOD_SANDBOX_ID", Value: boxName},
			{Name: "CUBEPOD_CERT_FILE", Value: "/etc/cubepod/certs/tls.crt"},
			{Name: "CUBEPOD_KEY_FILE", Value: "/etc/cubepod/certs/tls.key"},
			{Name: "CUBEPOD_CA_FILE", Value: "/etc/cubepod/certs/ca.crt"},
			{Name: "CUBEPOD_POLICY_FILE", Value: "/etc/cubepod/certs/policy.json"},
			{Name: "CUBEPOD_MITM_CA_CERT", Value: "/etc/cubepod/certs/mitm-ca.crt"},
			{Name: "CUBEPOD_MITM_CA_KEY", Value: "/etc/cubepod/certs/mitm-ca.key"},
			{Name: "CUBEPOD_ENABLE_MITM", Value: "true"},
		}
		if spec.NetworkPolicy != nil && spec.NetworkPolicy.UpstreamProxy != "" {
			egressEnv = append(egressEnv, EnvVar{
				Name:  "CUBEPOD_UPSTREAM_PROXY",
				Value: spec.NetworkPolicy.UpstreamProxy,
			})
		}
		if opts.TransparentEgress {
			egressEnv = append(egressEnv, EnvVar{
				Name:  "CUBEPOD_TRANSPARENT",
				Value: "1",
			})
		}
		if opts.VolumeNFS.Ready() {
			port := opts.VolumeNFS.Port
			if port <= 0 {
				port = volumenfs.DefaultNFSPort
			}
			egressEnv = append(egressEnv, EnvVar{
				Name:  "CUBEPOD_VOLUME_NFS_ADDR",
				Value: volumenfs.NFSAddr(opts.VolumeNFS.ClusterIP, port),
			})
		}

		egressContainer := ContainerManifest{
			Name:  "egress",
			Image: egressImg,
			VolumeMounts: []VolumeMount{
				{
					Name:      "egress-mtls",
					MountPath: "/etc/cubepod/certs",
					ReadOnly:  true,
				},
			},
			Env: egressEnv,
		}
		if opts.TransparentEgress {
			egressContainer.SecurityContext = &SecurityContextSpec{
				Capabilities: &CapabilitiesSpec{
					Add: []string{"NET_ADMIN"},
				},
			}
		}
		containers = append(containers, mainContainer, egressContainer)
	} else {
		containers = append(containers, mainContainer)
	}

	labels := map[string]string{
		"app.kubernetes.io/name": "cubepod",
		"pod.cubeplex.ai/box":    boxName,
	}
	if spec.WarmPoolRef != "" {
		labels["cubepod.io/warmpool"] = SanitizeLabelValue(spec.WarmPoolRef)
		labels["cubepod.io/template"] = SanitizeLabelValue(spec.TemplateRef)
		if !strings.HasPrefix(boxName, spec.WarmPoolRef+"-warm-") || status.Reason == "WarmPoolClaimed" {
			labels["cubepod.io/claimed"] = "true"
			labels["cubepod.io/claimed-by"] = boxName
		} else {
			labels["cubepod.io/claimed"] = "false"
		}
	}

	var pullSecrets []string
	for _, raw := range opts.ImagePullSecrets {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" || strings.HasPrefix(s, "cubepod-template-src-") {
				continue
			}
			if !slices.Contains(pullSecrets, s) {
				pullSecrets = append(pullSecrets, s)
			}
		}
	}

	return PodManifest{
		Name:             podName,
		RuntimeClassName: rtClass,
		Labels:           labels,
		InitContainers:   initContainers,
		Containers:       containers,
		Volumes:          volumes,
		ImagePullSecrets: pullSecrets,
	}
}
