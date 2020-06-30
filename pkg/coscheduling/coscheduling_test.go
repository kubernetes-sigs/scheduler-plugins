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

package coscheduling

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	"k8s.io/kubernetes/pkg/scheduler/listers"
)

const (
	queueSortPlugin = "no-op-queue-sort-plugin"
	bindPlugin      = "bind-plugin"
)

var emptyArgs = make([]config.PluginConfig, 0)

var _ framework.QueueSortPlugin = &TestQueueSortPlugin{}

// TestQueueSortPlugin is a no-op implementation for QueueSort extension point.
type TestQueueSortPlugin struct{}

func newQueueSortPlugin(_ *runtime.Unknown, _ framework.FrameworkHandle) (framework.Plugin, error) {
	return &TestQueueSortPlugin{}, nil
}

func (pl *TestQueueSortPlugin) Name() string {
	return queueSortPlugin
}

func (pl *TestQueueSortPlugin) Less(_, _ *framework.PodInfo) bool {
	return false
}

var _ framework.BindPlugin = &TestBindPlugin{}

// TestBindPlugin is a no-op implementation for Bind extension point.
type TestBindPlugin struct{}

func newBindPlugin(_ *runtime.Unknown, _ framework.FrameworkHandle) (framework.Plugin, error) {
	return &TestBindPlugin{}, nil
}

func (t TestBindPlugin) Name() string {
	return bindPlugin
}

func (t TestBindPlugin) Bind(ctx context.Context, state *framework.CycleState, p *corev1.Pod, nodeName string) *framework.Status {
	return nil
}

type fakeLister struct {
	called int
}

// List lists all Pods in the indexer.
func (*fakeLister) List(selector labels.Selector) (ret []*corev1.Pod, err error) {
	if selector.Matches(labels.Set{PodGroupName: "pg1"}) {
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg1-1", Namespace: "namespace1"}})
		return ret, nil
	}

	if selector.Matches(labels.Set{PodGroupName: "pg2"}) {
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-1", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-2", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-3", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		return ret, nil
	}

	return ret, nil
}

// Pods returns an object that can list and get Pods.
func (*fakeLister) Pods(namespace string) v1.PodNamespaceLister {
	return &fakePodNamespaceLister{}
}

func (*fakeLister) FilteredList(podFilter listers.PodFilter, selector labels.Selector) ([]*corev1.Pod, error) {
	return nil, nil
}

type fakePodNamespaceLister struct {
}

func (*fakePodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	return nil, nil
}

func (*fakePodNamespaceLister) List(selector labels.Selector) (ret []*corev1.Pod, err error) {
	if selector.Matches(labels.Set{PodGroupName: "pg1"}) {
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg1-1", Namespace: "namespace1"}})
		return ret, nil
	}

	if selector.Matches(labels.Set{PodGroupName: "pg2"}) {
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-1", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-2", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		ret = append(ret, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pg2-3", Namespace: "namespace2"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
		return ret, nil
	}

	return ret, nil
}

type fakeSharedLister struct {
}

func (*fakeSharedLister) Pods() listers.PodLister {
	return &fakeLister{}
}
func (*fakeSharedLister) NodeInfos() listers.NodeInfoLister {
	return nil
}

func newFrameworkWithQueueSortAndBind(r framework.Registry, pl *config.Plugins, plc []config.PluginConfig, opts ...framework.Option) (framework.Framework, error) {
	if _, ok := r[queueSortPlugin]; !ok {
		r[queueSortPlugin] = newQueueSortPlugin
	}
	if _, ok := r[bindPlugin]; !ok {
		r[bindPlugin] = newBindPlugin
	}
	plugins := &config.Plugins{}
	plugins.Append(pl)
	if plugins.QueueSort == nil || len(plugins.QueueSort.Enabled) == 0 {
		plugins.Append(&config.Plugins{
			QueueSort: &config.PluginSet{
				Enabled: []config.Plugin{{Name: queueSortPlugin}},
			},
		})
	}
	if plugins.Bind == nil || len(plugins.Bind.Enabled) == 0 {
		plugins.Append(&config.Plugins{
			Bind: &config.PluginSet{
				Enabled: []config.Plugin{{Name: bindPlugin}},
			},
		})
	}
	return framework.NewFramework(r, plugins, plc, opts...)
}

func TestLess(t *testing.T) {
	labels1 := map[string]string{
		PodGroupName:         "pg1",
		PodGroupMinAvailable: "3",
	}
	labels2 := map[string]string{
		PodGroupName:         "pg2",
		PodGroupMinAvailable: "5",
	}

	var lowPriority, highPriority = int32(10), int32(100)
	t1 := time.Now()
	t2 := t1.Add(time.Second)
	for _, tt := range []struct {
		name     string
		p1       *framework.PodInfo
		p2       *framework.PodInfo
		expected bool
	}{
		{
			name: "p1.priority less than p2.priority",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority less than p2.priority, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},

		{
			name: "p1.priority less than p2.priority, p1 and p2 belong to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 and p2 belong to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: corev1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 and p2 belong to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "equal priority. equal create time, p1 and p2 belong to podGroup1",
			p1: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: corev1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			coscheduling := &Coscheduling{}
			if got := coscheduling.Less(tt.p1, tt.p2); got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestPreFilter(t *testing.T) {
	coscheduling := &Coscheduling{podLister: &fakeLister{}}
	labels1 := map[string]string{
		PodGroupName:         "pg1",
		PodGroupMinAvailable: "3",
	}
	labels2 := map[string]string{
		PodGroupName:         "pg2",
		PodGroupMinAvailable: "3",
	}
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected framework.Code
	}{
		{
			name: "pod not belongs any podGroup",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
			},
			expected: framework.Success,
		},
		{
			name: "pod belongs podGroup1, but total pods count not match min",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
			},
			expected: framework.Unschedulable,
		},
		{
			name: "pod belongs podGroup1, and total pods count match min",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels2},
			},
			expected: framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coscheduling.setPodGroupInfo(tt.pod, time.Now())
			if got := coscheduling.PreFilter(nil, nil, tt.pod); got.Code() != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestPermit(t *testing.T) {
	permitLabel := map[string]string{
		PodGroupName:         "permit",
		PodGroupMinAvailable: "3",
	}

	tests := []struct {
		name     string
		pods     []*corev1.Pod
		expected []framework.Code
	}{
		{
			name: "common pod not belongs any podGroup",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3"}},
			},
			expected: []framework.Code{framework.Success, framework.Success, framework.Success},
		},
		{
			name: "pods belongs podGroup",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Labels: permitLabel, UID: types.UID("pod1")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Labels: permitLabel, UID: types.UID("pod2")}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Labels: permitLabel, UID: types.UID("pod3")}},
			},
			expected: []framework.Code{framework.Wait, framework.Wait, framework.Success},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := framework.Registry{}
			cfgPls := &config.Plugins{Permit: &config.PluginSet{}}
			if err := registry.Register(Name,
				func(_ *runtime.Unknown, handle framework.FrameworkHandle) (framework.Plugin, error) {
					coscheduling := &Coscheduling{frameworkHandle: handle, podLister: &fakeLister{}}
					for _, pod := range tt.pods {
						coscheduling.setPodGroupInfo(pod, time.Now())
					}
					return coscheduling, nil
				}); err != nil {
				t.Fatalf("fail to register filter plugin (%s)", Name)
			}

			// append plugins to permit pluginset
			cfgPls.Permit.Enabled = append(
				cfgPls.Permit.Enabled,
				config.Plugin{Name: Name})

			f, err := newFrameworkWithQueueSortAndBind(registry, cfgPls, emptyArgs, framework.WithSnapshotSharedLister(&fakeSharedLister{}))
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			if got := f.RunPermitPlugins(context.TODO(), nil, tt.pods[0], ""); got.Code() != tt.expected[0] {
				t.Errorf("expected %v, got %v", tt.expected[0], got.Code())
			}
			if got := f.RunPermitPlugins(context.TODO(), nil, tt.pods[1], ""); got.Code() != tt.expected[1] {
				t.Errorf("expected %v, got %v", tt.expected[1], got.Code())
			}
			if got := f.RunPermitPlugins(context.TODO(), nil, tt.pods[2], ""); got.Code() != tt.expected[2] {
				t.Errorf("expected %v, got %v", tt.expected[2], got.Code())
			}
		})
	}
}
