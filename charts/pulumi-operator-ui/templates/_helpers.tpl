{{- define "pou.fullname" -}}
{{- if contains "pulumi-operator-ui" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-pulumi-operator-ui" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pou.selectorLabels" -}}
app.kubernetes.io/name: pulumi-operator-ui
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pou.labels" -}}
{{ include "pou.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "pou.validate" -}}
{{- $_ := required "database.urlSecret.name is required" .Values.database.urlSecret.name -}}
{{- $_ := required "oidc.issuer is required" .Values.oidc.issuer -}}
{{- $_ := required "oidc.clientID is required" .Values.oidc.clientID -}}
{{- $_ := required "oidc.redirectURL is required" .Values.oidc.redirectURL -}}
{{- $_ := required "oidc.clientSecret.name is required" .Values.oidc.clientSecret.name -}}
{{- $_ := required "session.keySecret.name is required" .Values.session.keySecret.name -}}
{{- if not .Values.auth.allowed -}}
{{- fail "auth.allowed must list at least one claim value" -}}
{{- end -}}
{{- end -}}

{{- define "pou.rules" -}}
- apiGroups: ["pulumi.com"]
  resources: ["stacks"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["auto.pulumi.com"]
  resources: ["updates"]
  verbs: ["get", "list", "watch"]
{{- end -}}
