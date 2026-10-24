# Running e2e locally

Requirements: docker, go, go-task, crane; werf for dev builds; `gh` for the `pr<N>` tag
default. Run every `task` command with `TASK_X_REMOTE_TASKFILES=1` (add `--yes` in
non-interactive shells).

## Cluster

```shell
task test:e2e:setup      # kind cluster "d8-operator-helm" + Deckhouse (Stable, CE)
task test:e2e:cleanup    # delete the kind cluster
```

Setup runs `scripts/kind-d8-ci.sh`: it installs kind into `kind/bin` and writes the
kubeconfig to `kind/d8-operator-helm/kubeconfig-external`. Pass `KIND_DECKHOUSE_CHANNEL` to
change the release channel, or run the script directly with `--key <license>` for EE. For a
registry with a private CA, make the kind node trust it at setup time via
`KIND_REGISTRY_HOST`, `KIND_REGISTRY_CA_FILE`, and `KIND_REGISTRY_AUTH`.

## The dev loop (`dev:*` tasks)

Log in first: `werf cr login <registry>` (crane uses the same docker config).

- `task dev:build` — builds the working tree (werf `--dev`) and publishes the module via
  `hack/dkp-build-module.sh`;
- `task dev:test:e2e:env` — resolves the module digest with crane and writes
  `tests/e2e/.env`, which every e2e run then picks up through the root Taskfile's dotenv;
- `task dev:test:e2e` — chains build, env, and the test run.

The registry, tag, and digest travel as one set; each resolves in the same order:

| Input | 1. explicit | 2. recorded in `tests/e2e/.env` | 3. default |
|---|---|---|---|
| registry | `DEV_REGISTRY=…` | `DEV_REGISTRY` / `E2E_MODULE_SOURCE_REPO` | `dev-registry.deckhouse.io/sys/deckhouse-oss/modules` — only in `dev:test:e2e:env`; `dev:build` refuses to build without 1 or 2 |
| tag | `DEV_TAG=…` | `E2E_MODULE_TAG_NAME` | `pr<N>` of the current branch's PR (via `gh`), else the sanitized branch name |
| digest | — | — | always `crane digest <registry>/operator-helm:<tag>` |

So a bare `task dev:test:e2e:env` with no `.env` points the suite at what CI published for
the current PR (CI pushes `dev-registry.deckhouse.io/sys/deckhouse-oss/modules/operator-helm:<tag>`,
tag `pr<N>` per PR, `main` for main), and after the first run no arguments are needed at all.

`tests/e2e/.env` is gitignored. The generator rewrites only the keys it has values for on
this run; the private-registry keys (`DEV_REGISTRY_DOCKER_CONFIG`, `E2E_MODULE_SOURCE_CA`,
`KIND_REGISTRY_HOST`/`CA_FILE`/`AUTH`) can be passed to `dev:test:e2e:env` as variables, and
hand-written entries — including the multiline CA — survive regeneration.

A machine-local copy of the file can live in `~/.config/operator-helm/dev.env`: when bb
provisions a fresh worktree, the root `.bb-env-setup.sh` copies it to `tests/e2e/.env` and
re-exports the kind kubeconfig, so new workspaces are e2e-ready without manual steps.

## Running the tests

```shell
task test:e2e            # ginkgo ./... with a 30m timeout, reads tests/e2e/.env
```

The suite needs `E2E_MODULE_SOURCE_REPO`, `E2E_MODULE_TAG_NAME`, and `E2E_MODULE_DIGEST`
(mandatory — the suite refuses to run without it), plus `DEV_REGISTRY_DOCKER_CONFIG` and
`E2E_MODULE_SOURCE_CA` for a private registry; `dev:test:e2e:env` fills the first three.
`E2E_POST_CLEANUP=no` keeps the module enabled after the run for faster iteration. The full
set of `E2E_*` knobs lives in `internal/framework/config.go`.

## Deploying a PR build to an existing cluster

CI prints this recipe in the "Show dev config" job: create a `ModulePullOverride` named
`operator-helm` with `spec.imageTag: pr<N>` and `spec.source: deckhouse`, plus a
`ModuleConfig` with `spec.enabled: true` (or patch an existing mpo's `imageTag`).
