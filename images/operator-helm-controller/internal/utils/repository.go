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
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	helmv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
)

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

type dockerConfig struct {
	Auths map[string]dockerConfigEntry `json:"auths"`
}

type dockerConfigEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// BuildDockerConfigJSON renders the credentials for the registry behind repositoryURL in the
// docker config format, the only shape OCIRepository resolves registry credentials from.
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

const dockerHubAuthKey = "https://index.docker.io/v1/" // Docker Hub's legacy config key.

var dockerHubHosts = map[string]struct{}{
	"docker.io":            {},
	"index.docker.io":      {},
	"registry-1.docker.io": {},
}

func registryAuthKeys(host string) []string {
	// Docker Hub resolves under its legacy key and its canonical host, however the url spells it.
	if _, ok := dockerHubHosts[host]; !ok {
		return []string{host}
	}

	keys := []string{dockerHubAuthKey, "index.docker.io"}
	if host != "index.docker.io" {
		keys = append(keys, host)
	}

	return keys
}

// SplitOCIRef splits an oci:// reference taken from a repository index into the repository
// address and the tag, and rejects a reference that is not addressable.
func SplitOCIRef(ref, fallbackTag string) (string, string, error) {
	url, tag, err := helmv1alpha1.SplitOCIRef(ref, fallbackTag)
	if err != nil {
		return "", "", err
	}

	// Parsed only to reject the unaddressable; the returned address keeps the index's spelling.
	if _, err := name.NewTag(strings.TrimPrefix(url, "oci://") + ":" + tag); err != nil {
		return "", "", fmt.Errorf("oci reference %q is not a valid tagged reference: %w", ref, err)
	}

	return url, tag, nil
}
