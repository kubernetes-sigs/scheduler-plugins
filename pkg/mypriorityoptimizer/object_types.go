package mypriorityoptimizer

// WorkloadKind represents the kind of workload.
type WorkloadKind int

const (
	wkReplicaSet WorkloadKind = iota
	wkStatefulSet
	wkDaemonSet
	wkJob
)

// WorkloadKey is a key to identify a workload.
type WorkloadKey struct {
	// What kind of workload
	Kind WorkloadKind
	// Namespace of the workload
	Namespace string
	// Name of the workload
	Name string
}

// WorkloadStatus represents the status of a workload.
//   - hasLive:    at least one live pod (not terminating) for this workload
//   - hasPending: at least one live *pending* pod for this workload
type wkStatus struct {
	HasLive    bool
	HasPending bool
}
