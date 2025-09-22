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

	if !pl.IsSolverEnabled() { // ensure at least one solver is enabled
		klog.Error(ErrNoSolverEnabled)
		return nil, ErrNoSolverEnabled
	}

	// Warm informers
	f := h.SharedInformerFactory()
	podsInf := f.Core().V1().Pods().Informer()
	if err := podsInf.AddIndexers(cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	}); err != nil {
		klog.ErrorS(err, "failed adding namespace indexer to Pod informer")
	}
	nodesInf := f.Core().V1().Nodes().Informer()
	cmsInf := f.Core().V1().ConfigMaps().Informer()
	rsInf := f.Apps().V1().ReplicaSets().Informer()
	ssInf := f.Apps().V1().StatefulSets().Informer()
	dsInf := f.Apps().V1().DaemonSets().Informer()
	jobInf := f.Batch().V1().Jobs().Informer()
	go pl.waitForPluginReadiness(ctx, podsInf, nodesInf, cmsInf, rsInf, ssInf, dsInf, jobInf)

	// Ensure Active is set to false
	pl.Active.Store(false)

	// Plugin configuration
	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "mode", strategyToString())
	klog.InfoS("Plugin configuration", "planTimeout", PlanExecutionTimeout.String(), "pythonSolver", SolverPythonEnabled, "bfsSolver", SolverBfsEnabled, "localSearchSolver", SolverLocalSearchEnabled, "solverTimeout", SolverPythonTimeout.String())
	if optimizeAllSynch() || optimizeAllAsynch() {
		klog.InfoS("Loop configuration", "optimizationInterval", OptimizationInterval.String())
	}

	return pl, nil
}
