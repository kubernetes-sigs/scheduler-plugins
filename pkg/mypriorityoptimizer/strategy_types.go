package mypriorityoptimizer

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
