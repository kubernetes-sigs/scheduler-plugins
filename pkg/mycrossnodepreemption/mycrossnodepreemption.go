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
	Strategy StrategyIngress = StrategyBatchPostFilter

	// ======= Batch settings =======
	BatchSolveInterval = 60 * time.Second // periodic cohort solve
	BatchInitialDelay  = 15 * time.Second // small delay before first run

	// ======= Plan settings =======
	PlanExecutionTTL  = 60 * time.Second // how long a plan may run before being terminated; it can take up to 60 seconds to complete a plan
	SolverTimeout     = 55 * time.Second
	SolverMode        = SolverModeLexi // SolverModeLexi or SolverModeWeighted
	SolverLogProgress = false          // log search progress (may be verbose here in GO)
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
		go pl.batchLoop(context.Background())
	}
	return pl, nil
}
