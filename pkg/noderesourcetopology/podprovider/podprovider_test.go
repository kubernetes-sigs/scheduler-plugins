/*
Copyright The Kubernetes Authors.

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

package podprovider

import (
	"errors"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	podlisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/ptr"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
)

type fakeClientGoPodLister struct {
	pods []*corev1.Pod
	err  error
}

func (f *fakeClientGoPodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.pods, f.err
}

func (f *fakeClientGoPodLister) Pods(namespace string) podlisterv1.PodNamespaceLister {
	panic("Pods() should not be called")
}

func makePod(name string, phase corev1.PodPhase, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      name,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func podNames(pods []*corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}

func TestFilteredListerList(t *testing.T) {
	allPods := []*corev1.Pod{
		makePod("running", corev1.PodRunning, "node1"),
		makePod("succeeded", corev1.PodSucceeded, "node1"),
		makePod("failed", corev1.PodFailed, "node1"),
		makePod("pending", corev1.PodPending, "node1"),
		makePod("unbound-running", corev1.PodRunning, ""),
		makePod("unbound-succeeded", corev1.PodSucceeded, ""),
	}

	tcases := []struct {
		description  string
		filter       PodFilterFunc
		pods         []*corev1.Pod
		listerErr    error
		expected     []string
		expectedErr  error
	}{
		{
			description: "shared keeps only Running",
			filter:      IsPodRelevantShared,
			pods:        allPods,
			// Shared only checks phase; unbound Running still matches.
			expected: []string{"running", "unbound-running"},
		},
		{
			description: "shared drops Succeeded and Failed",
			filter:      IsPodRelevantShared,
			pods: []*corev1.Pod{
				makePod("succeeded", corev1.PodSucceeded, "node1"),
				makePod("failed", corev1.PodFailed, "node1"),
			},
			expected: []string{},
		},
		{
			description: "dedicated keeps terminal phases and drops Pending/unbound",
			filter:      IsPodRelevantDedicated,
			pods:        allPods,
			expected:    []string{"running", "succeeded", "failed"},
		},
		{
			description: "dedicated drops Pending",
			filter:      IsPodRelevantDedicated,
			pods: []*corev1.Pod{
				makePod("pending", corev1.PodPending, "node1"),
			},
			expected: []string{},
		},
		{
			description: "dedicated drops unbound",
			filter:      IsPodRelevantDedicated,
			pods: []*corev1.Pod{
				makePod("unbound", corev1.PodSucceeded, ""),
			},
			expected: []string{},
		},
		{
			description: "propagates lister error",
			filter:      IsPodRelevantShared,
			listerErr:   errors.New("list failed"),
			expectedErr: errors.New("list failed"),
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			fl := &filteredLister{
				lister: &fakeClientGoPodLister{
					pods: tcase.pods,
					err:  tcase.listerErr,
				},
				filter: tcase.filter,
			}
			got, err := fl.List(testr.New(t), labels.Everything())
			if tcase.expectedErr != nil {
				if err == nil || err.Error() != tcase.expectedErr.Error() {
					t.Fatalf("error mismatch: got %v expected %v", err, tcase.expectedErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tcase.expected, podNames(got)); diff != "" {
				t.Errorf("unexpected pods (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWantsDedicatedInformer(t *testing.T) {
	tcases := []struct {
		description string
		cacheConf   *apiconfig.NodeResourceTopologyCache
		expected    bool
	}{
		{
			description: "nil cache config uses shared",
			cacheConf:   nil,
			expected:    false,
		},
		{
			description: "nil InformerMode uses shared",
			cacheConf:   &apiconfig.NodeResourceTopologyCache{},
			expected:    false,
		},
		{
			description: "Shared InformerMode",
			cacheConf: &apiconfig.NodeResourceTopologyCache{
				InformerMode: ptr.To(apiconfig.CacheInformerShared),
			},
			expected: false,
		},
		{
			description: "Dedicated InformerMode",
			cacheConf: &apiconfig.NodeResourceTopologyCache{
				InformerMode: ptr.To(apiconfig.CacheInformerDedicated),
			},
			expected: true,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			got := wantsDedicatedInformer(tcase.cacheConf)
			if got != tcase.expected {
				t.Errorf("got %v expected %v", got, tcase.expected)
			}
		})
	}
}
