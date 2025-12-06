// pod_set_helpers.go
package mypriorityoptimizer

import (
	"sort"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Test hooks / indirections for PodSet helpers
// -----------------------------------------------------------------------------

// podsListerForPodSets returns the PodLister used by activatePods/pruneSet.
// In production it delegates to pl.podsLister(), but tests can override it.
var podsListerForPodSets = func(pl *SharedState) corev1listers.PodLister {
	return pl.podsLister()
}

// activatePodsCall performs the actual framework.Handle.Activate call.
// In tests we override this to capture which pods would be activated.
var activatePodsCall = func(pl *SharedState, toAct map[string]*v1.Pod) {
	pl.Handle.Activate(klog.Background(), toAct)
}

// -----------------------------------------------------------------------------
// PodSet helpers
// -----------------------------------------------------------------------------

// activateBlockedPods activates up to 'max' pods from the blocked set; clear only the ones activated.
// It returns the UIDs of the pods that were attempted to be activated (in priority/time order).
// if max <= 0, all pods are activated.
func (pl *SharedState) activatePods(podSet *PodSet, removeActivated bool, max int) (tried []types.UID) {
	// Prune stale entries first
	_ = pl.pruneSet(podSet)

	// If no blocked pods, nothing to do
	if podSet == nil || podSet.Size() == 0 {
		return
	}

	// Snapshot and resolve current Pod objects
	blockedPods := podSet.Snapshot()
	items := make([]PodSetItem, 0, len(blockedPods))
	// Get current Pod objects so that we don't return stale/deleted ones.
	podsLister := podsListerForPodSets(pl)
	for _, k := range blockedPods {
		if p, err := podsLister.Pods(k.Namespace).Get(k.Name); err == nil && p != nil {
			items = append(items, PodSetItem{p: p, key: k})
		}
	}
	if len(items) == 0 {
		return
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

	// Build activation map and record "tried" UIDs
	toAct := make(map[string]*v1.Pod, limit)
	for _, it := range items[:limit] {
		toAct[combineNsName(it.p.Namespace, it.p.Name)] = it.p
		tried = append(tried, it.key.UID)
	}

	if len(toAct) > 0 {
		activatePodsCall(pl, toAct)
		klog.InfoS("activated pods", "set", podSet.Name, "count", len(toAct))
		if removeActivated {
			// Remove only the ones we just activated
			for _, it := range items[:limit] {
				podSet.RemovePod(it.key.UID)
			}
		}
	}
	return tried
}

// prunePending removes from `set` any pod that:
//   - no longer exists,
//   - has been recreated (UID changed),
//   - is terminating (DeletionTimestamp != nil), or
//   - is already bound (Spec.NodeName != "").
//
// It returns the number of entries removed.
func (pl *SharedState) pruneSet(podSet *PodSet) int {
	if podSet == nil || podSet.Size() == 0 {
		return 0
	}
	snap := podSet.Snapshot()
	podsLister := podsListerForPodSets(pl)

	removed := 0
	for uid, key := range snap {
		cur, err := podsLister.Pods(key.Namespace).Get(key.Name)
		switch {
		case apierrors.IsNotFound(err):
			podSet.RemovePod(uid)
			removed++
		case err != nil:
			// conservatively keep on lister error
		default:
			// drop if recreated, terminating, or not pending anymore
			if string(cur.UID) != string(uid) || cur.DeletionTimestamp != nil || cur.Spec.NodeName != "" {
				podSet.RemovePod(uid)
				removed++
			}
		}
	}
	if removed > 0 {
		klog.V(MyV).InfoS("pruned stale entries", "set", podSet.Name, "removed", removed)
	}
	return removed
}

// newPodSet creates a new PodSet.
func newPodSet(name string) *PodSet { return &PodSet{Name: name, m: make(map[types.UID]PodKey)} }

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
