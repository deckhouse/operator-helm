---
title: "Module operator-helm"
description: "Deckhouse Kubernetes Platform — the operator-helm module for declarative Helm chart management."
weight: 10
---

The `operator-helm` module allows you to declaratively manage Helm chart deployments in the cluster. It is designed for cluster administrators and DevOps engineers and automates application installation using custom resources.

The module controller monitors the state of HelmClusterAddon resources and automatically reconciles Helm releases in the cluster with the specified parameters.

## Main Features

- Deploying Helm charts from classic HTTP/HTTPS repositories and OCI registries through a unified declarative API.
- Automatic chart version discovery and tracking via HelmClusterAddonChart resources.
- Configurable chart values through HelmClusterAddon resources.
- Maintenance mode to pause reconciliation on managed releases.
- TLS verification and authentication support for private Helm and OCI repositories.
- Management through CLI (`d8 k`) or the Deckhouse web interface.


## Custom Resources

The following custom resources are used to manage Helm charts in the module:

- **HelmClusterAddonRepository** — a Helm or OCI registry containing Helm charts for deployment in the cluster.
- **HelmClusterAddonChart** — a Helm chart discovered in the connected repository. These resources are automatically created and updated by the controller during repository synchronization and are protected from manual changes.
- **HelmClusterAddon** — a declarative description of a specific Helm chart release. The resource contains the target chart version, the namespace name for deployment, and custom values.

## Limitations

- Admin privileges (the `cluster-admin` role) are required to manage HelmClusterAddon and HelmClusterAddonRepository resources.
- A HelmClusterAddon resource referencing a specific HelmClusterAddonChart can only be created as a single instance in the cluster. This is because Helm charts can contain custom resource definitions (CRDs), and installing them multiple times at the cluster level is not allowed.

See [usage examples](example.html) for practical scenarios.

## Repository status

`HelmClusterAddonRepository` reports four conditions.

`Ready` tells whether the repository is usable: its auxiliary resources are in
place, its internal source object is healthy, and the repository responded to a
catalog read on the current spec. A transient read failure does not flip `Ready`
to `False` — installed addons keep working and only the catalog goes stale.

`Synced` tells whether the chart catalog is up to date.

`Reconciling` and `Stalled` follow the kstatus convention and are present only
while they apply. `Reconciling` means work is in progress or a retry is
scheduled; `Stalled` means the repository will not recover on its own.

| Ready | Synced | What it means | What to do |
|---|---|---|---|
| True | True | The repository is healthy. | Nothing. |
| True | False | The catalog read failed but the repository was usable before. | Check the `Reconciling` message and `Last Sync`. Retries are already scheduled. |
| False | True | The catalog is fresh, but the source of chart artifacts is unhealthy. | Check the `Ready` message: it is translated from the internal source object. |
| False | False | The repository is unreachable or misconfigured. | Check `Stalled`: `AuthenticationFailed`, `SourceNotFound` and `InvalidRepositoryURL` need a change in `spec`. |
| Unknown | any | The first catalog read on the current spec has not succeeded yet. | Wait for the next attempt shown in `Next Sync`. |

Synchronization runs every 5 minutes. After a failed read the delay doubles —
5m, 10m, 20m, 40m — up to one hour, and the repository is reported as `Stalled`
with reason `RetriesExceeded` once the delay reaches the cap. Retries continue
at that cadence, because the cause may disappear on the repository side. The
schedule is visible in `status.nextSyncTime` and with
`kubectl get helmclusteraddonrepository -o wide`.
