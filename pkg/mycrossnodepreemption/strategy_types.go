package mycrossnodepreemption

// OptimizationMode indicates how we optimize (for every pod, batch, continuous).
type OptimizationMode int

const (
	// OptimizeEvery indicates we optimize for every new pod.
	OptimizeEvery OptimizationMode = iota
	// OptimizeAllSynch indicates we optimize in batches.
	OptimizeAllSynch
	// OptimizeAllAsynch indicates we optimize continuously.
	// OptimizeAt is ignored in this mode.
	OptimizeAllAsynch
)

// OptimizationAt indicates at which scheduling phase to optimize.
type OptimizationAt int

const (
	// OptimizeAtPreEnqueue indicates we optimize at the PreEnqueue phase.
	OptimizeAtPreEnqueue OptimizationAt = iota
	// OptimizeAtPostFilter indicates we optimize at the PostFilter phase.
	OptimizeAtPostFilter
)

// Phase indicates which phase of scheduling we are in.
type Phase int

const (
	// PhaseNone indicates we are not in a phase we care about.
	PhaseNone Phase = iota // for periodic optimization
	// PhasePreEnqueue indicates we are in the PreEnqueue phase.
	PhasePreEnqueue
	// PhasePostFilter indicates we are in the PostFilter phase.
	PhasePostFilter
)

// StrategyDecision indicates the decision made by the plugin.
type StrategyDecision int

const (
	// DecidePassThrough indicates we let the pod pass through without optimization.
	DecidePassThrough StrategyDecision = iota
	// DecideBatch indicates we batch the pod for later optimization.
	DecideBatch
	// DecideEvery indicates we should optimize this new single pod.
	DecideEvery
	// DecideBlock indicates we block the pod due to an active plan.
	DecideBlock
)
