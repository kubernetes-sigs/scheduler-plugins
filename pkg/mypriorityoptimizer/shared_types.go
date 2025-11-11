// shared_types.go

package mypriorityoptimizer

import (
	"k8s.io/apimachinery/pkg/types"
)

// Pod represents a pod with minimal info.
type Pod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid,omitempty"`
	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`
	// Name of the pod
	Name string `json:"name,omitempty"`
}

// NewPlacement represents a pod placement from one node to another.
type NewPlacement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Previous node of the pod; empty if new pod
	FromNode string `json:"from_node,omitempty"`
	// New node of the pod
	ToNode string `json:"to_node"`
}

// Placement represents a pod placement on a node.
type Placement struct {
	// Pod being placed
	Pod Pod `json:"pod"`
	// Node the pod is placed on
	Node string `json:"node"`
}
