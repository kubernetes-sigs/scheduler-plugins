// shared_types.go
package mypriorityoptimizer

import (
	"k8s.io/apimachinery/pkg/types"
)

// Node represents a node in the cluster for the solver.
type Node struct {
	// Name of the node
	Name string `json:"name"`
	// Total CPU capacity in millicores
	CapCPUm int64 `json:"cap_cpu_m"`
	// Total memory capacity in bytes
	CapMemBytes int64 `json:"cap_mem_bytes"`
	// Allocated (used) CPU in millicores (not serialized)
	AllocCPUm int64 `json:"-"`
	// Allocated (used) memory in bytes (not serialized)
	AllocMemBytes int64 `json:"-"`
	// Labels of the node
	Labels map[string]string `json:"labels,omitempty"`
	// Pods currently on the node (not serialized)
	Pods map[types.UID]*Pod `json:"-"`
}

// Pod represents a pod in the cluster.
type Pod struct {
	// Unique identifier for the pod
	UID types.UID `json:"uid,omitempty"`
	// Namespace of the pod
	Namespace string `json:"namespace,omitempty"`
	// Name of the pod
	Name string `json:"name,omitempty"`
	// Requested CPU in millicores
	ReqCPUm int64 `json:"req_cpu_m,omitempty"`
	// Requested memory in bytes
	ReqMemBytes int64 `json:"req_mem_bytes,omitempty"`
	// Priority of the pod
	Priority int32 `json:"priority,omitempty"`
	// Whether the pod is protected from preemption
	Protected bool `json:"protected,omitempty"`
	// Current node of the pod (empty if new pod)
	Node string `json:"node,omitempty"`
	// Old node of the pod (empty if new pod)
	OldNode string `json:"old_node,omitempty"`
}