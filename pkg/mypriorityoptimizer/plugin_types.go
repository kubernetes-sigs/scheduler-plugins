// plugin_types.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"

	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// SharedState is the shared state of the MyPriorityOptimizer plugin used across scheduling cycles and workers.
type SharedState struct {
	// Handle to the framework
	Handle framework.Handle
	// Kubernetes client
	Client kubernetes.Interface
	// Whether a plan is active
	Active atomic.Bool
	// Currently active plan (if any)
	ActivePlan atomic.Pointer[ActivePlan]
	// Set of blocked pods
	BlockedWhileActive *SafePodSet
	// Whether the plugin is ready (caches warmed up and usable node found)
	PluginReady atomic.Bool
}

// ActivePlan represents the state of an active plan.
type ActivePlan struct {
	// Unique plan ID
	ID string
	// Workload quotas for moved or new pods that are part of a workload
	WorkloadQuotas WorkloadQuotasAtomics
	// Placements by name for standalone pods that is either moved or new: ns/name -> node
	PlacementByName map[string]string // pod ns/name -> targetNode
	// Context to cancel the plan
	Ctx context.Context
	// Cancel function to cancel the plan
	Cancel context.CancelFunc
}

// WorkloadQuotasAtomics is a map of workloadKey -> node -> remaining count
// The atomic.Int32 allows concurrent safe decrement during plan execution.
type WorkloadQuotasAtomics map[string]map[string]*atomic.Int32
