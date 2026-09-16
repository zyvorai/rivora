{{- define "rivora.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "rivora.fullname" -}}
{{- .Release.Name -}}
{{- end -}}

{{- define "rivora.labels" -}}
app.kubernetes.io/name: {{ include "rivora.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "rivora.rivorad.labels" -}}
{{ include "rivora.labels" . }}
app.kubernetes.io/component: rivorad
{{- end -}}

{{- define "rivora.rivorad.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rivora.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: rivorad
{{- end -}}

{{- define "rivora.controller.labels" -}}
{{ include "rivora.labels" . }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "rivora.controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rivora.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "rivora.rivorad.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{ include "rivora.fullname" . }}-rivorad
{{- else -}}
default
{{- end -}}
{{- end -}}

{{- define "rivora.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{ include "rivora.fullname" . }}-controller
{{- else -}}
default
{{- end -}}
{{- end -}}
