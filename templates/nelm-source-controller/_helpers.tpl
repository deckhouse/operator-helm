{{- define "nelm-source-controller.envs" -}}
- name: RUNTIME_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: TUF_ROOT
  value: /tmp/.sigstore
{{- end }}
