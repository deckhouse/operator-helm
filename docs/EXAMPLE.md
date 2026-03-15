---
title: "Examples"
description: "Deckhouse Kubernetes Platform — usage examples for the operator-helm module."
weight: 30
---

## Adding a Helm Repository

{{< alert level="warning" >}}
In the MVP stage, only Helm repositories (using schema "http(s)://") are supported as chart sources. Support for OCI registries (using schema "oci://") will be added in the alpha version.
{{< /alert >}}

To add a repository, you need to create a HelmClusterAddonRepository resource:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
```

After creating the repository, you can view the Helm charts available in it using the command below:

```shell
kubectl get helmclusteraddoncharts.helm.deckhouse.io -l repository=podinfo
NAME              AGE
podinfo-podinfo   56s
```

To view the list of versions available for a specific chart, run the following command:

```shell
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  creationTimestamp: "2026-03-12T02:24:04Z"
  generation: 1
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-podinfo
  ownerReferences:
  - apiVersion: helm.deckhouse.io/v1alpha1
    blockOwnerDeletion: true
    controller: true
    kind: HelmClusterAddonRepository
    name: podinfo
    uid: d5e026f9-6151-4f9f-a4bc-756d96b86e95
  resourceVersion: "28306911"
  uid: 7f232359-5553-463e-beed-d6f175596b0b
status:
  versions:
  - digest: a5c4b7381a0907128243354ab100d2eecc480d7dcac5014ff7272b0acef03780
    pulled: false
    version: 6.11.0
  - digest: 9f1cdb52fc5a57848f377b146919f8eb2c4a2c0ab8815bd019ec41c1d1895c0c
    pulled: false
    version: 6.10.2
```

## Deploying an Application

To deploy an application, you must create a `HelmClusterAddon` resource, specifying the name of the previously created repository, the chart name and version, and the namespace where the application will be deployed.

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
Only one instance of `HelmClusterAddon` using a specific Helm chart from a specific repository can be deployed at a time. However, different Helm charts from the same repository can be deployed simultaneously.
{{< /alert >}}

{{< alert level="info" >}}
It is permissible to omit a specific chart version in the .spec.chart.version parameter. In this case, the latest version of the application will be installed.
{{< /alert >}}