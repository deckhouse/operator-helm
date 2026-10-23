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
- Creating a HelmApplication is effectively equivalent to having administrator rights inside its namespace: the module
  ships a ClusterRole `d8:operator-helm:application` with the verbs `get`, `list`, `watch`, `create`, `update`, `patch`,
  `delete` and `deletecollection` on every kind (`apiGroups: ["*"]`, `resources: ["*"]`), and the controller binds it to
  the application's ServiceAccount with a RoleBinding in the application's namespace. The binding is owned by the module
  and reconciled: the controller watches it and restores its own role reference, subjects and labels, so re-pointing or
  deleting it does not outlast the application that needs it. Ownership is decided by the `helm.deckhouse.io/managed-by:
  operator-helm` label: a RoleBinding occupying that name without the label — pre-created by someone else, or stripped
  of the label afterwards — is never adopted, patched or deleted, and the application reports `Stalled` with the reason
  `ForeignRBACObject` and installs nothing. Restoring the label resumes the application on its own. Four kinds stay out
  of these rights, because they decide the limits of the namespace itself: admission policies forbid the application's
  identity to write Namespace, ResourceQuota, LimitRange and ProjectNamespace, to write a Role granting any of them, and
  to write a RoleBinding referencing a ClusterRole. A chart whose Role asks for `verbs: ["*"]` or for a verb outside
  that list, such as `bind`, `escalate` or `impersonate`, is therefore refused by the API server's escalation check. The
  policies judge only what the application itself writes: a Role someone else created in the namespace can still be
  bound by the chart, so keep Roles granting those four kinds out of namespaces that host applications. Because the
  granted rights come from the module rather than from the creator's own rights, granting someone only the right to
  create a HelmApplication — without other rights in the namespace — hands them the same namespace-admin-level access
  through the installed chart; as with any namespace administrator, that includes the tokens of every ServiceAccount
  living in the namespace.
- A HelmApplication cannot be created in a system namespace (`default`, `kube-system`, `kube-public`, `kube-node-lease`, or any namespace whose name starts with `d8-`, including the module's own `d8-operator-helm`); the admission webhook rejects it.
- Every repository kind stores its registry credentials in plaintext (`spec.auth.username` and `spec.auth.password`; there is no `secretRef` alternative), so any right to read a repository resource is a right to read its password. Reading a repository is therefore granted nowhere below the level that writes it: a namespaced HelmApplicationRepository is visible to the namespace administrator who creates it, and the two cluster-scoped repositories — HelmClusterAddonRepository and HelmClusterApplicationRepository — only to a cluster administrator. In the current role model this means no capability the module ships exposes a cluster-scoped repository at all.
- The access model splits by what a level can do with charts, not by the module as a whole, and the levels accumulate upwards.
  - A read-only level — `PrivilegedUser` in the legacy model, the `user` namespace capability in the current one — reads HelmApplication and the namespaced chart catalog HelmApplicationChart, but no repository. `PrivilegedUser` also reads the cluster-wide catalog HelmClusterApplicationChart; the `user` capability does not, for the reason given below.
  - `Admin`, or the `admin` namespace capability, owns the namespaced application family: all writes on HelmApplication and HelmApplicationRepository in its namespace, plus reading those repositories. Creating an application is equivalent to namespace-admin rights, as explained above, so this is the lowest level that may install anything.
  - `ClusterAdmin` in the legacy model owns everything cluster-scoped: full rights over HelmClusterAddon and over both cluster-scoped repositories, and a read of HelmClusterAddonChart. The current model ships no capability that writes any cluster-scoped kind of the module — not even the `d8:manage:all:manager` role gets that — so there every such write stays with the built-in `cluster-admin` role.
  - In the current role model the module additionally ships a delivery-subsystem capability: its `view` level reads the module's ModuleConfig, HelmClusterAddon and both cluster-scoped catalogs, and its `edit` level writes that ModuleConfig. Reading an addon is as far as either goes: neither writes an addon, and neither sees a repository of any kind.
  - No level may write a chart catalog of any kind — the controller is its only author.
- A namespace capability is only ever bound inside a namespace, so it cannot grant a read of the cluster-wide HelmClusterApplicationChart catalog. A namespace administrator who should browse the charts of a HelmClusterApplicationRepository needs the delivery `view` capability, or the chart name from whoever created the repository; the application itself resolves the chart regardless.
- A HelmClusterAddon resource referencing a specific HelmClusterAddonChart can only be created as a single instance in the cluster. This is because Helm charts can contain custom resource definitions (CRDs), and installing them multiple times at the cluster level is not allowed.

See [usage examples](example.html) for practical scenarios.
