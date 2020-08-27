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
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	"k8s.io/kubernetes/pkg/scheduler/listers"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

// FakeNew is used for test.
func FakeNew(clock util.Clock, stop chan struct{}) (*Coscheduling, error) {
	cs := &Coscheduling{
		clock: clock,
	}
	go wait.Until(cs.podGroupInfoGC, time.Duration(cs.args.PodGroupGCIntervalSeconds)*time.Second, stop)
	return cs, nil
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
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
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
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
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
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup1",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
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
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},

		{
			name: "p1.priority less than p2.priority, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "p1.priority greater than p2.priority, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
						Priority: &lowPriority,
					},
				},
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p1 is added to schedulingQ earlier than p2, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority. p2 is added to schedulingQ earlier than p1, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t2,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: false, // p2 should be ahead of p1 in the queue
		},
		{
			name: "equal priority and creation time, p1 belongs to podGroup1 and p2 belongs to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1", Labels: labels1},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			expected: true, // p1 should be ahead of p2 in the queue
		},
		{
			name: "equal priority and creation time, p2 belong to podGroup2",
			p1: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "namespace1"},
					Spec: v1.PodSpec{
						Priority: &highPriority,
					},
				},
				InitialAttemptTimestamp: t1,
			},
			p2: &framework.PodInfo{
				Pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "namespace2", Labels: labels2},
					Spec: v1.PodSpec{
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
	tests := []struct {
		name     string
		pod      *v1.Pod
		pods     []*v1.Pod
		expected framework.Code
	}{
		{
			name: "pod does not belong to any podGroup",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(PodGroupName, "pg1").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(PodGroupName, "pg2").Obj(),
			},
			expected: framework.Success,
		},
		{
			name: "pod belongs to podGroup1 and its PodGroupMinAvailable does not match the group's",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "2").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "3").Obj(),
			},
			expected: framework.Unschedulable,
		},
		{
			name: "pod belongs to podGroup1 and its priority does not match the group's",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Priority(20).Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "2").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Priority(10).Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "2").Obj(),
			},
			expected: framework.Unschedulable,
		},
		{
			name: "pod belongs to podGroup1, the number of total pods is less than minAvailable",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "3").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "3").Obj(),
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(PodGroupName, "pg2").Label(PodGroupMinAvailable, "1").Obj(),
			},
			expected: framework.Unschedulable,
		},
		{
			name: "pod belongs to podGroup2, the number of total pods is not less than minAvailable",
			pod:  st.MakePod().Name("p").UID("p").Namespace("ns1").Label(PodGroupName, "pg2").Label(PodGroupMinAvailable, "3").Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("pg2-1").UID("pg2-1").Namespace("ns1").Label(PodGroupName, "pg2").Label(PodGroupMinAvailable, "3").Obj(),
				st.MakePod().Name("pg1-1").UID("pg1-1").Namespace("ns1").Label(PodGroupName, "pg1").Label(PodGroupMinAvailable, "1").Obj(),
				st.MakePod().Name("pg2-2").UID("pg2-2").Namespace("ns1").Label(PodGroupName, "pg2").Label(PodGroupMinAvailable, "3").Obj(),
			},
			expected: framework.Success,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods()
			coscheduling := &Coscheduling{podLister: podInformer.Lister()}
			for _, p := range tt.pods {
				coscheduling.getOrCreatePodGroupInfo(p, time.Now())
				podInformer.Informer().GetStore().Add(p)
			}

			podInformer.Informer().GetStore().Add(tt.pod)
			if got := coscheduling.PreFilter(nil, nil, tt.pod); got.Code() != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got.Code())
			}
		})
	}
}

func TestPermit(t *testing.T) {
	tests := []struct {
		name     string
		pods     []*v1.Pod
		expected []framework.Code
	}{
		{
			name: "pods do not belong to any podGroup",
			pods: []*v1.Pod{
				st.MakePod().Name("pod1").UID("pod1").Obj(),
				st.MakePod().Name("pod2").UID("pod2").Obj(),
				st.MakePod().Name("pod3").UID("pod3").Obj(),
			},
			expected: []framework.Code{framework.Success, framework.Success, framework.Success},
		},
		{
			name: "pods belong to a podGroup",
			pods: []*v1.Pod{
				st.MakePod().Name("pod1").UID("pod1").Label(PodGroupName, "permit").Label(PodGroupMinAvailable, "3").Obj(),
				st.MakePod().Name("pod2").UID("pod2").Label(PodGroupName, "permit").Label(PodGroupMinAvailable, "3").Obj(),
				st.MakePod().Name("pod3").UID("pod3").Label(PodGroupName, "permit").Label(PodGroupMinAvailable, "3").Obj(),
			},
			expected: []framework.Code{framework.Wait, framework.Wait, framework.Success},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			for _, p := range tt.pods {
				podInformer.GetStore().Add(p)
			}

			registeredPlugins := []st.RegisterPluginFunc{
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				st.RegisterPluginAsExtensions(Name, New, "QueueSort", "Permit"),
			}
			fakeSharedLister := &fakeSharedLister{pods: tt.pods}
			f, err := st.NewFramework(
				registeredPlugins,
				framework.WithClientSet(cs),
				framework.WithInformerFactory(informerFactory),
				framework.WithSnapshotSharedLister(fakeSharedLister),
			)
			if err != nil {
				t.Fatalf("fail to create framework: %s", err)
			}

			for i := range tt.pods {
				if got := f.RunPermitPlugins(context.TODO(), nil, tt.pods[i], ""); got.Code() != tt.expected[i] {
					t.Errorf("[%v] want %v, but got %v", i, tt.expected[i], got.Code())
				}

				// This operation simulates the operation of AssumePod in scheduling cycle.
				// The current pod does not exist in the snapshot during this scheduling cycle.
				tt.pods[i].Spec.NodeName = "Node"
			}
		})
	}
}

func TestPodGroupClean(t *testing.T) {
	tests := []struct {
		name         string
		pod          *v1.Pod
		podGroupName string
	}{
		{
			name:         "pod belongs to a podGroup",
			pod:          st.MakePod().Name("pod1").UID("pod1").Label(PodGroupName, "gc").Label(PodGroupMinAvailable, "3").Obj(),
			podGroupName: "gc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := make(chan struct{})
			defer close(stop)

			c := clock.NewFakeClock(time.Now())
			cs, err := FakeNew(c, stop)
			if err != nil {
				t.Fatalf("fail to init coscheduling: %s", err)
			}

			cs.getOrCreatePodGroupInfo(tt.pod, time.Now())
			_, ok := cs.podGroupInfos.Load(fmt.Sprintf("%v/%v", tt.pod.Namespace, tt.podGroupName))
			if !ok {
				t.Fatalf("fail to create PodGroup in coscheduling: %s", tt.pod.Name)
			}

			cs.markPodGroupAsExpired(tt.pod)
			pg, ok := cs.podGroupInfos.Load(fmt.Sprintf("%v/%v", tt.pod.Namespace, tt.podGroupName))
			if ok && pg.(*PodGroupInfo).deletionTimestamp == nil {
				t.Fatalf("fail to clean up PodGroup : %s", tt.pod.Name)
			}

			c.Step(time.Duration(cs.args.PodGroupExpirationTimeSeconds)*time.Second + time.Second)
			// Wait for asynchronous deletion.
			err = wait.Poll(time.Millisecond*200, 1*time.Second, func() (bool, error) {
				_, ok = cs.podGroupInfos.Load(fmt.Sprintf("%v/%v", tt.pod.Namespace, tt.podGroupName))
				return !ok, nil
			})

			if err != nil {
				t.Fatalf("fail to gc PodGroup in coscheduling: %s", tt.pod.Name)
			}
		})
	}
}

var _ listers.SharedLister = &fakeSharedLister{}

type fakeSharedLister struct {
	pods []*v1.Pod
}

func (f *fakeSharedLister) Pods() listers.PodLister {
	return f
}

func (f *fakeSharedLister) List(_ labels.Selector) ([]*v1.Pod, error) {
	return f.pods, nil
}

func (f *fakeSharedLister) FilteredList(podFilter listers.PodFilter, selector labels.Selector) ([]*v1.Pod, error) {
	pods := make([]*v1.Pod, 0, len(f.pods))
	for _, pod := range f.pods {
		if podFilter(pod) && selector.Matches(labels.Set(pod.Labels)) {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

func (f *fakeSharedLister) NodeInfos() listers.NodeInfoLister {
	return nil
}
