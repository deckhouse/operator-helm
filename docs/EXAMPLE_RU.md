---
title: "Примеры использования"
description: "Deckhouse Kubernetes Platform — примеры использования модуля operator-helm."
weight: 30
---

## Добавление Helm репозитория

{{< alert level="warning" >}}
На стадии MVP в качестве источников чартов поддерживаются только Helm репозитории (http(s)://). Поддержка OCI репозиториев (oci://) будет добавлена в alpha версии.
{{< /alert >}}

Для добавления репозитория необходимо добавить ресурс `HelmClusterAddonRepository`:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
```

После создания репозитория, можно просмотреть доступные в нем Helm-чарты с помощью команды ниже:

```shell
kubectl get helmclusteraddoncharts.helm.deckhouse.io -l helm.deckhouse.io/cluster-addon-repository=podinfo
NAME              AGE
podinfo-podinfo   56s
```

Для просмотра списка версий, доступных для заданного чарта, необходимо выполнить команду ниже:
```shell
kubectl get helmclusteraddoncharts.helm.deckhouse.io podinfo-podinfo -o yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  creationTimestamp: "2026-03-11T05:31:14Z"
  generation: 1
  labels:
    helm.deckhouse.io/cluster-addon-repository: podinfo
    helm.deckhouse.io/managed-by: operator-helm
  name: podinfo-podinfo
  ownerReferences:
  - apiVersion: helm.deckhouse.io/v1alpha1
    blockOwnerDeletion: true
    controller: true
    kind: HelmClusterAddonRepository
    name: podinfo
    uid: 073d6efc-aa19-4ccd-9d8e-d3b1253f94cf
  resourceVersion: "27054847"
  uid: cef0e7aa-6d36-4ade-bc6d-9e66b853badf
status:
  versions:
  - digest: a5c4b7381a0907128243354ab100d2eecc480d7dcac5014ff7272b0acef03780
    pulled: false
    version: 6.11.0
  - digest: 9f1cdb52fc5a57848f377b146919f8eb2c4a2c0ab8815bd019ec41c1d1895c0c
    pulled: false
    version: 6.10.2
```

## Деплой приложения

Для деплоя приложения необходимо создать ресурс `HelmClusterAddon` указав имя ранее созданного репозитория, имя и версию чарта, и namespace в который будет развернуто приложение.

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
Одновременного допускается развертывание только одного экземпляра `HelmClusterAddon` использующего заданный Helm чарт из заданного репозитория. При этом из одноного репозитория одновременно могут быть развернуты разные Helm чарты.
{{< /alert >}}

{{< alert level="info" >}}
Допустимо не указывать конкретную версию чарта в параметре `.Spec.chart.version`. В этом случае будет установлена последняя версия приложения.
{{< /alert >}}