package controller

import (
	"slices"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// KubeSecret represents a Kubernetes core/v1 Secret payload.
type KubeSecret struct {
	APIVersion string              `json:"apiVersion,omitempty"`
	Kind       string              `json:"kind,omitempty"`
	Metadata   v1alpha1.ObjectMeta `json:"metadata"`
	Type       string              `json:"type,omitempty"`
	Data       map[string][]byte   `json:"data,omitempty"`
	StringData map[string]string   `json:"stringData,omitempty"`
}

// Kubernetes core/v1 Event types.
const (
	KubeEventTypeNormal  = "Normal"
	KubeEventTypeWarning = "Warning"
)

// KubeObjectReference identifies the object a Kubernetes Event is attached to (involvedObject).
type KubeObjectReference struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// KubeEventSource identifies the component that emitted a Kubernetes Event.
type KubeEventSource struct {
	Component string `json:"component,omitempty"`
	Host      string `json:"host,omitempty"`
}

// KubeEvent represents a Kubernetes core/v1 Event payload.
type KubeEvent struct {
	APIVersion          string              `json:"apiVersion,omitempty"`
	Kind                string              `json:"kind,omitempty"`
	Metadata            v1alpha1.ObjectMeta `json:"metadata"`
	InvolvedObject      KubeObjectReference `json:"involvedObject"`
	Reason              string              `json:"reason,omitempty"`
	Message             string              `json:"message,omitempty"`
	Type                string              `json:"type,omitempty"`
	Source              KubeEventSource     `json:"source,omitempty"`
	FirstTimestamp      *time.Time          `json:"firstTimestamp,omitempty"`
	LastTimestamp       *time.Time          `json:"lastTimestamp,omitempty"`
	Count               int32               `json:"count,omitempty"`
	ReportingController string              `json:"reportingComponent,omitempty"`
	ReportingInstance   string              `json:"reportingInstance,omitempty"`
}

// VolumeSnapshotSource identifies the PersistentVolumeClaim used as the snapshot source.
type VolumeSnapshotSource struct {
	PersistentVolumeClaimName string `json:"persistentVolumeClaimName,omitempty"`
}

// VolumeSnapshotSpec defines the desired state of a snapshot.storage.k8s.io/v1 VolumeSnapshot.
type VolumeSnapshotSpec struct {
	VolumeSnapshotClassName *string              `json:"volumeSnapshotClassName,omitempty"`
	Source                  VolumeSnapshotSource `json:"source"`
}

// VolumeSnapshotError describes an error encountered during snapshot creation.
type VolumeSnapshotError struct {
	Time    string `json:"time,omitempty"`
	Message string `json:"message,omitempty"`
}

// VolumeSnapshotStatus reports the observed state of a VolumeSnapshot.
type VolumeSnapshotStatus struct {
	BoundVolumeSnapshotContentName string               `json:"boundVolumeSnapshotContentName,omitempty"`
	CreationTime                   string               `json:"creationTime,omitempty"`
	ReadyToUse                     *bool                `json:"readyToUse,omitempty"`
	RestoreSize                    string               `json:"restoreSize,omitempty"`
	Error                          *VolumeSnapshotError `json:"error,omitempty"`
}

// VolumeSnapshot represents a Kubernetes snapshot.storage.k8s.io/v1 VolumeSnapshot object.
type VolumeSnapshot struct {
	APIVersion string               `json:"apiVersion,omitempty"`
	Kind       string               `json:"kind,omitempty"`
	Metadata   v1alpha1.ObjectMeta  `json:"metadata"`
	Spec       VolumeSnapshotSpec   `json:"spec"`
	Status     VolumeSnapshotStatus `json:"status,omitempty"`
}

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
	APIGroup *string `json:"apiGroup,omitempty"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
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
	SubPath   string `json:"subPath,omitempty"`
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

// coreV1Capabilities holds POSIX capabilities to add or drop in a container securityContext.
type coreV1Capabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

// coreV1SecurityContext holds container-level security attributes.
type coreV1SecurityContext struct {
	Privileged   *bool               `json:"privileged,omitempty"`
	RunAsUser    *int64              `json:"runAsUser,omitempty"`
	RunAsGroup   *int64              `json:"runAsGroup,omitempty"`
	Capabilities *coreV1Capabilities `json:"capabilities,omitempty"`
}

// coreV1HTTPGetAction represents an HTTPGet probe action.
type coreV1HTTPGetAction struct {
	Path string `json:"path"`
	Port int    `json:"port"`
}

// coreV1Probe represents a Kubernetes container probe.
type coreV1Probe struct {
	HTTPGet             *coreV1HTTPGetAction `json:"httpGet,omitempty"`
	InitialDelaySeconds int32                `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32                `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int32                `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    int32                `json:"successThreshold,omitempty"`
	FailureThreshold    int32                `json:"failureThreshold,omitempty"`
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
	SecurityContext *coreV1SecurityContext      `json:"securityContext,omitempty"`
	ReadinessProbe  *coreV1Probe                `json:"readinessProbe,omitempty"`
}

type coreV1PodCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type coreV1PodStatus struct {
	Phase      string               `json:"phase,omitempty"`
	PodIP      string               `json:"podIP,omitempty"`
	Conditions []coreV1PodCondition `json:"conditions,omitempty"`
}

// coreV1PodSpec is a description of a pod.
type coreV1PodSpec struct {
	InitContainers   []coreV1Container            `json:"initContainers,omitempty"`
	Containers       []coreV1Container            `json:"containers,omitempty"`
	Volumes          []coreV1Volume               `json:"volumes,omitempty"`
	RuntimeClassName string                       `json:"runtimeClassName,omitempty"`
	RestartPolicy    string                       `json:"restartPolicy,omitempty"`
	ImagePullSecrets []coreV1LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// coreV1Pod represents a Kubernetes v1 Pod JSON payload.
type coreV1Pod struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   coreV1Metadata   `json:"metadata"`
	Spec       coreV1PodSpec    `json:"spec"`
	Status     *coreV1PodStatus `json:"status,omitempty"`
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
	if pvc.DataSourceSnapshot != "" {
		apiGroup := pvc.DataSourceAPIGroup
		if apiGroup == "" {
			apiGroup = "snapshot.storage.k8s.io"
		}
		kind := pvc.DataSourceKind
		if kind == "" {
			kind = "VolumeSnapshot"
		}
		res.Spec.DataSource = &coreV1DataSource{
			APIGroup: &apiGroup,
			Kind:     kind,
			Name:     pvc.DataSourceSnapshot,
		}
	} else if pvc.DataSourcePVC != "" {
		kind := pvc.DataSourceKind
		if kind == "" {
			kind = "PersistentVolumeClaim"
		}
		ds := &coreV1DataSource{
			Kind: kind,
			Name: pvc.DataSourcePVC,
		}
		if pvc.DataSourceAPIGroup != "" {
			ag := pvc.DataSourceAPIGroup
			ds.APIGroup = &ag
		}
		res.Spec.DataSource = ds
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
			SubPath:   vm.SubPath,
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
	hasTopLevelSC := c.Privileged || c.RunAsUser != nil || c.RunAsGroup != nil
	hasCaps := c.SecurityContext != nil && c.SecurityContext.Capabilities != nil &&
		(len(c.SecurityContext.Capabilities.Add) > 0 || len(c.SecurityContext.Capabilities.Drop) > 0)
	hasNestedSC := c.SecurityContext != nil &&
		(c.SecurityContext.Privileged != nil || c.SecurityContext.RunAsUser != nil || c.SecurityContext.RunAsGroup != nil || hasCaps)
	if hasTopLevelSC || hasNestedSC {
		sc := &coreV1SecurityContext{
			RunAsUser:  c.RunAsUser,
			RunAsGroup: c.RunAsGroup,
		}
		if c.Privileged {
			priv := true
			sc.Privileged = &priv
		}
		if c.SecurityContext != nil {
			if c.SecurityContext.Privileged != nil {
				priv := *c.SecurityContext.Privileged
				sc.Privileged = &priv
			}
			if c.SecurityContext.RunAsUser != nil {
				u := *c.SecurityContext.RunAsUser
				sc.RunAsUser = &u
			}
			if c.SecurityContext.RunAsGroup != nil {
				g := *c.SecurityContext.RunAsGroup
				sc.RunAsGroup = &g
			}
			if hasCaps {
				sc.Capabilities = &coreV1Capabilities{
					Add:  slices.Clone(c.SecurityContext.Capabilities.Add),
					Drop: slices.Clone(c.SecurityContext.Capabilities.Drop),
				}
			}
		}
		res.SecurityContext = sc
	}
	if c.ReadinessProbe != nil {
		probe := &coreV1Probe{
			InitialDelaySeconds: c.ReadinessProbe.InitialDelaySeconds,
			PeriodSeconds:       c.ReadinessProbe.PeriodSeconds,
			TimeoutSeconds:      c.ReadinessProbe.TimeoutSeconds,
			SuccessThreshold:    c.ReadinessProbe.SuccessThreshold,
			FailureThreshold:    c.ReadinessProbe.FailureThreshold,
		}
		if c.ReadinessProbe.HTTPGet != nil {
			probe.HTTPGet = &coreV1HTTPGetAction{
				Path: c.ReadinessProbe.HTTPGet.Path,
				Port: c.ReadinessProbe.HTTPGet.Port,
			}
		}
		res.ReadinessProbe = probe
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
	for _, s := range pod.ImagePullSecrets {
		if s != "" {
			imagePullSecrets = append(imagePullSecrets, coreV1LocalObjectReference{Name: s})
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
			RestartPolicy:    pod.RestartPolicy,
			ImagePullSecrets: imagePullSecrets,
		},
	}
}
