#!/usr/bin/env bash
# bb worktree provisioning: bring machine-local dev artifacts into the fresh
# workspace. Everything here is optional — never fail provisioning.
set -u

env_src="${XDG_CONFIG_HOME:-$HOME/.config}/operator-helm/dev.env"
if [ -f "$env_src" ] && [ ! -f tests/e2e/.env ]; then
  cp "$env_src" tests/e2e/.env && echo "copied $env_src -> tests/e2e/.env"
fi

cluster="${KIND_CLUSTER_NAME:-d8-operator-helm}"
if command -v kind >/dev/null && kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  mkdir -p "tests/e2e/kind/$cluster"
  kind export kubeconfig --name "$cluster" \
    --kubeconfig "tests/e2e/kind/$cluster/kubeconfig-external" \
    && echo "kubeconfig exported for kind cluster $cluster"
fi

exit 0
