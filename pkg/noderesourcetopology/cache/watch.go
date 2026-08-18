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
	lh      logr.Logger
	eventCh chan<- NRTEvent
	// lastConf tracks the last TopologyManager config we successfully *delivered*
	// per node, used to diff incoming events and minimize the updates we forward.
	// It is advanced only on a successful send (see trySend/flushPending), so a
	// change whose send overflowed is not reflected here until it is delivered.
	// Delivery of overflowed changes is guaranteed by pending draining.
	lastConf map[string]nodeconfig.TopologyManager
	// pending holds changes which could not be delivered because the channel
	// was full. These are retried periodically, so a dropped change is
	// eventually delivered without depending on a further external change.
	// It is keyed by node name, so there is at most one pending item per node.
	// Events are level-triggered (they only tell the resync loop to re-read the
	// node, which fetches the current state), so we can safely overwrite a
	// parked entry with a later change.
	// We can wonder why we need a two-layer structure, bounded chan + parked map
	// vs an unbounded channel. First, we expect the channel to overflow rarely.
	// Second, with this approach parked events are naturally squashed, so the
	// total memory consumption is always bounded, differently
	// from an unbounded channel.
	pending map[string]pendingNRTEvent
	done    chan struct{}
}

// pendingNRTEvent is a change parked for later delivery when the update channel
// was full. It embeds the ready-to-send NRTEvent and carries the config to advance
// the last delivered configuration.
type pendingNRTEvent struct {
	NRTEvent
	conf nodeconfig.TopologyManager
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
		pending:  make(map[string]pendingNRTEvent),
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

		pendingRetryInterval = 2 * time.Second // determined heuristically, no hard data yet.

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

	retryTicker := clock.RealClock{}.NewTicker(pendingRetryInterval)
	defer retryTicker.Stop()

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
			retryCh := retryTicker.C()
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

				case <-retryCh:
					wt.flushPending()

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
	wt.trySend(nrtEv, newConf)
}

func (wt *Watcher) TestOnlyNodeStatus(nodeName string) (nodeconfig.TopologyManager, bool, bool) {
	conf, hasConf := wt.lastConf[nodeName]
	_, hasPending := wt.pending[nodeName]
	return conf, hasConf, hasPending
}

// WatcherStatus describes the lifecycle state of a Watcher's watch loop.
type WatcherStatus string

const (
	// WatcherStatusDisabled means no watch loop was ever started for this
	// Watcher (e.g. resync scope doesn't require watching NRT objects).
	WatcherStatusDisabled WatcherStatus = "disabled"
	// WatcherStatusRunning means the watch loop is still running.
	WatcherStatusRunning WatcherStatus = "running"
	// WatcherStatusStopped means the watch loop has exited.
	WatcherStatusStopped WatcherStatus = "stopped"
)

// TestOnlyWatcherStatus is safe to call on nil objects. It reports whether
// the watch loop was ever started, is still running, or has exited.
// to be used only in tests.
func (wt *Watcher) TestOnlyWatcherStatus() WatcherStatus {
	if wt == nil || wt.done == nil {
		return WatcherStatusDisabled
	}
	select {
	case <-wt.done:
		return WatcherStatusStopped
	default:
		return WatcherStatusRunning
	}
}

// trySend attempts a non-blocking send of ev. On success lastConf is advanced
// to conf and any parked retry for the node is cleared; on failure the change is
// parked in wt.pending so flushPending can retry it later without needing a new
// watch event.
func (wt *Watcher) trySend(ev NRTEvent, conf nodeconfig.TopologyManager) {
	select {
	case wt.eventCh <- ev:
		// Advance lastConf only after a successful send, so an overflowed change
		// stays visible as a diff until pending delivers it.
		wt.lastConf[ev.NodeName] = conf
		delete(wt.pending, ev.NodeName) // if any
		wt.lh.V(2).Info("NRT async update", "reason", ev.Reason.String(), logging.KeyNode, ev.NodeName)
	default:
		// update is level-triggered, so overwriting with a later event is safe
		wt.pending[ev.NodeName] = pendingNRTEvent{NRTEvent: ev, conf: conf}
		wt.lh.V(2).Info("NRT event channel full, parked for retry", logging.KeyNode, ev.NodeName, "pending", len(wt.pending))
	}
}

// flushPending retries delivering parked changes. It runs on the watch
// goroutine, so it shares no state with the resync loop beyond eventCh. It
// stops at the first send that would block, leaving the rest for the next tick.
func (wt *Watcher) flushPending() {
	for name, pe := range wt.pending {
		select {
		case wt.eventCh <- pe.NRTEvent:
			wt.lastConf[name] = pe.conf
			delete(wt.pending, name)
			wt.lh.V(2).Info("NRT async update retried", "reason", pe.Reason.String(), logging.KeyNode, name)
		default:
			return
		}
	}
}
