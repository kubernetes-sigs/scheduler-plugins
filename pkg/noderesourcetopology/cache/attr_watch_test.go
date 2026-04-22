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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/go-logr/logr/testr"
	"k8s.io/klog/v2"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
)

func TestWatcherFiltersEvents(t *testing.T) {
	ch := make(chan string, 10)
	wt := Watcher{
		lh:       klog.Background(),
		eventCh:  ch,
		lastConf: make(map[string]nodeconfig.TopologyManager),
	}

	tcases := []struct {
		description string
		ev          watch.Event
		forwarded   bool
	}{
		{
			description: "deleted event ignored",
			ev: watch.Event{
				Type: watch.Deleted,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{Name: "node-7"},
				},
			},
			forwarded: false,
		},
		{
			description: "added event ignored",
			ev: watch.Event{
				Type: watch.Added,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{Name: "topologyManagerScope", Value: "container"},
						{Name: "topologyManagerPolicy", Value: "single-numa-node"},
					},
				},
			},
			forwarded: false,
		},
		{
			description: "first modified for unknown node forwarded",
			ev: watch.Event{
				Type: watch.Modified,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{Name: "topologyManagerScope", Value: "container"},
						{Name: "topologyManagerPolicy", Value: "single-numa-node"},
					},
				},
			},
			forwarded: true,
		},
		{
			description: "modified for known node no attr change",
			ev: watch.Event{
				Type: watch.Modified,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{Name: "topologyManagerScope", Value: "container"},
						{Name: "topologyManagerPolicy", Value: "single-numa-node"},
					},
				},
			},
			forwarded: false,
		},
		{
			description: "modified for known node with attr change",
			ev: watch.Event{
				Type: watch.Modified,
				Object: &topologyv1alpha2.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{Name: "node-0"},
					Attributes: []topologyv1alpha2.AttributeInfo{
						{Name: "topologyManagerScope", Value: "pod"},
						{Name: "topologyManagerPolicy", Value: "single-numa-node"},
					},
				},
			},
			forwarded: true,
		},
	}

	for _, tcase := range tcases {
		t.Run(tcase.description, func(t *testing.T) {
			before := len(ch)
			wt.processEvent(tcase.ev)
			after := len(ch)
			if tcase.forwarded && after != before+1 {
				t.Errorf("event should have been forwarded")
			}
			if !tcase.forwarded && after != before {
				t.Errorf("event should NOT have been forwarded")
			}
		})
	}
}

func TestWatcherOverflowSelfHealing(t *testing.T) {
	// Channel capacity 1: a second send will overflow.
	ch := make(chan string, 1)
	wt := Watcher{
		lh:      testr.New(t),
		eventCh: ch,
		// Pre-populate two known nodes so we can trigger attr-change sends.
		lastConf: map[string]nodeconfig.TopologyManager{
			"node-A": {Policy: "single-numa-node", Scope: "container"},
			"node-B": {Policy: "single-numa-node", Scope: "container"},
		},
	}

	// First attr-change send succeeds — fills the channel.
	wt.processEvent(watch.Event{
		Type: watch.Modified,
		Object: &topologyv1alpha2.NodeResourceTopology{
			ObjectMeta: metav1.ObjectMeta{Name: "node-A"},
			Attributes: []topologyv1alpha2.AttributeInfo{
				{Name: "topologyManagerScope", Value: "pod"},
				{Name: "topologyManagerPolicy", Value: "single-numa-node"},
			},
		},
	})
	if len(ch) != 1 {
		t.Fatalf("first event should have been forwarded, channel len=%d", len(ch))
	}

	// Second attr-change for a different node overflows. lastConf
	// must NOT be updated so the next attempt retries.
	wt.processEvent(watch.Event{
		Type: watch.Modified,
		Object: &topologyv1alpha2.NodeResourceTopology{
			ObjectMeta: metav1.ObjectMeta{Name: "node-B"},
			Attributes: []topologyv1alpha2.AttributeInfo{
				{Name: "topologyManagerScope", Value: "pod"},
				{Name: "topologyManagerPolicy", Value: "single-numa-node"},
			},
		},
	})
	if len(ch) != 1 {
		t.Fatalf("channel should still have 1 item (overflow dropped), got %d", len(ch))
	}
	if conf := wt.lastConf["node-B"]; conf.Scope != "container" {
		t.Fatalf("lastConf for node-B should still have old config after dropped send, got scope=%q", conf.Scope)
	}

	// Drain the channel to make room.
	<-ch

	// Same Modified event for node-B retries and succeeds because
	// lastConf still has the old config (overflow didn't update it).
	wt.processEvent(watch.Event{
		Type: watch.Modified,
		Object: &topologyv1alpha2.NodeResourceTopology{
			ObjectMeta: metav1.ObjectMeta{Name: "node-B"},
			Attributes: []topologyv1alpha2.AttributeInfo{
				{Name: "topologyManagerScope", Value: "pod"},
				{Name: "topologyManagerPolicy", Value: "single-numa-node"},
			},
		},
	})
	if len(ch) != 1 {
		t.Fatalf("retry event should have been forwarded after draining, channel len=%d", len(ch))
	}
	if conf := wt.lastConf["node-B"]; conf.Scope != "pod" {
		t.Fatalf("lastConf for node-B should be updated after successful retry, got scope=%q", conf.Scope)
	}
}

func TestDrainNRTEvents(t *testing.T) {
	lh := testr.New(t)
	ch := make(chan string, 10)
	ov := &OverReserve{
		lh:                  lh,
		nodesWithAttrUpdate: newCounter(),
		nrtUpdateCh:         ch,
	}

	// Send two node names
	ch <- "worker-0"
	ch <- "worker-1"

	ov.drainNRTEvents(lh)

	if !ov.nodesWithAttrUpdate.IsSet("worker-0") {
		t.Errorf("worker-0 should be queued")
	}
	if !ov.nodesWithAttrUpdate.IsSet("worker-1") {
		t.Errorf("worker-1 should be queued")
	}

	// Draining again with empty channel should be a no-op
	ov.drainNRTEvents(lh)
}
