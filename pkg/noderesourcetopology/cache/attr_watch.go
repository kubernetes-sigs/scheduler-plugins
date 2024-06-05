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

type Watcher struct {
	lh    logr.Logger
	nrts  *nrtStore
	nodes counter
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
				wt.ProcessEvent(ev)

			case <-ctx.Done():
				wt.lh.Info("stop watching NRT objects")
				wa.Stop()
				done = true
			}
		}

		wt.lh.Info("done watching NRT objects")
	}
}

func (wt Watcher) ProcessEvent(ev watch.Event) bool {
	if ev.Type != watch.Modified {
		return false
	}

	nrtObj, ok := ev.Object.(*topologyv1alpha2.NodeResourceTopology)
	if !ok {
		wt.lh.Info("unexpected object %T", ev.Object)
		return false
	}

	nrtCur := wt.nrts.GetNRTCopyByNodeName(nrtObj.Name)
	if nrtCur == nil {
		wt.lh.Info("modified non-existent NRT", logging.KeyNode, nrtObj.Name)
		return false
	}

	if !areAttrsChanged(nrtCur, nrtObj) {
		return false
	}

	wt.lh.V(4).Info("attribute change", logging.KeyNode, nrtObj.Name)
	wt.nodes.Incr(nrtObj.Name)
	return true
}

func areAttrsChanged(oldNrt, newNrt *topologyv1alpha2.NodeResourceTopology) bool {
	lh := logr.Discard() // avoid spam in the logs
	oldConf := nodeconfig.TopologyManagerFromNodeResourceTopology(lh, oldNrt)
	newConf := nodeconfig.TopologyManagerFromNodeResourceTopology(lh, newNrt)
	return !oldConf.Equal(newConf)
}
