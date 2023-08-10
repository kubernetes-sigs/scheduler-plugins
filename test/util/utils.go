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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
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
		pg.Spec.MinResources = *minResource
	}
	return pg
}

func UpdatePGStatus(pg *v1alpha1.PodGroup, phase v1alpha1.PodGroupPhase, occupiedBy string, scheduled int32, running int32, succeeded int32, failed int32) *v1alpha1.PodGroup {
	pg.Status = v1alpha1.PodGroupStatus{
		Phase:      phase,
		OccupiedBy: occupiedBy,
		Running:    running,
		Succeeded:  succeeded,
		Failed:     failed,
	}
	return pg
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

// PodNotExist returns true if the given pod does not exist.
func PodNotExist(cs clientset.Interface, podNamespace, podName string) bool {
	_, err := cs.CoreV1().Pods(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	return errors.IsNotFound(err)
}

// WithLimits adds a new app or init container to the inner pod with a given resource map.
func WithLimits(p *st.PodWrapper, resMap map[string]string, initContainer bool) *st.PodWrapper {
	if len(resMap) == 0 {
		return p
	}

	containers, cntName := findContainerList(p, initContainer)

	*containers = append(*containers, corev1.Container{
		Name:  fmt.Sprintf("%s-%d", cntName, len(*containers)+1),
		Image: imageutils.GetPauseImageName(),
		Resources: corev1.ResourceRequirements{
			Limits: makeResListMap(resMap),
		},
	})

	return p
}

// WithRequests adds a new app or init container to the inner pod with a given resource map.
func WithRequests(p *st.PodWrapper, resMap map[string]string, initContainer bool) *st.PodWrapper {
	if len(resMap) == 0 {
		return p
	}

	containers, cntName := findContainerList(p, initContainer)

	*containers = append(*containers, corev1.Container{
		Name:  fmt.Sprintf("%s-%d", cntName, len(*containers)+1),
		Image: imageutils.GetPauseImageName(),
		Resources: corev1.ResourceRequirements{
			Requests: makeResListMap(resMap),
		},
	})

	return p
}

func findContainerList(p *st.PodWrapper, initContainer bool) (*[]corev1.Container, string) {
	if initContainer {
		return &p.Obj().Spec.InitContainers, "initcnt"
	}
	return &p.Obj().Spec.Containers, "cnt"
}

func makeResListMap(resMap map[string]string) corev1.ResourceList {
	res := corev1.ResourceList{}
	for k, v := range resMap {
		res[corev1.ResourceName(k)] = resource.MustParse(v)
	}
	return res
}

func MustNewPodInfo(t testing.TB, pod *corev1.Pod) *framework.PodInfo {
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatalf("expected err to be nil got: %v", err)
	}

	return podInfo
}
