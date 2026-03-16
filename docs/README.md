---
title: "Module operator-helm"
description: "Deckhouse Kubernetes Platform — the operator-helm module for declarative Helm chart management."
weight: 10
---

The `operator-helm` module provides declarative management of Helm chart deployments for cluster administrators and DevOps engineers. It deploys applications through custom resources, reducing the amount of manual configuration required.

The module acts as a Kubernetes operator that reconciles the desired state described in HelmClusterAddon resources with the actual Helm releases in the cluster.

## Main Features

- Deploying Helm charts from classic HTTP/HTTPS repositories and OCI registries through a unified declarative API.
- Automatic chart version discovery and tracking via HelmClusterAddonChart resources.
- Configurable chart values through HelmClusterAddon resources.
- Maintenance mode to pause reconciliation on managed releases.
- TLS verification and authentication support for private Helm and OCI repositories.
- Management through CLI (`d8 k`) or the Deckhouse web interface.

See [usage examples](example.html) for practical scenarios.
