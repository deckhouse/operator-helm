---
title: "Использование"
description: Использование модуля operator-helm.
---

## Включение модуля

Включить модуль можно одним из следующих способов:
- **С помощью [веб-интерфейса Deckhouse](/products/kubernetes-platform/documentation/v1/user/web/ui.html).**

  В разделе «Система» → «Управление системой» → «Deckhouse» → «Модули», откройте модуль `operator-helm`, включите переключатель «Модуль включен». Сохраните изменения.

- **С помощью [Deckhouse CLI](/products/kubernetes-platform/documentation/v1/cli/d8/).**

  Выполните следующую команду для включения модуля:

  ```shell
  d8 system module enable operator-helm
  ```

- **С помощью ModuleConfig `operator-helm`.**

  Установите `spec.enabled` в `true` или `false` в ModuleConfig `operator-helm` (создайте его, при необходимости).

  Пример манифеста для включения модуля:

  ```yaml
  apiVersion: deckhouse.io/v1alpha1
  kind: ModuleConfig
  metadata:
    name: operator-helm
  spec:
    enabled
  ```

## Выключение модуля

Выключить модуль можно одним из следующих способов:
- **С помощью [веб-интерфейса Deckhouse](/products/kubernetes-platform/documentation/v1/user/web/ui.html).**

  В разделе «Система» → «Управление системой» → «Deckhouse» → «Модули», откройте модуль `operator-helm`, выключите переключатель «Модуль включен». Сохраните изменения.

- **С помощью [Deckhouse CLI](/products/kubernetes-platform/documentation/v1/cli/d8/).**

  Выполните следующие команды для выключения модуля:

  ```shell
  d8 k annotate mc operator-helm modules.deckhouse.io/allow-disabling=true
  d8 system module disable operator-helm
  ```
