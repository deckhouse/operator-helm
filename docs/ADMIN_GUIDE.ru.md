---
title: "Руководство администратора"
description: "Deckhouse Platform — управление кластерными ресурсами модуля operator-helm: репозитории, каталоги чартов и аддоны."
weight: 40
---

Руководство описывает работу с кластерными ресурсами модуля: репозитории чартов, их каталоги и аддоны. Для работы с данными кастомными ресурсами необходимо иметь полномочия не ниже чем [`ClusterAdmin`](/modules/user-authz/#текущая-ролевая-модель).

## Добавление репозитория аддонов

Репозиторий — точка входа для всех остальных ресурсов: пока он не добавлен, выбирать чарт не из чего.

Создайте ресурс [`HelmClusterAddonRepository`](/modules/operator-helm/cr.html#helmclusteraddonrepository):

{{< tabs name="create-addon-repository" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: podinfo
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории аддонов».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя репозитория.
1. В поле «URL» введите URL репозитория с чартами.
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
При задании URL репозитория могут использоваться две схемы: `http(s)://` (Helm-репозиторий, презентующий файл `index.yaml` с перечнем доступных Helm-чартов) и `oci://` (реестр контейнеров, поддерживающий хранение Helm-чартов).
{{< /alert >}}

Модуль синхронизирует репозиторий и создаст по объекту [`HelmClusterAddonChart`](/modules/operator-helm/cr.html#helmclusteraddonchart) на каждый найденный чарт. Для просмотра чартов репозитория:

{{< tabs name="list-addon-charts" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddoncharts -l repository=podinfo
```

Пример вывода:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

Имя объекта каталога формируется из имени репозитория, имени чарта и хеша, поэтому выбирать чарт удобнее по лейблам `repository` и `chart`, а не по имени.

Доступные версии чарта перечислены в его статусе. Выведите их:

```shell
d8 k get helmclusteraddonchart -l repository=podinfo,chart=podinfo -o yaml
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
  name: podinfo-chart-podinfo-dfbe83e63b0b
status:
  versions:
    - version: 6.11.0
    - version: 6.10.2
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Чарты аддонов».

{{% /tab %}}
{{< /tabs >}}

## Развёртывание аддона

Создайте ресурс [`HelmClusterAddon`](/modules/operator-helm/cr.html#helmclusteraddon), указав репозиторий, имя и версию чарта, а также неймспейс развёртывания:

{{< tabs name="create-addon" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
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
EOF
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Аддоны».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя аддона.
1. В поле «Репозиторий» выберите репозиторий с Helm-чартами аддонов.
1. В поле «Чарт» выберите Helm-чарт.
1. В поле «Версия» выберите версию Helm-чарта.
1. В поле «Неймспейс» выберите неймспейс, в котором будут развёрнуты ресурсы Helm-чарта.
1. Нажмите кнопку «Создать».

{{< alert level="info" >}}
При необходимости вы можете скорректировать параметры Helm-чарта. Для получения параметров используемых по умолчанию, нажмите на ссылку «Показать значения по умолчанию» в форме создания аддона.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Заданный чарт заданного репозитория может обслуживать только один ресурс `HelmClusterAddon`. При этом из одного репозитория одновременно могут разворачиваться разные чарты.
{{< /alert >}}

### Проверка состояния репозитория

Состояние репозитория отражают условия в его статусе. Для оценки состояния репозитория:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddonrepository podinfo -o yaml
```

Пример вывода:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  creationTimestamp: "2026-09-22T15:28:51Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 2
  name: podinfo
  resourceVersion: "48926557"
  uid: 0fbfec2f-6669-40ba-a7ef-0cd223aabfca
spec:
  url: https://stefanprodan.github.io/podinfo
status:
  chartCount: 1
  conditions:
  - lastTransitionTime: "2026-09-22T15:28:52Z"
    message: ""
    observedGeneration: 2
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T15:28:51Z"
    message: ""
    observedGeneration: 2
    reason: Success
    status: "True"
    type: Synced
  lastSuccessfulSyncTime: "2026-09-22T15:28:51Z"
  nextSyncTime: "2026-09-22T15:34:09Z"
  observedGeneration: 2
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Репозитории».
1. Выберите нужный репозиторий и наведите мышкой на его статус. Во всплывающем окне будет приведена информация о его состоянии.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний репозитория" >}}

| Условие | Значение | Причина | Что это значит |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | Репозиторий доступен, каталог чартов построен. Можно выбирать чарт для аддона. |
| `Ready` | `Unknown` | `AwaitingInitialSync` | Репозиторий только создан, первое чтение ещё не завершилось. Дождитесь окончания синхронизации. |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | Не удалось создать служебный секрет с учётными данными репозитория. Проверьте свои полномочия в неймспейсе. |
| `Synced` | `True` | `Success` | Каталог чартов соответствует содержимому репозитория. |
| `Synced` | `False` | `SyncFailed` | Репозиторий не удалось прочитать. Проверьте URL и доступность реестра из кластера. |
| `Synced` | `False` | `CatalogUpdateFailed` | Репозиторий прочитан, но записать каталог чартов в кластер не удалось. Попытка повторится автоматически. |
| `Synced` | `False` | `PartialSync` | При первом чтении часть версий разобрать не удалось. Остальные уже доступны, пропущенные подтянутся при следующей синхронизации. |
| `Reconciling` | `True` | `Synchronization` | Идёт плановая синхронизация с репозиторием. |
| `Reconciling` | `True` | `ForceReconcile` | Идёт синхронизация, запрошенная вручную. |
| `Reconciling` | `True` | `ProgressingWithRetry` | Предыдущая попытка не удалась, запланирован повтор. |
| `Stalled` | `True` | `UnsupportedRepositoryType` | Схема в URL не поддерживается. Допустимы только `http(s)://` и `oci://`. |
| `Stalled` | `True` | `InvalidRepositoryURL` | URL не удалось разобрать. Проверьте адрес репозитория. |
| `Stalled` | `True` | `AuthenticationFailed` | Реестр отклонил учётные данные. Проверьте логин и пароль в спецификации репозитория. |
| `Stalled` | `True` | `SourceNotFound` | По указанному URL репозиторий не найден. |
| `Stalled` | `True` | `SourceRejectedRequest` | Реестр отклонил запрос. Обратитесь к владельцу реестра. |
| `Stalled` | `True` | `RetriesExceeded` | Попытки чтения исчерпаны. Устраните причину и запросите принудительную реконсиляцию. |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют, только пока применимы: первое — пока работа не завершена, второе — пока причина сбоя не устранена.

{{< /alert >}}

{{< /details >}}

### Проверка состояния аддона

Состояние аддона отражают условия в его статусе. Для оценки состояния аддона:

{{< tabs name="check-addon-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddon podinfo -o yaml
```

Пример вывода:

```yaml
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddon
metadata:
  creationTimestamp: "2026-09-22T09:50:34Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 5
  name: podinfo
  resourceVersion: "48927375"
  uid: 5365281f-0f8c-4d3d-b174-5096d6bf255d
spec:
  chart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo-helm-repository
    version: 6.15.0
  maintenance: ""
  namespace: default
status:
  conditions:
  - lastTransitionTime: "2026-09-22T15:29:56Z"
    message: Helm upgrade succeeded for release default/podinfo.v2 with chart podinfo@6.15.0
    observedGeneration: 5
    reason: UpgradeSucceeded
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T09:50:41Z"
    message: Helm install succeeded for release default/podinfo.v1 with chart podinfo@6.15.0
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  - lastTransitionTime: "2026-09-22T10:30:50Z"
    message: Maintenance mode disabled
    observedGeneration: 5
    reason: MaintenanceModeInactive
    status: "True"
    type: Managed
  lastAppliedChart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo-helm-repository
    version: 6.15.0
  lastForceReconcileTime: "2026-09-22T10:53:51Z"
  observedGeneration: 5
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Аддоны».
1. Выберите нужный аддон и наведите мышкой на его статус. Во всплывающем окне будет приведена информация о его состоянии.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний аддона" >}}

| Условие | Значение | Причина | Что это значит |
| --- | --- | --- | --- |
| `Ready` | `True` | `InstallSucceeded`, `UpgradeSucceeded` | Релиз развёрнут и соответствует спецификации. Причину в этом случае подставляет Helm. |
| `Ready` | `Unknown` | `Reconciling` | Работа идёт: чарт загружается или релиз раскатывается. |
| `Ready` | `False` | `ReleaseFailed` | Helm не смог установить или обновить релиз. Текст ошибки приведён в поле `message`. |
| `Ready` | `False` | `TestFailed` | Тесты чарта завершились неудачно. |
| `Ready` | `False` | `Remediated` | Выполнен откат к предыдущему состоянию релиза. |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | Чарт не удалось загрузить из репозитория или сохранить в кластере. |
| `Ready` | `False` | `OCIFetchFailed`, `OCIIncludeUnavailable`, `OCIStorageFailed`, `OCIVerificationFailed` | Не удалось получить или проверить чарт из OCI-реестра. |
| `Ready` | `False` | `ChartVersionRemoved` | Указанная версия чарта больше не публикуется репозиторием. Выберите другую версию. |
| `Ready` | `False` | `ChartClaimConflict` | Этот чарт репозитория уже развёрнут другим аддоном: одну пару «репозиторий — чарт» может обслуживать только один `HelmClusterAddon`. Занявший её ресурс указан в поле `message`. Состояние разрешится само в течение полуминуты после того, как тот аддон удалят или перенацелят на другой чарт. |
| `Ready` | `False` | `UnsupportedRepositoryType` | У репозитория, на который ссылается аддон, нечитаемый URL. Обратитесь к владельцу репозитория. |
| `Ready` | `False` | `Failed` | Прочие ошибки. Причина приведена в поле `message`. |
| `Installed` | как у `Ready` | та же, что у `Ready` | Результат первой установки релиза. |
| `UpdateInstalled` | как у `Ready` | та же, что у `Ready` | Результат обновления релиза. Появляется при смене версии чарта. |
| `ConfigurationApplied` | как у `Ready` | та же, что у `Ready` | Результат применения значений чарта. Появляется при изменении значений. |
| `Managed` | `True` | `MaintenanceModeInactive` | Аддон находится под управлением модуля. |
| `Managed` | `False` | `MaintenanceModeActive` | Включён режим обслуживания, реконсиляция приостановлена. |
| `Reconciling` | `True` | `Reconciling` | Идёт раскатка релиза. |
| `Reconciling` | `True` | `ProgressingWithRetry` | Произошёл сбой, запланирован повтор. |
| `Reconciling` | `True` | `ForceReconcile` | Идёт реконсиляция, запрошенная вручную. |
| `Stalled` | `True` | причина того сбоя, который его вызвал | Повтор не поможет: нужно исправить спецификацию аддона, дождаться изменений в репозитории или убрать мешающий объект. Пока причина не устранена, попытки прекращены. |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют, только пока применимы: первое — пока работа не завершена, второе — пока причина сбоя не устранена. `Installed`, `UpdateInstalled` и `ConfigurationApplied` появляются по мере того, как аддон проходит соответствующие этапы, и несут тот же вердикт, что и `Ready`.

{{< /alert >}}

{{< /details >}}

## Добавление репозитория с чартами приложений

С помощью создания [`HelmClusterApplicationRepository`](/modules/operator-helm/cr.html#helmclusterapplicationrepository) администратор платформы может централизованно предоставить администраторам неймспейсов доступ к Helm-чартам. Helm-чарты данного репозитория будут доступны администраторам всех неймспейсов для развёртывания [`HelmApplication`](/modules/operator-helm/cr.html#helmapplication).

Создайте ресурс [`HelmClusterApplicationRepository`](/modules/operator-helm/cr.html#helmclusterapplicationrepository):

{{< tabs name="create-cluster-application-repository" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterApplicationRepository
metadata:
  name: podinfo-shared
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории приложений».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя репозитория.
1. В поле «URL» введите URL репозитория с чартами.
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
При задании URL репозитория могут использоваться две схемы: `http(s)://` (Helm-репозиторий, презентующий файл `index.yaml` с перечнем доступных Helm-чартов) и `oci://` (реестр контейнеров, поддерживающий хранение Helm-чартов).
{{< /alert >}}

Каталог такого репозитория публикуется в ресурсах [`HelmClusterApplicationChart`](/modules/operator-helm/cr.html#helmclusterapplicationchart). Выведите его:

{{< tabs name="list-cluster-application-charts" >}}
{{% tab name="В командной строке" %}}

```shell
d8 k get helmclusterapplicationcharts -l repository=podinfo-shared
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории приложений».
1. Выберите интересующий вас репозиторий из списка и нажмите на его имя.
1. В открывшейся форме во вкладке «Чарты» вы увидите список доступных Helm-чартов.

{{% /tab %}}
{{< /tabs >}}

Дальнейшая работа с чартами из этого репозитория описана в [руководстве пользователя](user_guide.html).

## Подключение приватного репозитория

Учётные данные и параметры TLS задаются в спецификации репозитория. Пример настройки репозитория с аутентификацией и самоподписанным сертификатом:

{{< tabs name="connect-private-repository" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmClusterAddonRepository
metadata:
  name: private
spec:
  url: oci://registry.example.com/charts
  auth:
    username: admin
    password: secret
  caCertificate: |
    -----BEGIN CERTIFICATE-----
    ...
    -----END CERTIFICATE-----
EOF
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории аддонов».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя репозитория.
1. В поле «URL» введите URL репозитория с чартами.
1. В разделе «Аутентификация» укажите учётные данные для доступа к репозиторию.
1. В разделе «CA-сертификат» укажите CA-сертификат в формате PEM, либо воспользуйтесь формой загрузки сертификата, нажав на ссылку «нажмите, чтобы загрузить».
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Учётные данные хранятся в ресурсе открытым текстом. Право на чтение репозитория — это право на чтение его учётных данных.
{{< /alert >}}

## Принудительный запуск реконсиляции

При работе с аддонами и репозиториями может возникнуть необходимость принудительного запуска реконсиляции. В штатном режиме работы запуск реконсиляции происходит автоматически в случае внесения изменений в ресурсы либо изменения состояния их зависимостей.

В случае с аддонами принудительная реконсиляция может быть полезна, если при развёртывании либо изменении настроек аддона возникла терминальная ошибка. Без ручного вмешательства контроллеры в составе модуля более не будут предпринимать попытки реконсиляции.

При работе с репозиториями запуск принудительной реконсиляции позволяет выполнить синхронизацию репозитория, не дожидаясь очередного запуска по расписанию.

{{< tabs name="force-reconcile-addon" >}}
{{% tab name="В командной строке" %}}

Для принудительной реконсиляции [`HelmClusterAddon`](/modules/operator-helm/cr.html#helmclusteraddon) выполните команду:

```shell
d8 k annotate helmclusteraddon podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Для принудительной реконсиляции [`HelmClusterAddonRepository`](/modules/operator-helm/cr.html#helmclusteraddonrepository) выполните команду:

```shell
d8 k annotate helmclusteraddonrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
Модуль проверяет только наличие аннотации, её содержимое он не читает. Временная метка в примерах нужна лишь для того, чтобы повторный запрос отличался от предыдущего.
{{< /alert >}}

{{< alert level="info" >}}
Завершение принудительной реконсиляции можно отследить по полю [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmclusteraddon-v1alpha1-status-lastforcereconciletime) ресурса. Например:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{< /alert >}}

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

Для принудительной реконсиляции аддона:

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Аддоны».
1. Выберите нужный аддон и нажмите на иконку «Принудительная реконсиляция».

Результат принудительной реконсиляции будет отражён в столбце «Статус».

Для принудительной реконсиляции репозитория аддонов:

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории аддонов».
1. Выберите нужный репозиторий и нажмите на иконку «Принудительная реконсиляция».

Результат принудительной реконсиляции будет отражён в столбце «Статус».

{{< alert level="info" >}}
Реконсиляция может происходить очень быстро, поэтому в веб-интерфейсе может не успеть отобразиться изменение статуса. Убедиться в том, что принудительная синхронизация выполнена, можно по значению поля `.status.lastForceReconcileTime` ресурса. Для этого нажмите на имя интересующего ресурса и перейдите на вкладку «YAML» в открывшейся форме.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

## Режим обслуживания

Режим обслуживания приостанавливает реконсиляцию аддона, что позволяет вмешаться в релиз вручную, корректируя параметры ранее развёрнутых ресурсов (изменять количество реплик, менять параметры и другое).

{{< tabs name="enable-addon-maintenance" >}}
{{% tab name="В командной строке" %}}

Для включения режима обслуживания выполните команду:

```shell
d8 k patch helmclusteraddon podinfo --type=merge -p '{"spec":{"maintenance":"NoResourceReconciliation"}}'
```

Проверить, что режим обслуживания включён, можно командой:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Пример успешного вывода:

```text
MaintenanceModeActive
```

Для выключения режима обслуживания выполните команду:

```shell
d8 k patch helmclusteraddon podinfo --type=json -p '[{"op":"remove","path":"/spec/maintenance"}]'
```

Проверить, что режим обслуживания выключен, можно командой:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Пример успешного вывода:

```text
MaintenanceModeInactive
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

Для управления режимом обслуживания аддона:

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Аддоны».
1. Выберите нужный аддон и нажмите на его имя.
1. В открывшейся форме будет доступна опция «Режим обслуживания».

У аддона, находящегося в режиме обслуживания, будет установлен статус «Обслуживание».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Аддон в режиме обслуживания не поддерживает принудительную реконсиляцию и не может быть удалён.
{{< /alert >}}
