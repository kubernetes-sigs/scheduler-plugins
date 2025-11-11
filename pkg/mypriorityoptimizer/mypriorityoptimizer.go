// mypriorityoptimizer.go

package mypriorityoptimizer

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func (pl *MyPriorityOptimizer) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}

	pl := &MyPriorityOptimizer{
		Handle:             h,
		Client:             client,
		BlockedWhileActive: newPodSet("BlockedWhileActive"),
	}

	if !pl.isAnySolverEnabled() { // ensure at least one solver is enabled
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
	klog.InfoS("Plan configuration", "executionTimeout", PlanExecutionTimeout.String())
	klog.InfoS("Solver configuration", solverConfigArgs()...)

	// Start HTTP server for manual triggering
	if optimizeAllSynch() || optimizeAllAsynch() || optimizeManualAllSynch() {
		go pl.startHTTPServer(ctx, HTTPAddr)
	}

	return pl, nil
}
