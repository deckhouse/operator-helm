---
name: werf-to-delivery-kit-migration
description: Миграция сборочных конфигураций (werf.yaml, werf.inc.yaml) с werf на delivery-kit — перенос сетевых операций из shell-инструкций в декларативные директивы git и packages, включение SBOM. Использовать при миграции или ревью werf-конфигураций под delivery-kit.
---

# Миграция werf → delivery-kit

delivery-kit — форк werf. Формат `werf.yaml` сохраняется, но добавляется контроль зависимостей:

- **`build.sbom.enable: true`** включает SBOM-flow и **отключает сеть для всех shell-стадий** (`beforeInstall`, `install`, `beforeSetup`, `setup`). Сеть остаётся только у стадии `packagesInstall`.
- **`packages:`** — единственный декларативный канал ввода зависимостей (языковые пакеты и OS-пакеты).
- **`git:`** — канал доставки исходного кода (включая удалённые репозитории), заменяет `git clone` в shell.

Порядок стадий: `from → beforeInstall → dependenciesBeforeInstall → gitArchive → packagesInstall → install → … → setup`.
Следствие: файлы, добавленные через `git:` (например `go.mod`/`go.sum`) и через `import` с `before: install`, уже доступны на стадии `packagesInstall`.

## Правила миграции

### 1. Инвентаризация сетевых операций в shell

Найти в секциях `shell:` (и `ansible:`) все обращения к сети:

| Паттерн в shell | Куда мигрирует |
|---|---|
| `git clone …` | директива `git:` с `url:` (remote git) |
| `go mod download`, `GOPROXY=… go mod download` | `packages: - type: go-mod` |
| `apt-get/apt/apk/yum/dnf install …` | `packages: - type: os-pm` |
| `npm install` / `npm ci` | `packages: - type: javascript-npm` |
| `yarn install` | `packages: - type: javascript-yarn` |
| `pnpm install` | `packages: - type: javascript-pnpm` |
| `pip install -r …` | `packages: - type: python-pip` |
| `uv sync` | `packages: - type: python-uv` |
| `poetry install` | `packages: - type: python-poetry` |
| `cargo fetch` / скачивание crates при build | `packages: - type: rust-cargo` |
| `luarocks install` | `packages: - type: lua-rock` |
| `curl/wget <бинарь или архив>` | `import:` из SSDLC-образа пакета либо `packages: os-pm` |

В shell остаются **только** компиляция/сборка/компоновка, работающие без сети: `go build`, `cp`, `mkdir`, генерация файлов и т.п.

### 2. git clone → git:

```yaml
# Было (shell):
shell:
  install:
    - git clone --depth 1 --branch v1.2.3 $SOURCE_REPO_GIT/org/repo.git /src

# Стало:
git:
  - url: {{ env "SOURCE_REPO_GIT" }}/org/repo.git
    tag: v1.2.3        # или branch: / commit:
    add: /
    to: /src
    stageDependencies:
      install:
        - go.mod
        - go.sum
      setup:
        - "**/*.go"
```

### 3. Языковые зависимости → packages (file-based)

Каждый file-based тип запускает команду установки пакетного менеджера на стадии `packagesInstall` (с сетью) и скармливает lock-файл syft'у для SBOM. Сам пакетный менеджер должен быть предустановлен в сборочном образе.

| type | Команда | spec (default) | lock (default) |
|---|---|---|---|
| `go-mod` | `go mod download` | `go.mod` | `go.sum` |
| `python-uv` | `uv sync --frozen` | `pyproject.toml` | `uv.lock` |
| `python-pip` | `pip install --no-cache-dir -r <spec>` | `requirements.txt` | — (lock запрещён) |
| `python-poetry` | `poetry sync --no-root` | `pyproject.toml` | `poetry.lock` |
| `rust-cargo` | `cargo fetch` | `Cargo.toml` | `Cargo.lock` |
| `javascript-npm` | `npm ci` | `package.json` | `package-lock.json` |
| `javascript-yarn` | `yarn install --frozen-lockfile` | `package.json` | `yarn.lock` |
| `javascript-pnpm` | `pnpm install --frozen-lockfile` | `package.json` | `pnpm-lock.yaml` |
| `lua-rock` | `luarocks install --only-deps <spec>` | — (`spec` обязателен) | — (lock запрещён) |

Поля file-based записи: `workdir` (обязателен), `spec` (опционально, переопределяет манифест), `lock` (опционально), `env` (map плоских переменных окружения, имена по POSIX `[a-zA-Z_][a-zA-Z0-9_]*`).

```yaml
packages:
  - type: go-mod
    workdir: /src/images/my-image
    env:
      GOFLAGS: "-mod=readonly"
```

Секреты (`secrets:`) монтируются и на стадии `packagesInstall` — доступны в `/run/secrets/<id>`, но команды установки генерируются delivery-kit'ом, поэтому прокси/приватные зеркала передавайте через `env` директивы или переменные, зашитые в сборочный образ.

### 4. OS-пакеты → packages type os-pm

`os-pm` использует **только inline-синтаксис**: `spec` — непустой YAML-список имён пакетов, версия пиннится через `имя==версия`. File-based формат (`pm.yaml`/`pm.lock`, строка-путь в `spec`, поле `lock`) **не поддерживается** — строка в `spec` даёт ошибку конфигурации (`use inline package list instead of file path`). Поле `workdir` для `os-pm` запрещено. Поле `env` поддерживается.

```yaml
packages:
  - type: os-pm
    spec:
      - curl==8.12.1
      - jq
```

Требования:
- Базовый образ должен содержать бинарь `pm` в `$PATH` (Deckhouse builder base image с лейблом `io.deckhouse.internal.builder=true`). Иначе сборка падает.
- `pm` требует `PACKAGES_VERSION` (и опционально `REGISTRY`): значения берутся из ENV сборочного образа либо из `/run/secrets/…` (директива `secrets: - env: PACKAGES_VERSION` + разрешение в `werf-giterminism.yaml`).
- Пакеты, установленные в родительском образе, наследуются через `fromImage` и попадают в SBOM дочернего образа.

`apt-get`/`apk` в shell работать не будут — сети нет.

### 5. Включение SBOM

В мета-секции проекта (`werf.yaml`, первый документ):

```yaml
build:
  sbom:
    enable: true
    standard: cyclonedx@1.6   # обязателен при enable: true
```

- Требуется `--repo` при сборке (SBOM хранится как OCI-артефакт рядом с образом).
- Все базовые образы (`from`/`fromImage`) и образы в `import:` **должны иметь прикреплённый SBOM** в registry. Исключение (deprecated, с warning): старые `registry.deckhouse.io/container-factory/builder/{golang,alpine}`.
- `scratch` даёт пустой SBOM — допустим.

## Пример: типовой Go-образ (до/после)

Было:

```yaml
image: my-image-artifact
final: false
fromImage: builder/golang
import:
  - image: my-image-src-artifact
    add: /src
    to: /src
    before: install
secrets:
  - id: GOPROXY
    value: {{ .GOPROXY }}
shell:
  install:
    - cd /src/images/my-image
    - GOPROXY=$(cat /run/secrets/GOPROXY) go mod download   # сеть!
    - go build -o /out/app ./cmd/app
```

Стало:

```yaml
image: my-image-artifact
final: false
fromImage: builder/golang
import:
  - image: my-image-src-artifact
    add: /src
    to: /src
    before: install          # доступно уже на packagesInstall
packages:
  - type: go-mod
    workdir: /src/images/my-image
shell:
  install:
    - cd /src/images/my-image
    - go build -mod=readonly -o /out/app ./cmd/app   # без сети
```

## Подводные камни

- **`-mod=readonly`**: рекомендуйте для `go build`, чтобы сборка не пыталась дотянуть недостающие модули (упадёт понятной ошибкой вместо сетевого таймаута).
- **Полнота lock-файлов**: `go.sum`/`package-lock.json`/… должны покрывать все зависимости — доустановка на shell-стадиях невозможна.
- **Кеш-mount'ы** (`mount: fromPath: ~/go-pkg-cache`): могут маскировать незадекларированные зависимости (сборка проходит из кеша раннера, но зависимость не в SBOM). При миграции проверять сборку с чистым кешем.
- **vendor/**: зависимости, положенные в репозиторий напрямую, не проходят через `packages` и не попадают в SBOM — это gap; предупреждать.
- **`workdir` в packages** — путь внутри контейнера, к моменту `packagesInstall` там уже должны лежать манифест и lock (через `git:` или `import … before: install`).
- **`stageDependencies`**: при добавлении file-based `packages` добавьте зависимость на манифест/lock, чтобы стадия пересобиралась при их изменении. Для `os-pm` файлов нет — inline-список входит в werf.yaml и сам влияет на дайджест стадии.
- **Несколько записей** `packages` в одном образе допустимы (разные типы и workdir'ы).
- **Не смешивать**: `shell` и `ansible` по-прежнему взаимоисключающие; `packages` совместим с обоими.

## Чек-лист миграции образа

1. [ ] `build.sbom.enable: true` + `standard: cyclonedx@1.6` в мета-секции.
2. [ ] Все `git clone` из shell перенесены в `git:` (url/tag|branch|commit, add/to).
3. [ ] Все установки языковых пакетов перенесены в `packages:` соответствующего типа.
4. [ ] Все `apt/apk/yum` перенесены в `packages: os-pm`; базовый образ содержит `pm`; `PACKAGES_VERSION` доступен.
5. [ ] Все `curl/wget` бинарей заменены на `import:` из SSDLC-образов или `os-pm`.
6. [ ] В shell не осталось сетевых операций (проверить grep'ом: `git clone|curl|wget|apt|apk|yum|dnf|go mod download|go get|npm|yarn|pnpm|pip|poetry|uv |cargo|luarocks`).
7. [ ] `go build` использует `-mod=readonly` (или эквивалентную строгость для других экосистем).
8. [ ] Базовые и импортируемые образы имеют SBOM в registry.
9. [ ] Пробная сборка с чистым кешем проходит.
