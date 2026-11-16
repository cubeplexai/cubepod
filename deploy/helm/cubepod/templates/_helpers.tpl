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
