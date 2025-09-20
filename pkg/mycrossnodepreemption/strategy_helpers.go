package mycrossnodepreemption

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// OptimizationCadenceMode indicates how we optimize (for every pod, in batches, continuously).
type OptimizationCadenceMode int

const (
	OptimizeForEvery OptimizationCadenceMode = iota
	OptimizeInBatches
	OptimizeContinuously
)

// OptimizationAtMode indicates at which scheduling phase to optimize.
type OptimizationAtMode int

const (
	OptimizeAtPreEnqueue OptimizationAtMode = iota
	OptimizeAtPostFilter
)

// StrategyDecision indicates the decision made by the plugin.
type StrategyDecision int

const (
	DecidePassThrough StrategyDecision = iota
	DecideBatch
	DecideEvery
	DecideBlockActive
)

// Phase indicates which phase of scheduling we are in.
type Phase string

const (
	PhasePreEnqueue Phase = "PreEnqueue"
	PhasePostFilter Phase = "PostFilter"
	PhaseBatch      Phase = "BatchLoop"
	PhaseContinuous Phase = "ContinuousLoop"
)

// optimizeForEvery is the optimizer cadence that optimizes for every new pod.
func optimizeForEvery() bool { return OptimizeCadence == OptimizeForEvery }

// optimizeInBatches is the optimizer cadence that optimizes in batches.
func optimizeInBatches() bool { return OptimizeCadence == OptimizeInBatches }

// optimizeContinuously is the optimizer cadence that tries to optimize continuously if the cluster state hasn't changed during solver optimization.
func optimizeContinuously() bool { return OptimizeCadence == OptimizeContinuously }

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

// atPreEnqueue returns true if the phase is PreEnqueue.
func (phase Phase) atPreEnqueue() bool { return phase == PhasePreEnqueue }

// atPostFilter returns true if the phase is PostFilter.
func (phase Phase) atPostFilter() bool { return phase == PhasePostFilter }

// atBatch returns true if the phase is BatchLoop.
func (phase Phase) atBatch() bool { return phase == PhaseBatch }

// atContinuous returns true if the phase is ContinuousLoop.
func (phase Phase) atContinuous() bool { return phase == PhaseContinuous }

// decideStrategy determines the optimization strategy based on the current phase.
// Continuously mode, never blocks or batches pods.
// Other modes, always block new pods, while actively optimizing.
// If not actively optimizing:
// OptimizeInBatches@PreEnqueue and OptimizeInBatches@PostFilter: batch new pods at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
// OptimizeForEvery@PreEnqueue and OptimizeForEvery@PostFilter: optimize for every new pod at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
func (pl *MyCrossNodePreemption) decideStrategy(phase Phase) StrategyDecision {
	// Mode: Continuously; never blocks or batches due to the optimizer.
	if optimizeContinuously() {
		return DecidePassThrough
	}
	// If not in continuous mode and there's an active plan, block all new pods.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlockActive
	}
	// Modes: OptimizeInBatches@PreEnqueue or OptimizeInBatches@PostFilter
	if optimizeInBatches() {
		if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
			return DecideBatch // batch new pods
		}
		return DecidePassThrough // if not in the phase of optimization, allow all pods
	}
	// Modes: OptimizeForEvery@PreEnqueue or OptimizeForEvery@PostFilter
	if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
		return DecideEvery // optimize for every new pod
	}
	return DecidePassThrough // if not at the phase of optimization, allow all pods
}

// strategyToString returns a string representation of the optimization mode.
func strategyToString() string {
	a := "ForEvery"
	switch OptimizeCadence {
	case OptimizeInBatches:
		a = "InBatches"
	case OptimizeContinuously:
		a = "Continuously"
		return a
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// parseCadence parses a cadence string and returns the corresponding OptimizationCadenceMode.
func parseCadence(s string) OptimizationCadenceMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "for_every":
		return OptimizeForEvery
	case "in_batches":
		return OptimizeInBatches
	case "continuously":
		return OptimizeContinuously
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_CADENCE value; defaulting to in_batches", "value", s)
		return OptimizeInBatches
	}
}

// parseOptimizeAt parses an optimization "at" string and returns the corresponding OptimizationAtMode.
func parseOptimizeAt(s string) OptimizationAtMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "preenqueue":
		return OptimizeAtPreEnqueue
	case "postfilter":
		return OptimizeAtPostFilter
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_AT value; defaulting to postfilter", "value", s)
		return OptimizeAtPostFilter
	}
}
