---
title: "Examples"
description: "Deckhouse Kubernetes Platform — usage examples for the operator-helm module."
weight: 30
---

## Adding a Helm repository

To add a repository, create a HelmClusterAddonRepository resource:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
```

After creating the repository, view the available Helm charts:

```shell
d8 k get helmclusteraddoncharts.helm.deckhouse.io -l repository=podinfo
```

Example output:

```text
NAME              AGE
podinfo-podinfo   56s
```

To view the list of versions available for a specific chart:

```shell
d8 k get helmclusteraddonchart podinfo-podinfo -o yaml
```

Example output:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-podinfo
status:
  versions:
    - digest: a5c4b7381a0907128243354ab100d2eecc480d7dcac5014ff7272b0acef03780
      pulled: false
      version: 6.11.0
    - digest: 9f1cdb52fc5a57848f377b146919f8eb2c4a2c0ab8815bd019ec41c1d1895c0c
      pulled: false
      version: 6.10.2
```

## Deploying an application

To deploy an application, create a HelmClusterAddon resource specifying the repository name, chart name and version, and the target namespace:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddon
metadata:
  name: podinfo
spec:
  namespace: test
  chart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo
    version: 6.10.2
```

{{< alert level="warning" >}}
Only one instance of HelmClusterAddon using a specific Helm chart from a specific repository can be deployed at a time. Different Helm charts from the same repository can be deployed simultaneously.
{{< /alert >}}

{{< alert level="info" >}}
The `.spec.chart.version` parameter is optional. If omitted, the latest available version of the chart will be installed.
{{< /alert >}}

## Triggering a manual reconciliation

To trigger an immediate reconciliation of a resource without waiting for the next scheduled sync, annotate it with `reconcile.helm.deckhouse.io/force`. The controller will detect the annotation, run a full reconciliation cycle, and remove the annotation automatically once processing is complete.

To trigger reconciliation of a HelmClusterAddon:

```shell
d8 k annotate helmclusteraddon podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

To trigger reconciliation of a HelmClusterAddonRepository:

```shell
d8 k annotate helmclusteraddonrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
The annotation value is not significant — only its presence on the resource matters. The controller removes the annotation after the reconciliation is complete.
{{< /alert >}}
