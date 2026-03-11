---
title: "operator-helm"
menuTitle: "operator-helm"
moduleStatus: Experimental
weight: 10
---

The operator-helm module is designed for declarative management of Helm charts. It enables application deployment via Custom Resources (CRs), minimizing the amount of required input data.

## Supported Sources
The module provides flexibility in choosing application sources, supporting:
* Helm repositories (classic HTTP/HTTPS repositories);
* OCI registries that support Helm chart storage.

## Management Methods
Management of the module's resources is unified and accessible via:
* Command Line Interface (CLI): using the `d8` or `kubectl` utility.
* Web Interface: through the Deckhouse Kubernetes Platform graphical console.

See module usage examples in [Usage examples](example.html) section.
