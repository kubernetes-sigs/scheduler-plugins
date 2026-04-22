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
	"context"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/watch"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
)

// Watcher forwards node names to a channel when it detects NRT events that
// require action by the Resync loop. The resync loop is the only component
// which modifies the counters and act upon that. This model has a clearer,
// ownership, trends towards share nothing (less coupling) and it's friendlier
// if we eventually enable node-level parallelism (see global lock in OverReserve).
// Watcher tracks the TopologyManager attributes change locally to minimize
// the updates it sends back to the Resync goroutine.
type Watcher struct {
	lh       logr.Logger
	eventCh  chan<- string
	lastConf map[string]nodeconfig.TopologyManager
}

func (wt Watcher) NodeResourceTopologies(ctx context.Context, client ctrlclient.WithWatch) {
	done := false
	for !done {
		wt.lh.Info("start watching NRT objects")

		nrtObjs := topologyv1alpha2.NodeResourceTopologyList{}
		wa, err := client.Watch(ctx, &nrtObjs)
		if err != nil {
			wt.lh.Error(err, "cannot watch NRT objects")
			return
		}

		for !done {
			select {
			case ev := <-wa.ResultChan():
				wt.processEvent(ev)

			case <-ctx.Done():
				wt.lh.Info("stop watching NRT objects")
				wa.Stop()
				done = true
			}
		}

		wt.lh.Info("done watching NRT objects")
	}
}

func (wt Watcher) processEvent(ev watch.Event) {
	// TODO: handle node deletion. How common do we expect it to be?
	// Modified is trivially verified; Added can happen if we
	// happen to run the scheduler before node update agents,
	// so turns out not so uncommon.
	if ev.Type != watch.Modified {
		return
	}
	nrtObj, ok := ev.Object.(*topologyv1alpha2.NodeResourceTopology)
	if !ok {
		return
	}

	newConf := nodeconfig.TopologyManagerFromNodeResourceTopology(wt.lh, nrtObj)

	oldConf := wt.lastConf[nrtObj.Name]
	if oldConf.Equal(newConf) {
		return
	}

	select {
	case wt.eventCh <- nrtObj.Name:
		// Update lastConf only after a successful send; so, if the channel is
		// full, the next update will retry automatically another send.
		wt.lastConf[nrtObj.Name] = newConf
		wt.lh.V(2).Info("attribute change", logging.KeyNode, nrtObj.Name)
	default:
		wt.lh.V(2).Info("NRT event channel full, will retry", logging.KeyNode, nrtObj.Name)
	}
}
