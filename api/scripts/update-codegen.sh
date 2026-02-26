#!/bin/bash

# Copyright 2024 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -o errexit
set -o nounset
set -o pipefail

function usage {
  cat <<EOF
Usage: $(basename "$0") { v1alpha1 | crds | all }
Example:
   $(basename "$0") v1alpha1 
EOF
}

function source::settings {
  echo "Preparing variables and sourcing dependency scripts.."

  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
  API_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
  ROOT="$(cd "${API_ROOT}/.." && pwd -P)"
  CODEGEN_PKG="$(go env GOMODCACHE)/$(go list -f '{{.Path}}@{{.Version}}' -m k8s.io/code-generator)"
  THIS_PKG="github.com/deckhouse/operator-helm/api"

  source "${CODEGEN_PKG}/kube_codegen.sh"

  echo "Completed!"
}

function generate::v1alpha1 {
  echo "Generating v1alpha1 soruces.."

  kube::codegen::gen_helpers \
    --boilerplate "${SCRIPT_DIR}/boilerplate.go.txt" \
    "${API_ROOT}/v1alpha1"

  go tool openapi-gen \
    --output-pkg "openapi" \
    --output-dir "${ROOT}/images/operator-helm-artifact/pkg/api/openapi" \
    --output-file "zz_generated.openapi.go" \
    --go-header-file "${SCRIPT_DIR}/boilerplate.go.txt" \
    -r /dev/null \
    "${THIS_PKG}/v1alpha1" "k8s.io/apimachinery/pkg/apis/meta/v1" "k8s.io/apimachinery/pkg/version"

  kube::codegen::gen_client \
    --with-watch \
    --output-dir "${API_ROOT}/client/generated" \
    --output-pkg "${THIS_PKG}/client/generated" \
    --boilerplate "${SCRIPT_DIR}/boilerplate.go.txt" \
    "${ROOT}"

  echo "Completed!"
}

function generate::crds {
  echo "Generating CRDs.."

  OUTPUT_BASE=$(mktemp -d)
  trap 'rm -rf "${OUTPUT_BASE}"' ERR EXIT

  go tool controller-gen crd paths="${API_ROOT}/v1alpha1/...;" output:crd:dir="${OUTPUT_BASE}"

  # shellcheck disable=SC2044
  for file in $(find "${OUTPUT_BASE}"/* -type f -iname "*.yaml"); do
    cp "$file" "${ROOT}/crds/$(echo $file | awk -Fio_ '{print $2}')"
  done

  echo "Completed!"
}

WHAT=$1
if [ "$#" != 1 ] || [ "${WHAT}" == "--help" ]; then
  usage
  exit
fi

case "$WHAT" in
v1alpha1)
  source::settings
  generate::v1alpha1
  ;;
crds)
  source::settings
  generate::crds
  ;;
all)
  source::settings
  generate::v1alpha1
  generate::crds
  ;;
*)
  echo "Invalid argument: $WHAT"
  usage
  exit 1
  ;;
esac
