// pod_set_helpers.go

package mycrossnodepreemption

import (
	"sort"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// activateBlockedPods activates up to 'max' pods from the blocked set; clear only the ones activated.
// It returns the UIDs of the pods that were attempted to be activated (in priority/time order).
func (pl *MyCrossNodePreemption) activateBlockedPods(max int) []types.UID {
	// Prune stale entries first
	_ = pl.pruneSet(pl.Blocked, "Blocked")

	// If no blocked pods, nothing to do
	if pl.Blocked == nil || pl.Blocked.Size() == 0 {
		return nil
	}

	// Snapshot and resolve current Pod objects
	blockedPods := pl.Blocked.Snapshot()
	items := make([]PodSetItem, 0, len(blockedPods))
	// Get current Pod objects so that we don't return stale/deleted ones.
	podsLister := pl.podsLister()
	for _, k := range blockedPods {
		if p, err := podsLister.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, PodSetItem{p: p, key: k})
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
		toAct[combineNsName(it.p.Namespace, it.p.Name)] = it.p
		tried = append(tried, it.key.UID)
	}

	// Activate and remove only those attempted
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(MyV).InfoS("activated blocked pods", "count", len(toAct), "max", max)
		for _, it := range items[:limit] {
			pl.Blocked.RemovePod(it.key.UID)
		}
	}

	return tried
}

// Activate up to 'max' pods from the batched set; remove only those that were activated
// or explicitly provided via podsToRemove.
func (pl *MyCrossNodePreemption) activateBatchedPods(podsToRemove []*v1.Pod, max int) {
	_ = pl.pruneSet(pl.Batched, "Batched")
	if pl.Batched == nil || pl.Batched.Size() == 0 {
		return
	}
	snap := pl.Batched.Snapshot()
	podsLister := pl.podsLister()
	items := make([]PodSetItem, 0, len(snap))
	for _, k := range snap {
		if p, err := podsLister.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, PodSetItem{p: p, key: k})
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
		toAct[combineNsName(it.p.Namespace, it.p.Name)] = it.p
	}
	if len(toAct) > 0 {
		pl.Handle.Activate(klog.Background(), toAct)
		klog.V(MyV).InfoS("activated batched pods", "count", len(toAct), "max", max)
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

// prunePending removes from `set` any pod that:
//   - no longer exists,
//   - has been recreated (UID changed),
//   - is terminating (DeletionTimestamp != nil), or
//   - is already bound (Spec.NodeName != "").
//
// It returns the number of entries removed.
func (pl *MyCrossNodePreemption) pruneSet(set *PodSet, setName string) int {
	if set == nil || set.Size() == 0 {
		return 0
	}
	snap := set.Snapshot()
	podsLister := pl.podsLister()

	removed := 0
	for uid, key := range snap {
		cur, err := podsLister.Pods(key.Namespace).Get(key.Name)
		switch {
		case apierrors.IsNotFound(err):
			set.RemovePod(uid)
			removed++
		case err != nil:
			// conservatively keep on lister error
		default:
			// drop if recreated, terminating, or not pending anymore
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil || cur.Spec.NodeName != "" {
				set.RemovePod(uid)
				removed++
			}
		}
	}
	if removed > 0 {
		klog.V(MyV).InfoS("pruned stale entries", "set", setName, "removed", removed)
	}
	return removed
}

// snapshotBatch returns a snapshot of the current batch of pods.
func (pl *MyCrossNodePreemption) snapshotBatch() []*v1.Pod {
	keys := pl.Batched.Snapshot()
	if len(keys) == 0 { // no pods in batch
		return nil
	}
	// Get current Pod objects so that we don't return stale/deleted ones.
	podsLister := pl.podsLister()
	snapshot := make([]*v1.Pod, 0, len(keys))
	for _, k := range keys {
		if pod, err := podsLister.Pods(k.Namespace).Get(k.Name); err == nil {
			snapshot = append(snapshot, pod)
		}
	}
	return snapshot
}

// newPodSet creates a new PodSet.
func newPodSet() *PodSet { return &PodSet{m: make(map[types.UID]PodKey)} }

// AddPod adds a pod to the set.
// Use mutex to protect the map such that only one goroutine can modify the map at a time.
func (s *PodSet) AddPod(p *v1.Pod) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.m[p.UID] = PodKey{UID: p.UID, Namespace: p.Namespace, Name: p.Name}
	s.mu.Unlock()
}

// RemovePod removes a pod from the set.
// Use mutex to protect the map such that only one goroutine can modify the map at a time.
func (s *PodSet) RemovePod(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// Clear removes all pods from the set.
// Use mutex to protect the map such that only one goroutine can modify the map at a time.
func (s *PodSet) Clear() {
	s.mu.Lock()
	s.m = make(map[types.UID]PodKey)
	s.mu.Unlock()
}

// Size returns the number of pods in the set.
// Use mutex so that we can read the map safely.
func (s *PodSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

// Snapshot returns a snapshot of the current pods in the set.
// Use mutex so that we can read the map safely.
func (s *PodSet) Snapshot() map[types.UID]PodKey {
	s.mu.RLock()
	out := make(map[types.UID]PodKey, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}
