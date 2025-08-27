// mycrossnodepreemption.go

package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Name    = "MyCrossNodePreemption"
	Version = "v1.5.0"

	EXTRA_VERBOSE = false

	// ======= Optimality where/when settings =======
	OptimizeCadence          = OptimizeForEvery     // Choices: OptimizeForEvery, OptimizeInBatches
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

var V2 = klog.Level(2)

func (pl *MyCrossNodePreemption) Name() string { return Name }

func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	client, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, err
	}

	// Ensure the Pod informer has the namespace index.
	podInf := h.SharedInformerFactory().Core().V1().Pods().Informer()
	if err := podInf.AddIndexers(cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc, // "namespace" index
	}); err != nil {
		klog.ErrorS(err, "failed adding namespace indexer to Pod informer")
	}

	pl := &MyCrossNodePreemption{
		Handle:  h,
		Client:  client,
		Blocked: newPodSet(),
		Batched: newPodSet(),
	}

	if EXTRA_VERBOSE { // if set, then klog promotes V(2) logs to V(0)
		V2 = 0
		klog.InfoS("Extra verbose logging enabled")
	}

	// Continue to wait for informer caches to sync
	synced := h.SharedInformerFactory().WaitForCacheSync(ctx.Done())
	allSynced := true
	for _, v := range synced {
		if !v {
			allSynced = false
			break
		}
	}
	if !allSynced {
		klog.Error("SharedInformerFactory caches not fully synced")
	}

	time.Sleep(4 * time.Second) // wait a bit before starting optimization cycles

	pl.Active.Store(false)

	klog.InfoS("Plugin initialized", "name", Name, "version", Version, "mode", modeToString())

	if optimizeInBatches() {
		go pl.batchLoop(ctx)
	}
	return pl, nil
}
