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

{{- define "operator-helm.enable_rbacv2" -}}
  {{- $raw := (.Values.global).deckhouseVersion | default "dev" | toString -}}
  {{- $mm := regexFind "^v?[0-9]+[.][0-9]+" $raw -}}
  {{- if $mm -}}
    {{- semverCompare ">= 1.78" (printf "%s.0" $mm) -}}
  {{- else -}}
    {{- /* "dev" or "unknown": a build off any branch says the same, so answer with the model
           whose mistake only loses access. A dev stand below 1.78 flips this to false. */ -}}
    true
  {{- end -}}
{{- end -}}
