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
	"math"
	"time"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/clock"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	topologyv1alpha2 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"

	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/logging"
	"sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/nodeconfig"
)

type WatchReason int

const (
	WatchReasonNone WatchReason = iota
	WatchReasonAttrChanged
	WatchReasonNewlyAdded
)

type NRTEvent struct {
	Reason   WatchReason
	NodeName string
}

func (wr WatchReason) String() string {
	switch wr {
	case WatchReasonAttrChanged:
		return "attribute change"
	case WatchReasonNewlyAdded:
		return "newly added"
	}
	return "none"
}

// Watcher forwards node names to a channel when it detects NRT events that
// require action by the Resync loop. The resync loop is the only component
// which modifies the counters and act upon that. This model has a clearer,
// ownership, trends towards share nothing (less coupling) and it's friendlier
// if we eventually enable node-level parallelism (see global lock in OverReserve).
// Watcher tracks the TopologyManager attributes change locally to minimize
// the updates it sends back to the Resync goroutine.
type Watcher struct {
	lh       logr.Logger
	eventCh  chan<- NRTEvent
	lastConf map[string]nodeconfig.TopologyManager
	done     chan struct{}
}

func NewWatcher(lh logr.Logger, eventCh chan<- NRTEvent, nrtObjs []topologyv1alpha2.NodeResourceTopology) *Watcher {
	// need to pre-initialize knownConf state with the data we know at startup,
	// to avoid spurious/unnecessary NewlyAdded event which can confuse the cache or create unnecessary churn.
	initConf := make(map[string]nodeconfig.TopologyManager, len(nrtObjs))
	for idx := range nrtObjs {
		nrt := &nrtObjs[idx]
		initConf[nrt.Name] = nodeconfig.TopologyManagerFromNodeResourceTopology(lh, nrt)
	}
	return &Watcher{
		lh:       lh,
		eventCh:  eventCh,
		lastConf: initConf,
		done:     make(chan struct{}),
	}
}

// Wait is safe to be called on nil objects. It returns when the watch is stopped and the watch loop is ended
func (wt *Watcher) Wait() {
	if wt == nil || wt.done == nil {
		return
	}
	<-wt.done
}

// NodeResourceTopologies start watching for changes and must be run on its own goroutine
func (wt *Watcher) NodeResourceTopologies(ctx context.Context, client ctrlclient.WithWatch) {
	const (
		watchInitBackoff  = 700 * time.Millisecond
		streamInitBackoff = 200 * time.Millisecond

		watchMaxBackoff  = 30 * time.Second
		streamMaxBackoff = 5 * time.Second

		watchResetInterval  = 2 * time.Minute
		streamResetInterval = 1 * time.Minute

		factor = 2.0
		jitter = 1.0
	)

	watchDelay := wait.Backoff{
		Duration: watchInitBackoff,
		Cap:      watchMaxBackoff,
		Steps:    int(math.Ceil(float64(watchMaxBackoff) / float64(watchInitBackoff))),
		Factor:   factor,
		Jitter:   jitter,
	}.DelayWithReset(clock.RealClock{}, watchResetInterval)

	streamDelay := wait.Backoff{
		Duration: streamInitBackoff,
		Cap:      streamMaxBackoff,
		Steps:    int(math.Ceil(float64(streamMaxBackoff) / float64(streamInitBackoff))),
		Factor:   factor,
		Jitter:   jitter,
	}.DelayWithReset(clock.RealClock{}, streamResetInterval)

	for ctx.Err() == nil {
		wt.lh.Info("start watching NRT objects")
		nrtObjs := topologyv1alpha2.NodeResourceTopologyList{}
		wa, err := client.Watch(ctx, &nrtObjs)
		if err != nil {
			wt.lh.Error(err, "cannot watch NRT objects, retrying")
			select {
			case <-time.After(watchDelay()):
			case <-ctx.Done():
				wt.lh.Info("stop watching NRT objects")
			}
		} else {
			doneEvents := false
			for !doneEvents {
				select {
				case ev, ok := <-wa.ResultChan():
					if !ok {
						wt.lh.Info("watch channel closed, retrying watch")
						doneEvents = true
						select {
						case <-time.After(streamDelay()):
						case <-ctx.Done():
							wt.lh.Info("stop watching NRT objects")
						}
						continue
					}
					wt.processEvent(ev)

				case <-ctx.Done():
					wt.lh.Info("stop watching NRT objects")
					wa.Stop()
					doneEvents = true
				}
			}
		}
		wt.lh.Info("done watching NRT objects")
	}
	close(wt.done)
}

func (wt *Watcher) processEvent(ev watch.Event) {
	// Added
	//   was initially estimated to be rare, but in practice
	//   turned out to be a legit albeit not so common occurrence;
	//   it happens most frequently (mostly?) in CI environments
	// Modified
	//   is a common occurrence, happens relatively regularly.
	// Deleted
	//   is not handled. TODO: how common do we expect it to be?
	if ev.Type != watch.Added && ev.Type != watch.Modified {
		return
	}
	nrtObj, ok := ev.Object.(*topologyv1alpha2.NodeResourceTopology)
	if !ok {
		return
	}

	newConf := nodeconfig.TopologyManagerFromNodeResourceTopology(wt.lh, nrtObj)

	oldConf, known := wt.lastConf[nrtObj.Name]

	reason := WatchReasonNone
	if !known {
		reason = WatchReasonNewlyAdded
	} else if !oldConf.Equal(newConf) {
		reason = WatchReasonAttrChanged
	}

	if reason == WatchReasonNone {
		return // nothing to do
	}

	nrtEv := NRTEvent{
		Reason:   reason,
		NodeName: nrtObj.Name,
	}

	select {
	case wt.eventCh <- nrtEv:
		// Update lastConf only after a successful send; so, if the channel is
		// full, the next update will retry automatically another send.
		wt.lastConf[nrtObj.Name] = newConf
		wt.lh.V(2).Info("NRT async update", "reason", reason.String(), logging.KeyNode, nrtObj.Name)
	default:
		wt.lh.V(2).Info("NRT event channel full, will retry", logging.KeyNode, nrtObj.Name)
	}
}
