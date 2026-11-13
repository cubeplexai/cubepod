package controller

import (
	"fmt"
	"strings"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

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
	workspaceName := fmt.Sprintf("%s-workspace-pvc", boxName)

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
			rootfsPVC.DataSourcePVC = fmt.Sprintf("%s-rootfs-pvc", spec.ForkFrom.PodBoxName)
			workspacePVC.DataSourcePVC = fmt.Sprintf("%s-workspace-pvc", spec.ForkFrom.PodBoxName)
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
		workspacePVC = fmt.Sprintf("%s-workspace-pvc", boxName)
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

	containers := []ContainerManifest{mainContainer}

	if tpl.Egress.Enabled {
		volumes = append(volumes, VolumeManifest{
			Name:       "egress-mtls",
			SecretName: fmt.Sprintf("%s-egress-mtls", boxName),
		})

		egressImg := tpl.Egress.Image
		if egressImg == "" {
			egressImg = "cubepod-egress:latest"
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
			Env: []EnvVar{
				{
					Name:  "EXCHANGE_URL",
					Value: tpl.Egress.ExchangeURL,
				},
				{
					Name:  "SANDBOX_ID",
					Value: boxName,
				},
			},
		}
		containers = append(containers, egressContainer)
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

	pullSecrets := []string{"regcred", "hub.sensedeal.vip"}
	if len(volumeMode) > 1 && volumeMode[1] != "" {
		pullSecrets = append(pullSecrets, volumeMode[1])
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
