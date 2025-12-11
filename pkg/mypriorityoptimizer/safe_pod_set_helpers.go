// pod_set_helpers.go
package mypriorityoptimizer

import (
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// newSafePodSet creates a new PodSet.
func newSafePodSet(name string) *SafePodSet {
	return &SafePodSet{Name: name, m: make(map[types.UID]PlannerPod)}
}

// doesSafePodSetExist returns true if the pod set is non-nil and has at least one pod.
func doesSafePodSetExist(podSet *SafePodSet) bool {
	if podSet == nil {
		return false
	}
	return podSet.Size() > 0
}

// prunePending removes from `set` any pod that is no longer pending.
// It returns the number of removed pods.
func (pl *SharedState) pruneSafePodSet(podSet *SafePodSet) int {
	if !doesSafePodSetExist(podSet) {
		return 0
	}
	snap := podSet.SnapshotSafely()
	removed := 0
	for uid, key := range snap {
		cur, err := pl.getPodByName(key.Namespace, key.Name)
		switch {
		case apierrors.IsNotFound(err):
			podSet.RemovePodSafely(uid)
			removed++
		case err != nil:
			// conservatively keep on lister error
		default:
			// drop if recreated, terminating, or not pending anymore
			if !isSamePodUID(cur.UID, uid) || isPodDeleted(cur) || getPodAssignedNodeName(cur) != "" {
				podSet.RemovePodSafely(uid)
				removed++
			}
		}
	}
	if removed > 0 {
		klog.V(MyV).InfoS("pruned stale entries", "set", podSet.Name, "removed", removed)
	}
	return removed
}

// AddPodSafely adds a pod to the set.
// Use mutex to protect the map such that only one goroutine can modify the map at a time.
func (s *SafePodSet) AddPodSafely(p *v1.Pod) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.m[p.UID] = PlannerPod{UID: p.UID, Namespace: p.Namespace, Name: p.Name}
	s.mu.Unlock()
}

// RemovePodSafely removes a pod from the set.
// Use mutex to protect the map such that only one goroutine can modify the map at a time.
func (s *SafePodSet) RemovePodSafely(uid types.UID) {
	s.mu.Lock()
	delete(s.m, uid)
	s.mu.Unlock()
}

// Size returns the number of pods in the set.
// Use mutex so that we can read the map safely.
func (s *SafePodSet) Size() int {
	s.mu.RLock()
	n := len(s.m)
	s.mu.RUnlock()
	return n
}

// SnapshotSafely returns a snapshot of the current pods in the set.
// Use mutex so that we can read the map safely.
func (s *SafePodSet) SnapshotSafely() map[types.UID]PlannerPod {
	s.mu.RLock()
	out := make(map[types.UID]PlannerPod, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	s.mu.RUnlock()
	return out
}
