---
title: "Руководство пользователя"
description: "Установка Helm-чартов в собственном неймспейсе с помощью модуля operator-helm."
weight: 50
---

Руководство описывает порядок работы с namespaced-ресурсами модуля, включая репозитории чартов, их каталоги и приложения. Для работы с данными кастомными ресурсами необходимы права не ниже уровня [роли `Admin`](/modules/user-authz/#текущая-ролевая-модель) в соответствующем неймспейсе.

## Добавление репозитория приложений

Helm-репозиторий приложений — это точка входа для всех остальных ресурсов.
Он содержит Helm-чарты для последующей установки в рамках выбранного неймспейса.

Чтобы добавить новый Helm-репозиторий приложений, создайте ресурс [HelmApplicationRepository](/modules/operator-helm/cr.html#helmapplicationrepository) в необходимом неймспейсе:

{{< tabs name="create-application-repository" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationRepository
metadata:
  name: podinfo
  namespace: test
spec:
  url: https://stefanprodan.github.io/podinfo
EOF
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Репозитории».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя репозитория.
1. В поле «URL» введите URL-адрес репозитория с чартами.
1. Нажмите кнопку «Применить».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="info" >}}
В URL-адресе репозитория используйте одну из двух схем:

- `http(s)://` — Helm-репозиторий, содержащий файл `index.yaml` с перечнем доступных Helm-чартов;
- `oci://` — хранилище образов контейнеров, поддерживающее хранение Helm-чартов.
{{< /alert >}}

После создания ресурса HelmApplicationRepository модуль синхронизирует репозиторий и создаст отдельный [объект HelmApplicationChart](/modules/operator-helm/cr.html#helmapplicationchart) для каждого чарта в репозитории. Для просмотра чартов репозитория:

{{< tabs name="list-application-charts" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k -n test get helmapplicationcharts -l repository=podinfo
```

Пример вывода:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo
```

Имя объекта каталога формируется из имени репозитория, имени чарта и хеша, поэтому выбирать чарт удобнее по лейблам `repository` и `chart`, а не по имени.

Доступные версии чарта перечислены в его статусе. Для просмотра списка версий используйте следующую команду:

```shell
d8 k -n test get helmapplicationchart -l repository=podinfo,chart=podinfo -o yaml
```

Пример вывода:

```text
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationChart
metadata:
  labels:
    chart: podinfo
    heritage: deckhouse
    repository: podinfo
  name: podinfo-chart-podinfo-dfbe83e63b0b
  namespace: test
status:
  versions:
    - version: 6.11.0
    - version: 6.10.2
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Чарты».

{{% /tab %}}
{{< /tabs >}}

### Проверка состояния репозитория

Состояние репозитория отражают условия в его статусе ([`status.conditions`](cr.html#helmapplicationrepository-v1alpha1-status-conditions)). Для оценки состояния репозитория:

{{< tabs name="check-repository-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k -n test get helmapplicationrepository podinfo -o yaml
```

Пример вывода:

```text
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplicationRepository
metadata:
  creationTimestamp: "2026-09-22T13:41:25Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 1
  name: podinfo
  namespace: test
  resourceVersion: "48844673"
  uid: f081a6d7-610a-4996-a10c-3d663928f027
spec:
  url: https://stefanprodan.github.io/podinfo
status:
  chartCount: 1
  conditions:
  - lastTransitionTime: "2026-09-22T13:41:26Z"
    message: ""
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-22T13:41:25Z"
    message: ""
    observedGeneration: 1
    reason: Success
    status: "True"
    type: Synced
  lastSuccessfulSyncTime: "2026-09-22T13:41:25Z"
  nextSyncTime: "2026-09-22T13:46:10Z"
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Репозитории».
1. Выберите нужный репозиторий и наведите курсор на его статус. Во всплывающем окне отобразится информация о его состоянии.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний репозитория" >}}

| Условие | Значение | Причина | Описание |
| --- | --- | --- | --- |
| `Ready` | `True` | `Success` | Репозиторий доступен, каталог чартов построен. Можно выбирать чарт для приложения |
| `Ready` | `Unknown` | `AwaitingInitialSync` | Репозиторий был создан, первое чтение ещё не завершилось. Дождитесь окончания синхронизации |
| `Ready` | `False` | `AuxiliaryResourcesFailed` | Не удалось создать служебный секрет с учётными данными репозитория. Проверьте свои полномочия в неймспейсе |
| `Synced` | `True` | `Success` | Каталог чартов соответствует содержимому репозитория |
| `Synced` | `False` | `SyncFailed` | Не удалось прочитать репозиторий. Проверьте URL-адрес и доступность репозитория из кластера |
| `Synced` | `False` | `CatalogUpdateFailed` | Репозиторий прочитан, но записать каталог чартов в кластер не удалось. Попытка повторится автоматически |
| `Synced` | `False` | `PartialSync` | Не удалось разобрать часть версий при первом чтении. Остальные версии уже доступны, а пропущенные будут разобраны при следующей синхронизации |
| `Reconciling` | `True` | `Synchronization` | Выполняется плановая реконсиляция |
| `Reconciling` | `True` | `ForceReconcile` | Выполняется принудительная реконсиляция |
| `Reconciling` | `True` | `ProgressingWithRetry` | Предыдущая попытка реконсиляции не удалась, запланирована повторная попытка |
| `Stalled` | `True` | `UnsupportedRepositoryType` | Схема в URL-адресе не поддерживается. Допустимы только `http(s)://` и `oci://` |
| `Stalled` | `True` | `InvalidRepositoryURL` | URL-адрес не удалось разобрать. Проверьте адрес репозитория |
| `Stalled` | `True` | `AuthenticationFailed` | Репозиторий отклонил учётные данные. Проверьте логин и пароль в спецификации репозитория |
| `Stalled` | `True` | `SourceNotFound` | По указанному URL-адресу репозиторий не найден. |
| `Stalled` | `True` | `SourceRejectedRequest` | Репозиторий отклонил запрос. Обратитесь к владельцу репозитория |
| `Stalled` | `True` | `RetriesExceeded` | Превышено допустимое количество попыток чтения. Устраните причину сбоя и запросите принудительную реконсиляцию |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют только пока они применимы: `Reconciling` — пока работа не завершена, `Stalled` — пока причина сбоя не устранена.

{{< /alert >}}

{{< /details >}}

## Развёртывание приложения

Чтобы развернуть приложение, создайте [ресурс HelmApplication](/modules/operator-helm/cr.html#helmapplication) в том же неймспейсе, указав репозиторий, имя и версию чарта:

{{< tabs name="create-application" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k apply -f - <<EOF
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplication
metadata:
  name: podinfo
  namespace: test
spec:
  chart:
    name: podinfo
    repository: podinfo
    version: 6.10.2
EOF
```

{{< alert level="info" >}}
Приложения могут разворачиваться не только из Helm-чартов локального для неймспейса репозитория, но и из общего репозитория, который завёл администратор Deckhouse Platform. Общие репозитории описываются с помощью [ресурса HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository), а их каталог — [ресурсами HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart).

У каждого пользователя в неймспейсе есть доступ на чтение [HelmClusterApplicationChart](/modules/operator-helm/cr.html#helmclusterapplicationchart).

Для использования чарта из общего репозитория при описании ресурса HelmApplication укажите поле [`spec.chart.clusterRepository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-clusterrepository) вместо [`spec.chart.repository`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-spec-chart-repository).

{{< details summary="Просмотр доступных общих Helm-чартов приложений" >}}

Для просмотра списка общих Helm-чартов выполните следующую команду:

```shell
d8 k get helmclusterapplicationcharts --show-labels
```

Пример вывода:

```text
NAME                                 AGE   LABELS
podinfo-chart-podinfo-dfbe83e63b0b   11d   chart=podinfo,heritage=deckhouse,repository=podinfo-shared
```

{{< /details >}}

{{< /alert >}}

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Приложения».
1. Нажмите кнопку «Создать».
1. В открывшейся форме в поле «Имя» введите произвольное имя ресурса.
1. В поле «Репозиторий» выберите репозиторий с Helm-чартами приложений или общий репозиторий приложений, созданный администратором.
1. В поле «Чарт» выберите Helm-чарт.
1. В поле «Версия» выберите версию Helm-чарта.
1. Нажмите кнопку «Создать».

{{< alert level="info" >}}
В списке репозиториев у некоторых может быть указан постфикс «(cluster)». Это означает, что данный репозиторий — общий ([HelmClusterApplicationRepository](/modules/operator-helm/cr.html#helmclusterapplicationrepository)) и его создал администратор Deckhouse Platform.
{{< /alert >}}

{{< alert level="info" >}}
При необходимости вы можете скорректировать параметры Helm-чарта. Для получения параметров, используемых по умолчанию, нажмите «Показать значения по умолчанию» в форме создания приложения.
{{< /alert >}}

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Развёртывание [HelmApplication](/modules/operator-helm/cr.html#helmapplication) выполняется с полными привилегиями в рамках неймспейса.

{{< details summary="Правила роли, используемой при развёртывании приложения" >}}

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  labels:
    helm.deckhouse.io/managed-by: operator-helm
  name: operator-helm-application
  namespace: test
rules:
- apiGroups:
  - '*'
  resources:
  - '*'
  verbs:
  - '*'
```

{{< /details >}}

{{< /alert >}}

### Проверка состояния приложения

Состояние приложения отражают условия в его статусе ([`status.conditions`](cr.html#helmapplication-v1alpha1-status-conditions)). Для оценки состояния приложения:

{{< tabs name="check-application-conditions" >}}
{{% tab name="В командной строке" %}}

Выполните команду:

```shell
d8 k -n test get helmapplication podinfo -o yaml
```

Пример вывода:

```text
apiVersion: helm.deckhouse.io/v1alpha1
kind: HelmApplication
metadata:
  creationTimestamp: "2026-09-18T08:19:55Z"
  finalizers:
  - helm.deckhouse.io/cleanup
  generation: 1
  name: podinfo
  namespace: test 
  resourceVersion: "48926122"
  uid: f4059036-a6c9-401a-be8d-67d8f2410c6f
spec:
  chart:
    repository: podinfo
    name: podinfo
    version: 6.15.0
status:
  conditions:
  - lastTransitionTime: "2026-09-22T15:28:11Z"
    message: Helm upgrade succeeded for release test/hap-podinfo-3fb7b289386f.v2
      with chart podinfo@6.15.0
    observedGeneration: 1
    reason: UpgradeSucceeded
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-09-18T08:20:03Z"
    message: Helm install succeeded for release test/hap-podinfo-3fb7b289386f.v1
      with chart podinfo@6.15.0
    observedGeneration: 1
    reason: InstallSucceeded
    status: "True"
    type: Installed
  lastAppliedChart:
    repository: podinfo
    name: podinfo
    version: 6.15.0
  observedGeneration: 1
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Приложения».
1. Выберите нужное приложение и наведите курсор на его статус. Во всплывающем окне отобразится информация о его состоянии.

{{% /tab %}}
{{< /tabs >}}

{{< details summary="Просмотр возможных состояний приложения" >}}

| Условие | Значение | Причина | Описание |
| --- | --- | --- | --- |
| `Ready` | `True` | `InstallSucceeded`, `UpgradeSucceeded` | Релиз развёрнут и соответствует спецификации. Причину подставляет Helm |
| `Ready` | `Unknown` | `Reconciling` | Чарт загружается или релиз разворачивается |
| `Ready` | `False` | `ReleaseFailed` | Helm не смог установить или обновить релиз. Текст ошибки приведён в поле `message` |
| `Ready` | `False` | `TestFailed` | Тесты чарта завершились с ошибкой |
| `Ready` | `False` | `Remediated` | Выполнен откат к предыдущему состоянию релиза |
| `Ready` | `False` | `ChartFetchFailed`, `ChartStorageFailed` | Чарт не удалось загрузить из репозитория или сохранить в кластере |
| `Ready` | `False` | `OCIFetchFailed`, `OCIIncludeUnavailable`, `OCIStorageFailed`, `OCIVerificationFailed` | Не удалось получить или проверить чарт из OCI-хранилища |
| `Ready` | `False` | `ChartVersionRemoved` | Указанная версия чарта больше не публикуется репозиторием. Выберите другую версию |
| `Ready` | `False` | `RBACSetupFailed` | Не удалось подготовить ресурс ServiceAccount, Role или RoleBinding, от имени которых устанавливается чарт. Будет предпринята повторная попытка |
| `Ready` | `False` | `ForeignRBACObject` | Имя ресурса Role или RoleBinding, которые модуль создаёт для приложения, уже занято. Role всегда называется `operator-helm-application`, а имя RoleBinding совпадает с именем ServiceAccount приложения и приведено в поле `message`. Объект с таким именем создан не модулем, поэтому модуль его не меняет. Удалите чужой объект и запросите принудительную реконсиляцию |
| `Ready` | `False` | `UnsupportedRepositoryType` | У репозитория, на который ссылается приложение, нечитаемый URL-адрес. Обратитесь к владельцу репозитория |
| `Ready` | `False` | `Failed` | Прочие ошибки. Причина приведена в поле `message` |
| `Installed` | Как у `Ready` | Та же, что у `Ready` | Результат первой установки релиза |
| `UpdateInstalled` | Как у `Ready` | Та же, что у `Ready` | Результат обновления релиза. Появляется при смене версии чарта |
| `ConfigurationApplied` | Как у `Ready` | Та же, что у `Ready` | Результат применения значений чарта. Появляется при изменении значений |
| `Managed` | `True` | `MaintenanceModeInactive` | Приложение находится под управлением модуля |
| `Managed` | `False` | `MaintenanceModeActive` | Включён режим обслуживания, реконсиляция приостановлена |
| `Reconciling` | `True` | `Reconciling` | Идёт разворачивание релиза |
| `Reconciling` | `True` | `ProgressingWithRetry` | Произошёл сбой, запланирована повторная попытка |
| `Reconciling` | `True` | `ForceReconcile` | Выполняется принудительная реконсиляция |
| `Stalled` | `True` | Причина сбоя | Необходимо исправить спецификацию приложения, дождаться изменений в репозитории или убрать мешающий объект. Пока причина не устранена, повторные попытки прекращены |

{{< alert level="info" >}}

Условия `Reconciling` и `Stalled` присутствуют, только пока применимы: `Reconciling` — пока работа не завершена, `Stalled` — пока причина сбоя не устранена. Условия `Installed`, `UpdateInstalled` и `ConfigurationApplied` появляются по мере того, как приложение проходит соответствующие этапы, и несут тот же вердикт, что и `Ready`.

{{< /alert >}}

{{< /details >}}

## Принудительный запуск реконсиляции

При работе с приложениями и репозиториями может возникнуть необходимость принудительного запуска реконсиляции. В штатном режиме работы запуск реконсиляции происходит автоматически в случае внесения изменений в ресурсы либо изменения состояния их зависимостей.

В случае с приложениями принудительная реконсиляция может быть полезна, если при развёртывании либо изменении настроек приложения возникла критическая ошибка. Без ручного вмешательства контроллеры в составе модуля более не будут предпринимать попытки реконсиляции.

При работе с репозиториями запуск принудительной реконсиляции позволяет выполнить синхронизацию репозитория, не дожидаясь очередного запуска по расписанию.

{{< tabs name="force-reconcile-application" >}}
{{% tab name="В командной строке" %}}

Для принудительной реконсиляции [ресурса HelmApplication](/modules/operator-helm/cr.html#helmapplication) добавьте к нему аннотацию `reconcile.helm.deckhouse.io/force`, выполнив следующую команду:

```shell
d8 k -n test annotate helmapplication podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

Для принудительной реконсиляции [ресурса HelmApplicationRepository](/modules/operator-helm/cr.html#helmapplicationrepository) добавьте к нему аннотацию `reconcile.helm.deckhouse.io/force`, выполнив следующую команду:

```shell
d8 k -n test annotate helmapplicationrepository podinfo reconcile.helm.deckhouse.io/force="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
```

{{< alert level="info" >}}
Значение аннотации не учитывается — модуль проверяет только её наличие на ресурсе. Временная метка в примерах нужна лишь для того, чтобы повторный запрос отличался от предыдущего.
{{< /alert >}}

Завершение принудительной реконсиляции можно отследить по полю [`status.lastForceReconcileTime`](/modules/operator-helm/cr.html#helmapplication-v1alpha1-status-lastforcereconciletime) ресурса. Например:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.lastForceReconcileTime}'
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

Для принудительной реконсиляции приложения:

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Приложения».
1. Выберите нужное приложение и нажмите на иконку «Принудительная реконсиляция».

Результат принудительной реконсиляции будет отражён в столбце «Статус».

Для принудительной реконсиляции репозитория приложений:

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Репозитории».
1. Выберите нужный репозиторий и нажмите на иконку «Принудительная реконсиляция».

Результат принудительной реконсиляции будет отражён в столбце «Статус».

{{< alert level="info" >}}

Реконсиляция может происходить очень быстро, поэтому в веб-интерфейсе может не успеть отобразиться изменение статуса. Убедиться в том, что принудительная синхронизация выполнена, можно по значению поля `.status.lastForceReconcileTime` ресурса. Для этого нажмите на имя интересующего ресурса и перейдите на вкладку «YAML» в открывшейся форме.

{{< /alert >}}

{{% /tab %}}

{{< /tabs >}}

## Режим обслуживания

Режим обслуживания приостанавливает реконсиляцию приложения, что позволяет скорректировать релиз вручную, изменив параметры ранее развёрнутых ресурсов (количество реплик и другие параметры).

{{< tabs name="enable-application-maintenance" >}}
{{% tab name="В командной строке" %}}

Чтобы включить режим обслуживания, выполните следующую команду:

```shell
d8 k -n test patch helmapplication podinfo --type=merge -p '{"spec":{"maintenance":"NoResourceReconciliation"}}'
```

Чтобы убедиться, что режим обслуживания включён, выполните следующую команду:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Пример успешного вывода:

```text
MaintenanceModeActive
```

Чтобы выключить режим обслуживания, выполните следующую команду:

```shell
d8 k -n test patch helmapplication podinfo --type=json -p '[{"op":"remove","path":"/spec/maintenance"}]'
```

Чтобы убедиться, что режим обслуживания выключен, выполните следующую команду:

```shell
d8 k -n test get helmapplication podinfo -o jsonpath='{.status.conditions[?(@.type=="Managed")].reason}'
```

Пример успешного вывода:

```text
MaintenanceModeInactive
```

{{% /tab %}}

{{% tab name="В веб-интерфейсе" %}}

Для управления режимом обслуживания приложения:

1. Перейдите на вкладку «Проекты» и выберите нужный проект.
1. Перейдите в раздел «Helm-оператор» → «Приложения».
1. Выберите нужное приложение и нажмите на его имя.
1. В открывшейся форме будет доступна опция «Режим обслуживания».

У приложения, находящегося в режиме обслуживания, будет установлен статус «Обслуживание».

{{% /tab %}}
{{< /tabs >}}

{{< alert level="warning" >}}
Приложение в режиме обслуживания не поддерживает принудительную реконсиляцию и не может быть удалено.
{{< /alert >}}
