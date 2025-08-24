// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	// ======= Batch strategy =======
	// Batch strategy: BatchPostFilter, BatchPreEnqueue, BatchOff
	BatchMode BatchIngressMode = BatchOff

	// Every-preemptor strategy (mutually exclusive with any BatchMode != BatchOff)
	ModeEveryPreemptor = true

	BatchSolveInterval = 60 * time.Second // periodic cohort solve
	BatchInitialDelay  = 15 * time.Second // small delay before first run

	// ======= Plan settings =======
	PlanExecutionTTL     = 60 * time.Second // how long a plan may run before being terminated
	EvictionPollTimeout  = 20 * time.Second
	EvictionPollInterval = 1 * time.Second
	SolverTimeout        = 50 * time.Second
)

func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	// Exactly one strategy
	if (batchingEnabled() && ModeEveryPreemptor) || (!batchingEnabled() && !ModeEveryPreemptor) {
		return nil, fmt.Errorf("%s: invalid config: enable exactly one of {BatchMode!=BatchOff, PostFilterSinglePreemptor}", Name)
	}

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
	klog.InfoS("Plugin initialized", "name", Name, "version", Version,
		"batchMode", batchModeToString(), "everyPreemptorMode", ModeEveryPreemptor)

	if batchingEnabled() {
		go pl.batchLoop(context.Background())
	}
	return pl, nil
}
