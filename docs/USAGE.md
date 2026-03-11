---
title: "Usage"
description: Usage of the operator-helm Deckhouse module.
weight: 15
---

## Enabling the module

You can enable the module in one of the following ways:

- **Using the [Deckhouse web interface](/products/kubernetes-platform/documentation/v1/user/web/ui.html).**

  In the "System" section → "System Management" → "Deckhouse" → "Modules", open the `operator-helm` module, enable the "Module enabled" switch. Save the changes.

- **Using [Deckhouse CLI](/products/kubernetes-platform/documentation/v1/cli/d8/).**

  Execute the following command to enable the module:

  ```shell
  d8 system module enable operator-helm
  ```

- **Using ModuleConfig `operator-helm`.**

  Set `spec.enabled` to `true` or `false` in ModuleConfig `operator-helm` (create it if necessary).

  Example manifest to enable the module:

  ```yaml
  apiVersion: deckhouse.io/v1alpha1
  kind: ModuleConfig
  metadata:
    name: operator-helm
  spec:
    enabled: true
  ```

## Disabling the module

You can disable the module using one of the following methods:

- **Using the [Deckhouse web interface](/products/kubernetes-platform/documentation/v1/user/web/ui.html).**

  In the "System" → "System Management" → "Deckhouse" → "Modules" section, open the `operator-helm` module and turn off the "Module Enabled" switch. Save the changes.

- **Using [Deckhouse CLI](/products/kubernetes-platform/documentation/v1/cli/d8/).**

  Execute the following commands to disable the module:

  ```shell
  d8 k annotate mc operator-helm modules.deckhouse.io/allow-disabling=true
  d8 system module disable operator-helm
  ```
