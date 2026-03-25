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
	operatorhelmclient "github.com/deckhouse/operator-helm/api/client/generated/clientset/versioned"
	apiv1alpha1 "github.com/deckhouse/operator-helm/api/v1alpha1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var clients Clients

func GetClients() Clients {
	return clients
}

type Clients struct {
	kubeClient     kubernetes.Interface
	operatorClient operatorhelmclient.Interface
	generic        client.Client
	dynamic        dynamic.Interface
}

func (c Clients) KubeClient() kubernetes.Interface {
	return c.kubeClient
}

func (c Clients) OperatorClient() operatorhelmclient.Interface {
	return c.operatorClient
}

func (c Clients) GenericClient() client.Client {
	return c.generic
}

func (c Clients) DynamicClient() dynamic.Interface {
	return c.dynamic
}

func init() {
	onceLoadConfig()

	restConfig, err := conf.ClusterTransport.RestConfig()
	if err != nil {
		panic(err)
	}

	clients.kubeClient, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		panic(err)
	}

	clients.operatorClient, err = operatorhelmclient.NewForConfig(restConfig)
	if err != nil {
		panic(err)
	}

	clients.dynamic, err = dynamic.NewForConfig(restConfig)
	if err != nil {
		panic(err)
	}

	scheme := apiruntime.NewScheme()
	for _, addToScheme := range []func(*apiruntime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiv1alpha1.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			panic(err)
		}
	}

	clients.generic, err = client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
}
