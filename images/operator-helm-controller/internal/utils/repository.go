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
