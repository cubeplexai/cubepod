package controller

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
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

// WorkspacePVCName returns the workspace PVC name for a PodBox. When
// spec.VolumeKey is non-empty, it returns a deterministic DNS-1123 name based on
// VolumeKey so workspace data persists across delete -> create cycles.
func WorkspacePVCName(podBoxName string, spec v1alpha1.PodBoxSpec) string {
	if spec.VolumeKey != "" {
		return SanitizeVolumeKey(spec.VolumeKey) + workspacePVCSuffix
	}
	return fmt.Sprintf("%s-workspace-pvc", podBoxName)
}

// BlockPVCManifest represents the Kubernetes PersistentVolumeClaim specification
// for a raw block volume (volumeMode: Block).
type BlockPVCManifest struct {
	Name             string            `json:"name"`
	Namespace        string            `json:"namespace,omitempty"`
	StorageClassName string            `json:"storageClassName"`
	VolumeMode       string            `json:"volumeMode"`
	Size             string            `json:"size"`
	AccessModes      []string          `json:"accessModes"`
	DataSourcePVC    string            `json:"dataSourcePVC,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
}

// VolumeMount represents a file system volume mount inside a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
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
}

// BuildBlockPVCs generates volumeMode: Block or Filesystem PVC specifications for RootFS Upper and Workspace.
// When ForkFrom is specified in spec, DataSourcePVC is populated pointing to the parent sandbox's PVCs (in Block mode).
// When volumeMode is "Filesystem", DataSourcePVC cloning is skipped so NFS provisioners do not reject the PVC.
func BuildBlockPVCs(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, volumeMode ...string) (rootfsPVC, workspacePVC BlockPVCManifest) {
	vm := "Block"
	if len(volumeMode) > 0 && volumeMode[0] != "" {
		vm = volumeMode[0]
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

	if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
		if !strings.EqualFold(vm, "Filesystem") {
			rootfsSource := fmt.Sprintf("%s-rootfs-pvc", spec.ForkFrom.PodBoxName)
			if len(volumeMode) > 1 && volumeMode[1] != "" {
				rootfsSource = volumeMode[1]
			}
			wsSource := fmt.Sprintf("%s-workspace-pvc", spec.ForkFrom.PodBoxName)
			if len(volumeMode) > 2 && volumeMode[2] != "" {
				wsSource = volumeMode[2]
			}
			rootfsPVC.DataSourcePVC = rootfsSource
			workspacePVC.DataSourcePVC = wsSource
		}
	}

	return rootfsPVC, workspacePVC
}

// BuildKataPod synthesizes the complete Kubernetes Pod manifest for running a Kata or container sandbox.
// In volumeMode: Block, it maps raw block devices /dev/cubepod-rootfs and /dev/cubepod-workspace.
// In volumeMode: Filesystem, it mounts rootfs-pvc at /mnt/cubepod/rootfs and workspace-pvc at /workspace
// via ContainerManifest.VolumeMounts instead of VolumeDevices, and adds CUBEPOD_SKIP_PIVOT=1 when RuntimeClassName == "".
func BuildKataPod(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, status v1alpha1.PodBoxStatus, volumeMode ...string) PodManifest {
	vm := "Block"
	if len(volumeMode) > 0 && volumeMode[0] != "" {
		vm = volumeMode[0]
	}

	podName := status.PodName
	if podName == "" {
		podName = fmt.Sprintf("%s-kata-pod", boxName)
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
			EnvVar{Name: "HTTP_PROXY", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "HTTPS_PROXY", Value: "http://127.0.0.1:18080"},
			EnvVar{Name: "NO_PROXY", Value: "127.0.0.1,localhost"},
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
		containers = append(containers, mainContainer, egressContainer)
	} else {
		containers = append(containers, mainContainer)
	}

	labels := map[string]string{
		"app.kubernetes.io/name": "cubepod",
		"pod.cubeplex.ai/box":    boxName,
	}
	if spec.WarmPoolRef != "" {
		labels["cubepod.io/warmpool"] = spec.WarmPoolRef
		labels["cubepod.io/template"] = spec.TemplateRef
		if !strings.HasPrefix(boxName, spec.WarmPoolRef+"-warm-") || status.Reason == "WarmPoolClaimed" {
			labels["cubepod.io/claimed"] = "true"
			labels["cubepod.io/claimed-by"] = boxName
		} else {
			labels["cubepod.io/claimed"] = "false"
		}
	}

	var pullSecrets []string
	if len(volumeMode) > 1 {
		for _, raw := range volumeMode[1:] {
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
