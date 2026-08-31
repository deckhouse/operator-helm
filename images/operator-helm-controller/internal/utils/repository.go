/*
Copyright 2026 Flant JSC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
)

type InternalRepositoryType string

const (
	InternalHelmRepository InternalRepositoryType = "helm"
	InternalOCIRepository  InternalRepositoryType = "oci"
)

func GetRepositoryType(s string) (InternalRepositoryType, error) {
	parsedURL, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("cannot parse url: %w", err)
	}

	switch parsedURL.Scheme {
	case "http", "https":
		return InternalHelmRepository, nil
	case "oci":
		return InternalOCIRepository, nil
	default:
		return "", fmt.Errorf("unsupported repository schema in use: %s", parsedURL.Scheme)
	}
}

// GetRegistryHost extracts the registry host (with port, if any) from a repository
// URL. Registry credentials in the docker config format are keyed by host, the
// chart path inside the registry is not part of the key.
func GetRegistryHost(s string) (string, error) {
	parsedURL, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("cannot parse url: %w", err)
	}

	if parsedURL.Host == "" {
		return "", fmt.Errorf("url %q has no host", s)
	}

	return parsedURL.Host, nil
}

// dockerConfig is the subset of the docker config format carried by
// kubernetes.io/dockerconfigjson secrets.
type dockerConfig struct {
	Auths map[string]dockerConfigEntry `json:"auths"`
}

type dockerConfigEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// BuildDockerConfigJSON renders the credentials for the registry behind
// repositoryURL in the docker config format. OCIRepository resolves registry
// credentials from a kubernetes.io/dockerconfigjson secret, so basic auth cannot be
// passed to it as plain username/password keys.
func BuildDockerConfigJSON(repositoryURL, username, password string) (string, error) {
	host, err := GetRegistryHost(repositoryURL)
	if err != nil {
		return "", err
	}

	entry := dockerConfigEntry{
		Username: username,
		Password: password,
		Auth:     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
	}

	config := dockerConfig{Auths: map[string]dockerConfigEntry{}}
	for _, key := range registryAuthKeys(host) {
		config.Auths[key] = entry
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshaling docker config: %w", err)
	}

	return string(encoded), nil
}

// dockerHubAuthKey is the legacy config key credentials for Docker Hub are looked
// up under.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// dockerHubHosts lists the spellings of the Docker Hub registry.
var dockerHubHosts = map[string]struct{}{
	"docker.io":            {},
	"index.docker.io":      {},
	"registry-1.docker.io": {},
}

// registryAuthKeys returns the docker config keys the credentials for host have to
// be stored under. Normally that is the host itself, but Docker Hub is resolved
// under its legacy key and under the canonical host regardless of how the
// repository URL spells it, so credentials are stored under every alias.
func registryAuthKeys(host string) []string {
	if _, ok := dockerHubHosts[host]; !ok {
		return []string{host}
	}

	keys := []string{dockerHubAuthKey, "index.docker.io"}
	if host != "index.docker.io" {
		keys = append(keys, host)
	}

	return keys
}
