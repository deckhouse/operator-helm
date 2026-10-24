#!/usr/bin/env bash

# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds and publishes the module the way the "Build and Push for Dev" workflow
# does (deckhouse/modules-actions/build), but into a registry of your choosing.
#
# Usage:
#   hack/dkp-build-module.sh --registry REGISTRY/PATH [--tag TAG] [--name NAME] [--dev]
#                            [--no-release] [--no-register] [-- WERF_BUILD_ARGS...]
#
# The registry is the module source: images go to REGISTRY/PATH/NAME, the bundle
# is tagged REGISTRY/PATH/NAME:TAG, the release-channel image
# REGISTRY/PATH/NAME/release:TAG, and the module is registered as
# REGISTRY/PATH:NAME, exactly as CI does.
#
# Log in first: werf cr login REGISTRY (crane uses the same docker config).
# Credentials need pull/push access to all three repositories above; access to
# REGISTRY/PATH/NAME does not imply access to its /release child or its parent.
# For an already registered module using ModulePullOverride.spec.imageTag,
# --no-release --no-register publishes only the images and tagged bundle.
set -euo pipefail

usage() {
  sed -n '/^# Builds/,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
}

registry="${MODULES_REGISTRY:-}"
tag="${MODULES_MODULE_TAG:-}"
name="${MODULES_MODULE_NAME:-}"
dev=""
release=1
register=1

while [ $# -gt 0 ]; do
  case "$1" in
    --registry) registry="$2"; shift 2 ;;
    --registry=*) registry="${1#*=}"; shift ;;
    --tag) tag="$2"; shift 2 ;;
    --tag=*) tag="${1#*=}"; shift ;;
    --name) name="$2"; shift 2 ;;
    --name=*) name="${1#*=}"; shift ;;
    --dev) dev="--dev"; shift ;;
    --no-release) release=0; shift ;;
    --no-register) register=0; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$registry" ]; then
  echo "--registry is required (or set MODULES_REGISTRY)" >&2
  exit 2
fi
registry="${registry%/}"

for tool in werf crane jq git; do
  command -v "$tool" >/dev/null || { echo "$tool not found in PATH" >&2; exit 1; }
done

root="$(git rev-parse --show-toplevel)"
cd "$root"

if [ -z "$name" ]; then
  name="$(sed -n 's/^name: *//p' module.yaml | head -1)"
fi
if [ -z "$name" ]; then
  echo "cannot read the module name from module.yaml, pass --name" >&2
  exit 1
fi

# CI tags main and release branches by name and pull requests by number; locally
# the branch name is sanitized into something a registry accepts.
if [ -z "$tag" ]; then
  tag="$(git rev-parse --abbrev-ref HEAD | tr -c 'A-Za-z0-9_.\n-' '-' | sed 's/^[.-]*//; s/-*$//')"
  tag="${tag:0:128}"
fi

repo="$registry/$name"
report="$(mktemp -t images_tags_werf.XXXXXX.json)"
trap 'rm -f "$report"' EXIT

echo "module:   $name"
echo "tag:      $tag"
echo "repo:     $repo"
if [ -n "$dev" ]; then
  echo "mode:     dev (uncommitted changes are built)"
fi

MODULES_MODULE_TAG="$tag" \
  werf build $dev --repo="$repo" --save-build-report --build-report-path "$report" "$@"

bundle_src="$(jq -r '.Images.bundle.DockerImageName' "$report")"
bundle_repo="$(jq -r '.Images.bundle.DockerRepo' "$report")"
echo "bundle:   $bundle_src -> $bundle_repo:$tag"
crane copy "$bundle_src" "$bundle_repo:$tag"

if [ "$release" = 1 ]; then
  release_src="$(jq -r '.Images."release-channel-version".DockerImageName' "$report")"
  release_repo="$(jq -r '.Images."release-channel-version".DockerRepo' "$report")"
  echo "release:  $release_src -> $release_repo/release:$tag"
  if ! crane copy "$release_src" "$release_repo/release:$tag"; then
    echo "Release publishing failed; the bundle is already published at $bundle_repo:$tag." >&2
    echo "For 401/403 errors, grant pull/push access to $release_repo/release (a separate repository)." >&2
    echo "For a bundle-only ModulePullOverride deployment, rerun with --no-release --no-register." >&2
    exit 1
  fi
fi

if [ "$register" = 1 ]; then
  echo "register: $registry:$name"
  if ! crane append --oci-empty-base --new_layer "" --new_tag "$registry:$name"; then
    echo "Module registration failed. For 401/403 errors, grant pull/push access to $registry (a separate repository)." >&2
    echo "If the module is already registered, rerun with --no-register." >&2
    exit 1
  fi
fi

echo
echo "digest: $(crane digest "$bundle_repo:$tag")"
echo
echo "To run it in a cluster whose ModuleSource points at $registry:"
cat <<EOF
kubectl patch mpo $name --type merge -p '{"spec":{"imageTag":"$tag"}}'
EOF
