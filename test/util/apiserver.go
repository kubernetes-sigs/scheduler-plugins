/*
Copyright 2020 The Kubernetes Authors.

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

package util

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"testing"
	"time"

	"github.com/google/uuid"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericapiserveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/webhook"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"
	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/controlplane"
	"k8s.io/kubernetes/test/utils"
)

// TestServerSetup holds configuration information for a kube-apiserver test server.
type TestServerSetup struct {
	ModifyServerRunOptions func(*options.ServerRunOptions)
	ModifyServerConfig     func(*controlplane.Config)
}

// StartApi runs a kube-apiserver, optionally calling out to the setup.ModifyServerRunOptions and setup.ModifyServerConfig functions
func StartApi(t *testing.T, stopCh <-chan struct{}) (client.Interface, *rest.Config) {
	certDir, _ := ioutil.TempDir("", "test-integration")
	go func() {
		<-stopCh
		os.RemoveAll(certDir)
	}()

	_, defaultServiceClusterIPRange, _ := net.ParseCIDR("10.0.0.0/24")
	clientSigningKey, err := utils.NewPrivateKey()
	if err != nil {
		t.Fatalf("%+v", err)
	}
	clientSigningCert, err := cert.NewSelfSignedCACert(cert.Config{CommonName: "client-ca"}, clientSigningKey)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	clientCACertFile, _ := ioutil.TempFile(certDir, "client-ca.crt")
	if err := ioutil.WriteFile(clientCACertFile.Name(), utils.EncodeCertPEM(clientSigningCert), 0644); err != nil {
		t.Fatalf("%+v", err)
	}

	listener, _, err := genericapiserveroptions.CreateListener("tcp", "127.0.0.1:0", net.ListenConfig{})
	if err != nil {
		t.Fatalf("%+v", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(clientSigningKey)
	if err != nil {
		t.Fatal(err)
	}

	keyPath := path.Join(certDir, "client.key")

	if err := ioutil.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}), 0666); err != nil {
		t.Fatal(err)
	}

	kubeAPIServerOptions := options.NewServerRunOptions()
	kubeAPIServerOptions.SecureServing.Listener = listener
	kubeAPIServerOptions.SecureServing.BindAddress = net.ParseIP("127.0.0.1")
	kubeAPIServerOptions.SecureServing.ServerCert.CertDirectory = certDir
	kubeAPIServerOptions.Etcd.StorageConfig.Prefix = path.Join("/", uuid.New().String(), "registry")
	kubeAPIServerOptions.Etcd.StorageConfig.Transport.ServerList = IntegrationEtcdServers()
	kubeAPIServerOptions.ServiceClusterIPRanges = defaultServiceClusterIPRange.String()
	kubeAPIServerOptions.Authentication.ClientCert.ClientCA = clientCACertFile.Name()
	kubeAPIServerOptions.Authorization.Modes = []string{"Node", "RBAC"}
	kubeAPIServerOptions.ServiceAccountSigningKeyFile = keyPath
	kubeAPIServerOptions.Admission.GenericAdmission.DisablePlugins = []string{"Priority", "TaintNodesByCondition"}

	completedOptions, err := app.Complete(kubeAPIServerOptions)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	tunneler, proxyTransport, err := app.CreateNodeDialer(completedOptions)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	kubeAPIServerConfig, serviceResolver, pluginInitializer, err := app.CreateKubeAPIServerConfig(completedOptions, tunneler, proxyTransport)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	// If additional API servers are added, they should be gated.
	apiExtensionsConfig, err := createAPIExtensionsConfig(*kubeAPIServerConfig.GenericConfig, kubeAPIServerConfig.ExtraConfig.VersionedInformers, pluginInitializer, completedOptions.ServerRunOptions, completedOptions.MasterCount,
		serviceResolver, webhook.NewDefaultAuthenticationInfoResolverWrapper(proxyTransport, kubeAPIServerConfig.GenericConfig.EgressSelector, kubeAPIServerConfig.GenericConfig.LoopbackClientConfig))
	if err != nil {
		t.Fatalf("%+v", err)
	}
	apiExtensionsServer, err := createAPIExtensionsServer(apiExtensionsConfig, genericapiserver.NewEmptyDelegate())
	if err != nil {
		t.Fatalf("%+v", err)
	}

	kubeAPIServer, err := app.CreateKubeAPIServer(kubeAPIServerConfig, apiExtensionsServer.GenericAPIServer)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	// aggregator comes last in the chain
	aggregatorConfig, err := createAggregatorConfig(*kubeAPIServerConfig.GenericConfig, completedOptions.ServerRunOptions, kubeAPIServerConfig.ExtraConfig.VersionedInformers, serviceResolver, proxyTransport, pluginInitializer)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	aggregatorServer, err := createAggregatorServer(aggregatorConfig, kubeAPIServer.GenericAPIServer, apiExtensionsServer.Informers)
	if err != nil {
		// we don't need special handling for innerStopCh because the aggregator server doesn't create any go routines
		t.Fatalf("%+v", err)
	}

	go func() {
		prepared, err := aggregatorServer.PrepareRun()
		if err != nil {
			t.Fatalf("%+v", err)
		}

		prepared.Run(stopCh)
	}()

	// Adjust the loopback config for external use (external server name and CA)
	kubeAPIServerClientConfig := rest.CopyConfig(kubeAPIServerConfig.GenericConfig.LoopbackClientConfig)
	kubeAPIServerClientConfig.CAFile = path.Join(certDir, "apiserver.crt")
	kubeAPIServerClientConfig.CAData = nil
	kubeAPIServerClientConfig.ServerName = ""

	// wait for health
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		healthzConfig := rest.CopyConfig(kubeAPIServerClientConfig)
		healthzConfig.ContentType = ""
		healthzConfig.AcceptContentTypes = ""
		kubeClient, err := client.NewForConfig(healthzConfig)
		if err != nil {
			// this happens because we race the API server start
			t.Logf("%+v", err)
			return false, nil
		}

		healthStatus := 0
		kubeClient.Discovery().RESTClient().Get().AbsPath("/healthz").Do(context.TODO()).StatusCode(&healthStatus)
		if healthStatus != http.StatusOK {
			return false, nil
		}

		if _, err := kubeClient.CoreV1().Namespaces().Get(context.TODO(), "default", metav1.GetOptions{}); err != nil {
			return false, nil
		}
		if _, err := kubeClient.CoreV1().Namespaces().Get(context.TODO(), "kube-system", metav1.GetOptions{}); err != nil {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		t.Fatalf("%+v", err)
	}

	kubeAPIServerClient, err := client.NewForConfig(kubeAPIServerClientConfig)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	return kubeAPIServerClient, kubeAPIServerClientConfig
}
