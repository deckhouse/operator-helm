---
title: "User guide"
description: "Installing Helm charts in your own namespace with the operator-helm module."
weight: 50
---

This guide describes how to work with the namespaced resources of the module, including chart repositories, their catalogs and applications.

{{< alert level="warning" >}}
Working with the custom resources of the module requires permissions of no lower than [`Admin`](/modules/user-authz/#current-role-based-model) role in a designated namespace.

In the [granular role-based model](/modules/user-authz/#granular-role-based-model) (Deckhouse Platform 1.78 and later), the `d8:namespace:admin` role is enough to manage [HelmApplication](/modules/operator-helm/cr.html#helmapplication) resources, while viewing and managing [HelmApplicationRepository](/modules/operator-helm/cr.html#helmapplicationrepository) resources requires the `d8:namespace:superadmin` role.
{{< /alert >}}

## Adding an application repository

A Helm application repository is the entry point for every other resource.
It contains Helm charts for a following installation within a designated namespace.

To add a new Helm application repository, create a [HelmApplicationRepository](/modules/operator-helm/cr.html#helmapplicationrepository) resource in the target namespace:

{{< tabs name="create-application-repository" >}}
{{% tab name="Via command line" %}}

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

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Repositories".
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

After the HelmApplicationRepository resource is created, the module synchronizes the repository and creates a separate [HelmApplicationChart](/modules/operator-helm/cr.html#helmapplicationchart) object for each chart in the repository. To view the charts in a repository:

{{< tabs name="list-application-charts" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplicationcharts -l repository=podinfo --show-labels
```

Example output:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

The name of a catalog object is composed of the repository name, the chart name and a hash, so it is more convenient to select a chart by the `repository` and `chart` labels than by name.

The available chart versions are listed in its status. To see a list of versions, run the following command:

```shell
d8 k -n test get helmapplicationchart -l repository=podinfo,chart=podinfo -o yaml
```

Example output:

```text
apiVersion: v1
kind: List
items:
- apiVersion: helm.deckhouse.io/v1alpha1
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

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Charts".

{{% /tab %}}
{{< /tabs >}}

### Checking the repository state

The state of a repository is reflected by the conditions in its status ([`status.conditions`](cr.html#helmapplicationrepository-v1alpha1-status-conditions)). To assess the state of a repository:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplicationrepository podinfo -o yaml
```

Example output:

```text
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
  lastSyncedGeneration: 1
  nextSyncTime: "2026-09-22T13:46:10Z"
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Repositories".
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
| `Synced` | `False` | `CatalogUpdateFailed` | The repository was read, but the chart catalog could not be written to the cluster. The attempt will be repeated automatically |
| `Synced` | `False` | `PartialSync` | The chart catalog does not reflect the whole repository, the rest of the charts are available. The `message` field lists the causes: charts whose names cannot be recorded on a catalog object (they are skipped at every synchronization), versions not resolved during the first read, or versions not examined because the read ran out of time (the next synchronization continues in about a minute) |
| `Reconciling` | `True` | `Synchronization` | A scheduled reconciliation is in progress |
| `Reconciling` | `True` | `ForceReconcile` | A forced reconciliation is in progress |
| `Reconciling` | `True` | `ProgressingWithRetry` | The previous attempt failed and a retry is scheduled |
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

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply: `Reconciling` is present until the work is finished, `Stalled` is present until the cause of the failure is fixed.

{{< /alert >}}

{{< /details >}}

## Deploying an application

To deploy an application, create a [HelmApplication](/modules/operator-helm/cr.html#helmapplication) resource in the same namespace, specifying the repository, chart name and version:

{{< tabs name="create-application" >}}
{{% tab name="Via command line" %}}

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
Applications can be deployed not only from the Helm charts of a repository local to the namespace, but also from a shared repository set up by a Deckhouse Platform administrator. Shared repositories are described with the [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository) resource, and their catalog with [HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart) resources.

Every user in a namespace has read-level access to [HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart).

To use a chart from a shared repository, specify the [`spec.chart.clusterRepository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-clusterrepository) field instead of [`spec.chart.repository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-repository) when describing the HelmApplication resource.

{{< details summary="Viewing the available shared application Helm charts" >}}

To view a list of shared Helm charts, run the following command:

```shell
d8 k get helmclusterapplicationcharts --show-labels
```

Example output:

```text
NAME                                        AGE   LABELS
podinfo-shared-chart-podinfo-135858825b65   11d   chart=podinfo,heritage=deckhouse,repository=podinfo-shared
```

{{< /details >}}

{{< /alert >}}

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Applications".
1. Click the "Create" button.
1. In the form that opens, enter an arbitrary resource name in the "Name" field.
1. In the "Repository" field, select the repository holding the application Helm charts, or a shared application repository created by an administrator.
1. In the "Chart" field, select the Helm chart.
1. In the "Version" field, select the Helm chart version.
1. Click the "Create" button.

{{< alert level="info" >}}
Some repositories in the list may carry the "(cluster)" suffix. This means that the repository is shared ([HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository)) and was created by a Deckhouse Platform administrator.
{{< /alert >}}

{{< alert level="info" >}}
You can adjust the Helm chart parameters if needed. To see the parameters used by default, click "Show default values" in the application creation form.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
A [HelmApplication](/modules/operator-helm/cr.html#helmapplication) is deployed with the permissions of the namespace administrator role. To do this, the module creates a RoleBinding resource in the application namespace that binds the application's ServiceAccount to the `d8:namespace:admin` ClusterRole (`d8:use:role:admin` in Deckhouse Platform versions earlier than 1.78).

If these permissions are not sufficient to deploy the Helm chart, extend the permissions of the namespace administrator role. For details, refer to the [`user-authz` module documentation](/modules/user-authz/faq.html#how-do-i-extend-a-role-or-create-a-new-one).

{{< details summary="The RoleBinding resource used when deploying an application" >}}

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  labels:
    heritage: deckhouse
    helm.deckhouse.io/application: my-app
    helm.deckhouse.io/managed-by: operator-helm
    helm.deckhouse.io/source-namespace: test
  name: hap-test-my-app-<hash>
  namespace: test
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: d8:namespace:admin
subjects:
- kind: ServiceAccount
  name: hap-test-my-app-<hash>
  namespace: d8-operator-helm
```

{{< /details >}}

{{< /alert >}}

### Checking the application state

The state of an application is reflected by the conditions in its status ([`status.conditions`](cr.html#helmapplication-v1alpha1-status-conditions)). To assess the state of an application:

{{< tabs name="check-application-conditions" >}}
{{% tab name="Via command line" %}}

Run the following command:

```shell
d8 k -n test get helmapplication podinfo -o yaml
```

Example output:

```text
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
    version: 6.10.2
status:
  conditions:
  - lastTransitionTime: "2026-09-18T08:20:03Z"
    message: Helm install succeeded for release test/hap-podinfo-3fb7b289386f.v1
      with chart podinfo@6.10.2
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-18T08:20:03Z"
    message: Helm install succeeded for release test/hap-podinfo-3fb7b289386f.v1
      with chart podinfo@6.10.2
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  lastAppliedChart:
    repository: podinfo
    name: podinfo
    version: 6.10.2
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="Via web interface" %}}

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Applications".
1. Select the application you need and hover over its status. The pop-up window shows information about its state.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Viewing the possible application states" >}}

| Condition | Value | Reason | Description |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | The release has been deployed and matches the specification |
| `Ready` | `Unknown` | `Reconciling` | The chart is being downloaded, the release is being rolled out, or the application is being deleted and the release is being uninstalled |
| `Ready` | `False` | `ReleaseFailed` | Helm could not install or upgrade the release. The error text is given in the `message` field. Attempts stop (`Stalled`): fix the cause, then change the specification or request a forced reconciliation |
| `Ready` | `False` | `RetriesExceeded` | Helm has used up its attempts to deploy the release. Fix the cause and request a forced reconciliation |
| `Ready` | `False` | `AccessDenied` | The application's ServiceAccount has no permissions for an object of the chart. The object is named in the `message` field. For details, see the note on permissions in the "Deploying an application" section. Attempts stop (`Stalled`): fix the cause, then change the specification or request a forced reconciliation |
| `Ready` | `False` | `RenderFailed` | The chart could not be rendered into manifests. Attempts stop (`Stalled`): fix the cause, then change the specification or request a forced reconciliation |
| `Ready` | `False` | `ChartNotFound` | The repository catalog has no chart with the name given in `spec.chart.name` |
| `Ready` | `False` | `ChartVersionNotFound` | The chart has no version given in `spec.chart.version` in the repository catalog |
| `Ready` | `False` | `ChartVersionUnusable` | The version is listed in the repository catalog but cannot be installed. The cause is given in the `unavailableReason` field of the version in the chart catalog |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | The chart could not be downloaded from the repository or stored in the cluster, or the chart version has not been resolved in the repository catalog yet |
| `Ready` | `False` | `ChartSourceNotReady` | The internal chart source has nothing to install from yet |
| `Ready` | `False` | `OCIFetchFailed`, `OCIStorageFailed` | The chart could not be retrieved from the OCI registry or stored in the cluster |
| `Ready` | `False` | `UnsupportedChartArtifact`, `SourceNotFound`, `InvalidRepositoryURL`, `InvalidCACertificate` | The repository index points the chart version to an OCI registry, and the artifact there is not a Helm chart, is missing, has an invalid reference, or the CA certificate of the repository is invalid |
| `Ready` | `False`, `Unknown` | `ChartVersionRemoved` | Only for a chart stored in an OCI registry. The specified chart version is no longer published by the repository. Select another version |
| `Ready` | `False` | `HelmChartFailed`, `HelmReleaseFailed` | The module could not create or update its internal objects for the release. The attempt will be repeated automatically |
| `Ready` | `False` | `RBACSetupFailed` | The ServiceAccount or RoleBinding resource used to install the chart could not be prepared. The attempt will be repeated automatically |
| `Ready` | `False` | `ForeignRBACObject` | The name of the RoleBinding resource that the module creates for the application is already taken. The name of the RoleBinding matches the name of the application's ServiceAccount and is given in the `message` field. An object with such a name was not created by the module, so the module does not modify it. Delete the foreign object and request a forced reconciliation |
| `Ready` | `False` | `UnsupportedRepositoryType` | The repository the application refers to has an unreadable URL. Contact the repository owner |
| `Ready` | `False` | `MaintenanceModeActive` | Maintenance mode is on. `Ready` carries the same value as `Managed` |
| `Ready` | `False` | `UninstallFailed` | The application is being deleted, and the release or the internal objects of the module could not be removed. The cause is given in the `message` field |
| `Ready` | `False` | `Failed` | Other errors. The cause is given in the `message` field |
| `Installed` | Same as `Ready` | `InstallSucceeded`, `Installing`, `InstallFailed` | The outcome of the first installation of the release |
| `UpdateInstalled` | Same as `Ready` | `UpgradeSucceeded`, `Upgrading`, `UpgradeFailed` | The outcome of a release upgrade. Appears when the chart, its version or the repository changes |
| `ConfigurationApplied` | Same as `Ready` | `ValuesApplied`, `ApplyingValues`, `ValuesApplyFailed` | The outcome of applying the chart values. Appears when the values change |
| `Managed` | `True` | `MaintenanceModeInactive` | Maintenance mode has been turned off, and the application is managed by the module again |
| `Managed` | `False` | `MaintenanceModeActive` | Maintenance mode is on, reconciliation is paused |
| `UninstallFailed` | `True` | `UninstallFailed` | The application is being deleted, and the release or the internal objects of the module could not be removed |
| `UninstallFailed` | `Unknown` | `Reconciling` | The application is being deleted, and the release is being uninstalled |
| `Reconciling` | `True` | `Reconciling` | The release is being rolled out, or the module is waiting for a dependency |
| `Reconciling` | `True` | `ProgressingWithRetry` | A failure occurred and a retry is scheduled |
| `Reconciling` | `True` | `ChartVersionRemoved` | Only for a chart stored in an OCI registry. The specified chart version is no longer published by the repository, and the module is waiting for the outcome of the attempt to get it |
| `Reconciling` | `True` | `ForceReconcile` | A forced reconciliation is in progress |
| `Stalled` | `True` | The failure cause | Fix the application specification, wait for the repository to change, or remove the object standing in the way, then request a forced reconciliation if needed. Attempts are stopped until the cause is resolved |

{{< alert level="info" >}}

The `Reconciling` and `Stalled` conditions are present only while they apply: `Reconciling` is present until the work is finished, `Stalled` is present until the cause of the failure is fixed.

The `Installed`, `UpdateInstalled` and `ConfigurationApplied` conditions appear as the application passes the corresponding stages. The module updates the conditions of the current operation together with `Ready` and with the same status: their reasons denote success, an operation in progress or a Helm failure, and for other failures they carry the same reason as `Ready`. The conditions of other operations keep their last outcome. A failure that occurs before the chart is looked up in the repository catalog (for example, `RBACSetupFailed`, `ForeignRBACObject` or `UnsupportedRepositoryType`) changes only `Ready`.

The `ChartNotFound`, `ChartVersionNotFound` and `ChartVersionUnusable` reasons denote an error in the specification: neither `Reconciling` nor `Stalled` is set, and the module does not retry until the application specification or the repository catalog changes.

The `Managed` condition appears after maintenance mode is turned on for the first time. The `UninstallFailed` condition appears only while the application is being deleted.

{{< /alert >}}

{{< /details >}}

## Forcing reconciliation

While working with applications and repositories, you may need to force a reconciliation. In normal operation, reconciliation starts automatically whenever the resources are changed or the state of their dependencies changes.

For applications, a forced reconciliation can be useful if a terminal error occurred while deploying the application or changing its settings. Without manual intervention, the module's controllers make no further reconciliation attempts.

For repositories, a forced reconciliation lets you synchronize the repository without waiting for the next scheduled run.

{{< tabs name="force-reconcile-application" >}}
{{% tab name="Via command line" %}}

To force the reconciliation of a [HelmApplication](/modules/operator-helm/cr.html#helmapplication) resource, add the annotation `reconcile.helm.deckhouse.io/force` to it by running the following command:

```shell
d8 k -n test annotate helmapplication podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

To force the reconciliation of a [HelmApplicationRepository](/modules/operator-helm/cr.html#helmapplicationrepository), add the annotation `reconcile.helm.deckhouse.io/force` to it by running the following command:

```shell
d8 k -n test annotate helmapplicationrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
The annotation value is ignored. The module only checks that the annotation is present on the resource. The time stamp in the examples is only to make one request differ from another.
{{< /alert >}}

The processing of a forced reconciliation request is recorded in the [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-status-lastforcereconciletime) field of the resource. The outcome is reflected in the `Ready` condition for an application and in the `Synced` condition for a repository. For example:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{< alert level="info" >}}
If the reconciliation of an application stops at an error that retries cannot fix before the chart is processed (for example, `ForeignRBACObject` or `UnsupportedRepositoryType`), the module removes the annotation without updating the `status.lastForceReconcileTime` field. Check the `Ready` condition in that case.
{{< /alert >}}

{{% /tab %}}

{{% tab name="Via web interface" %}}

To force the reconciliation of an application:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Applications".
1. Select the application you need and click the "Force Reconcile" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

To force the reconciliation of an application repository:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Repositories".
1. Select the repository you need and click the "Force Reconcile" icon.

The outcome of the forced reconciliation is shown in the "Status" column.

{{< alert level="info" >}}

Reconciliation can be very fast, so the web interface may not have time to show the status change. To make sure that the forced reconciliation request has been processed, check the value of the `.status.lastForceReconcileTime` field of the resource. The outcome of the synchronization is shown by the `Ready` and `Synced` conditions. To do this, click the name of the resource you are interested in and switch to the "YAML" tab in the form that opens.

{{< /alert >}}

{{% /tab %}}

{{< /tabs >}}

## Maintenance mode

Maintenance mode pauses the reconciliation of an application, which lets you modify the release manually by adjusting the parameters of the previously deployed resources (such as the number of replicas and other parameters).

{{< tabs name="enable-application-maintenance" >}}
{{% tab name="Via command line" %}}

To enable maintenance mode, run the following command:

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

To disable maintenance mode, run the following command:

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

{{% tab name="Via web interface" %}}

To manage the maintenance mode of an application:

1. Go to the "Projects" tab and select the project you need.
1. Go to "Operator Helm" → "Applications".
1. Select the application you need and click its name.
1. The "Maintenance" option is available in the form that opens.

An application that is in maintenance mode gets the "Maintenance" status.

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
An application in maintenance mode does not support forced reconciliation and cannot be deleted. The only exception is deleting the namespace of the application: the application is then deleted together with it.
{{< /alert >}}
