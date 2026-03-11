{{- /* Return logLevel as a string. */}}
{{- define "moduleLogLevel" -}}
{{- dig "logLevel" "" .Values.operatorHelm -}}
{{- end }}

{{- define "priorityClassName" -}}
system-cluster-critical
{{- end }}

{{- define "vpa.policyUpdateMode" -}}
{{-   $kubeVersion := .Values.global.discovery.kubernetesVersion -}}
{{-   $updateMode := "" -}}
{{-   if semverCompare ">=1.33.0" $kubeVersion -}}
{{-     $updateMode = "InPlaceOrRecreate" -}}
{{-   else -}}
{{-     $updateMode = "Recreate" -}}
{{-   end }}
{{- $updateMode }}
{{- end }}
