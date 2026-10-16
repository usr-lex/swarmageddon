{{/* Image reference for one of our images. */}}
{{- define "swarmageddon.image" -}}
{{- $root := index . 0 -}}
{{- $name := index . 1 -}}
{{- printf "%s/%s:%s" $root.Values.image.registry $name (default $root.Chart.AppVersion $root.Values.image.tag) -}}
{{- end -}}

{{- define "swarmageddon.labels" -}}
app.kubernetes.io/part-of: swarmageddon
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ default .Chart.AppVersion .Values.image.tag | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* Pod and container security settings shared by our Deployments and Jobs. */}}
{{- define "swarmageddon.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "swarmageddon.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}
