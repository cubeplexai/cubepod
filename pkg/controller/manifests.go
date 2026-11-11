package controller

import (
	"fmt"

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
	Name          string                        `json:"name"`
	Image         string                        `json:"image"`
	Command       []string                      `json:"command,omitempty"`
	Args          []string                      `json:"args,omitempty"`
	VolumeMounts  []VolumeMount                 `json:"volumeMounts,omitempty"`
	VolumeDevices []VolumeDevice                `json:"volumeDevices,omitempty"`
	Resources     v1alpha1.ResourceRequirements `json:"resources,omitempty"`
	Env           []EnvVar                      `json:"env,omitempty"`
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
}

// BuildBlockPVCs generates volumeMode: Block PVC specifications for RootFS Upper and Workspace.
// When ForkFrom is specified in spec, DataSourcePVC is populated pointing to the parent sandbox's PVCs.
func BuildBlockPVCs(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec) (rootfsPVC, workspacePVC BlockPVCManifest) {
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

	rootfsPVC = BlockPVCManifest{
		Name:             rootfsName,
		StorageClassName: storageClass,
		VolumeMode:       "Block",
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
		VolumeMode:       "Block",
		Size:             workspaceSize,
		AccessModes:      []string{"ReadWriteOnce"},
		Labels: map[string]string{
			"app.kubernetes.io/name":   "cubepod",
			"pod.cubeplex.ai/box":      boxName,
			"pod.cubeplex.ai/vol-type": "workspace",
		},
	}

	if spec.ForkFrom != nil && spec.ForkFrom.PodBoxName != "" {
		rootfsPVC.DataSourcePVC = fmt.Sprintf("%s-rootfs-pvc", spec.ForkFrom.PodBoxName)
		workspacePVC.DataSourcePVC = fmt.Sprintf("%s-workspace-pvc", spec.ForkFrom.PodBoxName)
	}

	return rootfsPVC, workspacePVC
}

// BuildKataPod synthesizes the complete Kubernetes Pod manifest for running a Kata MicroVM sandbox.
// It injects:
//  1. podd-installer initContainer copying podd-init and podd into /opt/cubepod/bin (emptyDir)
//  2. Main container mounting /opt/cubepod/bin and mapping raw block devices /dev/cubepod-rootfs
//     and /dev/cubepod-workspace
//  3. Optional zero-trust Egress sidecar container and per-pod mTLS certificate secret volume
func BuildKataPod(boxName string, tpl v1alpha1.PodTemplateSpec, spec v1alpha1.PodBoxSpec, status v1alpha1.PodBoxStatus) PodManifest {
	podName := status.PodName
	if podName == "" {
		podName = fmt.Sprintf("%s-kata-pod", boxName)
	}

	rtClass := tpl.RuntimeClassName
	if rtClass == "" {
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
			Name:  "podd-installer",
			Image: poddImg,
			Command: []string{
				"/bin/sh",
				"-c",
				"cp /podd-init /podd /opt/cubepod/bin/ 2>/dev/null || cp /usr/local/bin/podd* /opt/cubepod/bin/",
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
		Name:  "sandbox",
		Image: mainImg,
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
		VolumeDevices: []VolumeDevice{
			{
				Name:       "rootfs-volume",
				DevicePath: "/dev/cubepod-rootfs",
			},
			{
				Name:       "workspace-volume",
				DevicePath: "/dev/cubepod-workspace",
			},
		},
		Resources: tpl.Resources,
	}

	if spec.NetworkPolicy != nil {
		for _, b := range spec.NetworkPolicy.SecretBindings {
			mainContainer.Env = append(mainContainer.Env, EnvVar{
				Name:  b.EnvName,
				Value: b.Placeholder,
			})
		}
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

	return PodManifest{
		Name:             podName,
		RuntimeClassName: rtClass,
		Labels: map[string]string{
			"app.kubernetes.io/name": "cubepod",
			"pod.cubeplex.ai/box":    boxName,
		},
		InitContainers: initContainers,
		Containers:     containers,
		Volumes:        volumes,
	}
}
