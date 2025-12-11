// plan_registration.go
package mypriorityoptimizer

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var buildPlanFn = func(
	pl *SharedState,
	out *PlannerOutput,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*Plan, error) {
	return pl.buildPlan(out, preemptor, pods)
}

var exportPlanToConfigMapFn = func(
	pl *SharedState,
	ctx context.Context,
	id string,
	stored *StoredPlan,
) error {
	return pl.exportPlanToConfigMap(ctx, id, stored)
}

// planRegistration builds and registers a new plan as active, exporting it to a ConfigMap.
func (pl *SharedState) planRegistration(
	ctx context.Context,
	solverResult PlannerResult,
	out *PlannerOutput,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*Plan, *ActivePlan, error) {
	if out == nil {
		return nil, nil, fmt.Errorf("nil solver output for plan registration")
	}

	plan, err := buildPlanFn(pl, out, preemptor, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("build actions: %w", err)
	}

	id := getUniqueId(PlanConfigMapNamePrefix)
	pl.setActivePlan(plan, id, pods)

	storedPlan := &StoredPlan{
		PluginVersion:        PluginVersion,
		OptimizationStrategy: combinedModeToString(),
		GeneratedAt:          time.Now().UTC(),
		PlanStatus:           PlanStatusActive,
		Plan:                 plan,
		SolverResult:         solverResult,
	}
	if err := exportPlanToConfigMapFn(pl, ctx, id, storedPlan); err != nil {
		klog.ErrorS(err, "export plan to ConfigMap failed")
	}

	return plan, pl.getActivePlan(), nil
}
