package resourcepolicy

// schedulingContext is created when the pod scheduling started. and the context is deleted
// when the pod is deleted or bound.
type schedulingContext struct {
	matched keyStr
	// when begin to schedule, unitIdx is the index of last scheduling attempt
	// in the scheduling cycle, unitIdx is the index of current scheduling attempt
	unitIdx         int
	resourceVersion string
}
