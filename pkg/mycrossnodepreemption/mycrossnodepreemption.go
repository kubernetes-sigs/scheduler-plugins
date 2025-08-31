// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
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

	// Ensure the Pod informer has the namespace index
	podInf := h.SharedInformerFactory().Core().V1().Pods().Informer()
	if err := podInf.AddIndexers(cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	}); err != nil {
		klog.ErrorS(err, "failed adding namespace indexer to Pod informer")
	}

	pl.Active.Store(false)

	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "mode", modeToString())
	klog.InfoS("Solver configuration", "timeout", SolverTimeout.String())
	klog.InfoS("Plan configuration", "executionTimeout", PlanExecutionTimeout.String())
	if optimizeInBatches() || optimizeContinuously() {
		klog.InfoS("Loop configuration", "optimizationInterval", OptimizationInterval.String())
	}

	if optimizeInBatches() {
		go pl.periodicOptimizeLoop(ctx, PhaseBatch)
	} else if optimizeContinuously() {
		go pl.periodicOptimizeLoop(ctx, PhaseContinuous)
	} else if optimizeForEvery() && optimizeAtPreEnqueue() {
		go pl.idleNudgeBlockedLoop(ctx)
	}

	return pl, nil
}
