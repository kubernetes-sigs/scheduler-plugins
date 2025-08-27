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

	// Ensure the Pod informer has the namespace index
	podInf := h.SharedInformerFactory().Core().V1().Pods().Informer()
	if err := podInf.AddIndexers(cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	}); err != nil {
		klog.ErrorS(err, "failed adding namespace indexer to Pod informer")
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
