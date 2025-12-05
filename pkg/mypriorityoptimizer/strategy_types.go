// strategy_types.go

package mypriorityoptimizer

// OptimizeModeType indicates *when* we optimize.
type OptimizeModeType int

const (
	// ModeEvery optimizes for every new pod.
	ModeEvery OptimizeModeType = iota

	// ModePeriodic runs periodic global optimization over the accumulated pending set.
	ModePeriodic

	// ModeManual collects like ModePeriodic but only optimizes when the HTTP
	// /solve endpoint is called.
	ModeManual

	// ModeInterlude runs global optimization only during "quiet" periods where the
	// pending set has been stable for some time.
	ModeInterlude
)

// Stage indicates which stage of scheduling we are in.
type StageType int

const (
	// StageNone indicates we are not in a stage we care about.
	StageNone StageType = iota // for periodic/interlude optimization
	// StagePreEnqueue indicates we are in the PreEnqueue stage.
	StagePreEnqueue
	// StagePostFilter indicates we are in the PostFilter stage.
	StagePostFilter
)

// StrategyDecision indicates the decision made by the plugin.
type StrategyDecision int

const (
	// Let the pod pass through without optimization.
	DecidePass StrategyDecision = iota
	// Block the pod until optimization is done.
	DecideBlock
	// Process this new single pod.
	DecideProcess
	// Set the pod as pending for later optimization.
	DecideProcessLater
)
