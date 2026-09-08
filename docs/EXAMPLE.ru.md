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
NAME                                                AGE   LABELS
podinfo-chart-podinfo                               11d   chart=podinfo,heritage=deckhouse,repository=podinfo
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
    - version: 6.11.0
    - version: 6.10.2
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

### Наблюдение за принудительной реконсиляцией

Пока принудительный проход выполняется, на ресурсе присутствует условие `Reconciling` с причиной `ForceReconcile`:

```shell
d8 k get helmclusteraddonrepository podinfo -o jsonpath='{.status.conditions[?(@.type=="Reconciling")]}'
```

Синхронизация по обычному расписанию выставляет то же условие с причиной `Synchronization`, поэтому причина позволяет различить эти два случая.

После завершения прохода это условие снимается, а в `.status.lastForceReconcileTime` записывается время обработки запроса:

```shell
d8 k get helmclusteraddonrepository podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

Отметка времени фиксирует, что запрос был обработан, а не что он завершился успешно — результат отражают условия `Ready` и `Synced`.

{{< alert level="warning" >}}
HelmClusterAddon в режиме обслуживания (`.spec.maintenance: NoResourceReconciliation`) не согласовывается вовсе, поэтому запрос принудительной реконсиляции для него невыполним. Контроллер удаляет аннотацию, а не удерживает её до выхода из режима обслуживания; `.status.lastForceReconcileTime` при этом не меняется. Сначала выйдите из режима обслуживания, затем запрашивайте реконсиляцию.
{{< /alert >}}

## Развёртывание версии, опубликованной в OCI-регистри

Классический HTTP-репозиторий может публиковать часть версий своих чартов в OCI-регистри. Такая запись указывает артефакт в `urls` вместо ссылки на архив `.tgz`:

```yaml
apiVersion: v1
entries:
  airflow:
    - name: airflow
      version: 25.0.2
      urls:
        - oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2
```

Ни в HelmClusterAddonRepository, ни в HelmClusterAddon ничего менять не нужно: репозиторий по-прежнему добавляется по своему HTTP-адресу, а аддон по-прежнему запрашивает версию по имени:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: bitnami
spec:
  url: https://charts.example.com/bitnami
```

Версия попадает в каталог вместе со ссылкой, которую дал ей индекс:

```console
d8 k get helmclusteraddonchart bitnami-airflow -o jsonpath='{.status.versions[0]}'
{"ociRef":"oci://registry-1.docker.io/bitnamicharts/airflow:25.0.2","version":"25.0.2"}
```

Когда аддон запрашивает такую версию, контроллер скачивает её из регистри, а не из репозитория. Регистри должен быть доступен для чтения без аутентификации: креденшлы репозитория не отправляются на хост, который назван только в его индексе.
