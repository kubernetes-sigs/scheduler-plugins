// mode_types.go
package mypriorityoptimizer

// ModeType indicates *when* we optimize.
type ModeType int

const (
	// ModePerPod optimizes for every new pod.
	ModePerPod ModeType = iota
	// ModePeriodic runs periodic optimization over the accumulated pending set.
	ModePeriodic
	// ModeInterlude runs optimization only during "quiet" periods where the
	// pending set has been stable for some time.
	ModeInterlude
	// ModeManual collects like ModePeriodic but only optimizes when the HTTP
	// /solve endpoint is called.
	ModeManual
	// ModeManualBlocking blocks the normal scheduling flow until /solve is called.
	ModeManualBlocking
)
