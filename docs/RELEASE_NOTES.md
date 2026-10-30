---
title: "Release Notes"
description: "Release notes for Deckhouse operator-helm."
---

## v0.5.0

### Breaking Changes

* The `default` namespace is now treated as a system namespace: a `HelmClusterAddon` can no longer deploy into it, and a `HelmApplication` can no longer be created in it. An existing `HelmClusterAddon` that targets `default` stops being reconciled and its changes are rejected; the objects it has already deployed stay in the cluster.

### New Features

* Added namespace-scoped Helm applications: the new `HelmApplication`, `HelmApplicationRepository` and `HelmClusterApplicationRepository` resources let project users deploy Helm charts into their own namespaces, while `HelmApplicationChart` and `HelmClusterApplicationChart` list the charts available in the connected repositories.
* Added the inventory of deployed objects to `status.inventory` of `HelmClusterAddon` and `HelmApplication`, so you can see which Kubernetes objects a release has created.
* Added the `spec.timeout` field to `HelmClusterAddon` and `HelmApplication` to set how long a Helm operation may take when deploying an addon or application (default is `5m`, maximum is `2h`).

### Chore

* Updated FluxCD components: helm-controller to v1.6.4 and source-controller to v1.9.5.
* Reworked the module documentation: the examples page is replaced by an administrator guide and a user guide, the overview and the resource field descriptions are rewritten.

## v0.4.0

### Breaking Changes

* Do not enable module by default when using Deckhouse Platform minimal bundle. If you are using the minimal bundle, you need to explicitly enable the operator-helm module in the platform configuration

## v0.3.0

### New Features

* Added Helm repositories support where chart URLs have OCI schemas.

## v0.2.0

### New Features

* Reworked HelmClusterAddonRepository status semantics.
* Support legacy OCI chart media type with incremental indexing.
* Surface force reconcile progress and completion in status.
* Report scheduled repository synchronization in status.

### Bug Fixes

* Propagate force reconcile to internal sources.

### Chore

* Added dev registry cleanup job.

## v0.1.1

### Bug Fixes

* Fixed authentication issue when working with a private OCI repository.

### Chore

* Documentation is now available to the AI agent built into the platform.

## v0.1.0

### New Features

* Enforced restricted PSS.

### Bug Fixes

* Forbidden to use system namespaces.

### Chore

* Added changelog and release notes generation.

## v0.0.8

### Bug Fixes

* Resolved race on module disable which could lead to application disruption.

### Chore

* Watch shadow custom resources in module namespace only.

## v0.0.7

### New Features

* Added ability to review chart default values in console during addon creation.

## v0.0.6

### New Features

* Do not mark possible status conditions as initialized on reconcile.

### Chore

* Added weight annotations to validation webhook.

## v0.0.5

### Chore

* Minor documentation updates.

## v0.0.4

### Chore

* Updated main documentation page alerts formatting.

## v0.0.3

### New Features

* The first public alpha release with HelmClusterAddon, HelmClusterAddonChart, and HelmClusterAddonRepository CRDs support.

## v0.0.2

### New Features

* Applied deckhouse runtime review recommendations.

## v0.0.1

### New Features

* Initial release with basic capabilities.
