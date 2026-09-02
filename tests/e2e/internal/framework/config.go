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

package framework

import (
	"os"
	"regexp"
	"strconv"
	"sync"

	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
)

var (
	conf *Config
	once sync.Once
)

func onceLoadConfig() {
	once.Do(func() {
		c, err := loadConfig()
		if err != nil {
			panic(err)
		}
		conf = c
	})
}

func GetConfig() *Config {
	onceLoadConfig()
	copied := *conf
	return &copied
}

func loadConfig() (*Config, error) {
	cfgPath := "./default_config.yaml"
	if e, ok := os.LookupEnv("E2E_CONFIG"); ok {
		cfgPath = e
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yamlv3.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	cfg.applyEnvOverrides()
	cfg.compileRegexps()

	return &cfg, nil
}

type Config struct {
	ClusterTransport      ClusterTransport   `yaml:"clusterTransport"`
	Controllers           []ControllerConfig `yaml:"controllers"`
	ModuleSource          string
	ModuleSourceDockerCfg string
	ModuleTagName         string
	ModuleDigest          string
}

type ControllerConfig struct {
	Name          string     `yaml:"name"`
	Namespace     string     `yaml:"namespace"`
	LabelSelector string     `yaml:"labelSelector"`
	Containers    []string   `yaml:"containers"`
	LogFilters    LogFilters `yaml:"logFilters"`

	compiledRegexps []*regexp.Regexp
}

func (c *ControllerConfig) CompiledRegexps() []*regexp.Regexp {
	return c.compiledRegexps
}

type LogFilters struct {
	Exclude       []string `yaml:"exclude"`
	ExcludeRegexp []string `yaml:"excludeRegexp"`
}

type ClusterTransport struct {
	KubeConfig           string `yaml:"kubeConfig"`
	Token                string `yaml:"token"`
	Endpoint             string `yaml:"endpoint"`
	CertificateAuthority string `yaml:"certificateAuthority"`
	InsecureTLS          bool   `yaml:"insecureTls"`
}

func (c ClusterTransport) RestConfig() (*rest.Config, error) {
	flags := genericclioptions.ConfigFlags{}
	if c.KubeConfig != "" {
		flags.KubeConfig = &c.KubeConfig
	}
	if c.Token != "" {
		flags.BearerToken = &c.Token
	}
	if c.InsecureTLS {
		flags.Insecure = &c.InsecureTLS
	}
	if c.CertificateAuthority != "" {
		flags.CAFile = &c.CertificateAuthority
	}
	if c.Endpoint != "" {
		flags.APIServer = &c.Endpoint
	}
	return flags.ToRESTConfig()
}

func (c *Config) applyEnvOverrides() {
	if e, ok := os.LookupEnv("E2E_CLUSTERTRANSPORT_KUBECONFIG"); ok {
		c.ClusterTransport.KubeConfig = e
	}
	if e, ok := os.LookupEnv("E2E_CLUSTERTRANSPORT_TOKEN"); ok {
		c.ClusterTransport.Token = e
	}
	if e, ok := os.LookupEnv("E2E_CLUSTERTRANSPORT_ENDPOINT"); ok {
		c.ClusterTransport.Endpoint = e
	}
	if e, ok := os.LookupEnv("E2E_CLUSTERTRANSPORT_CERTIFICATEAUTHORITY"); ok {
		c.ClusterTransport.CertificateAuthority = e
	}
	if e, ok := os.LookupEnv("E2E_CLUSTERTRANSPORT_INSECURETLS"); ok {
		v, err := strconv.ParseBool(e)
		if err == nil {
			c.ClusterTransport.InsecureTLS = v
		}
	}
	if s, ok := os.LookupEnv("E2E_MODULE_TAG_NAME"); ok {
		c.ModuleTagName = s
	}
	if s, ok := os.LookupEnv("E2E_MODULE_DIGEST"); ok {
		c.ModuleDigest = s
	}
	if s, ok := os.LookupEnv("E2E_MODULE_SOURCE"); ok {
		c.ModuleSource = s
	} else {
		c.ModuleSource = "deckhouse"
	}
	if s, ok := os.LookupEnv("DEV_REGISTRY_DOCKER_CONFIG"); ok {
		c.ModuleSourceDockerCfg = s
	}
}

func (c *Config) compileRegexps() {
	for i := range c.Controllers {
		ctrl := &c.Controllers[i]
		for _, pattern := range ctrl.LogFilters.ExcludeRegexp {
			re, err := regexp.Compile(pattern)
			if err == nil {
				ctrl.compiledRegexps = append(ctrl.compiledRegexps, re)
			}
		}
	}
}
