---
title: "Administrator guide"
description: "Managing the cluster-wide resources of the operator-helm module: repositories, chart catalogs and addons."
weight: 40
---

This guide describes how to work with the `operator-helm` module using cluster-wide resources.

{{< alert level="warning" >}}
Working with these custom resources requires permissions no lower than the [`SuperAdmin`](/modules/user-authz/#current-role-based-model) role.

In the [granular role-based model](/modules/user-authz/#granular-role-based-model) (Deckhouse Platform 1.78 and later), managing [HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon), [HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository) and [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository) resources requires the `d8:subsystem:delivery:superadmin` role. The `d8:subsystem:delivery:viewer` role grants read access to HelmClusterAddon, HelmClusterAddonChart and HelmClusterApplicationChart resources.
{{< /alert >}}

## Adding an addon repository

A Helm repository is the entry point for every other resource. It contains Helm charts for a following installation in the cluster.

To add a new Helm addon repository, create a [HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository) resource:

{{< tabs name="create-addon-repository" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addon Repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
Use one of the two schemes in the repository URL:

- `http(s)://`: Helm repository that publishes an `index.yaml` file listing the available Helm charts.
- `oci://`: Repository of a single Helm chart in a container registry that supports storing Helm charts, for example `oci://registry.example.com/charts/podinfo`. The last path segment is the chart name, and the image tags are its versions.
{{< /alert >}}

After the HelmClusterAddonRepository resource is created, the module synchronizes the repository and creates a separate [HelmClusterAddonChart](/modules/operator-helm/cr.html#helmclusteraddonchart) object for each chart in the repository. To view the charts in a repository:

{{< tabs name="list-addon-charts" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k get helmclusteraddoncharts -l repository=podinfo --show-labels
```

Example output:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

The name of a catalog object is composed of the repository name, the chart name and a hash, so it is more convenient to select a chart by the `repository` and `chart` labels than by name.

The available chart versions are listed in its status. To see a list of versions, run the following command:

```shell
d8 k get helmclusteraddonchart -l repository=podinfo,chart=podinfo -o yaml
```

Example output:

```text
apiVersion: v1
items:
- apiVersion: helm.deckhouse.io/v1alpha1
  kind: HelmClusterAddonChart
  metadata:
    creationTimestamp: "2026-10-30T07:13:31Z"
    generation: 1
    labels:
      chart: podinfo
      heritage: deckhouse
      repository: podinfo
    name: podinfo-chart-podinfo-dfbe83e63b0b
    ownerReferences:
    - apiVersion: helm.deckhouse.io/v1alpha1
      blockOwnerDeletion: true
      controller: true
      kind: HelmClusterAddonRepository
      name: podinfo
      uid: e782d1df-cee1-4a35-bcb5-09c307bd06b4
    resourceVersion: "95969705"
    uid: 20111084-bc34-4f12-acbc-1f72cf328bc7
  status:
    versions:
    - version: 6.17.0
    - version: 6.16.0
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addon Charts".

{{% /tab %}}
{{< /tabs >}}

### Checking the repository state

The state of a repository is reflected by the conditions in its status ([`status.conditions`](cr.html#helmclusteraddonrepository-v1alpha1-status-conditions)). To assess the state of a repository:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k get helmclusteraddonrepository podinfo -o yaml
```

Example output:

```text
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  creationTimestamp: "2026-09-22T15:28:51Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 2
  name: podinfo
  resourceVersion: "48926557"
  uid: 0fbfec2f-6669-40ba-a7ef-0cd223aabfca
spec:
  url: https://stefanprodan.github.io/podinfo
status:
  chartCount: 1
  conditions:
  - lastTransitionTime: "2026-09-22T15:28:52Z"
    message: ""
    observedGeneration: 2
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T15:28:51Z"
    message: ""
    observedGeneration: 2
    reason: Success
    status: "True"
    type: Synced
  lastSuccessfulSyncTime: "2026-09-22T15:28:51Z"
  lastSyncedGeneration: 2
  nextSyncTime: "2026-09-22T15:34:09Z"
  observedGeneration: 2
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addon Repositories".
1. Select the repository you need and hover over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible repository states" >}}

| Condition | Value | Reason | Description |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | The repository has been read successfully. Whether the chart catalog is up to date is shown by the `Synced` condition |
| `Ready` | `Unknown` | `AwaitingInitialSync` | The repository has just been created and the first read has not finished yet. Wait for the synchronization to complete |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | The module could not create, update or delete the service secrets holding the repository credentials and CA certificate. The attempt will be repeated automatically. The cause is given in the `message` field |
| `Ready` | `False` | The `Stalled` reason | While the repository is stalled, `Ready` carries the same reason and message as `Stalled`, unless one of the failures described in the neighbouring rows is reported instead |
| `Ready` | `False` | `Reconciling` or the reason of the internal source | Only for an `http(s)://` repository. The internal object the module downloads charts through is not ready yet or reports an error. The details are given in the `message` field |
| `Ready` | `Unknown` | `Reconciling` | The repository is being deleted, and the module is removing its internal objects |
| `Ready` | `False` | `Failed` | The module could not create, update or delete the internal objects of the repository, including while the repository is being deleted. The attempt will be repeated automatically. The cause is given in the `message` field |
| `Synced` | `True` | `Success` | The chart catalog matches the contents of the repository |
| `Synced` | `False` | `SyncFailed` | The repository could not be read. Check the URL and that the repository is reachable from the cluster |
| `Synced` | `False` | `CatalogUpdateFailed` | The repository has been read, but the chart catalog could not be written to the cluster. The attempt will be repeated automatically |
| `Synced` | `False` | `PartialSync` | The chart catalog does not reflect the whole repository, the rest of the charts are available. The `message` field lists the causes: charts whose names cannot be recorded on a catalog object (they are skipped at every synchronization), versions not resolved during the first read, or versions not examined because the read ran out of time (the next synchronization continues in about a minute) |
| `Reconciling` | `True` | `Synchronization` | A scheduled reconciliation is in progress |
| `Reconciling` | `True` | `ForceReconcile` | A forced reconciliation is in progress |
| `Reconciling` | `True` | `ProgressingWithRetry` | The previous reconciliation attempt has failed and a retry is scheduled |
| `Reconciling` | `True` | `AwaitingInitialSync` | The first read of the repository has not finished yet |
| `Reconciling` | `True` | `Reconciling` | The module is waiting for its internal source object to become ready |
| `Stalled` | `True` | `UnsupportedRepositoryType` | The scheme in the URL is not supported. Only `http(s)://` and `oci://` are allowed |
| `Stalled` | `True` | `InvalidRepositoryURL` | The URL could not be parsed. Check the repository address |
| `Stalled` | `True` | `InvalidCACertificate` | The `caCertificate` field contains no PEM-encoded certificate |
| `Stalled` | `True` | `AuthenticationFailed` | The repository rejected the credentials. Check the username and the password in the repository specification |
| `Stalled` | `True` | `SourceNotFound` | No repository was found at the given URL |
| `Stalled` | `True` | `SourceRejectedRequest` | The repository rejected the request. Contact the repository owner |
| `Stalled` | `True` | `RetriesExceeded` | Several reads in a row have failed. Attempts continue at growing intervals, up to once an hour, and the condition is removed after the first successful read. To check the repository sooner after fixing the cause, request a forced reconciliation |
| `Stalled` | `True` | The reason of the internal source | Only for an `http(s)://` repository. The internal object the module downloads charts through has stopped retrying. The details are given in the `message` field |

{{< /details >}}

## Deploying an addon

To deploy an addon, create a [HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon) resource, specifying the repository, the chart name and version, and the namespace to deploy into:

{{< tabs name="create-addon" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
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
EOF
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addons".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary addon name in the "Name" field.
1. In the "Repository" field, select the repository holding the addon Helm charts.
1. In the "Chart" field, select the Helm chart.
1. In the "Version" field, select the Helm chart version.
1. In the "Namespace" field, select the namespace the chart resources will be deployed into.
1. Click the "Create" button.

{{< alert level="info" >}}
You can adjust the Helm chart parameters if needed. To see the parameters used by default, click "Show default values" in the addon creation form.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Only one HelmClusterAddon instance using a given Helm chart from a given repository can be deployed at a time. However, different Helm charts from the same repository can be deployed simultaneously.
{{< /alert >}}

### Checking the addon state

The state of an addon is reflected by the conditions in its status ([`status.conditions`](cr.html#helmclusteraddon-v1alpha1-status-conditions)). To assess the state of an addon:

{{< tabs name="check-addon-conditions" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k get helmclusteraddon podinfo -o yaml
```

Example output:

```text
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddon
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"helm.deckhouse.io/v1alpha1","kind":"HelmClusterAddon","metadata":{"annotations":{},"name":"podinfo"},"spec":{"chart":{"helmClusterAddonChart":"podinfo","helmClusterAddonRepository":"podinfo","version":"6.10.2"},"namespace":"test"}}
  creationTimestamp: "2026-10-30T07:18:01Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 1
  name: podinfo
  resourceVersion: "95974870"
  uid: ecf21939-1068-42de-a3a2-c812600f1303
spec:
  chart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo
    version: 6.10.2
  namespace: test
status:
  conditions:
  - lastTransitionTime: "2026-10-30T07:18:19Z"
    message: Helm install succeeded for release test/podinfo.v1 with chart podinfo@6.10.2
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-10-30T07:18:19Z"
    message: Helm install succeeded for release test/podinfo.v1 with chart podinfo@6.10.2
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  inventory:
  - apiVersion: apps/v1
    kind: Deployment
    name: podinfo
    namespace: test
  - apiVersion: v1
    kind: Service
    name: podinfo
    namespace: test
  lastAppliedChart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo
    version: 6.10.2
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addons".
1. Click the name of the addon you need. The addon page opens on the "Overview" tab. The "Lifecycle" block describes the current state of the release.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible addon states" >}}

| Condition | Value | Reason | Description |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | The release has been deployed and matches the specification |
| `Ready` | `Unknown` | `Reconciling` | The chart is being downloaded, the release is being rolled out, or the addon is being deleted and the release is being uninstalled |
| `Ready` | `False` | `ReleaseFailed` | Helm could not install or upgrade the release. The error text is given in the `message` field. Attempts stop (`Stalled`): fix the cause, then change the specification or request a forced reconciliation |
| `Ready` | `False` | `RetriesExceeded` | Helm has used up its attempts to deploy the release. Fix the cause and request a forced reconciliation |
| `Ready` | `False` | `RenderFailed` | The chart could not be rendered into manifests. Attempts stop (`Stalled`): fix the cause, then change the specification or request a forced reconciliation |
| `Ready` | `False` | `ChartNotFound` | The repository catalog has no chart with the name given in `spec.chart.helmClusterAddonChart` |
| `Ready` | `False` | `ChartVersionNotFound` | The chart has no version given in `spec.chart.version` in the repository catalog |
| `Ready` | `False` | `ChartVersionUnusable` | The version is listed in the repository catalog but cannot be installed. The cause is given in the `unavailableReason` field of the version in the chart catalog |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | The chart could not be downloaded from the repository or stored in the cluster, or the chart version has not been resolved in the repository catalog yet |
| `Ready` | `False` | `ChartSourceNotReady` | The internal chart source has nothing to install from yet |
| `Ready` | `False` | `OCIFetchFailed`, `OCIStorageFailed` | The chart could not be retrieved from the OCI registry or stored in the cluster |
| `Ready` | `False` | `UnsupportedChartArtifact`, `SourceNotFound`, `InvalidRepositoryURL`, `InvalidCACertificate` | The repository index points the chart version to an OCI registry, and the artifact there is not a Helm chart, is missing, has an invalid reference, or the CA certificate of the repository is invalid |
| `Ready` | `False`, `Unknown` | `ChartVersionRemoved` | Only for a chart stored in an OCI registry. The specified chart version is no longer published by the repository. Select another version |
| `Ready` | `False` | `HelmChartFailed`, `HelmReleaseFailed` | The module could not create or update its internal objects for the release. The attempt will be repeated automatically |
| `Ready` | `False` | `ChartClaimConflict` | This chart of this repository is already deployed by another addon: a single repository–chart pair can be served by only one HelmClusterAddon. The resource holding it is named in the `message` field. The state resolves on its own within half a minute after that addon is deleted or pointed at another chart |
| `Ready` | `False` | `UnsupportedRepositoryType` | The repository the addon refers to has an unreadable URL. Contact the repository owner |
| `Ready` | `False` | `MaintenanceModeActive` | Maintenance mode is on. `Ready` carries the same value as `Managed` |
| `Ready` | `False` | `UninstallFailed` | The addon is being deleted, and the release or the internal objects of the module could not be removed. The cause is given in the `message` field |
| `Ready` | `False` | `Failed` | Other errors. The cause is given in the `message` field |
| `Installed` | Same as `Ready` | `InstallSucceeded`, `Installing`, `InstallFailed` | The outcome of the first installation of the release |
| `UpdateInstalled` | Same as `Ready` | `UpgradeSucceeded`, `Upgrading`, `UpgradeFailed` | The outcome of a release upgrade. Appears when the chart, its version or the repository changes |
| `ConfigurationApplied` | Same as `Ready` | `ValuesApplied`, `ApplyingValues`, `ValuesApplyFailed` | The outcome of applying the chart values. Appears when the values change |
| `Managed` | `True` | `MaintenanceModeInactive` | Maintenance mode has been turned off, and the addon is managed by the module again |
| `Managed` | `False` | `MaintenanceModeActive` | Maintenance mode is on, reconciliation is paused |
| `UninstallFailed` | `True` | `UninstallFailed` | The addon is being deleted, and the release or the internal objects of the module could not be removed |
| `UninstallFailed` | `Unknown` | `Reconciling` | The addon is being deleted, and the release is being uninstalled |
| `Reconciling` | `True` | `Reconciling` | The release is being rolled out, or the module is waiting for a dependency |
| `Reconciling` | `True` | `ProgressingWithRetry` | A failure occurred and a retry is scheduled |
| `Reconciling` | `True` | `ChartVersionRemoved` | Only for a chart stored in an OCI registry. The specified chart version is no longer published by the repository, and the module is waiting for the outcome of the attempt to get it |
| `Reconciling` | `True` | `ForceReconcile` | A manually requested reconciliation is in progress |
| `Stalled` | `True` | The failure cause | Fix the addon specification, wait for the repository to change, or remove the object standing in the way, then request a forced reconciliation if needed. Attempts are stopped until the cause is resolved |

{{< /details >}}

## Adding a repository with application charts

By creating a [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository), a platform administrator can give namespace administrators centralized access to Helm charts. The Helm charts of such a repository become available to the administrators of every namespace for deploying [HelmApplication](/modules/operator-helm/cr.html#helmapplication).

To add an application chart repository, create a [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository) resource:

{{< tabs name="create-cluster-application-repository" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterApplicationRepository
metadata:
  name: podinfo-shared
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Application Repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
Use one of the two schemes in the repository URL:

- `http(s)://`: Helm repository that publishes an `index.yaml` file listing the available Helm charts.
- `oci://`: Repository of a single Helm chart in a container registry that supports storing Helm charts, for example `oci://registry.example.com/charts/podinfo`. The last path segment is the chart name, and the image tags are its versions.
{{< /alert >}}

The catalog of such a repository is published in [HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart) resources. To list all Helm charts available in a repository:

{{< tabs name="list-cluster-application-charts" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k get helmclusterapplicationcharts -l repository=podinfo-shared
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Application Repositories".
1. Select the repository you are interested in from the list and click its name.
1. In the form that opens, the "Charts" tab shows the list of the available Helm charts.

{{% /tab %}}
{{< /tabs >}}

For details on working with the charts in this repository, refer to the ["User guide"](user_guide.html) page.

## Connecting a private repository

To connect a private repository, specify the credentials and the TLS parameters in the repository specification. An example of configuring a repository with authentication and a self-signed certificate:

{{< tabs name="connect-private-repository" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: private
spec:
  url: oci://registry.example.com/charts/podinfo
  auth:
    username: admin
    password: secret
  caCertificate: |
    -----BEGIN CERTIFICATE-----
    ...
    -----END CERTIFICATE-----
EOF
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addon Repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. In the "Authentication" section, specify the credentials for accessing the repository.
1. In the "CA Certificate" section, specify the CA certificate in PEM format, or use the certificate upload form by clicking the "click to upload" link.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
The credentials are stored in the resource in plain text. A user that has a read-level access to a repository can view its credentials as well.
{{< /alert >}}

## Forcing reconciliation

While working with addons and repositories, you may need to force a reconciliation. In normal operation, reconciliation starts automatically whenever the resources are changed or the state of their dependencies changes.

For addons, a forced reconciliation can be useful if a terminal error occurred while deploying the addon or changing its settings. Without manual intervention, the module's controllers make no further reconciliation attempts.

For repositories, a forced reconciliation lets you synchronize the repository without waiting for the next scheduled run.

{{< tabs name="force-reconcile-addon" >}}
{{% tab name="Via command line" %}}

To force the reconciliation of a [HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon), add the annotation `reconcile.helm.deckhouse.io/force` to it by running the following command:

```shell
d8 k annotate helmclusteraddon podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

To force the reconciliation of a [HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository), add the annotation `reconcile.helm.deckhouse.io/force` to it by running the following command:

```shell
d8 k annotate helmclusteraddonrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
The annotation value is ignored. The module only checks that the annotation is present on the resource. The time stamp in the examples is only to make one request differ from another.
{{< /alert >}}

The processing of a forced reconciliation request is recorded in the [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmclusteraddon-v1alpha1-status-lastforcereconciletime) field of the resource. The outcome is reflected in the `Ready` condition for an addon and in the `Synced` condition for a repository. For example:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{< alert level="info" >}}
If the reconciliation of an addon stops at an error that retries cannot fix before the chart is processed (for example, `UnsupportedRepositoryType`), the module removes the annotation without updating the `status.lastForceReconcileTime` field. Check the `Ready` condition in that case.
{{< /alert >}}

{{% /tab %}}

{{% tab name="Via web interface" %}}

To force the reconciliation of an addon:

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addons".
1. Select the addon you need and click the "Force Reconcile" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

To force the reconciliation of an addon repository:

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addon Repositories".
1. Select the repository you need and click the "Force Reconcile" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

{{< alert level="info" >}}
Reconciliation can be very fast, so the web interface may not have time to show the status change. To make sure that the forced reconciliation request has been processed, check the value of the `.status.lastForceReconcileTime` field of the resource. The outcome of the synchronization is shown by the `Ready` and `Synced` conditions. To do this, click the name of the resource you are interested in and switch to the "YAML" tab in the form that opens.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

## Maintenance mode

Maintenance mode pauses the reconciliation of an addon, which lets you modify the release manually by adjusting the parameters of the previously deployed resources (such as the number of replicas and other parameters).

{{< tabs name="enable-addon-maintenance" >}}
{{% tab name="Via command line" %}}

To enable maintenance mode, run the following command:

```shell
d8 k patch helmclusteraddon podinfo --type=merge -p '{"spec":{"maintenance":"NoResourceReconciliation"}}'
```

To check that maintenance mode is on, run the following command:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Example of successful output:

```text
MaintenanceModeActive
```

To disable maintenance mode, run the following command:

```shell
d8 k patch helmclusteraddon podinfo --type=json -p '[{"op":"remove","path":"/spec/maintenance"}]'
```

To check that maintenance mode is off, run the following command:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Example of successful output:

```text
MaintenanceModeInactive
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

To manage the maintenance mode of an addon:

1. Go to the "System" tab.
1. Go to "Operator Helm" → "Addons".
1. Click the name of the addon you need.
1. The "Maintenance" option is available on the "Configuration" tab.

An addon that is in maintenance mode gets the "Maintenance" status.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
An addon in maintenance mode does not support forced reconciliation and cannot be deleted.
{{< /alert >}}
