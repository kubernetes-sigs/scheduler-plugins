// pod_set_types.go
package mypriorityoptimizer

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PodSet is a thread-safe set of pods.
type PodSet struct {
	// Name of the set (for logging)
	Name string
	// mu protects the map
	mu sync.RWMutex
	// m maps pod UID to PodKey
	m map[types.UID]PodKey
}

// PodKey is a minimal key for identifying a pod.
type PodKey struct {
	// Unique identifier for the pod
	UID types.UID // for fast lookup, we only use types.UID as key (not as a string)
	// Namespace of the pod
	Namespace string
	// Name of the pod
	Name string
}

// PodSetItem represents an item in the PodSet.
type PodSetItem struct {
	// Pod pointer (if needed)
	p *v1.Pod
	// Key for identifying the pod
	key PodKey
}
