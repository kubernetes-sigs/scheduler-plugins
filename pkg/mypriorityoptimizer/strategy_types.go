// strategy_types.go

package mypriorityoptimizer

// Mode indicates how we optimize
type OptimizeModeType int

const (
	// ModeEvery indicates we optimize for every new pod.
	ModeEvery OptimizeModeType = iota
	// ModeAllSynch indicates we optimize all pods and blocks until done.
	ModeAllSynch
	// ModeAllAsynch indicates we optimize all pods but do not block (only while applying the plan).
	// OptimizeAt is ignored in this mode.
	ModeAllAsynch
	// ModeManualAllSynch is the same as ModeAllSynch but only triggers manual optimization via HTTP.
	ModeManualAllSynch
	// ModeFreeTimeSynch indicates we optimize during free time when no new pods are arriving, synchronously.
	ModeFreeTimeSynch
	// ModeFreeTimeAsynch indicates we optimize during free time when no new pods are arriving, asynchronously.
	ModeFreeTimeAsynch
)

// Stage indicates which stage of scheduling we are in.
type StageType int

const (
	// StageNone indicates we are not in a stage we care about.
	StageNone StageType = iota // for periodic optimization
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
