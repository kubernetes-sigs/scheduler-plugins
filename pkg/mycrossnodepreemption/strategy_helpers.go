// strategy_helpers.go

package mycrossnodepreemption

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// optimizeEvery is the optimizer cadence that optimizes for every new pod.
func optimizeEvery() bool { return OptimizeMode == OptimizeEvery }

// optimizeAllSynch is the optimizer cadence that optimizes all pods and blocks while optimizing.
func optimizeAllSynch() bool { return OptimizeMode == OptimizeAllSynch }

// optimizeAllAsynch is the optimizer cadence that tries to optimize continuous if the cluster state hasn't drift too much during solver optimization.
func optimizeAllAsynch() bool { return OptimizeMode == OptimizeAllAsynch }

// optimizeAtPreEnqueue is the action point that triggers optimization at the PreEnqueue stage.
func optimizeAtPreEnqueue() bool { return OptimizeAt == OptimizeAtPreEnqueue }

// optimizeAtPostFilter is the action point that triggers optimization at the PostFilter stage.
func optimizeAtPostFilter() bool { return OptimizeAt == OptimizeAtPostFilter }

// atPreEnqueue returns true if the phase is PreEnqueue.
func (phase Phase) atPreEnqueue() bool { return phase == PhasePreEnqueue }

// atPostFilter returns true if the phase is PostFilter.
func (phase Phase) atPostFilter() bool { return phase == PhasePostFilter }

// strategyToString returns a string representation of the optimization mode.
func strategyToString() string {
	a := "Every"
	switch OptimizeMode {
	case OptimizeAllSynch:
		a = "AllSynch"
	case OptimizeAllAsynch:
		a = "AllAsynch"
		return a
	}
	b := "PreEnqueue"
	if optimizeAtPostFilter() {
		b = "PostFilter"
	}
	return fmt.Sprintf("%s/%s", a, b)
}

// parseOptimizeMode parses a cadence string and returns the corresponding OptimizationCadenceMode.
func parseOptimizeMode(s string) OptimizationMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "every":
		return OptimizeEvery
	case "all_synch":
		return OptimizeAllSynch
	case "all_asynch":
		return OptimizeAllAsynch
	default:
		klog.InfoS("Unknown ENV: OPTIMIZE_CADENCE value; defaulting to 'batch'", "value", s)
		return OptimizeAllSynch
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

// decideStrategy determines the optimization strategy based on the current phase.
func (pl *MyCrossNodePreemption) decideStrategy(phase Phase) StrategyDecision {
	// If there's an active plan, block all new pods.
	ap := pl.getActivePlan()
	if ap != nil {
		return DecideBlockWhileActive
	}
	// Modes: AllSynch@PreEnqueue or AllSynch@PostFilter
	if optimizeAllSynch() && ((optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter())) {
		return DecidePending
	}
	// Modes: Every@PreEnqueue or Every@PostFilter
	if optimizeEvery() && ((optimizeAtPreEnqueue() && phase.atPreEnqueue()) || (optimizeAtPostFilter() && phase.atPostFilter())) {
		return DecideEvery // optimize for every new pod
	}
	return DecidePass // if not at the phase of optimization, allow all pods
}
