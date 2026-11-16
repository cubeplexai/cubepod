{{/*
Expand the name of the chart.
*/}}
{{- define "cubepod.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cubepod.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "cubepod.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "cubepod.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "cubepod.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cubepod.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "cubepod.serviceAccountName" -}}
{{- printf "%s-controller" (include "cubepod.fullname" .) }}
{{- end }}

{{/*
Default cubepod-egress sidecar image
*/}}
{{- define "cubepod.egressImage" -}}
{{- printf "%s/%s:%s" .Values.egress.image.registry .Values.egress.image.repository .Values.egress.image.tag -}}
{{- end }}

{{/*
Comma-separated list of global.imagePullSecrets
*/}}
{{- define "cubepod.imagePullSecretsCSV" -}}
{{- $names := list -}}
{{- range .Values.global.imagePullSecrets -}}
  {{- if kindIs "map" . -}}
    {{- if .name -}}
      {{- $names = append $names .name -}}
    {{- end -}}
  {{- else if . -}}
    {{- $names = append $names (. | toString) -}}
  {{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end }}

{{/*
Render pod spec imagePullSecrets block from global.imagePullSecrets
*/}}
{{- define "cubepod.podImagePullSecrets" -}}
{{- if .Values.global.imagePullSecrets }}
imagePullSecrets:
{{- range .Values.global.imagePullSecrets }}
  {{- if kindIs "map" . }}
  - name: {{ .name | quote }}
  {{- else }}
  - name: {{ . | toString | quote }}
  {{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Effective templatePullSecret (templateBuild.registry.pullSecret when templateBuild is enabled, or runtime.templatePullSecret)
*/}}
{{- define "cubepod.effectiveTemplatePullSecret" -}}
{{- if and .Values.templateBuild.enabled .Values.templateBuild.registry.pullSecret -}}
{{- .Values.templateBuild.registry.pullSecret -}}
{{- else if .Values.runtime.templatePullSecret -}}
{{- .Values.runtime.templatePullSecret -}}
{{- end -}}
{{- end }}

{{/*
Effective gateway stateDir
*/}}
{{- define "cubepod.effectiveGatewayStateDir" -}}
{{- if .Values.gateway.stateDir -}}
{{- .Values.gateway.stateDir -}}
{{- else if .Values.gateway.persistence.enabled -}}
{{- "/var/lib/cubepod/state" -}}
{{- end -}}
{{- end }}

{{/*
Egress CA environment variables when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAEnv" -}}
{{- if .Values.egress.caSecretName }}
- name: CUBEPOD_EGRESS_CA_CERT
  value: "/etc/cubepod-egress-ca/ca.crt"
- name: CUBEPOD_EGRESS_CA_KEY
  value: "/etc/cubepod-egress-ca/ca.key"
{{- if .Values.egress.upstreamCAEnabled }}
- name: CUBEPOD_EGRESS_UPSTREAM_CA_FILE
  value: "/etc/cubepod-egress-ca/ca.crt"
{{- end }}
{{- end }}
{{- end }}

{{/*
Egress CA volumeMount when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAVolumeMount" -}}
{{- if .Values.egress.caSecretName }}
- name: cubepod-egress-ca
  mountPath: /etc/cubepod-egress-ca
  readOnly: true
{{- end }}
{{- end }}

{{/*
Egress CA volume when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAVolume" -}}
{{- if .Values.egress.caSecretName }}
- name: cubepod-egress-ca
  secret:
    secretName: {{ .Values.egress.caSecretName | quote }}
{{- end }}
{{- end }}


{{/*
True ("true") when E2B Volumes should be active (explicitly enabled OR rwxStorageClassName is set)
*/}}
{{- define "cubepod.e2bVolumesActive" -}}
{{- if or .Values.e2bVolumes.enabled (ne (default "" .Values.e2bVolumes.rwxStorageClassName) "") -}}
true
{{- end -}}
{{- end }}

{{/*
Effective K8sE2BVolumeMounts gate ("true" or "false")
*/}}
{{- define "cubepod.k8sE2BVolumeMountsEnabled" -}}
{{- if or .Values.featureGates.k8sE2BVolumeMounts (eq (include "cubepod.e2bVolumesActive" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Effective TransparentEgress gate ("true" or "false")
*/}}
{{- define "cubepod.transparentEgressEnabled" -}}
{{- if and .Values.egress.transparent .Values.featureGates.transparentEgress -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Comma-separated feature gates string for controller and gateway
*/}}
{{- define "cubepod.featureGatesCSV" -}}
{{- printf "E2BTemplateAPI=%t,TransparentEgress=%s,DesktopStack=%t,StatelessGatewayHA=%t,E2BFilesystemWatch=%t,E2BSecretsAPI=%t,SandboxEventsAndWebhooks=%t,K8sE2BVolumeMounts=%s" (default false .Values.featureGates.e2bTemplateAPI) (include "cubepod.transparentEgressEnabled" .) (default true .Values.featureGates.desktopStack) (default true .Values.featureGates.statelessGatewayHA) (default true .Values.featureGates.e2bFilesystemWatch) (default true .Values.featureGates.e2bSecretsAPI) (default true .Values.featureGates.sandboxEventsAndWebhooks) (include "cubepod.k8sE2BVolumeMountsEnabled" .) -}}
{{- end }}

{{/*
Effective Kubernetes Secret name for CUBEPOD_SECRETS_KEY
*/}}
{{- define "cubepod.secretsKeySecretName" -}}
{{- $secrets := .Values.secrets | default dict -}}
{{- if $secrets.existingSecret -}}
{{- $secrets.existingSecret -}}
{{- else if .Values.gateway.secretsKeySecret -}}
{{- .Values.gateway.secretsKeySecret -}}
{{- else if and (default true $secrets.enabled) $secrets.encryptionKey -}}
{{- printf "%s-secrets-key" (include "cubepod.fullname" .) -}}
{{- end -}}
{{- end }}