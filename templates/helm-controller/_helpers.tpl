{{- define "helm-controller.envs" -}}
- name: RUNTIME_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- end }}
