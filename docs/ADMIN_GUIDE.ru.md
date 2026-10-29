---
title: "Руководство администратора"
description: "Управление cluster-wide-ресурсами модуля operator-helm: репозитории, каталоги чартов и аддоны."
weight: 40
---

Руководство описывает порядок работы с модулем `operator-helm` через cluster-wide-ресурсы.

{{< alert level="warning" >}}
Для работы с данными кастомными ресурсами необходимы права не ниже уровня [роли `SuperAdmin`](/modules/user-authz/#текущая-ролевая-модель).

В [гранулярной ролевой модели](/modules/user-authz/#гранулярная-ролевая-модель) (Deckhouse Platform 1.78 и новее) для управления ресурсами [HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon), [HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository) и [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository) требуется роль `d8:subsystem:delivery:superadmin`. Роль `d8:subsystem:delivery:viewer` даёт доступ на чтение ресурсов HelmClusterAddon, HelmClusterAddonChart и HelmClusterApplicationChart.
{{< /alert >}}

## Добавление репозитория аддонов

Helm-репозиторий аддонов — это точка входа для всех остальных ресурсов.
Он содержит Helm-чарты для последующей установки в кластере.

Чтобы добавить новый Helm-репозиторий аддонов, создайте [ресурс HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository):

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
1. В поле «URL» введите URL-адрес репозитория с чартами.
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
В URL-адресе репозитория используйте одну из двух схем:

- `http(s)://` — Helm-репозиторий, содержащий файл `index.yaml` с перечнем доступных Helm-чартов;
- `oci://` — репозиторий одного Helm-чарта в хранилище образов контейнеров, поддерживающем хранение Helm-чартов, например `oci://registry.example.com/charts/podinfo`. Последний сегмент пути — имя чарта, а теги образа — его версии.
{{< /alert >}}

После создания ресурса HelmClusterAddonRepository модуль синхронизирует репозиторий и создаст отдельный [объект HelmClusterAddonChart](/modules/operator-helm/cr.html#helmclusteraddonchart) для каждого чарта в репозитории. Для просмотра чартов репозитория:

{{< tabs name="list-addon-charts" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddoncharts -l repository=podinfo --show-labels
```

Пример вывода:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

Имя объекта каталога формируется из имени репозитория, имени чарта и хеша, поэтому выбирать чарт удобнее по лейблам `repository` и `chart`, а не по имени.

Доступные версии чарта перечислены в его статусе. Для просмотра списка версий используйте следующую команду:

```shell
d8 k get helmclusteraddonchart -l repository=podinfo,chart=podinfo -o yaml
```

Пример вывода:

```text
apiVersion: v1
kind: List
items:
- apiVersion: helm.deckhouse.io/v1alpha1
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

### Проверка состояния репозитория

Состояние репозитория отражают условия в его статусе ([`status.conditions`](cr.html#helmclusteraddonrepository-v1alpha1-status-conditions)). Для оценки состояния репозитория:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddonrepository podinfo -o yaml
```

Пример вывода:

```text
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
  lastSyncedGeneration: 2
  nextSyncTime: "2026-09-22T15:34:09Z"
  observedGeneration: 2
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Репозитории аддонов».
1. Выберите нужный репозиторий и наведите курсор на его статус. Во всплывающем окне отобразится информация о его состоянии.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний репозитория" >}}

| Условие | Значение | Причина | Описание |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | Репозиторий успешно прочитан. Актуальность каталога чартов отражает условие `Synced` |
| `Ready` | `Unknown` | `AwaitingInitialSync` | Репозиторий был создан, но первое чтение ещё не завершилось. Дождитесь окончания синхронизации |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | Модулю не удалось создать, обновить или удалить служебные секреты с учётными данными и CA-сертификатом репозитория. Попытка повторится автоматически. Причина приведена в поле `message` |
| `Ready` | `False` | Причина из `Stalled` | Пока репозиторий в состоянии `Stalled`, условие `Ready` несёт ту же причину и сообщение, что и `Stalled`, если вместо этого не сообщается один из сбоев, описанных в соседних строках |
| `Ready` | `False` | `Reconciling` или причина внутреннего источника | Только для репозитория `http(s)://`. Внутренний объект, через который модуль загружает чарты, ещё не готов или сообщает об ошибке. Подробности приведены в поле `message` |
| `Ready` | `Unknown` | `Reconciling` | Репозиторий удаляется, модуль удаляет его внутренние объекты |
| `Ready` | `False` | `Failed` | Модулю не удалось создать, обновить или удалить внутренние объекты репозитория, в том числе при удалении репозитория. Будет предпринята повторная попытка. Причина приведена в поле `message` |
| `Synced` | `True` | `Success` | Каталог чартов соответствует содержимому репозитория |
| `Synced` | `False` | `SyncFailed` | Не удалось прочитать репозиторий. Проверьте URL-адрес и доступность репозитория из кластера |
| `Synced` | `False` | `CatalogUpdateFailed` | Репозиторий прочитан, но записать каталог чартов в кластер не удалось. Попытка повторится автоматически |
| `Synced` | `False` | `PartialSync` | Каталог чартов отражает репозиторий не полностью, остальные чарты доступны. В поле `message` перечислены причины: чарты, имя которых нельзя записать в объект каталога (они пропускаются при каждой синхронизации), версии, не разрешённые при первом чтении, или версии, которые не успели проверить за отведённое на чтение время (следующая синхронизация продолжит примерно через минуту) |
| `Reconciling` | `True` | `Synchronization` | Выполняется плановая реконсиляция |
| `Reconciling` | `True` | `ForceReconcile` | Выполняется принудительная реконсиляция |
| `Reconciling` | `True` | `ProgressingWithRetry` | Предыдущая попытка реконсиляции не удалась, запланирована повторная попытка |
| `Reconciling` | `True` | `AwaitingInitialSync` | Первое чтение репозитория ещё не завершилось |
| `Reconciling` | `True` | `Reconciling` | Модуль ожидает готовности своего внутреннего объекта-источника |
| `Stalled` | `True` | `UnsupportedRepositoryType` | Схема в URL-адресе не поддерживается. Допустимы только `http(s)://` и `oci://` |
| `Stalled` | `True` | `InvalidRepositoryURL` | URL-адрес не удалось разобрать. Проверьте адрес репозитория |
| `Stalled` | `True` | `InvalidCACertificate` | Поле `caCertificate` не содержит сертификата в формате PEM |
| `Stalled` | `True` | `AuthenticationFailed` | Репозиторий отклонил учётные данные. Проверьте логин и пароль в спецификации репозитория |
| `Stalled` | `True` | `SourceNotFound` | По указанному URL-адресу репозиторий не найден |
| `Stalled` | `True` | `SourceRejectedRequest` | Репозиторий отклонил запрос. Обратитесь к владельцу репозитория |
| `Stalled` | `True` | `RetriesExceeded` | Несколько попыток чтения подряд завершились ошибкой. Попытки продолжаются с растущим интервалом, но не реже раза в час, а условие снимается после первого успешного чтения. Чтобы проверить репозиторий раньше после устранения причины, запросите принудительную реконсиляцию |
| `Stalled` | `True` | Причина внутреннего источника | Только для репозитория `http(s)://`. Внутренний объект, через который модуль загружает чарты, прекратил повторные попытки. Подробности приведены в поле `message` |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют только пока они применимы: `Reconciling` — пока работа не завершена, `Stalled` — пока причина сбоя не устранена.

{{< /alert >}}

{{< /details >}}

## Развёртывание аддона

Чтобы развернуть аддон, создайте [ресурс HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon), указав репозиторий, имя и версию чарта, а также неймспейс развёртывания:

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
При необходимости вы можете скорректировать параметры Helm-чарта. Для получения параметров, используемых по умолчанию, нажмите «Показать значения по умолчанию» в форме создания аддона.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Одновременно допускается развёртывание только одного экземпляра HelmClusterAddon, использующего заданный Helm-чарт из заданного репозитория. При этом из одного репозитория одновременно могут быть развёрнуты разные Helm-чарты.
{{< /alert >}}

### Проверка состояния аддона

Состояние аддона отражают условия в его статусе ([`status.conditions`](cr.html#helmclusteraddon-v1alpha1-status-conditions)). Для оценки состояния аддона:

{{< tabs name="check-addon-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k get helmclusteraddon podinfo -o yaml
```

Пример вывода:

```text
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
    helmClusterAddonRepository: podinfo
    version: 6.11.0
  maintenance: ""
  namespace: test
status:
  conditions:
  - lastTransitionTime: "2026-09-22T15:29:56Z"
    message: Helm upgrade succeeded for release test/podinfo.v2 with chart podinfo@6.11.0
    observedGeneration: 5
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T09:50:41Z"
    message: Helm install succeeded for release test/podinfo.v1 with chart podinfo@6.10.2
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  - lastTransitionTime: "2026-09-22T15:29:56Z"
    message: Helm upgrade succeeded for release test/podinfo.v2 with chart podinfo@6.11.0
    observedGeneration: 5
    reason: UpgradeSucceeded
    status: "True"
    type: UpdateInstalled
  - lastTransitionTime: "2026-09-22T10:30:50Z"
    message: Maintenance mode disabled
    observedGeneration: 5
    reason: MaintenanceModeInactive
    status: "True"
    type: Managed
  lastAppliedChart:
    helmClusterAddonChart: podinfo
    helmClusterAddonRepository: podinfo
    version: 6.11.0
  lastForceReconcileTime: "2026-09-22T10:53:51Z"
  observedGeneration: 5
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Система».
1. Перейдите в раздел «Helm-оператор» → «Аддоны».
1. Нажмите на имя нужного аддона. Откроется страница аддона на вкладке «Обзор». Блок «Жизненный цикл» описывает текущее состояние релиза.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний аддона" >}}

| Условие | Значение | Причина | Описание |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | Релиз развёрнут и соответствует спецификации |
| `Ready` | `Unknown` | `Reconciling` | Чарт загружается, релиз разворачивается, либо аддон удаляется и релиз деинсталлируется |
| `Ready` | `False` | `ReleaseFailed` | Helm не смог установить или обновить релиз. Текст ошибки приведён в поле `message`. Повторные попытки прекращаются (`Stalled`): устраните причину, затем измените спецификацию или запросите принудительную реконсиляцию |
| `Ready` | `False` | `RetriesExceeded` | Helm исчерпал попытки развернуть релиз. Устраните причину сбоя и запросите принудительную реконсиляцию |
| `Ready` | `False` | `RenderFailed` | Не удалось сформировать манифесты из чарта. Повторные попытки прекращаются (`Stalled`): устраните причину, затем измените спецификацию или запросите принудительную реконсиляцию |
| `Ready` | `False` | `ChartNotFound` | В каталоге репозитория нет чарта с именем, указанным в `spec.chart.helmClusterAddonChart` |
| `Ready` | `False` | `ChartVersionNotFound` | В каталоге репозитория у чарта нет версии, указанной в `spec.chart.version` |
| `Ready` | `False` | `ChartVersionUnusable` | Версия есть в каталоге репозитория, но установить её нельзя. Причина приведена в поле `unavailableReason` этой версии в каталоге чартов |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | Чарт не удалось загрузить из репозитория или сохранить в кластере либо версия чарта ещё не разрешена в каталоге репозитория |
| `Ready` | `False` | `ChartSourceNotReady` | Во внутреннем источнике чарта пока нечего устанавливать |
| `Ready` | `False` | `OCIFetchFailed`, `OCIStorageFailed` | Не удалось получить чарт из OCI-хранилища или сохранить его в кластере |
| `Ready` | `False` | `UnsupportedChartArtifact`, `SourceNotFound`, `InvalidRepositoryURL`, `InvalidCACertificate` | Индекс репозитория указывает для версии чарта на OCI-хранилище, а артефакт там не является Helm-чартом, отсутствует, имеет некорректную ссылку, либо CA-сертификат репозитория некорректен |
| `Ready` | `False`, `Unknown` | `ChartVersionRemoved` | Только для чарта в OCI-хранилище. Указанная версия чарта больше не публикуется репозиторием. Выберите другую версию |
| `Ready` | `False` | `HelmChartFailed`, `HelmReleaseFailed` | Модулю не удалось создать или обновить свои внутренние объекты для релиза. Попытка повторится автоматически |
| `Ready` | `False` | `ChartClaimConflict` | Этот чарт репозитория уже развёрнут другим аддоном: одну пару «репозиторий — чарт» может обслуживать только один ресурс HelmClusterAddon. Занявший её ресурс указан в поле `message`. Состояние разрешится само в течение полуминуты после того, как тот аддон удалят или назначат на другой чарт |
| `Ready` | `False` | `UnsupportedRepositoryType` | У репозитория, на который ссылается аддон, нечитаемый URL-адрес. Обратитесь к владельцу репозитория |
| `Ready` | `False` | `MaintenanceModeActive` | Включён режим обслуживания. Условие `Ready` принимает то же значение, что и `Managed` |
| `Ready` | `False` | `UninstallFailed` | Аддон удаляется, но не удалось удалить релиз или внутренние объекты модуля. Причина приведена в поле `message` |
| `Ready` | `False` | `Failed` | Прочие ошибки. Причина приведена в поле `message` |
| `Installed` | Как у `Ready` | `InstallSucceeded`, `Installing`, `InstallFailed` | Результат первой установки релиза |
| `UpdateInstalled` | Как у `Ready` | `UpgradeSucceeded`, `Upgrading`, `UpgradeFailed` | Результат обновления релиза. Появляется при смене чарта, его версии или репозитория |
| `ConfigurationApplied` | Как у `Ready` | `ValuesApplied`, `ApplyingValues`, `ValuesApplyFailed` | Результат применения значений чарта. Появляется при изменении значений |
| `Managed` | `True` | `MaintenanceModeInactive` | Режим обслуживания выключен, и аддон снова находится под управлением модуля |
| `Managed` | `False` | `MaintenanceModeActive` | Включён режим обслуживания, реконсиляция приостановлена |
| `UninstallFailed` | `True` | `UninstallFailed` | Аддон удаляется, но не удалось удалить релиз или внутренние объекты модуля |
| `UninstallFailed` | `Unknown` | `Reconciling` | Аддон удаляется, идёт удаление релиза |
| `Reconciling` | `True` | `Reconciling` | Идёт разворачивание релиза, или модуль ожидает зависимость |
| `Reconciling` | `True` | `ProgressingWithRetry` | Произошёл сбой, запланирована повторная попытка |
| `Reconciling` | `True` | `ChartVersionRemoved` | Только для чарта в OCI-хранилище. Указанная версия чарта больше не публикуется репозиторием, модуль ожидает результата попытки её получить |
| `Reconciling` | `True` | `ForceReconcile` | Выполняется принудительная реконсиляция |
| `Stalled` | `True` | Причина сбоя | Необходимо исправить спецификацию аддона, дождаться изменений в репозитории или убрать мешающий объект, а затем при необходимости запросить принудительную реконсиляцию. Пока причина не устранена, повторные попытки прекращены |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют, только пока применимы: `Reconciling` — пока работа не завершена, `Stalled` — пока причина сбоя не устранена.

Условия `Installed`, `UpdateInstalled` и `ConfigurationApplied` появляются по мере того, как аддон проходит соответствующие этапы. Условия текущей операции модуль обновляет вместе с `Ready` и с тем же статусом: их причины обозначают успех, выполняемую операцию или сбой Helm, а при прочих сбоях они несут ту же причину, что и `Ready`. Условия других операций сохраняют свой последний результат. Сбой до поиска чарта в каталоге репозитория (например, `ChartClaimConflict` или `UnsupportedRepositoryType`) меняет только `Ready`.

Причины `ChartNotFound`, `ChartVersionNotFound` и `ChartVersionUnusable` указывают на ошибку в спецификации: условия `Reconciling` и `Stalled` не выставляются, и модуль не повторяет попытки, пока не изменится спецификация аддона или каталог репозитория.

Условие `Managed` появляется после первого включения режима обслуживания. Условие `UninstallFailed` появляется только при удалении аддона.

{{< /alert >}}

{{< /details >}}

## Добавление репозитория с чартами приложений

С помощью создания [HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository) администратор Deckhouse Platform может централизованно предоставить администраторам неймспейсов доступ к Helm-чартам. Helm-чарты данного репозитория будут доступны администраторам всех неймспейсов для развёртывания [HelmApplication](/modules/operator-helm/cr.html#helmapplication).

Чтобы добавить репозиторий с чартами приложений, создайте [ресурс HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository):

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
1. В поле «URL» введите URL-адрес репозитория с чартами.
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
В URL-адресе репозитория используйте одну из двух схем:

- `http(s)://` — Helm-репозиторий, содержащий файл `index.yaml` с перечнем доступных Helm-чартов;
- `oci://` — репозиторий одного Helm-чарта в хранилище образов контейнеров, поддерживающем хранение Helm-чартов, например `oci://registry.example.com/charts/podinfo`. Последний сегмент пути — имя чарта, а теги образа — его версии.
{{< /alert >}}

Каталог такого репозитория публикуется в ресурсах [HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart). Чтобы вывести список доступных в репозитории Helm-чартов:

{{< tabs name="list-cluster-application-charts" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

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

Дальнейшая работа с чартами из этого репозитория описана в [«Руководстве пользователя»](user_guide.html).

## Подключение приватного репозитория

Чтобы подключить приватный репозиторий, укажите учётные данные и параметры TLS в спецификации репозитория. Пример настройки репозитория с аутентификацией и самоподписанным сертификатом:

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
  url: oci://registry.example.com/charts/podinfo
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
1. В поле «URL» введите URL-адрес репозитория с чартами.
1. В разделе «Аутентификация» укажите учётные данные для доступа к репозиторию.
1. В разделе «CA-сертификат» укажите CA-сертификат в формате PEM, либо воспользуйтесь формой загрузки сертификата, нажав на ссылку «нажмите, чтобы загрузить».
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Учётные данные хранятся в ресурсе в открытом виде. Пользователь с правами на чтение репозитория сможет просмотреть учётные данные.
{{< /alert >}}

## Принудительный запуск реконсиляции

При работе с аддонами и репозиториями может возникнуть необходимость принудительного запуска реконсиляции. В штатном режиме работы запуск реконсиляции происходит автоматически в случае внесения изменений в ресурсы либо изменения состояния их зависимостей.

В случае с аддонами принудительная реконсиляция может быть полезна, если при развёртывании либо изменении настроек аддона возникла критическая ошибка. Без ручного вмешательства контроллеры в составе модуля более не будут предпринимать попытки реконсиляции.

При работе с репозиториями запуск принудительной реконсиляции позволяет выполнить синхронизацию репозитория, не дожидаясь очередного запуска по расписанию.

{{< tabs name="force-reconcile-addon" >}}
{{% tab name="В командной строке" %}}

Для принудительной реконсиляции [ресурса HelmClusterAddon](/modules/operator-helm/cr.html#helmclusteraddon) добавьте к нему аннотацию `reconcile.helm.deckhouse.io/force`, выполнив следующую команду:

```shell
d8 k annotate helmclusteraddon podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Для принудительной реконсиляции [ресурса HelmClusterAddonRepository](/modules/operator-helm/cr.html#helmclusteraddonrepository) добавьте к нему аннотацию `reconcile.helm.deckhouse.io/force`, выполнив следующую команду:

```shell
d8 k annotate helmclusteraddonrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
Значение аннотации не учитывается — модуль проверяет только её наличие на ресурсе. Временная метка в примерах нужна лишь для того, чтобы повторный запрос отличался от предыдущего.
{{< /alert >}}

Обработку запроса на принудительную реконсиляцию фиксирует поле [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmclusteraddon-v1alpha1-status-lastforcereconciletime) ресурса. Результат отражает условие `Ready` для аддона и условие `Synced` для репозитория. Например:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{< alert level="info" >}}
Если реконсиляция аддона останавливается до обработки чарта на ошибке, которую не исправят повторные попытки (например, `UnsupportedRepositoryType`), модуль снимает аннотацию, не обновляя поле `status.lastForceReconcileTime`. В этом случае проверьте условие `Ready`.
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
Реконсиляция может происходить очень быстро, поэтому в веб-интерфейсе может не успеть отобразиться изменение статуса. Убедиться в том, что запрос на принудительную реконсиляцию обработан, можно по значению поля `.status.lastForceReconcileTime` ресурса. Результат синхронизации отражают условия `Ready` и `Synced`. Для этого нажмите на имя интересующего ресурса и перейдите на вкладку «YAML» в открывшейся форме.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

## Режим обслуживания

Режим обслуживания приостанавливает реконсиляцию аддона, что позволяет скорректировать релиз вручную, изменив параметры ранее развёрнутых ресурсов (количество реплик и другие параметры).

{{< tabs name="enable-addon-maintenance" >}}
{{% tab name="В командной строке" %}}

Чтобы включить режим обслуживания, выполните следующую команду:

```shell
d8 k patch helmclusteraddon podinfo --type=merge -p '{"spec":{"maintenance":"NoResourceReconciliation"}}'
```

Чтобы убедиться, что режим обслуживания включён, выполните следующую команду:

```shell
d8 k get helmclusteraddon podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Пример успешного вывода:

```text
MaintenanceModeActive
```

Чтобы выключить режим обслуживания, выполните следующую команду:

```shell
d8 k patch helmclusteraddon podinfo --type=json -p '[{"op":"remove","path":"/spec/maintenance"}]'
```

Чтобы убедиться, что режим обслуживания выключен, выполните следующую команду:

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
1. Нажмите на имя нужного аддона.
1. На вкладке «Конфигурация» доступна опция «Режим обслуживания».

У аддона, находящегося в режиме обслуживания, будет установлен статус «Обслуживание».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Аддон в режиме обслуживания не поддерживает принудительную реконсиляцию и не может быть удалён.
{{< /alert >}}
