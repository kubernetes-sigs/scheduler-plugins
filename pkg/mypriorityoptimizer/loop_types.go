// loop_types.go
package mypriorityoptimizer

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type OptimizeLoopConfig struct {
	Label          string        // log label
	Interval       time.Duration // base tick interval
	InterludeDelay time.Duration // 0 => no "idle window"; >0 => require this long of stability
	CancelOnChange bool          // cancel in-flight run if pending set changes
}

// PendingSnapshot bundles the pieces of state that both the periodic and
// free-time loops need in order to decide whether to run the solver.
type PendingSnapshot struct {
	PendingUIDs  map[types.UID]struct{}
	PendingCount int
	Fingerprint  string     // clusterFingerprint(nodes, pods)
	Pods         []*v1.Pod  // live snapshot (for priority checks)
	Nodes        []*v1.Node // live snapshot (for solver input)
}
