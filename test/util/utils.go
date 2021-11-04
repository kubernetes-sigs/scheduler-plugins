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
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"
)

func PrintPods(t *testing.T, cs clientset.Interface, ns string) {
	podList, err := cs.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Errorf("Failed to list Pods: %v", err)
		return
	}
	for _, pod := range podList.Items {
		t.Logf("Pod %q scheduled to %q", pod.Name, pod.Spec.NodeName)
	}
}

func MakeNodesAndPods(labels map[string]string, existingPodsNum, allNodesNum int) (existingPods []*corev1.Pod, allNodes []*corev1.Node) {
	type keyVal struct {
		k string
		v string
	}
	var labelPairs []keyVal
	for k, v := range labels {
		labelPairs = append(labelPairs, keyVal{k: k, v: v})
	}
	// build nodes
	for i := 0; i < allNodesNum; i++ {
		res := map[corev1.ResourceName]string{
			corev1.ResourceCPU:  "1",
			corev1.ResourcePods: "20",
		}
		node := st.MakeNode().Name(fmt.Sprintf("node%d", i)).Capacity(res)
		allNodes = append(allNodes, &node.Node)
	}
	// build pods
	for i := 0; i < existingPodsNum; i++ {
		podWrapper := st.MakePod().Name(fmt.Sprintf("pod%d", i)).Node(fmt.Sprintf("node%d", i%allNodesNum))
		// apply labels[0], labels[0,1], ..., labels[all] to each pod in turn
		for _, p := range labelPairs[:i%len(labelPairs)+1] {
			podWrapper = podWrapper.Label(p.k, p.v)
		}
		existingPods = append(existingPods, podWrapper.Obj())
	}
	return
}

func MakePG(name, namespace string, min int32, creationTime *time.Time, minResource *corev1.ResourceList) *v1alpha1.PodGroup {
	var ti int32 = 10
	pg := &v1alpha1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.PodGroupSpec{MinMember: min, ScheduleTimeoutSeconds: &ti},
	}
	if creationTime != nil {
		pg.CreationTimestamp = metav1.Time{Time: *creationTime}
	}
	if minResource != nil {
		pg.Spec.MinResources = minResource
	}
	return pg
}

func createKubeConfig(clientCfg *restclient.Config) *clientcmdapi.Config {
	clusterNick := "cluster"
	userNick := "user"
	contextNick := "context"

	configCmd := clientcmdapi.NewConfig()

	credentials := clientcmdapi.NewAuthInfo()
	credentials.Token = clientCfg.BearerToken
	credentials.TokenFile = clientCfg.BearerTokenFile
	credentials.ClientCertificate = clientCfg.TLSClientConfig.CertFile
	if len(credentials.ClientCertificate) == 0 {
		credentials.ClientCertificateData = clientCfg.TLSClientConfig.CertData
	}
	credentials.ClientKey = clientCfg.TLSClientConfig.KeyFile
	if len(credentials.ClientKey) == 0 {
		credentials.ClientKeyData = clientCfg.TLSClientConfig.KeyData
	}
	configCmd.AuthInfos[userNick] = credentials

	cluster := clientcmdapi.NewCluster()
	cluster.Server = clientCfg.Host
	cluster.CertificateAuthority = clientCfg.CAFile
	if len(cluster.CertificateAuthority) == 0 {
		cluster.CertificateAuthorityData = clientCfg.CAData
	}
	cluster.InsecureSkipTLSVerify = clientCfg.Insecure
	configCmd.Clusters[clusterNick] = cluster

	context := clientcmdapi.NewContext()
	context.Cluster = clusterNick
	context.AuthInfo = userNick
	configCmd.Contexts[contextNick] = context
	configCmd.CurrentContext = contextNick

	return configCmd
}

func BuildKubeConfigFile(config *restclient.Config) string {
	kubeConfigPath := ""
	if tempFile, err := ioutil.TempFile(os.TempDir(), "kubeconfig-"); err == nil {
		kubeConfig := createKubeConfig(config)
		clientcmd.WriteToFile(*kubeConfig, tempFile.Name())
		kubeConfigPath = tempFile.Name()
		return kubeConfigPath
	}
	return ""
}

func MakePod(podName string, namespace string, memReq int64, cpuReq int64, priority int32, uid string, nodeName string) *corev1.Pod {
	pause := imageutils.GetPauseImageName()
	pod := st.MakePod().Namespace(namespace).Name(podName).Container(pause).
		Priority(priority).Node(nodeName).UID(uid).ZeroTerminationGracePeriod().Obj()
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(memReq, resource.DecimalSI),
			corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuReq, resource.DecimalSI),
		},
	}
	return pod
}
