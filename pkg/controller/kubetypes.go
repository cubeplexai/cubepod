package controller

// coreV1Metadata represents common object metadata in the Kubernetes v1 API.
type coreV1Metadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// coreV1ResourceRequirements describes the compute resource requirements for a container or PVC.
type coreV1ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// coreV1DataSource identifies the API object that was used to populate a PVC volume.
type coreV1DataSource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// coreV1PVCSpec represents the specification of a PersistentVolumeClaim.
type coreV1PVCSpec struct {
	AccessModes      []string                   `json:"accessModes,omitempty"`
	VolumeMode       string                     `json:"volumeMode,omitempty"`
	Resources        coreV1ResourceRequirements `json:"resources"`
	StorageClassName *string                    `json:"storageClassName,omitempty"`
	DataSource       *coreV1DataSource          `json:"dataSource,omitempty"`
}

// coreV1PVC represents a Kubernetes v1 PersistentVolumeClaim JSON payload.
type coreV1PVC struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   coreV1Metadata `json:"metadata"`
	Spec       coreV1PVCSpec  `json:"spec"`
}

// coreV1VolumeMount describes a mounting of a Volume within a container.
type coreV1VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// coreV1VolumeDevice describes a mapping of a raw block device within a container.
type coreV1VolumeDevice struct {
	Name       string `json:"name"`
	DevicePath string `json:"devicePath"`
}

// coreV1EnvVar represents an environment variable present in a Container.
type coreV1EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// coreV1EmptyDirVolumeSource represents an emptyDir volume.
type coreV1EmptyDirVolumeSource struct{}

// coreV1PersistentVolumeClaimVolumeSource references a PVC in the same namespace.
type coreV1PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName"`
}

// coreV1SecretVolumeSource adapts a Secret into a volume.
type coreV1SecretVolumeSource struct {
	SecretName string `json:"secretName"`
}

// coreV1Volume represents a named volume in a pod that may be accessed by any container in the pod.
type coreV1Volume struct {
	Name                  string                                   `json:"name"`
	EmptyDir              *coreV1EmptyDirVolumeSource              `json:"emptyDir,omitempty"`
	PersistentVolumeClaim *coreV1PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`
	Secret                *coreV1SecretVolumeSource                `json:"secret,omitempty"`
}

// coreV1LocalObjectReference contains enough information to let you locate the referenced object inside the same namespace.
type coreV1LocalObjectReference struct {
	Name string `json:"name"`
}

// coreV1Container represents a single container that is expected to be run on the host.
type coreV1Container struct {
	Name            string                      `json:"name"`
	Image           string                      `json:"image"`
	ImagePullPolicy string                      `json:"imagePullPolicy,omitempty"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	VolumeMounts    []coreV1VolumeMount         `json:"volumeMounts,omitempty"`
	VolumeDevices   []coreV1VolumeDevice        `json:"volumeDevices,omitempty"`
	Resources       *coreV1ResourceRequirements `json:"resources,omitempty"`
	Env             []coreV1EnvVar              `json:"env,omitempty"`
}

// coreV1PodSpec is a description of a pod.
type coreV1PodSpec struct {
	InitContainers   []coreV1Container            `json:"initContainers,omitempty"`
	Containers       []coreV1Container            `json:"containers,omitempty"`
	Volumes          []coreV1Volume               `json:"volumes,omitempty"`
	RuntimeClassName string                       `json:"runtimeClassName,omitempty"`
	ImagePullSecrets []coreV1LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// coreV1Pod represents a Kubernetes v1 Pod JSON payload.
type coreV1Pod struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   coreV1Metadata `json:"metadata"`
	Spec       coreV1PodSpec  `json:"spec"`
}

// toCoreV1PVC converts a BlockPVCManifest into a Kubernetes v1 PersistentVolumeClaim payload.
func toCoreV1PVC(pvc BlockPVCManifest, namespace string) coreV1PVC {
	res := coreV1PVC{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata: coreV1Metadata{
			Name:      pvc.Name,
			Namespace: namespace,
			Labels:    pvc.Labels,
		},
		Spec: coreV1PVCSpec{
			AccessModes: pvc.AccessModes,
			VolumeMode:  pvc.VolumeMode,
			Resources: coreV1ResourceRequirements{
				Requests: map[string]string{
					"storage": pvc.Size,
				},
			},
		},
	}
	if pvc.StorageClassName != "" {
		res.Spec.StorageClassName = &pvc.StorageClassName
	}
	if pvc.DataSourcePVC != "" {
		res.Spec.DataSource = &coreV1DataSource{
			Kind: "PersistentVolumeClaim",
			Name: pvc.DataSourcePVC,
		}
	}
	return res
}

// toCoreV1Container converts an internal ContainerManifest into a Kubernetes core/v1 container.
func toCoreV1Container(c ContainerManifest) coreV1Container {
	res := coreV1Container{
		Name:            c.Name,
		Image:           c.Image,
		ImagePullPolicy: c.ImagePullPolicy,
		Command:         c.Command,
		Args:            c.Args,
	}
	for _, vm := range c.VolumeMounts {
		res.VolumeMounts = append(res.VolumeMounts, coreV1VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
			ReadOnly:  vm.ReadOnly,
		})
	}
	for _, vd := range c.VolumeDevices {
		res.VolumeDevices = append(res.VolumeDevices, coreV1VolumeDevice{
			Name:       vd.Name,
			DevicePath: vd.DevicePath,
		})
	}
	requests := make(map[string]string)
	limits := make(map[string]string)
	if c.Resources.CPURequest != "" {
		requests["cpu"] = c.Resources.CPURequest
	}
	if c.Resources.MemoryRequest != "" {
		requests["memory"] = c.Resources.MemoryRequest
	}
	if c.Resources.CPULimit != "" {
		limits["cpu"] = c.Resources.CPULimit
	}
	if c.Resources.MemoryLimit != "" {
		limits["memory"] = c.Resources.MemoryLimit
	}
	if len(requests) > 0 || len(limits) > 0 {
		res.Resources = &coreV1ResourceRequirements{}
		if len(requests) > 0 {
			res.Resources.Requests = requests
		}
		if len(limits) > 0 {
			res.Resources.Limits = limits
		}
	}
	for _, ev := range c.Env {
		res.Env = append(res.Env, coreV1EnvVar{
			Name:  ev.Name,
			Value: ev.Value,
		})
	}
	return res
}

// toCoreV1Volume converts an internal VolumeManifest into a Kubernetes core/v1 volume.
func toCoreV1Volume(v VolumeManifest) coreV1Volume {
	vol := coreV1Volume{
		Name: v.Name,
	}
	if v.EmptyDir {
		vol.EmptyDir = &coreV1EmptyDirVolumeSource{}
	}
	if v.PersistentVolumeClaim != "" {
		vol.PersistentVolumeClaim = &coreV1PersistentVolumeClaimVolumeSource{
			ClaimName: v.PersistentVolumeClaim,
		}
	}
	if v.SecretName != "" {
		vol.Secret = &coreV1SecretVolumeSource{
			SecretName: v.SecretName,
		}
	}
	return vol
}

// toCoreV1Pod converts a PodManifest into a Kubernetes v1 Pod payload.
func toCoreV1Pod(pod PodManifest, namespace string) coreV1Pod {
	var volumes []coreV1Volume
	for _, v := range pod.Volumes {
		volumes = append(volumes, toCoreV1Volume(v))
	}

	var initContainers []coreV1Container
	for _, ic := range pod.InitContainers {
		initContainers = append(initContainers, toCoreV1Container(ic))
	}

	var containers []coreV1Container
	for _, cn := range pod.Containers {
		containers = append(containers, toCoreV1Container(cn))
	}

	var imagePullSecrets []coreV1LocalObjectReference
	if len(pod.ImagePullSecrets) > 0 {
		for _, s := range pod.ImagePullSecrets {
			imagePullSecrets = append(imagePullSecrets, coreV1LocalObjectReference{Name: s})
		}
	} else {
		imagePullSecrets = []coreV1LocalObjectReference{
			{Name: "regcred"},
			{Name: "hub.sensedeal.vip"},
		}
	}

	return coreV1Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: coreV1Metadata{
			Name:      pod.Name,
			Namespace: namespace,
			Labels:    pod.Labels,
		},
		Spec: coreV1PodSpec{
			InitContainers:   initContainers,
			Containers:       containers,
			Volumes:          volumes,
			RuntimeClassName: pod.RuntimeClassName,
			ImagePullSecrets: imagePullSecrets,
		},
	}
}
