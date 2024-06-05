/*
Copyright 2024 The Kubernetes Authors.

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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/klog/v2"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

func TestWatcherProcessEvent(t *testing.T) {
	nrts := []topologyv1alpha2.NodeResourceTopology{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-0",
			},
			Attributes: []topologyv1alpha2.AttributeInfo{
				{
					Name:  "topologyManagerScope",
					Value: "pod",
				},
				{
					Name:  "topologyManagerPolicy",
					Value: "restricted",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
			},
			Attributes: []topologyv1alpha2.AttributeInfo{
				{
					Name:  "topologyManagerScope",
					Value: "container",
				},
				{
					Name:  "topologyManagerPolicy",
					Value: "single-numa-node",
				},
			},
		},
	}

	tcases := []struct {
		description string
		ev          watch.Event
		expected    []string
	}{
		{
			description: "irrelevant object",
			ev: watch.Event{
				Type: watch.Added,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-7",
					},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{
							Name:  "topologyManagerScope",
							Value: "container",
						},
						{
							Name:  "topologyManagerPolicy",
							Value: "single-numa-node",
						},
					},
				},
			},
			expected: []string{},
		},
		{
			description: "scope change",
			ev: watch.Event{
				Type: watch.Modified,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
					},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{
							Name:  "topologyManagerScope",
							Value: "container",
						},
						{
							Name:  "topologyManagerPolicy",
							Value: "restricted",
						},
					},
				},
			},
			expected: []string{"node-0"},
		},
		{
			description: "all attr change",
			ev: watch.Event{
				Type: watch.Modified,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-0",
					},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{
							Name:  "topologyManagerScope",
							Value: "container",
						},
						{
							Name:  "topologyManagerPolicy",
							Value: "single-numa-node",
						},
					},
				},
			},
			expected: []string{"node-0"},
		},
	}

	wt := Watcher{
		lh:    klog.Background(),
		nrts:  newNrtStore(klog.Background(), nrts),
		nodes: newCounter(),
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			wt.ProcessEvent(tcase.ev)
			got := wt.nodes.Keys()
			if !reflect.DeepEqual(got, tcase.expected) {
				t.Errorf("got=%+v expected=%+v", got, tcase.expected)
			}
		})
	}
}
