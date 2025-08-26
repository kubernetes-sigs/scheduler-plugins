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

	// ======= Strategy =======
	// Choices: StrategyEveryPreemptor, StrategyBatchPostFilter, StrategyBatchPreEnqueue
	Strategy StrategyIngress = StrategyBatchPreEnqueue

	// ======= Batch settings =======
	BatchSolveInterval = 60 * time.Second // periodic solve interval
	BatchInitialDelay  = 15 * time.Second // small delay before first run

	// ======= Plan settings =======
	PlanExecutionTTL   = 60 * time.Second // how long a plan may run before being terminated; it can take up to 60 seconds to complete a plan
	ConfigMapNamespace = "kube-system"    // make sense to match it with the namespace of the kube-scheduler
	ConfigMapLabelKey  = "crossnode-plan"

	// ======= Solver settings =======
	SolverTimeout     = 55 * time.Second
	SolverMode        = SolverModeLexi // Choices: SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)
	PythonSolverPath  = "/opt/solver/main.py"
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
	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "strategy", strategyToString())

	if batchingEnabled() {
		go pl.batchLoop(ctx)
	}
	return pl, nil
}
