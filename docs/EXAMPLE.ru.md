---
title: "Примеры"
description: "Deckhouse Kubernetes Platform — примеры использования модуля operator-helm."
weight: 30
---

## Добавление Helm-репозитория

Для добавления репозитория создайте ресурс HelmClusterAddonRepository:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
```

После создания репозитория можно просмотреть доступные в нём Helm-чарты:

```shell
d8 k get helmclusteraddoncharts.helm.deckhouse.io -l repository=podinfo
```

Пример вывода:

```text
NAME              AGE
podinfo-podinfo   56s
```

Для просмотра списка версий, доступных для заданного чарта:

```shell
d8 k get helmclusteraddonchart podinfo-podinfo -o yaml
```

Пример вывода:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-podinfo
status:
  versions:
    - digest: a5c4b7381a0907128243354ab100d2eecc480d7dcac5014ff7272b0acef03780
      pulled: false
      version: 6.11.0
    - digest: 9f1cdb52fc5a57848f377b146919f8eb2c4a2c0ab8815bd019ec41c1d1895c0c
      pulled: false
      version: 6.10.2
```

## Развёртывание приложения

Для развёртывания приложения создайте ресурс HelmClusterAddon, указав имя репозитория, имя и версию чарта, а также целевое пространство имён:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddon
metadata:
  name: podinfo
spec:
  namespace: test
  chart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo
    version: 6.10.2
```

{{< alert level="warning" >}}
Одновременно допускается развёртывание только одного экземпляра HelmClusterAddon, использующего заданный Helm-чарт из заданного репозитория. При этом из одного репозитория одновременно могут быть развёрнуты разные Helm-чарты.
{{< /alert >}}

{{< alert level="info" >}}
Параметр `.spec.chart.version` является необязательным. Если он не указан, будет установлена последняя доступная версия чарта.
{{< /alert >}}

## Ручной запуск реконсиляции

Чтобы запустить немедленную реконсиляцию ресурса, не дожидаясь следующей запланированной синхронизации, добавьте к нему аннотацию `reconcile.helm.deckhouse.io/force`. Контроллер обнаружит аннотацию, выполнит полный цикл реконсиляции и автоматически удалит аннотацию после завершения обработки.

Запуск реконсиляции для HelmClusterAddon:

```shell
d8 k annotate helmclusteraddon podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Запуск реконсиляции для HelmClusterAddonRepository:

```shell
d8 k annotate helmclusteraddonrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
Значение аннотации не имеет значения — контроллер проверяет только её наличие на ресурсе. После завершения реконсиляции аннотация удаляется автоматически.
{{< /alert >}}
