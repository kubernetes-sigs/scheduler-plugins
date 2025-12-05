// plan_registration.go

package mypriorityoptimizer

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// planRegistration builds and registers a new plan as active, exporting it to a ConfigMap.
func (pl *SharedState) planRegistration(
	ctx context.Context,
	solver SolverResult,
	preemptor *v1.Pod,
	pods []*v1.Pod,
) (*Plan, *ActivePlan, error) {
	// Build the plan from the solver output
	plan, err := pl.buildPlan(solver.Output, preemptor, pods)
	if err != nil {
		return nil, nil, fmt.Errorf("build actions: %w", err)
	}

	// Unique plan id
	id := fmt.Sprintf("plan-%d", time.Now().UnixNano())

	// Set active plan
	pl.setActivePlan(plan, id, pods)

	// Store plan in ConfigMap for debugging purposes
	storedPlan := &StoredPlan{
		PluginVersion:        Version,
		OptimizationStrategy: modeToString(),
		GeneratedAt:          time.Now().UTC(),
		PlanStatus:           PlanStatusActive,
		Plan:                 plan,
		Solver:               summarizeAttempt(solver),
	}
	if preemptor != nil {
		storedPlan.Preemptor = &Preemptor{
			Pod: Pod{
				UID:       preemptor.UID,
				Namespace: preemptor.Namespace,
				Name:      preemptor.Name,
			},
			NominatedNode: plan.NominatedNode,
		}
	}
	if err := pl.exportPlanToConfigMap(ctx, id, storedPlan); err != nil {
		klog.ErrorS(err, "export plan to ConfigMap failed")
	}

	return plan, pl.getActivePlan(), nil
}
