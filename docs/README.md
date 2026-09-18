---
title: "Module operator-helm"
description: "Deckhouse Kubernetes Platform — the operator-helm module for declarative Helm chart management."
weight: 10
---

The `operator-helm` module allows you to declaratively manage Helm chart deployments in the cluster. It automates chart installation using custom resources and covers two scopes: a cluster-scoped addon family for cluster administrators and DevOps engineers, and a namespaced application family that lets a namespace owner install charts into their own namespace without cluster-wide privileges.

The module controller monitors the state of HelmClusterAddon and HelmApplication resources and automatically reconciles Helm releases in the cluster with the specified parameters.

## Main Features

- Deploying Helm charts from classic HTTP/HTTPS repositories and OCI registries through a unified declarative API.
- Automatic chart version discovery and tracking via HelmClusterAddonChart, HelmApplicationChart and HelmClusterApplicationChart resources.
- Configurable chart values through HelmClusterAddon and HelmApplication resources.
- Namespace-scoped chart installation through HelmApplication, in addition to cluster-wide installation through HelmClusterAddon.
- Maintenance mode to pause reconciliation on managed releases.
- TLS verification and authentication support for private Helm and OCI repositories.
- Management through CLI (`d8 k`) or the Deckhouse web interface.


## Custom Resources

The following custom resources are used to manage Helm charts in the module:

- **HelmClusterAddonRepository** — a Helm or OCI registry containing Helm charts for deployment in the cluster.
- **HelmClusterAddon** — a declarative description of a specific Helm chart release. The resource contains the target chart version, the namespace name for deployment, and custom values.
- **HelmApplicationRepository** — a Helm or OCI registry containing Helm charts that can be referenced by HelmApplication resources from the same namespace.
- **HelmClusterApplicationRepository** — a Helm or OCI registry containing Helm charts that can be referenced by HelmApplication resources from any namespace.
- **HelmApplication** — a declarative description of a Helm chart installation inside a single namespace. The release is always deployed into the namespace of the resource itself; the resource contains the target chart version, a reference to either a same-namespace HelmApplicationRepository or a cluster-wide HelmClusterApplicationRepository, and custom values.

Each repository also publishes a catalog of the charts it offers — HelmClusterAddonChart, HelmApplicationChart and HelmClusterApplicationChart. The controller creates and updates them during repository synchronization; they are read-only and are not edited by hand.

## Limitations

- The addon family (HelmClusterAddon, HelmClusterAddonChart, HelmClusterAddonRepository) is entirely cluster-scoped, so managing it requires the `ClusterAdmin` role.
- The application family is namespaced: a namespace owner can create and manage HelmApplication and HelmApplicationRepository in their own namespace without cluster-wide rights, with the `Admin` role. HelmClusterApplicationRepository is cluster-scoped, so creating one requires the `ClusterAdmin` role, but any HelmApplication may reference an existing one from its own namespace.
- Creating a HelmApplication is effectively equivalent to having administrator rights inside its namespace: on first use, the controller seeds a Role there with unrestricted rights over the namespace (`apiGroups: ["*"]`, `resources: ["*"]`, `verbs: ["*"]`) and binds it to the application's ServiceAccount; the namespace owner may narrow this Role afterwards, and the controller never resets it, so the narrowed rights persist even if the HelmApplication is recreated. Because the granted rights come from the module rather than from the creator's own rights, granting someone only the right to create a HelmApplication — without other rights in the namespace — hands them the same namespace-admin-level access through the installed chart.
- A HelmApplication cannot be created in a system namespace (`kube-system`, `kube-public`, `kube-node-lease`, or any namespace whose name starts with `d8-`, including the module's own `d8-operator-helm`); the admission webhook rejects it.
- `HelmApplicationRepository` and `HelmClusterApplicationRepository` store their registry credentials in plaintext (`spec.auth.username` and `spec.auth.password`; there is no `secretRef` alternative), so any right to read a repository resource is a right to read its password. That is one reason repositories are reachable no lower than `Admin`.
- Two Deckhouse roles reach this module, and the levels accumulate upwards. `Admin` may do anything with HelmApplication and HelmApplicationRepository, and may read both catalogs an application can pick a chart from: HelmApplicationChart and HelmClusterApplicationChart. `ClusterAdmin` covers the cluster-scoped kinds: full rights over HelmClusterAddon, HelmClusterAddonRepository and HelmClusterApplicationRepository, and a read of HelmClusterAddonChart. Note what the first of these means: installing an application is equivalent to namespace-admin rights, as explained above, so `Admin` is the lowest level that reaches this module at all. No level may write a chart catalog of any kind — the controller is its only author.
- A HelmClusterAddon resource referencing a specific HelmClusterAddonChart can only be created as a single instance in the cluster. This is because Helm charts can contain custom resource definitions (CRDs), and installing them multiple times at the cluster level is not allowed.

See [usage examples](example.html) for practical scenarios.
