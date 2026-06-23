{{/*
The DRA driver name. Every cluster/API object keys off this value so that two
installs under different driver names are fully isolated.
*/}}
{{- define "cxi-dra.driverName" -}}
{{- required "driverName is required" .Values.driverName -}}
{{- end -}}

{{/*
Base name for all owned objects, derived from the driver name (sanitized to a
DNS-1123 label) unless overridden. e.g. "cxi.slingshot.hpe.com" -> "cxi-slingshot-hpe-com".
Truncated to 63 chars. Override with nameOverride/fullnameOverride.
*/}}
{{- define "cxi-dra.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if .Values.nameOverride -}}
{{- .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $sanitized := regexReplaceAll "[^a-z0-9]+" (include "cxi-dra.driverName" . | lower) "-" -}}
{{- $sanitized | trimAll "-" | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Name of the leader-election Lease used by the namespace VNI allocator. Must
match what the driver derives at runtime: SetDriverName sets it to
<driverName>-vni-allocator-leader (see internal/driver/vniallocator.go).
*/}}
{{- define "cxi-dra.leaseName" -}}
{{- printf "%s-vni-allocator-leader" (include "cxi-dra.driverName" .) -}}
{{- end -}}

{{/*
Name of this install's DeviceClass. ResourceClaimTemplates reference it.
*/}}
{{- define "cxi-dra.deviceClassName" -}}
{{- printf "%s-device" (include "cxi-dra.fullname" .) -}}
{{- end -}}

{{/*
Standard labels applied to every object.
*/}}
{{- define "cxi-dra.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "cxi-dra.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — release-scoped via app.kubernetes.io/instance so each workload
owns only its own pods. This is what the workload .spec.selector uses; it does
NOT include the constant anti-affinity label.
*/}}
{{- define "cxi-dra.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ include "cxi-dra.fullname" . }}
{{- end -}}
