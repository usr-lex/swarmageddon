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

{{/* The Secret with the Sentry DSN, or nothing when error reporting is off. */}}
{{- define "swarmageddon.sentrySecret" -}}
{{- if .Values.sentry.existingSecret -}}
{{- .Values.sentry.existingSecret -}}
{{- else if .Values.sentry.dsn -}}
sentry
{{- end -}}
{{- end -}}

{{/* SENTRY_* for the operator and the control center, when reporting is on. */}}
{{- define "swarmageddon.sentryEnv" -}}
{{- with include "swarmageddon.sentrySecret" . -}}
- name: SENTRY_DSN
  valueFrom:
    secretKeyRef:
      name: {{ . }}
      key: dsn
      optional: true
- name: SENTRY_ENVIRONMENT
  value: {{ $.Values.sentry.environment | quote }}
- name: SENTRY_TRACES_SAMPLE_RATE
  value: {{ $.Values.sentry.tracesSampleRate | quote }}
{{- end -}}
{{- end -}}

{{/* A pod annotation that changes with the chart's DSN, so pods restart. */}}
{{- define "swarmageddon.sentryAnnotations" -}}
{{- if and .Values.sentry.dsn (not .Values.sentry.existingSecret) -}}
annotations:
  swarmageddon.dev/sentry: {{ .Values.sentry.dsn | sha256sum | trunc 10 }}
{{- end -}}
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
