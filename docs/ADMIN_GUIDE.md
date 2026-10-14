---
title: "Administrator guide"
description: "Managing the cluster-wide resources of the operator-helm module: repositories, chart catalogs and addons."
weight: 40
---

This guide describes how to work with the `operator-helm` module using cluster-wide resources. Working with these custom resources requires permissions no lower than the [`ClusterAdmin`](/modules/user-authz/#current-role-based-model) role.

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
1. Go to "Helm operator" → "Addon repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
Use one of the two schemes in the repository URL:

- `http(s)://`: Helm repository that publishes an `index.yaml` file listing the available Helm charts.
- `oci://`: Container registry that supports storing Helm charts.
{{< /alert >}}

After the HelmClusterAddonRepository resource is created, the module synchronizes the repository and creates a separate [HelmClusterAddonChart](/modules/operator-helm/cr.html#helmclusteraddonchart) object for each chart in the repository. To view the charts in a repository:

{{< tabs name="list-addon-charts" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k get helmclusteraddoncharts -l repository=podinfo
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
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-chart-podinfo-dfbe83e63b0b
status:
  versions:
    - version: 6.11.0
    - version: 6.10.2
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Helm operator" → "Addon charts".

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
  nextSyncTime: "2026-09-22T15:34:09Z"
  observedGeneration: 2
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Repositories".
1. Select the repository you need and hover over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible repository states" >}}

| Condition | Value | Reason | Description |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | The repository is reachable and the chart catalog has been built. You can select a chart for an addon |
| `Ready` | `Unknown` | `AwaitingInitialSync` | The repository has just been created and the first read has not finished yet. Wait for the synchronization to complete |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | The auxiliary secret holding the repository credentials could not be created. Check your permissions in the namespace |
| `Synced` | `True` | `Success` | The chart catalog matches the contents of the repository |
| `Synced` | `False` | `SyncFailed` | The repository could not be read. Check the URL and that the repository is reachable from the cluster |
| `Synced` | `False` | `CatalogUpdateFailed` | The repository has been read, but the chart catalog could not be written to the cluster. The attempt will be repeated automatically |
| `Synced` | `False` | `PartialSync` | Some versions could not be parsed during the first read. The rest versions are already available, and the skipped ones will be picked up at the next synchronization |
| `Reconciling` | `True` | `Synchronization` | A scheduled reconciliation is in progress |
| `Reconciling` | `True` | `ForceReconcile` | A forced reconciliation is in progress |
| `Reconciling` | `True` | `ProgressingWithRetry` | The previous reconciliation attempt has failed and a retry is scheduled |
| `Stalled` | `True` | `UnsupportedRepositoryType` | The scheme in the URL is not supported. Only `http(s)://` and `oci://` are allowed |
| `Stalled` | `True` | `InvalidRepositoryURL` | The URL could not be parsed. Check the repository address |
| `Stalled` | `True` | `AuthenticationFailed` | The repository rejected the credentials. Check the username and the password in the repository specification |
| `Stalled` | `True` | `SourceNotFound` | No repository was found at the given URL |
| `Stalled` | `True` | `SourceRejectedRequest` | The repository rejected the request. Contact the repository owner |
| `Stalled` | `True` | `RetriesExceeded` | The number of read attempts has been exceeded. Fix the cause and request a forced reconciliation |

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply. The `Reconciling` condition is present until the work is finished, the `Stalled` condition is present until the cause of the failure is fixed.

{{< /alert >}}

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
1. Go to "Helm operator" → "Addons".
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
  creationTimestamp: "2026-09-22T09:50:34Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 5
  name: podinfo
  resourceVersion: "48927375"
  uid: 5365281f-0f8c-4d3d-b174-5096d6bf255d
spec:
  chart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo-helm-repository
    version: 6.15.0
  maintenance: ""
  namespace: default
status:
  conditions:
  - lastTransitionTime: "2026-09-22T15:29:56Z"
    message: Helm upgrade succeeded for release default/podinfo.v2 with chart podinfo@6.15.0
    observedGeneration: 5
    reason: UpgradeSucceeded
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T09:50:41Z"
    message: Helm install succeeded for release default/podinfo.v1 with chart podinfo@6.15.0
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  - lastTransitionTime: "2026-09-22T10:30:50Z"
    message: Maintenance mode disabled
    observedGeneration: 5
    reason: MaintenanceModeInactive
    status: "True"
    type: Managed
  lastAppliedChart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo-helm-repository
    version: 6.15.0
  lastForceReconcileTime: "2026-09-22T10:53:51Z"
  observedGeneration: 5
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "System" tab.
1. Go to "Helm operator" → "Addons".
1. Select the addon you need and hover over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible addon states" >}}

| Condition | Value | Reason | Description |
| --- | --- | --- | --- |
| `Ready` | `True` | `InstallSucceeded`, `UpgradeSucceeded` | The release has been deployed and matches the specification. The reason is provided by Helm |
| `Ready` | `Unknown` | `Reconciling` | The chart is being downloaded or the release is being rolled out |
| `Ready` | `False` | `ReleaseFailed` | Helm could not install or upgrade the release. The error text is given in the `message` field |
| `Ready` | `False` | `TestFailed` | The chart tests failed |
| `Ready` | `False` | `Remediated` | The release was rolled back to its previous state |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | The chart could not be downloaded from the repository or stored in the cluster |
| `Ready` | `False` | `OCIFetchFailed`, `OCIIncludeUnavailable`, `OCIStorageFailed`, `OCIVerificationFailed` | The chart could not be retrieved from the OCI registry or verified |
| `Ready` | `False` | `ChartVersionRemoved` | The specified chart version is no longer published by the repository. Select another version |
| `Ready` | `False` | `ChartClaimConflict` | This chart of this repository is already deployed by another addon: a single repository–chart pair can be served by only one HelmClusterAddon. The resource holding it is named in the `message` field. The state resolves on its own within half a minute after that addon is deleted or pointed at another chart |
| `Ready` | `False` | `UnsupportedRepositoryType` | The repository the addon refers to has an unreadable URL. Contact the repository owner |
| `Ready` | `False` | `Failed` | Other errors. The cause is given in the `message` field |
| `Installed` | Same as `Ready` | Same as for `Ready` | The outcome of the first installation of the release |
| `UpdateInstalled` | Same as `Ready` | Same as for `Ready` | The outcome of a release upgrade. Appears when the chart version changes |
| `ConfigurationApplied` | Same as `Ready` | Same as for `Ready` | The outcome of applying the chart values. Appears when the values change |
| `Managed` | `True` | `MaintenanceModeInactive` | The addon is managed by the module |
| `Managed` | `False` | `MaintenanceModeActive` | Maintenance mode is on, reconciliation is paused |
| `Reconciling` | `True` | `Reconciling` | The release is being rolled out |
| `Reconciling` | `True` | `ProgressingWithRetry` | A failure occurred and a retry is scheduled |
| `Reconciling` | `True` | `ForceReconcile` | A manually requested reconciliation is in progress |
| `Stalled` | `True` | The failure cause | Fix the addon specification, wait for the repository to change, or remove the object standing in the way. Attempts are stopped until the cause is resolved |

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply: `Reconciling` is present until the work is finished, `Stalled` is present until the cause of the failure is fixed. The `Installed`, `UpdateInstalled` and `ConfigurationApplied` conditions appear as the addon passes the corresponding stages and carry the same verdict as `Ready`.

{{< /alert >}}

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
1. Go to "Helm operator" → "Application repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
Use one of the two schemes in the repository URL:

- `http(s)://`: Helm repository that publishes an `index.yaml` file listing the available Helm charts.
- `oci://`: Container registry that supports storing Helm charts.
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
1. Go to "Helm operator" → "Application repositories".
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
  url: oci://registry.example.com/charts
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
1. Go to "Helm operator" → "Addon repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. In the "Authentication" section, specify the credentials for accessing the repository.
1. In the "CA certificate" section, specify the CA certificate in PEM format, or use the certificate upload form by clicking the "click to upload" link.
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
The annotation value is ignored. The module only checks that the annotation is present on the resource. The time stamp in the examples in only to make one request differ from another.
{{< /alert >}}

The completion of a forced reconciliation can be tracked through the [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmclusteraddon-v1alpha1-status-lastforcereconciletime) field of the resource. For example:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

To force the reconciliation of an addon:

1. Go to the "System" tab.
1. Go to "Helm operator" → "Addons".
1. Select the addon you need and click the "Force reconciliation" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

To force the reconciliation of an addon repository:

1. Go to the "System" tab.
1. Go to "Helm operator" → "Addon repositories".
1. Select the repository you need and click the "Force reconciliation" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

{{< alert level="info" >}}
Reconciliation can be very fast, so the web interface may not have time to show the status change. To make sure that the forced synchronization has been carried out, check the value of the `.status.lastForceReconcileTime` field of the resource. To do this, click the name of the resource you are interested in and switch to the "YAML" tab in the form that opens.
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
1. Go to "Helm operator" → "Addons".
1. Select the addon you need and click its name.
1. The "Maintenance mode" option is available in the form that opens.

An addon that is in maintenance mode gets the "Maintenance" status.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
An addon in maintenance mode does not support forced reconciliation and cannot be deleted.
{{< /alert >}}
