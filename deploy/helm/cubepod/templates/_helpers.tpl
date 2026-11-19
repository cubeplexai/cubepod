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
{{- $egress := .Values.egress | default dict -}}
{{- $img := $egress.image | default dict -}}
{{- printf "%s/%s:%s" ($img.registry | default "ghcr.io") ($img.repository | default "cubeplexai/cubepod-egress") ($img.tag | default "v0.6.0") -}}
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
Normalize a boolean value with a fallback of true (unlike Sprig default, preserves explicit false).
*/}}
{{- define "cubepod.boolDefaultTrue" -}}
{{- if kindIs "bool" . -}}{{ . }}{{- else -}}true{{- end -}}
{{- end }}

{{/*
Egress CA environment variables when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAEnv" -}}
{{- $egress := .Values.egress | default dict -}}
{{- if $egress.caSecretName }}
- name: CUBEPOD_EGRESS_CA_CERT
  value: "/etc/cubepod-egress-ca/ca.crt"
- name: CUBEPOD_EGRESS_CA_KEY
  value: "/etc/cubepod-egress-ca/ca.key"
{{- if $egress.upstreamCAEnabled }}
- name: CUBEPOD_EGRESS_UPSTREAM_CA_FILE
  value: "/etc/cubepod-egress-ca/ca.crt"
{{- end }}
{{- end }}
{{- end }}

{{/*
Egress CA volumeMount when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAVolumeMount" -}}
{{- $egress := .Values.egress | default dict -}}
{{- if $egress.caSecretName }}
- name: cubepod-egress-ca
  mountPath: /etc/cubepod-egress-ca
  readOnly: true
{{- end }}
{{- end }}

{{/*
Egress CA volume when egress.caSecretName is configured
*/}}
{{- define "cubepod.egressCAVolume" -}}
{{- $egress := .Values.egress | default dict -}}
{{- if $egress.caSecretName }}
- name: cubepod-egress-ca
  secret:
    secretName: {{ $egress.caSecretName | quote }}
{{- end }}
{{- end }}


{{/*
True ("true") when E2B Volumes should be active (explicitly enabled OR rwxStorageClassName is set)
*/}}
{{- define "cubepod.e2bVolumesActive" -}}
{{- $ev := .Values.e2bVolumes | default dict -}}
{{- if or $ev.enabled (ne (default "" $ev.rwxStorageClassName) "") -}}
true
{{- end -}}
{{- end }}

{{/*
Effective K8sE2BVolumeMounts gate ("true" or "false")
*/}}
{{- define "cubepod.k8sE2BVolumeMountsEnabled" -}}
{{- $fg := .Values.featureGates | default dict -}}
{{- if or $fg.k8sE2BVolumeMounts (eq (include "cubepod.e2bVolumesActive" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Effective TransparentEgress gate ("true" or "false")
*/}}
{{- define "cubepod.transparentEgressEnabled" -}}
{{- $eg := .Values.egress | default dict -}}
{{- $fg := .Values.featureGates | default dict -}}
{{- if and (eq (include "cubepod.boolDefaultTrue" $eg.transparent) "true") (eq (include "cubepod.boolDefaultTrue" $fg.transparentEgress) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
Comma-separated feature gates string for controller and gateway
*/}}
{{- define "cubepod.featureGatesCSV" -}}
{{- $fg := .Values.featureGates | default dict -}}
{{- printf "E2BTemplateAPI=%s,TransparentEgress=%s,DesktopStack=%s,StatelessGatewayHA=%s,E2BFilesystemWatch=%s,E2BSecretsAPI=%s,SandboxEventsAndWebhooks=%s,K8sE2BVolumeMounts=%s" (include "cubepod.boolDefaultTrue" $fg.e2bTemplateAPI) (include "cubepod.transparentEgressEnabled" .) (include "cubepod.boolDefaultTrue" $fg.desktopStack) (include "cubepod.boolDefaultTrue" $fg.statelessGatewayHA) (include "cubepod.boolDefaultTrue" $fg.e2bFilesystemWatch) (include "cubepod.boolDefaultTrue" $fg.e2bSecretsAPI) (include "cubepod.boolDefaultTrue" $fg.sandboxEventsAndWebhooks) (include "cubepod.k8sE2BVolumeMountsEnabled" .) -}}
{{- end }}

{{/*
Normalized template context store backend ("local" or "s3")
*/}}
{{- define "cubepod.contextStoreBackend" -}}
{{- $tb := .Values.templateBuild | default dict -}}
{{- $cs := $tb.contextStore | default dict -}}
{{- lower (default "local" $cs.backend) -}}
{{- end }}

{{/*
Effective BuildKit replica count (>= 1)
*/}}
{{- define "cubepod.buildkitReplicaCount" -}}
{{- $tb := .Values.templateBuild | default dict -}}
{{- $bk := $tb.buildkit | default dict -}}
{{- $rc := int (default 1 $bk.replicaCount) -}}
{{- if lt $rc 1 -}}1{{- else -}}{{ $rc }}{{- end -}}
{{- end }}

{{/*
Effective BuildKit TLS Secret name when templateBuild.buildkit.tls.enabled=true
*/}}
{{- define "cubepod.buildkitTLSSecretName" -}}
{{- $tb := .Values.templateBuild | default dict -}}
{{- $bk := $tb.buildkit | default dict -}}
{{- $tls := $bk.tls | default dict -}}
{{- if $tls.enabled -}}
{{- default "cubepod-buildkit-tls" $tls.secretName -}}
{{- end -}}
{{- end }}

{{/*
Effective BuildKit gRPC address(es) for cubepod-controller
*/}}
{{- define "cubepod.buildkitEffectiveAddress" -}}
{{- $tb := .Values.templateBuild | default dict -}}
{{- $bk := $tb.buildkit | default dict -}}
{{- $rc := int (include "cubepod.buildkitReplicaCount" .) -}}
{{- $addr := default "" $bk.address -}}
{{- if and (gt $rc 1) (or (eq $addr "") (eq $addr "tcp://cubepod-buildkit.cubepod-system.svc:1234")) -}}
{{- $ns := .Release.Namespace -}}
{{- $list := list -}}
{{- range $i := until $rc -}}
{{- $list = append $list (printf "tcp://cubepod-buildkit-%d.cubepod-buildkit-headless.%s.svc:1234" $i $ns) -}}
{{- end -}}
{{- join "," $list -}}
{{- else if $addr -}}
{{- $addr -}}
{{- else -}}
{{- printf "tcp://cubepod-buildkit.%s.svc:1234" .Release.Namespace -}}
{{- end -}}
{{- end }}

{{/*
S3 ContextStore environment variables (credentials injected strictly via secretKeyRef)
*/}}
{{- define "cubepod.s3ContextEnv" -}}
{{- $tb := .Values.templateBuild | default dict -}}
{{- if and $tb.enabled (eq (include "cubepod.contextStoreBackend" .) "s3") -}}
{{- $cs := $tb.contextStore | default dict -}}
{{- $s3 := $cs.s3 | default dict -}}
- name: CUBEPOD_CONTEXT_STORE_BACKEND
  value: "s3"
{{- if $s3.endpoint }}
- name: CUBEPOD_S3_ENDPOINT
  value: {{ $s3.endpoint | quote }}
{{- end }}
{{- if $s3.bucket }}
- name: CUBEPOD_S3_BUCKET
  value: {{ $s3.bucket | quote }}
{{- end }}
- name: CUBEPOD_S3_REGION
  value: {{ default "us-east-1" $s3.region | quote }}
{{- if $s3.prefix }}
- name: CUBEPOD_S3_PREFIX
  value: {{ $s3.prefix | quote }}
{{- end }}
- name: CUBEPOD_S3_FORCE_PATH_STYLE
  value: {{ default false $s3.forcePathStyle | toString | quote }}
{{- if $s3.credentialsSecretName }}
- name: CUBEPOD_S3_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ $s3.credentialsSecretName | quote }}
      key: accessKeyId
- name: CUBEPOD_S3_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $s3.credentialsSecretName | quote }}
      key: secretAccessKey
- name: CUBEPOD_S3_SESSION_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $s3.credentialsSecretName | quote }}
      key: sessionToken
      optional: true
{{- end }}
{{- end -}}
{{- end }}

{{/*
Effective Kubernetes Secret name for CUBEPOD_SECRETS_KEY
*/}}
{{- define "cubepod.secretsKeySecretName" -}}
{{- $secrets := .Values.secrets | default dict -}}
{{- $fg := .Values.featureGates | default dict -}}
{{- $secretsEnabled := and (eq (include "cubepod.boolDefaultTrue" $secrets.enabled) "true") (eq (include "cubepod.boolDefaultTrue" $fg.e2bSecretsAPI) "true") -}}
{{- if not $secretsEnabled -}}
{{- else if $secrets.existingSecret -}}
{{- $secrets.existingSecret -}}
{{- else if .Values.gateway.secretsKeySecret -}}
{{- .Values.gateway.secretsKeySecret -}}
{{- else if and (eq (default "k8s" $secrets.backend) "file") $secrets.encryptionKey -}}
{{- printf "%s-secrets-key" (include "cubepod.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Name of the Kubernetes Secret that holds CUBEPOD_PANEL_SECRET
*/}}
{{- define "cubepod.panelSecretName" -}}
{{- printf "%s-panel-secret" (include "cubepod.fullname" .) -}}
{{- end }}
