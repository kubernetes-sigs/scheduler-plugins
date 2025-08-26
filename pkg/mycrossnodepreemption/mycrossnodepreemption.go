// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	// ======= Optimality settings (new) =======
	OptimizeCadence          = OptimizeInBatches    // Choices: OptimizeForEvery, OptimizeInBatches
	OptimizeAt               = OptimizeAtPreEnqueue // Choices: OptimizeAtPostFilter, OptimizeAtPreEnqueue
	OptimizationInterval     = 60 * time.Second
	OptimizationInitialDelay = 5 * time.Second

	// ======= Solver settings =======
	SolverTimeout     = 55 * time.Second
	SolverMode        = SolverModeLexi // Choices: SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)
	SolverPath        = "/opt/solver/main.py"

	// ======= Plan settings =======
	PlanExecutionTTL       = 60 * time.Second // how long a plan may run before being terminated
	PlanConfigMapNamespace = "kube-system"    // match kube-scheduler namespace
	PlanConfigMapLabelKey  = "crossnode-plan"
)

func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}

	pl := &MyCrossNodePreemption{
		Handle:  h,
		Client:  client,
		Blocked: newPodSet(),
		Batched: newPodSet(),
	}

	pl.Active.Store(false)

	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "mode", modeToString())

	if optimizeInBatches() {
		go pl.batchLoop(ctx)
	}
	return pl, nil
}
