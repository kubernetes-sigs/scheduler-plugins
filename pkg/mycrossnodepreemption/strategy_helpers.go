package mycrossnodepreemption

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// OptimizationCadence indicates how we optimize (for every pod, batch, continuous).
type OptimizationCadence int

const (
	OptimizeEvery OptimizationCadence = iota
	OptimizeBatch
	OptimizeContinuous
)

// OptimizationAt indicates at which scheduling phase to optimize.
type OptimizationAt int

const (
	OptimizeAtPreEnqueue OptimizationAt = iota
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
type Phase int

const (
	PhaseNone Phase = iota // for periodic optimization
	PhasePreEnqueue
	PhasePostFilter
)

// optimizeEvery is the optimizer cadence that optimizes for every new pod.
func optimizeEvery() bool { return OptimizeCadence == OptimizeEvery }

// optimizeBatch is the optimizer cadence that optimizes in batches.
func optimizeBatch() bool { return OptimizeCadence == OptimizeBatch }

// optimizeContinuous is the optimizer cadence that tries to optimize continuous if the cluster state hasn't changed during solver optimization.
func optimizeContinuous() bool { return OptimizeCadence == OptimizeContinuous }

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

// decideStrategy determines the optimization strategy based on the current phase.
// Continuous mode, never blocks or batches pods.
// Other modes, always block new pods, while actively optimizing.
// If not actively optimizing:
// OptimizeBatch@PreEnqueue and OptimizeBatch@PostFilter: batch new pods at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
// OptimizeEvery@PreEnqueue and OptimizeEvery@PostFilter: optimize for every new pod at phases PreEnqueue and PostFilter, respectively, and at other phases we let pods through.
func (pl *MyCrossNodePreemption) decideStrategy(phase Phase) StrategyDecision {
	// Mode: Continuous; never blocks or batches due to the optimizer.
	if optimizeContinuous() {
		return DecidePassThrough
	}
	// If not in continuous mode and there's an active plan, block all new pods.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlockActive
	}
	// Modes: OptimizeBatch@PreEnqueue or OptimizeBatch@PostFilter
	if optimizeBatch() {
		if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
			return DecideBatch // batch new pods
		}
		return DecidePassThrough // if not in the phase of optimization, allow all pods
	}
	// Modes: OptimizeEvery@PreEnqueue or OptimizeEvery@PostFilter
	if (optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter()) {
		return DecideEvery // optimize for every new pod
	}
	return DecidePassThrough // if not at the phase of optimization, allow all pods
}

// atPreEnqueue returns true if the phase is PreEnqueue.
func (phase Phase) atPreEnqueue() bool { return phase == PhasePreEnqueue }

// atPostFilter returns true if the phase is PostFilter.
func (phase Phase) atPostFilter() bool { return phase == PhasePostFilter }

// strategyToString returns a string representation of the optimization mode.
func strategyToString() string {
	a := "Every"
	switch OptimizeCadence {
	case OptimizeBatch:
		a = "Batch"
	case OptimizeContinuous:
		a = "Continuous"
		return a
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// parseCadence parses a cadence string and returns the corresponding OptimizationCadenceMode.
func parseCadence(s string) OptimizationCadence {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "every":
		return OptimizeEvery
	case "batch":
		return OptimizeBatch
	case "continuous":
		return OptimizeContinuous
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_CADENCE value; defaulting to 'batch'", "value", s)
		return OptimizeBatch
	}
}

// parseOptimizeAt parses an optimization "at" string and returns the corresponding OptimizationAtMode.
func parseOptimizeAt(s string) OptimizationAt {
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
