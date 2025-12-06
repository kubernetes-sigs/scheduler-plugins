// pod_set_helpers.go
package mypriorityoptimizer

import (
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

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
	podsLister := podsListerFor(pl)

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

// doesPodSetExist returns true if the pod set is non-nil and has at least one pod.
func doesPodSetExist(podSet *PodSet) bool {
	if podSet == nil {
		return false
	}
	return podSet.Size() > 0
}

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
