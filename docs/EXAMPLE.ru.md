---
title: "Примеры"
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
kubectl get helmclusteraddoncharts.helm.deckhouse.io -l repository=podinfo
NAME              AGE
podinfo-podinfo   56s
```

Для просмотра списка версий, доступных для заданного чарта, необходимо выполнить команду ниже:
```shell
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonChart
metadata:
  creationTimestamp: "2026-03-12T02:24:04Z"
  generation: 1
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-podinfo
  ownerReferences:
  - apiVersion: helm.deckhouse.io/v1alpha1
    blockOwnerDeletion: true
    controller: true
    kind: HelmClusterAddonRepository
    name: podinfo
    uid: d5e026f9-6151-4f9f-a4bc-756d96b86e95
  resourceVersion: "28306911"
  uid: 7f232359-5553-463e-beed-d6f175596b0b
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