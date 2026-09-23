---
title: "User guide"
description: "Deckhouse Platform — installing Helm charts in your own namespace with the operator-helm module."
weight: 50
---

This guide describes how to work with the resources of the module within a namespace: chart repositories, their catalogs and applications. Working with these custom resources requires permissions no lower than [`Admin`](/modules/user-authz/#current-role-based-model) in your namespace.

## Adding an application repository

A repository is the entry point for every other resource: until one is added, there is no chart to pick.

Create a [`HelmApplicationRepository`](/modules/operator-helm/cr.html#helmapplicationrepository) resource in your namespace:

{{< tabs name="create-application-repository" >}}
{{% tab name="Command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationRepository
metadata:
  name: podinfo
  namespace: test
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="Web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Repositories".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary repository name in the "Name" field.
1. Enter the URL of the chart repository in the "URL" field.
1. Click the "Apply" button.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
Two schemes can be used in a repository URL: `http(s)://` (a Helm repository that publishes an `index.yaml` file listing the available Helm charts) and `oci://` (a container registry that supports storing Helm charts).
{{< /alert >}}

The module synchronizes the repository and creates one [`HelmApplicationChart`](/modules/operator-helm/cr.html#helmapplicationchart) object per chart found. To view the charts of a repository:

{{< tabs name="list-application-charts" >}}
{{% tab name="Command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplicationcharts -l repository=podinfo
```

Example output:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

The name of a catalog object is composed of the repository name, the chart name and a hash, so it is more convenient to select a chart by the `repository` and `chart` labels than by name.

The available chart versions are listed in its status. To print them:

```shell
d8 k -n test get helmapplicationchart -l repository=podinfo,chart=podinfo -o yaml
```

Example output:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationChart
metadata:
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-chart-podinfo-dfbe83e63b0b
  namespace: test
status:
  versions:
    - version: 6.11.0
    - version: 6.10.2
```

{{% /tab %}}

{{% tab name="Web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Charts".

{{% /tab %}}
{{< /tabs >}}

### Checking the repository state

The state of a repository is reflected by the conditions in its status. To assess the state of a repository:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="Command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplicationrepository podinfo -o yaml
```

Example output:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationRepository
metadata:
  creationTimestamp: "2026-09-22T13:41:25Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 1
  name: podinfo
  namespace: test
  resourceVersion: "48844673"
  uid: f081a6d7-610a-4996-a10c-3d663928f027
spec:
  url: https://stefanprodan.github.io/podinfo
status:
  chartCount: 1
  conditions:
  - lastTransitionTime: "2026-09-22T13:41:26Z"
    message: ""
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T13:41:25Z"
    message: ""
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Synced
  lastSuccessfulSyncTime: "2026-09-22T13:41:25Z"
  nextSyncTime: "2026-09-22T13:46:10Z"
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="Web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Repositories".
1. Select the repository you need and hover the mouse over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible repository states" >}}

| Condition | Value | Reason | What it means |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | The repository is reachable and the chart catalog is built. You can select a chart for an application. |
| `Ready` | `Unknown` | `AwaitingInitialSync` | The repository has just been created and the first read has not finished yet. Wait for the synchronization to complete. |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | The auxiliary secret holding the repository credentials could not be created. Check your permissions in the namespace. |
| `Synced` | `True` | `Success` | The chart catalog matches the contents of the repository. |
| `Synced` | `False` | `SyncFailed` | The repository could not be read. Check the URL and that the registry is reachable from the cluster. |
| `Synced` | `False` | `CatalogUpdateFailed` | The repository was read, but the chart catalog could not be written to the cluster. The attempt will be repeated automatically. |
| `Synced` | `False` | `PartialSync` | Some versions could not be parsed during the first read. The rest are already available, and the skipped ones will be picked up at the next synchronization. |
| `Reconciling` | `True` | `Synchronization` | A scheduled synchronization with the repository is in progress. |
| `Reconciling` | `True` | `ForceReconcile` | A manually requested synchronization is in progress. |
| `Reconciling` | `True` | `ProgressingWithRetry` | The previous attempt failed and a retry is scheduled. |
| `Stalled` | `True` | `UnsupportedRepositoryType` | The scheme in the URL is not supported. Only `http(s)://` and `oci://` are allowed. |
| `Stalled` | `True` | `InvalidRepositoryURL` | The URL could not be parsed. Check the repository address. |
| `Stalled` | `True` | `AuthenticationFailed` | The registry rejected the credentials. Check the username and the password in the repository spec. |
| `Stalled` | `True` | `SourceNotFound` | No repository was found at the given URL. |
| `Stalled` | `True` | `SourceRejectedRequest` | The registry rejected the request. Contact the registry owner. |
| `Stalled` | `True` | `RetriesExceeded` | The read attempts are exhausted. Fix the cause and request a forced reconciliation. |

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply: the first until the work is finished, the second until the cause of the failure is fixed.

{{< /alert >}}

{{< /details >}}

## Deploying an application

Create a [`HelmApplication`](/modules/operator-helm/cr.html#helmapplication) resource in the same namespace, specifying the repository and the chart name and version:

{{< tabs name="create-application" >}}
{{% tab name="Command line" %}}

Run the following command:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplication
metadata:
  name: podinfo
  namespace: test
spec:
  chart:
    name: podinfo
    repository: podinfo
    version: 6.10.2
EOF
```

{{< alert level="info" >}}
Applications can be deployed not only from the Helm charts of a repository local to the namespace, but also from a shared repository set up by a platform administrator. Shared repositories are described with the [`HelmClusterApplicationRepository`](/modules/operator-helm/cr.html#helmclusterapplicationrepository) resource, and their catalog with [`HelmClusterApplicationChart`](/modules/operator-helm/cr.html#helmclusterapplicationchart) resources.

Every user in a namespace has read access to [`HelmClusterApplicationChart`](/modules/operator-helm/cr.html#helmclusterapplicationchart).

To use a chart from a shared repository, specify the [`spec.chart.clusterRepository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-clusterrepository) field instead of [`spec.chart.repository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-repository) when describing the `HelmApplication` resource.

{{< details summary="Viewing the available shared application Helm charts" >}}

To view the shared Helm charts, run the following command:

```shell
d8 k get helmclusterapplicationcharts --show-labels
```

Example output:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo-shared
```

{{< /details >}}

{{< /alert >}}

{{% /tab %}}

{{% tab name="Web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Applications".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary resource name in the "Name" field.
1. In the "Repository" field, select the repository holding the application Helm charts, or a shared application repository created by an administrator.
1. In the "Chart" field, select the Helm chart.
1. In the "Version" field, select the Helm chart version.
1. Click the "Create" button.

{{< alert level="info" >}}
Some repositories in the list may carry the "(cluster)" suffix. This means that the repository is shared ([`HelmClusterApplicationRepository`](/modules/operator-helm/cr.html#helmclusterapplicationrepository)) and was created by a platform administrator.
{{< /alert >}}

{{< alert level="info" >}}
You can adjust the Helm chart parameters if needed. To see the parameters used by default, click the "Show default values" link in the application creation form.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
A [`HelmApplication`](/modules/operator-helm/cr.html#helmapplication) is deployed with full privileges within the namespace.

{{< details summary="The rules of the role used when deploying an application" >}}

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  labels:
    helm.deckhouse.io/managed-by: operator-helm
  name: operator-helm-application
  namespace: test
rules:
- apiGroups:
  - '*'
  resources:
  - '*'
  verbs:
  - '*'
```

{{< /details >}}

{{< /alert >}}

### Checking the application state

The state of an application is reflected by the conditions in its status. To assess the state of an application:

{{< tabs name="check-application-conditions" >}}
{{% tab name="Command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplication podinfo -o yaml
```

Example output:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplication
metadata:
  creationTimestamp: "2026-09-18T08:19:55Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 1
  name: podinfo
  namespace: test
  resourceVersion: "48926122"
  uid: f4059036-a6c9-401a-be8d-67d8f2410c6f
spec:
  chart:
    repository: podinfo
    name: podinfo
    version: 6.15.0
status:
  conditions:
  - lastTransitionTime: "2026-09-22T15:28:11Z"
    message: Helm upgrade succeeded for release test/hap-podinfo-3fb7b289386f.v2
      with chart podinfo@6.15.0
    observedGeneration: 1
    reason: UpgradeSucceeded
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-18T08:20:03Z"
    message: Helm install succeeded for release test/hap-podinfo-3fb7b289386f.v1
      with chart podinfo@6.15.0
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  lastAppliedChart:
    repository: podinfo
    name: podinfo
    version: 6.15.0
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="Web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Applications".
1. Select the application you need and hover the mouse over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible application states" >}}

| Condition | Value | Reason | What it means |
| --- | --- | --- | --- |
| `Ready` | `True` | `InstallSucceeded`, `UpgradeSucceeded` | The release is deployed and matches the spec. The reason here is supplied by Helm. |
| `Ready` | `Unknown` | `Reconciling` | Work is in progress: the chart is being downloaded or the release is being rolled out. |
| `Ready` | `False` | `ReleaseFailed` | Helm could not install or upgrade the release. The error text is given in the `message` field. |
| `Ready` | `False` | `TestFailed` | The chart tests failed. |
| `Ready` | `False` | `Remediated` | The release was rolled back to its previous state. |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | The chart could not be downloaded from the repository or stored in the cluster. |
| `Ready` | `False` | `OCIFetchFailed`, `OCIIncludeUnavailable`, `OCIStorageFailed`, `OCIVerificationFailed` | The chart could not be retrieved from the OCI registry or verified. |
| `Ready` | `False` | `ChartVersionRemoved` | The specified chart version is no longer published by the repository. Select another version. |
| `Ready` | `False` | `RBACSetupFailed` | The `ServiceAccount`, `Role` or `RoleBinding` used to install the chart could not be prepared. The attempt will be repeated automatically. |
| `Ready` | `False` | `ForeignRBACObject` | The name of the `Role` or the `RoleBinding` that the module creates for the application is taken. The `Role` is always named `operator-helm-application`, and the name of the `RoleBinding` matches the name of the application's `ServiceAccount` and is given in the `message` field. An object with such a name was not created by the module, so the module does not touch it. Delete the foreign object and request a forced reconciliation. |
| `Ready` | `False` | `UnsupportedRepositoryType` | The repository the application refers to has an unreadable URL. Contact the repository owner. |
| `Ready` | `False` | `Failed` | Other errors. The cause is given in the `message` field. |
| `Installed` | same as `Ready` | same as for `Ready` | The outcome of the first installation of the release. |
| `UpdateInstalled` | same as `Ready` | same as for `Ready` | The outcome of a release upgrade. Appears when the chart version changes. |
| `ConfigurationApplied` | same as `Ready` | same as for `Ready` | The outcome of applying the chart values. Appears when the values change. |
| `Managed` | `True` | `MaintenanceModeInactive` | The application is managed by the module. |
| `Managed` | `False` | `MaintenanceModeActive` | Maintenance mode is on, reconciliation is paused. |
| `Reconciling` | `True` | `Reconciling` | The release is being rolled out. |
| `Reconciling` | `True` | `ProgressingWithRetry` | A failure occurred and a retry is scheduled. |
| `Reconciling` | `True` | `ForceReconcile` | A manually requested reconciliation is in progress. |
| `Stalled` | `True` | the reason for the failure that caused it | Retrying will not help: you have to fix the application spec, wait for the repository to change, or remove the object standing in the way. Attempts stop until the cause is resolved. |

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply: the first until the work is finished, the second until the cause of the failure is fixed. `Installed`, `UpdateInstalled` and `ConfigurationApplied` appear as the application passes the corresponding stages and carry the same verdict as `Ready`.

{{< /alert >}}

{{< /details >}}

## Forcing reconciliation

While working with applications and repositories, you may need to force a reconciliation. In normal operation, reconciliation starts automatically whenever the resources are changed or the state of their dependencies changes.

For applications, a forced reconciliation can be useful if a terminal error occurred while deploying the application or changing its settings. Without manual intervention, the module's controllers make no further reconciliation attempts.

For repositories, a forced reconciliation lets you synchronize the repository without waiting for the next scheduled run.

{{< tabs name="force-reconcile-application" >}}
{{% tab name="Command line" %}}

To force the reconciliation of a [`HelmApplication`](/modules/operator-helm/cr.html#helmapplication), run the following command:

```shell
d8 k -n test annotate helmapplication podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

To force the reconciliation of a [`HelmApplicationRepository`](/modules/operator-helm/cr.html#helmapplicationrepository), run the following command:

```shell
d8 k -n test annotate helmapplicationrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
The module only checks that the annotation is present; it does not read its contents. The timestamp in the examples is there only to make a repeated request differ from the previous one.
{{< /alert >}}

{{< alert level="info" >}}
The completion of a forced reconciliation can be tracked through the [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-status-lastforcereconciletime) field of the resource. For example:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{< /alert >}}

{{% /tab %}}

{{% tab name="Web interface" %}}

To force the reconciliation of an application:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Applications".
1. Select the application you need and click the "Force reconciliation" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

To force the reconciliation of an application repository:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Repositories".
1. Select the repository you need and click the "Force reconciliation" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

{{< alert level="info" >}}

Reconciliation can be very fast, so the web interface may not have time to show the status change. To make sure that the forced synchronization has been carried out, check the value of the `.status.lastForceReconcileTime` field of the resource. To do this, click the name of the resource you are interested in and switch to the "YAML" tab in the form that opens.

{{< /alert >}}

{{% /tab %}}

{{< /tabs >}}

## Maintenance mode

Maintenance mode pauses the reconciliation of an application, which lets you modify the release manually by adjusting the parameters of the previously deployed resources (changing the number of replicas, changing parameters and so on).

{{< tabs name="enable-application-maintenance" >}}
{{% tab name="Command line" %}}

To turn maintenance mode on, run the following command:

```shell
d8 k -n test patch helmapplication podinfo --type=merge -p '{"spec":{"maintenance":"NoResourceReconciliation"}}'
```

To check that maintenance mode is on, run the following command:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Example of successful output:

```text
MaintenanceModeActive
```

To turn maintenance mode off, run the following command:

```shell
d8 k -n test patch helmapplication podinfo --type=json -p '[{"op":"remove","path":"/spec/maintenance"}]'
```

To check that maintenance mode is off, run the following command:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Example of successful output:

```text
MaintenanceModeInactive
```

{{% /tab %}}

{{% tab name="Web interface" %}}

To manage the maintenance mode of an application:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Helm operator" → "Applications".
1. Select the application you need and click its name.
1. The "Maintenance mode" option is available in the form that opens.

An application that is in maintenance mode gets the "Maintenance" status.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
An application in maintenance mode does not support forced reconciliation and cannot be deleted.
{{< /alert >}}
