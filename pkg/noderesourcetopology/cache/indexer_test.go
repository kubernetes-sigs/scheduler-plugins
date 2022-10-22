/*
Copyright 2022 The Kubernetes Authors.

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

package cache

import (
	"testing"
	"time"

	k8scache "k8s.io/client-go/tools/cache"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestNodeNameIndexerByIndexAdd(t *testing.T) {
	tests := []struct {
		name             string
		podEvents        []podEvent
		nodeName         string
		expectedPodNames sets.String
	}{
		{
			name: "empty",
		},
		{
			name: "no match",
			podEvents: makeAddEvents([]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1",
						Name:      "pod1",
						UID:       types.UID("ns1pod1"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-A",
					},
				},
			}),
		},
		{
			name: "match",
			podEvents: makeAddEvents([]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1",
						Name:      "pod1",
						UID:       types.UID("ns1pod1"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-A",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns2",
						Name:      "pod2",
						UID:       types.UID("ns2pod2"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-B",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns3",
						Name:      "pod3",
						UID:       types.UID("ns3pod3"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-A",
					},
				},
			}),
			nodeName:         "worker-node-A",
			expectedPodNames: sets.NewString("ns1/pod1", "ns3/pod3"),
		},
		{
			name: "labels",
			podEvents: makeAddEvents([]*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns1",
						Name:      "pod1",
						Labels: map[string]string{
							"app": "foo",
						},
						UID: types.UID("ns1pod1"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-A",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns2",
						Name:      "pod2",
						Labels: map[string]string{
							"app": "bar",
						},
						UID: types.UID("ns2pod2"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-B",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "ns3",
						Name:      "pod3",
						Labels: map[string]string{
							"app": "baz",
						},
						UID: types.UID("ns3pod3"),
					},
					Spec: corev1.PodSpec{
						NodeName: "worker-node-A",
					},
				},
			}),
			nodeName:         "worker-node-A",
			expectedPodNames: sets.NewString("ns1/pod1", "ns3/pod3"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fi := &fakeInformer{
				events: tt.podEvents,
			}
			nni := NewNodeNameIndexer(fi)

			fi.SendEvents()

			objs, err := nni.GetPodNamespacedNamesByNode(tt.name, tt.nodeName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotNames := sets.NewString()
			for _, obj := range objs {
				gotNames.Insert(obj.String())
			}

			if !gotNames.Equal(tt.expectedPodNames) {
				t.Errorf("%s: got but not expected: %#v", tt.name, gotNames.Difference(tt.expectedPodNames))
				t.Errorf("%s: expected but not got: %#v", tt.name, tt.expectedPodNames.Difference(gotNames))
			}
		})
	}

}

const (
	evAdd = iota
	evUpdate
	evDelete
)

func makeAddEvents(pods []*corev1.Pod) []podEvent {
	var evs []podEvent
	for _, pod := range pods {
		evs = append(evs, podEvent{
			kind: evAdd,
			pod:  pod,
		})
	}
	return evs
}

type podEvent struct {
	kind int
	pod  *corev1.Pod
}

type fakeInformer struct {
	rev    k8scache.ResourceEventHandler
	events []podEvent
}

func (fi *fakeInformer) AddEventHandler(handler k8scache.ResourceEventHandler) {
	fi.rev = handler
}

func (fi *fakeInformer) SendEvents() {
	for _, ev := range fi.events {
		switch ev.kind {
		case evAdd:
			fi.rev.OnAdd(ev.pod)
		case evUpdate:
			fi.rev.OnUpdate(ev.pod, ev.pod) // TODO
		case evDelete:
			fi.rev.OnDelete(ev.pod)
		}
	}
}

func (fi *fakeInformer) AddEventHandlerWithResyncPeriod(handler k8scache.ResourceEventHandler, resyncPeriod time.Duration) {
}
func (fi *fakeInformer) GetStore() k8scache.Store                                      { return nil }
func (fi *fakeInformer) GetController() k8scache.Controller                            { return nil }
func (fi *fakeInformer) Run(stopCh <-chan struct{})                                    {}
func (fi *fakeInformer) HasSynced() bool                                               { return true }
func (fi *fakeInformer) LastSyncResourceVersion() string                               { return "" }
func (fi *fakeInformer) SetWatchErrorHandler(handler k8scache.WatchErrorHandler) error { return nil }
func (fi *fakeInformer) SetTransform(handler k8scache.TransformFunc) error             { return nil }

func TestNamespacedNameListToString(t *testing.T) {
	tests := []struct {
		name     string
		nns      []types.NamespacedName
		expected string
	}{
		{
			name: "empty",
		},
		{
			name: "single item",
			nns: []types.NamespacedName{
				{
					Namespace: "ns1",
					Name:      "obj1",
				},
			},
			expected: "ns1/obj1",
		},
		{
			name: "two items",
			nns: []types.NamespacedName{
				{
					Namespace: "ns1",
					Name:      "obj1",
				},
				{
					Namespace: "ns2",
					Name:      "obj2",
				},
			},
			expected: "ns1/obj1, ns2/obj2",
		},
		{
			name: "multiple items",
			nns: []types.NamespacedName{
				{
					Namespace: "ns1",
					Name:      "obj1",
				},
				{
					Namespace: "ns1",
					Name:      "obj2",
				},
				{
					Namespace: "ns2",
					Name:      "obj1",
				},
				{
					Namespace: "ns2",
					Name:      "obj2",
				},
				{
					Namespace: "ns3",
					Name:      "obj1",
				},
			},
			expected: "ns1/obj1, ns1/obj2, ns2/obj1, ns2/obj2, ns3/obj1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := namespacedNameListToString(tt.nns)
			if got != tt.expected {
				t.Errorf("got %q expected %q", got, tt.expected)
			}
		})
	}
}
