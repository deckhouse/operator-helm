#!/bin/bash

# Copyright 2022 Flant JSC
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

# Colors to identify the chip
BOLD='\033[1m'
GREEN='\033[0;32m'
PURPLE='\033[0;35m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

#  Checking OS and getting a chip name
if uname -s | grep -q "Darwin"; then
  chip_info=$(sysctl -n machdep.cpu.brand_string)
  if [[ "$chip_info" == *"Apple M"* ]]; then
    # Retrieving the processor generation for Apple on the M
    chip_model=$(echo "$chip_info" | awk -F'Apple ' '{print $2}' | cut -d' ' -f1-2 | sed 's/ / /')
    # Display an alert for Apple on M
    echo -e "${BOLD}${PURPLE}Warning. ${CYAN}Your computer has been identified as: ${GREEN}Apple $chip_model ${NC}
    ${YELLOW}Disable Rosetta support in Docker Desktop before installation.
    To do this, in Docker Desktop go to ${CYAN}Settings > General > Virtual Machine Options ${YELLOW}and uncheck the ${CYAN}Use Rosetta for x86_64/amd64 emulation on Apple Silicon ${YELLOW}option.${NC}"
  fi
fi

PARENT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." &>/dev/null && pwd)

KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-d8-operator-helm}
KIND_CONFIG_DIR=${KIND_CONFIG_DIR:-$PARENT_DIR/kind}/$KIND_CLUSTER_NAME
KIND_IMAGE=kindest/node:v1.31.6@sha256:28b7cbb993dfe093c76641a0c95807637213c9109b761f1d422c2400e22b8e87

D8_RELEASE_CHANNEL_TAG=rock-solid
D8_RELEASE_CHANNEL_NAME=RockSolid
D8_REGISTRY_ADDRESS=registry.deckhouse.io
D8_REGISTRY_PATH=${D8_REGISTRY_ADDRESS}/deckhouse/ce
D8_LICENSE_KEY=

OS_NAME=${OS_NAME:-}

KIND_INSTALL_DIRECTORY=$PARENT_DIR/kind/bin
KIND_PATH=kind
KIND_VERSION=v0.27.0

KUBECTL_INSTALL_DIRECTORY=$PARENT_DIR/kind/bin
KUBECTL_PATH=kubectl
KUBECTL_VERSION=v1.31.6

REQUIRE_MEMORY_MIN_BYTES=4000000000 # 4GB

usage() {
  printf "
 Usage: %s [--channel <CHANNEL NAME>] [--key <DECKHOUSE EE LICENSE KEY>] [--os <linux|mac>]

    --channel <CHANNEL NAME>
            Deckhouse Kubernetes Platform release channel name.
            Possible values: Alpha, Beta, EarlyAccess, Stable, RockSolid.
            Default: Stable.

    --key <DECKHOUSE EE LICENSE KEY>
            Deckhouse Kubernetes Platform Enterprise Edition license key.
            If no license key specified, Deckhouse Kubernetes Platform Community Edition will be installed.

    --os <linux|mac>
            Override the OS detection.

    --help|-h
            Print this message.

" "$0"
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
    --channel)
      case "$2" in
      "")
        echo "Release channel is empty. Please specify the release channel name."
        usage
        exit 1
        ;;
      *)
        if [[ "$2" =~ ^(Alpha|Beta|EarlyAccess|Stable|RockSolid)$ ]]; then
          D8_RELEASE_CHANNEL_NAME="$2"
          D8_RELEASE_CHANNEL_TAG=$(echo ${D8_RELEASE_CHANNEL_NAME} | sed 's/EarlyAccess/early-access/; s/RockSolid/rock-solid/' | tr '[:upper:]' '[:lower:]')
        else
          echo "Incorrect release channel. Use Alpha, Beta, EarlyAccess, Stable or RockSolid."
          usage
          exit 1
        fi
        shift
        ;;
      esac
      ;;
    --key)
      case "$2" in
      "")
        echo "License key is empty. Please specify the license key or don't use the --key parameter to install Deckhouse Kubernetes Platform Community Edition."
        usage
        exit 1
        ;;
      *)
        D8_LICENSE_KEY="$2"
        D8_REGISTRY_PATH=${D8_REGISTRY_ADDRESS}/deckhouse/ee
        shift
        ;;
      esac
      ;;
    --os)
      case "$2" in
      "")
        echo "Please specify 'linux' or 'mac' for the --os parameter."
        usage
        exit 1
        ;;
      *)
        OS_NAME="$2"
        shift
        ;;
      esac
      ;;
    --help | -h)
      usage
      exit 1
      ;;
    --*)
      echo "Illegal option $1"
      usage
      exit 1
      ;;
    esac
    shift $(($# > 0 ? 1 : 0))
  done
}

os_detect() {
  if [[ (-z "$OS_NAME") ]]; then
    # some systems dont have lsb-release yet have the lsb_release binary and
    # vice-versa
    if [ -e /etc/lsb-release ]; then
      . /etc/lsb-release

      OS_NAME=${DISTRIB_ID}

    elif [ "$(which lsb_release 2>/dev/null)" ]; then
      OS_NAME=$(lsb_release -i | cut -f2 | awk '{ print tolower($1) }')

    elif [ -e /etc/debian_version ]; then
      # some Debians have jessie/sid in their /etc/debian_version
      # while others have '6.0.7'
      OS_NAME=$(cat /etc/issue | head -1 | awk '{ print tolower($1) }')

    elif [[ "$OSTYPE" == 'darwin'* ]]; then
      OS_NAME=mac

    else
      echo "Failed to detect operating system. Use --os linux or --os mac."
      exit 1
    fi
  fi

  OS_NAME="${OS_NAME// /}"

  # Supported on ...
  if [[ ("$OS_NAME" == "Ubuntu") || ("$OS_NAME" == "ubuntu") || ("$OS_NAME" == "Debian") || ("$OS_NAME" == "debian") || ("$OS_NAME" == "cachyos") ]]; then
    OS_NAME=linux
  elif [[ ("$OS_NAME" != "mac") && ("$OS_NAME" != "linux") ]]; then
    OS_NAME=
  fi

  if [ -z "$OS_NAME" ]; then
    printf "Your operating system distribution and version might not supported by this script.

You can override the OS detection by setting the --os parameter to running this script.

E.g, to force Linux: --os linux
"

    exit 1
  fi

  MACHINE_ARCH=$(uname -m)

  echo "Detected operating system as $OS_NAME (${MACHINE_ARCH:-unknown})."
}

prerequisites_check() {
  echo "Checking for docker..."
  if command -v docker >/dev/null; then
    echo "Detected docker..."
  else
    echo "docker is not installed. Please install docker. You may go to https://docs.docker.com/engine/install/ for details."
    exit 1
  fi

  memory_check
  kubectl_check
  kind_check
  preinstall_checks
}

memory_check() {
  if [[ "$OS_NAME" == "linux" ]]; then
    MEMORY_TOTAL_BYTES=$(free --bytes 2>/dev/null | grep -i mem | awk '{print $2}' 2>/dev/null)
  else
    MEMORY_TOTAL_BYTES=$(sysctl -n hw.memsize 2>/dev/null)
  fi

  if [[ -n "$MEMORY_TOTAL_BYTES" && ("$MEMORY_TOTAL_BYTES" -gt "0") && ("$MEMORY_TOTAL_BYTES" -lt "$REQUIRE_MEMORY_MIN_BYTES") ]]; then
    echo "Insufficient memory to install Deckhouse Kubernetes Platform."
    echo "Deckhouse Kubernetes Platform requires at least 4 gigabytes of memory."
    exit 1
  fi

  if [[ -z "$MEMORY_TOTAL_BYTES" || ("$MEMORY_TOTAL_BYTES" -eq "0") ]]; then
    echo "Can't get the total memory value."
    echo "Note, that Deckhouse Kubernetes Platform requires at least 4 gigabytes of memory."
    echo "Press enter to continue..."
    read
  fi
}

kubectl_check() {
  echo "Checking for kubectl..."
  if command -v kubectl >/dev/null; then
    echo "Detected kubectl..."
  elif command -v ${KUBECTL_INSTALL_DIRECTORY}/kubectl >/dev/null; then
    echo "Detected ${KUBECTL_INSTALL_DIRECTORY}/kubectl..."
    KUBECTL_PATH=${KUBECTL_INSTALL_DIRECTORY}/kubectl
  else
    echo "kubectl is not installed."
    echo "Installing the latest stable kubectl version to ${KUBECTL_INSTALL_DIRECTORY}/kubectl ..."

    mkdir -p $KUBECTL_INSTALL_DIRECTORY
    curl -sLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS_NAME/mac/darwin}/${MACHINE_ARCH/x86_64/amd64}/kubectl"

    if [ "$?" -ne "0" ]; then
      echo "Unable to download kubectl."
      exit 1
    fi

    install -m 0755 kubectl "${KUBECTL_INSTALL_DIRECTORY}"/kubectl
    if [ "$?" -ne "0" ]; then
      echo "Insufficient permissions to install kubectl. Trying again with sudo..."
      sudo install -m 0755 kubectl "${KUBECTL_INSTALL_DIRECTORY}"/kubectl
      if [ "$?" -ne "0" ]; then
        echo "Unable to install kubectl. Check installation path and permissions."
        exit 1
      fi
    fi

    KUBECTL_PATH=${KUBECTL_INSTALL_DIRECTORY}/kubectl
  fi
}

kind_check() {
  echo "Checking for kind $KIND_VERSION..."
  if [[ "v$(kind version -q 2>/dev/null)" == "$KIND_VERSION" ]]; then
    echo "Detected kind $KIND_VERSION..."
  elif [[ "v$(${KIND_INSTALL_DIRECTORY}/kind version -q 2>/dev/null)" == "$KIND_VERSION" ]]; then
    echo "Detected ${KIND_INSTALL_DIRECTORY}/kind..."
    KIND_PATH=${KIND_INSTALL_DIRECTORY}/kind
  else
    echo "Installing kind to ${KIND_INSTALL_DIRECTORY}/kind ..."

    mkdir -p ${KIND_INSTALL_DIRECTORY}

    curl -sLo ./kind-binary "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-${OS_NAME/mac/darwin}-${MACHINE_ARCH/x86_64/amd64}"

    if [ "$?" -ne "0" ]; then
      echo "Unable to download kind."
      exit 1
    fi

    install -m 0755 kind-binary "${KIND_INSTALL_DIRECTORY}"/kind

    if [ "$?" -ne "0" ]; then
      echo "Insufficient permissions to install kind. Trying again with sudo..."
      sudo install -m 0755 kind-binary "${KIND_INSTALL_DIRECTORY}"/kind
      if [ "$?" -ne "0" ]; then
        echo "Unable to install kind. Check installation path and permissions."
        exit 1
      fi
    fi

    KIND_PATH=${KIND_INSTALL_DIRECTORY}/kind
  fi
}

preinstall_checks() {
  local cluster_exist=true

  while [[ "$cluster_exist" == "true" ]]; do

    # Check if a kind cluster with the name `d8` exist
    ${KIND_PATH} get clusters | grep -q "^${KIND_CLUSTER_NAME}$" &>/dev/null

    if [ "$?" -eq "0" ]; then
      cluster_exist=true
    else
      cluster_exist=false
    fi

    if [[ "$cluster_exist" == "true" ]]; then
      ${KIND_PATH} delete cluster --name "${KIND_CLUSTER_NAME}"
      sleep 3
    fi
  done
}

configs_create() {
  mkdir -p ${KIND_CONFIG_DIR}

  echo "Creating kind config file (${KIND_CONFIG_DIR}/kind.cfg)..."
  cat <<EOF >${KIND_CONFIG_DIR}/kind.cfg
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
featureGates:
  "ValidatingAdmissionPolicy": true
runtimeConfig:
  "admissionregistration.k8s.io/v1alpha1": true
nodes:
- role: control-plane
EOF

  echo "Creating Deckhouse Kubernetes Platform installation config file (${KIND_CONFIG_DIR}/config.yml)..."
  cat <<EOF >${KIND_CONFIG_DIR}/config.yml
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: deckhouse
spec:
  version: 1
  enabled: true
  settings:
    bundle: Minimal
    releaseChannel: EarlyAccess
    logLevel: Info
    allowExperimentalModules: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: global
spec:
  version: 2
  settings:
    modules:
      publicDomainTemplate: "%s.127.0.0.1.sslip.io"
      https:
        mode: Disabled
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: cert-manager
spec:
  version: 1
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: operator-prometheus-crd
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: prometheus-crd
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: prometheus
spec:
  version: 2
  enabled: true
  settings:
    longtermRetentionDays: 0
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: ingress-nginx
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: operator-prometheus
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: monitoring-kubernetes
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: monitoring-deckhouse
spec:
  enabled: true
---
apiVersion: deckhouse.io/v1alpha1
kind: ModuleConfig
metadata:
  name: monitoring-kubernetes-control-plane
spec:
  enabled: true
EOF

  if [[ -n "$D8_LICENSE_KEY" ]]; then
    docker login -u license-token -p $D8_LICENSE_KEY
    generate_ee_access_string "$D8_LICENSE_KEY"
    cat <<EOF >>${KIND_CONFIG_DIR}/config.yml
---
apiVersion: deckhouse.io/v1
kind: InitConfiguration
deckhouse:
  imagesRepo: $D8_REGISTRY_PATH
  registryDockerCfg: $D8_EE_ACCESS_STRING
EOF
  fi

  echo "Creating Deckhouse Kubernetes Platform resource file (${KIND_CONFIG_DIR}/resources.yml)..."
  cat <<EOF >${KIND_CONFIG_DIR}/resources.yml
apiVersion: deckhouse.io/v1
kind: IngressNginxController
metadata:
  name: nginx
spec:
  ingressClass: nginx
  inlet: HostPort
EOF
}

cluster_deletion_info() {

  printf "
To delete created cluster use the following command:

    ${KIND_PATH} delete cluster --name "${KIND_CLUSTER_NAME}"

"
}

cluster_create() {

  ${KIND_PATH} create cluster --name "${KIND_CLUSTER_NAME}" --image "${KIND_IMAGE}" --config "${KIND_CONFIG_DIR}/kind.cfg"

  if [ "$?" -ne "0" ]; then
    printf "
Error creating cluster. If error is like '...port is already allocated' or '... address already in use', then you need to free ports 80 and 443.
E.g., you can find programs that use these ports using the following command:

    sudo lsof -n -i TCP@0.0.0.0:80,443 -s TCP:LISTEN

"
    cluster_deletion_info
    exit 1
  fi

  ${KIND_PATH} get kubeconfig --internal --name "${KIND_CLUSTER_NAME}" >${KIND_CONFIG_DIR}/kubeconfig

}

deckhouse_install() {
  echo "Running Deckhouse installation..."

  # Use the --debug flag to see exactly why it's failing
  docker run --pull=always --rm --network kind \
    -v "${KIND_CONFIG_DIR}/config.yml:/config.yml" \
    -v "${KIND_CONFIG_DIR}/resources.yml:/resources.yml" \
    -v "${KIND_CONFIG_DIR}/kubeconfig:/kubeconfig" \
    ${D8_REGISTRY_PATH}/install:${D8_RELEASE_CHANNEL_TAG} \
    bash -c "dhctl bootstrap-phase install-deckhouse --kubeconfig=/kubeconfig --kubeconfig-context=kind-${KIND_CLUSTER_NAME} --config=/config.yml"

  # If that fails with the CRD error, we might need to wait 30s and try the second phase manually
  if [ "$?" -ne "0" ]; then
    echo "First phase might have timed out. Waiting 30s for CRDs to settle..."
    sleep 30
    # Try the resource creation phase separately
    docker run --rm --network kind -v "${KIND_CONFIG_DIR}/resources.yml:/resources.yml" -v "${KIND_CONFIG_DIR}/kubeconfig:/kubeconfig" \
      ${D8_REGISTRY_PATH}/install:${D8_RELEASE_CHANNEL_TAG} \
      dhctl bootstrap-phase create-resources --kubeconfig=/kubeconfig --kubeconfig-context=kind-${KIND_CLUSTER_NAME} --resources=/resources.yml
  fi
}

macos_force_qemu() {
  if [ "$OS_NAME" = "mac" ]; then
    ${KUBECTL_PATH} --context kind-"${KIND_CLUSTER_NAME}" patch daemonset node-exporter -n d8-monitoring --type='json' -p='[{"op": "add", "path": "/spec/template/spec/containers/1/env/-", "value": {"name": "EXPERIMENTAL_DOCKER_DESKTOP_FORCE_QEMU", "value": "1"}}]' 2>/dev/null
  fi
}

generate_ee_access_string() {
  if [ "$OS_NAME" != "mac" ]; then B64_ARG="-w0"; else B64_ARG=""; fi
  auth_part=$(echo -n "license-token:$1" | base64 $B64_ARG)
  D8_EE_ACCESS_STRING=$(echo -n "{\"auths\": { \"$D8_REGISTRY_ADDRESS\": { \"username\": \"license-token\", \"password\": \"$1\", \"auth\": \"$auth_part\"}}}" | base64 $B64_ARG)

  if [ "$?" -ne "0" ]; then
    echo "Error generation container registry access string for Deckhouse Kubernetes Platform Enterprise Edition"
    exit 1
  fi
}

extract_kubectl_context() {
  ${KUBECTL_PATH} config view --context "kind-${KIND_CLUSTER_NAME}" --minify --flatten >"${KIND_CONFIG_DIR}/kubeconfig-external"
}

# Usage: wait_until_pods_ready <namespace> <required_count> [label_selector] [timeout_in_seconds]
wait_until_pods_ready() {
  local namespace="${1}"
  local required_count="${2}"
  local selector="${3:-}"
  local timeout="${4:-900}"
  local interval=5
  local elapsed=0

  local label_flag=""
  [[ -n "$selector" ]] && label_flag="-l $selector"

  echo "Waiting for $required_count pod(s) in '$namespace' ${selector:+with labels [$selector] }to be ready..."

  while true; do
    local pod_list
    pod_list=$(${KUBECTL_PATH} get pods -n "$namespace" $label_flag -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)

    local actual_count
    actual_count=$(echo "$pod_list" | wc -w | xargs)

    if [[ "$actual_count" -lt "$required_count" ]]; then
      echo "Pending: Found $actual_count/$required_count pods..."
    else
      local statuses
      statuses=$(${KUBECTL_PATH} get pods -n "$namespace" $label_flag -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)

      local not_ready_count
      not_ready_count=$(echo "$statuses" | tr ' ' '\n' | grep -v "True" | wc -l | xargs)

      if [[ "$not_ready_count" -eq 0 ]]; then
        echo "Success: All $actual_count pods in '$namespace' are Ready!"
        return 0
      fi

      echo "Pending: $not_ready_count pod(s) are not ready yet..."
    fi

    if [[ "$elapsed" -ge "$timeout" ]]; then
      echo "Error: Timed out waiting for pods in $namespace after ${timeout}s"
      ${KUBECTL_PATH} get pods -n "$namespace" $label_flag
      return 1
    fi

    sleep "$interval"
    ((elapsed += interval))
  done
}

wait_until_deckhouse_ready() {
  echo "Waiting until deckhouse ready..."
  wait_until_pods_ready "d8-system" 2
}

main() {
  parse_args "$@"

  os_detect
  prerequisites_check
  configs_create
  cluster_create
  deckhouse_install
  macos_force_qemu
  extract_kubectl_context
  wait_until_deckhouse_ready
}

main "$@"
