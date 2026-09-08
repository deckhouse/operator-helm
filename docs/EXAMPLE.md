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
NAME                                                AGE   LABELS
podinfo-chart-podinfo                               11d   chart=podinfo,heritage=deckhouse,repository=podinfo
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
    - version: 6.11.0
    - version: 6.10.2
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

### Observing a forced reconciliation

While a forced pass is running, the resource carries the `Reconciling` condition with the reason `ForceReconcile`:

```shell
d8 k get helmclusteraddonrepository podinfo -o jsonpath='{.status.conditions[?(@.type=="Reconciling")]}'
```

A synchronization that runs on the ordinary schedule raises the same condition with the reason `Synchronization`, so the reason tells the two apart.

Once the pass finishes, that condition is removed and `.status.lastForceReconcileTime` records when the request was processed:

```shell
d8 k get helmclusteraddonrepository podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

The timestamp records that the request was acted on, not that it succeeded — the outcome is reported by the `Ready` and `Synced` conditions.

{{< alert level="warning" >}}
A HelmClusterAddon in maintenance mode (`.spec.maintenance: NoResourceReconciliation`) is not reconciled at all, so a force request on it cannot be honoured. The controller discards the annotation instead of holding it until maintenance is lifted, and `.status.lastForceReconcileTime` is left untouched. Lift maintenance first, then request the reconciliation.
{{< /alert >}}

## Deploying a version published in an OCI registry

A classic HTTP repository may publish some of its chart versions in an OCI registry. Such an entry names the artifact in `urls` instead of pointing at a `.tgz` archive:

```yaml
apiVersion: v1
entries:
  airflow:
    - name: airflow
      version: 25.0.2
      urls:
        - oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2
```

Nothing about the HelmClusterAddonRepository or the HelmClusterAddon changes — the repository is still added by its HTTP url, and the addon still asks for the version by name:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: bitnami
spec:
  url: https://charts.example.com/bitnami
```

The version is recorded in the catalog with the reference the index gave it:

```console
d8 k get helmclusteraddonchart bitnami-airflow -o jsonpath='{.status.versions[0]}'
{"ociRef":"oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2","version":"25.0.2"}
```

When an addon asks for that version, the controller pulls it from the registry rather than from the repository. The registry has to be publicly readable: the repository credentials are not sent to a host that only its index names.
