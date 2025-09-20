package mycrossnodepreemption

import (
	"context"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// snapshotBatch returns a snapshot of the current batch of pods.
func (pl *MyCrossNodePreemption) snapshotBatch() []*v1.Pod {
	keys := pl.Batched.Snapshot()
	if len(keys) == 0 { // no pods in batch
		return nil
	}
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	snapshot := make([]*v1.Pod, 0, len(keys))
	for _, k := range keys {
		if pod, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil {
			snapshot = append(snapshot, pod)
		}
	}
	return snapshot
}

// ---------- Cache Helpers ----------------

// waitForInformersSyncedAndNodes waits for all informers to sync and until a usable node is found.
func (pl *MyCrossNodePreemption) waitForInformersSyncedAndNodes(
	ctx context.Context, podsInf, nodesInf, cmsInf, rsInf, ssInf, dsInf, jobInf cache.SharedIndexInformer,
) {
	ok := cache.WaitForCacheSync(ctx.Done(),
		podsInf.HasSynced, nodesInf.HasSynced, cmsInf.HasSynced,
		rsInf.HasSynced, ssInf.HasSynced, dsInf.HasSynced, jobInf.HasSynced,
	)
	if !ok {
		klog.InfoS("Cache sync aborted (context done)")
		return
	}

	// Start a background watcher that waits for a usable node.
	// We immediately return so the caller can continue even if no usable nodes exist yet.
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				// Do NOT activate blocked pods here; only when a usable node is found.
				return

			case <-t.C:
				nodes, err := pl.getNodes()
				if err != nil {
					klog.V(MyVerbosity).InfoS("Cache warm-up watcher: getNodes error; retrying", "err", err)
					continue
				}
				usable := false
				for _, n := range nodes {
					if isNodeUsable(n) {
						usable = true
						break
					}
				}
				if !usable {
					klog.V(MyVerbosity).InfoS("Cache warm-up: waiting for a usable node")
					continue
				}

				// We have at least one usable node—mark warm and (optionally) unblock.
				pl.CachesWarm.Store(true)
				klog.InfoS("Caches ready and usable node(s) detected")

				// Avoid a surge in Every@PreEnqueue; the idle nudge will trickle them.
				if !(optimizeEvery() && optimizeAtPreEnqueue()) {
					_ = pl.activateBlockedPods(0) // activate all currently blocked
				}
				pl.startLoops(ctx)

				return
			}
		}
	}()

	// Return immediately; no usable nodes yet is fine — only unblock once watcher confirms usability.
}

// --------- Pod set Helpers ---------

// newPodSet creates a new PodSet.
func newPodSet() *PodSet { return &PodSet{m: make(map[types.UID]PodKey)} }

// AddPod adds a pod to the set.
func (s *PodSet) AddPod(p *v1.Pod) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.m[p.UID] = PodKey{UID: p.UID, Namespace: p.Namespace, Name: p.Name}
	s.mu.Unlock()
}

// RemovePod removes a pod from the set.
func (s *PodSet) RemovePod(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// Clear removes all pods from the set.
func (s *PodSet) Clear() {
	s.mu.Lock()
	s.m = make(map[types.UID]PodKey)
	s.mu.Unlock()
}

// Size returns the number of pods in the set.
func (s *PodSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

// Snapshot returns a snapshot of the current pods in the set.
func (s *PodSet) Snapshot() map[types.UID]PodKey {
	s.mu.RLock()
	out := make(map[types.UID]PodKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}

// pruneSet removes pods from the set that are no longer present (no longer exist or recreated/terminating).
func (pl *MyCrossNodePreemption) pruneSet(set *PodSet, keep func(cur *v1.Pod) bool) int {
	if set == nil || set.Size() == 0 {
		return 0
	}
	snap := set.Snapshot()
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	removed := 0
	for uid, key := range snap {
		cur, err := podLister.Pods(key.Namespace).Get(key.Name)
		switch {
		case apierrors.IsNotFound(err): // pod no longer exists; remove from set
			set.RemovePod(uid)
			removed++
		case err != nil:
			// keep if lister errored
		default: // pod has been recreated/terminating; remove from set
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil {
				set.RemovePod(uid)
				removed++
				continue
			}
			if keep != nil && !keep(cur) {
				set.RemovePod(uid)
				removed++
			}
		}
	}
	return removed
}

// activateBlockedPods activates up to 'max' pods from the blocked set; clear only the ones activated.
// It returns the UIDs of the pods that were *attempted* to be activated (in priority/time order).
func (pl *MyCrossNodePreemption) activateBlockedPods(max int) []types.UID {
	_ = pl.pruneSetEntries(pl.Blocked)
	if pl.Blocked == nil || pl.Blocked.Size() == 0 {
		return nil
	}

	// Snapshot and resolve current Pod objects
	blockedPods := pl.Blocked.Snapshot()

	type item struct {
		p   *v1.Pod
		key PodKey
	}
	items := make([]item, 0, len(blockedPods))
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	for _, k := range blockedPods {
		if p, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, item{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return nil
	}

	// Sort by priority, then creation timestamp (older first), then name
	sort.Slice(items, func(i, j int) bool {
		pi := getPodPriority(items[i].p)
		pj := getPodPriority(items[j].p)
		if pi != pj {
			return pi > pj
		}
		ti := items[i].p.GetCreationTimestamp().Time
		tj := items[j].p.GetCreationTimestamp().Time
		if ti.IsZero() || tj.IsZero() {
			return items[i].p.GetName() < items[j].p.GetName()
		}
		return ti.Before(tj)
	})

	// Limit the number of pods to activate
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}
	if limit == 0 {
		return nil
	}

	// Build activation map and record "tried" UIDs
	toAct := make(map[string]*v1.Pod, limit)
	tried := make([]types.UID, 0, limit)
	for _, it := range items[:limit] {
		toAct[it.p.Namespace+"/"+it.p.Name] = it.p
		tried = append(tried, it.key.UID)
	}

	// Activate and remove only those attempted
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(MyVerbosity).InfoS("Activated blocked pods", "count", len(toAct), "max", max)
		for _, it := range items[:limit] {
			pl.Blocked.RemovePod(it.key.UID)
		}
	}

	return tried
}

// Activate up to 'max' pods from the batched set; remove only those that were activated
// or explicitly provided via podsToRemove.
func (pl *MyCrossNodePreemption) activateBatchedPods(podsToRemove []*v1.Pod, max int) {
	_ = pl.pruneSetEntries(pl.Batched)
	if pl.Batched == nil || pl.Batched.Size() == 0 {
		return
	}
	snap := pl.Batched.Snapshot()
	podLister := pl.Handle.SharedInformerFactory().Core().V1().Pods().Lister()
	type item struct {
		p   *v1.Pod
		key PodKey
	}
	items := make([]item, 0, len(snap))
	for _, k := range snap {
		if p, err := podLister.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, item{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return
	}
	// Sort by priority and creation timestamp
	sort.Slice(items, func(i, j int) bool {
		pi := getPodPriority(items[i].p)
		pj := getPodPriority(items[j].p)
		if pi != pj {
			return pi > pj
		}
		ti := items[i].p.GetCreationTimestamp().Time
		tj := items[j].p.GetCreationTimestamp().Time
		if ti.IsZero() || tj.IsZero() {
			return items[i].p.GetName() < items[j].p.GetName()
		}
		return ti.Before(tj)
	})
	limit := len(items)
	if max > 0 && max < limit {
		limit = max
	}
	toAct := make(map[string]*v1.Pod, limit)
	for _, it := range items[:limit] {
		toAct[it.p.Namespace+"/"+it.p.Name] = it.p
	}
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(MyVerbosity).InfoS("Activated batched pods", "count", len(toAct), "max", max)
		// Remove only the ones we just activated
		for _, it := range items[:limit] {
			pl.Batched.RemovePod(it.key.UID)
		}
	}
	// If caller passes podsToRemove; remove them from the batched set
	for _, p := range podsToRemove {
		if p != nil {
			pl.Batched.RemovePod(p.UID)
		}
	}
}

// pruneSetEntries removes stale entries from the given pod set.
func (pl *MyCrossNodePreemption) pruneSetEntries(set *PodSet) int {
	removed := pl.pruneSet(set, func(cur *v1.Pod) bool {
		return cur.Spec.NodeName == "" // keep function: keep only pending pods
	})
	if removed > 0 {
		klog.V(MyVerbosity).InfoS("Pruned stale entries", "removed", removed)
	}
	return removed
}
