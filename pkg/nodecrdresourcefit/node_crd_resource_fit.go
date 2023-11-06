package nodecrdresourcefit

import (
	"context"
	"fmt"
	"math"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	crdclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	crdinformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	v1alpha1schedulinginformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
)

// NodeCRDResourceFit is a plugin which filters/scores nodes based on available
// resources on nodes provided through CRs
type NodeCRDResourceFit struct {
	handle      framework.Handle
	crdInformer v1alpha1schedulinginformers.ClusterScopedResourceInformer
}

var _ = framework.ScorePlugin(&NodeCRDResourceFit{})
var _ = framework.FilterPlugin(&NodeCRDResourceFit{})

// NodeCRDResourceFitName is the name of the plugin used in the Registry and configurations.
const NodeCRDResourceFitName = "NodeCRDResourceFit"

// Name returns name of the plugin. It is used in logs, etc.
func (fit *NodeCRDResourceFit) Name() string {
	return NodeCRDResourceFitName
}

// NewNodeCRDResourceFit initializes a new plugin and returns it.
func NewNodeCRDResourceFit(fitArgs runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	crdClient := crdclientset.NewForConfigOrDie(handle.KubeConfig())
	crdInformerFactory := crdinformers.NewSharedInformerFactory(crdClient, 0)
	crdInformer := crdInformerFactory.Scheduling().V1alpha1().ClusterScopedResources()
	crdInformer.Informer() // create ClusterScopedResources informer so it can be started

	ctx := context.TODO()
	crdInformerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), crdInformer.Informer().HasSynced) {
		err := fmt.Errorf("WaitForCacheSync failed")
		klog.ErrorS(err, "Cannot sync caches")
		return nil, err
	}

	return &NodeCRDResourceFit{
		handle:      handle,
		crdInformer: crdInformer,
	}, nil
}

func (fit *NodeCRDResourceFit) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	// Return error if the node info is not properly populated
	if nodeInfo.Node() == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}

	nodeClusterScopedResource, err := fit.crdInformer.Lister().Get(nodeInfo.Node().Name)
	if err != nil {
		klog.V(5).InfoS("CRD for node not found", "nodeName", nodeInfo.Node().Name)
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("CRD for node not found"))
	}

	// Check if requested resources are enabled on a node
	requestedResources := map[v1.ResourceName]struct{}{}
	for _, container := range pod.Spec.Containers {
		for resource := range container.Resources.Requests {
			requestedResources[resource] = struct{}{}
		}
	}

	for resource := range requestedResources {
		if filter, exists := nodeClusterScopedResource.Spec.ResourcesFilter[resource]; !exists || !filter {
			klog.V(5).InfoS("Node does not enable resource", "nodeName", nodeInfo.Node().Name, "resource", resource)
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("Node does not enable resource %v", resource))
		}
	}

	return framework.NewStatus(framework.Success, "")
}

func (fit *NodeCRDResourceFit) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	klog.V(5).InfoS("Scoring node", "nodeName", nodeName)

	nodeInfo, err := fit.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// Return error if the node info is not properly populated
	if nodeInfo.Node() == nil {
		return 0, framework.NewStatus(framework.Error, "node not found")
	}

	nodeClusterScopedResource, err := fit.crdInformer.Lister().Get(nodeInfo.Node().Name)
	if err != nil {
		klog.V(5).InfoS("CRD for node not found", "nodeName", nodeInfo.Node().Name)
		return 0, framework.NewStatus(framework.Unschedulable, fmt.Sprintf("CRD for node not found"))
	}

	requestedResources := map[v1.ResourceName]struct{}{}
	for _, container := range pod.Spec.Containers {
		for resource := range container.Resources.Requests {
			requestedResources[resource] = struct{}{}
		}
	}

	// Give highest score to the node which has the least amount requested
	var weightedSum float64 = 0.0
	var total float64 = 0
	for resource := range requestedResources {
		score, exists := nodeClusterScopedResource.Spec.ResourcesScore[resource]
		if !exists || score < 0 {
			score = 0
		}
		if score > framework.MaxTotalScore {
			score = framework.MaxTotalScore
		}
		weightedSum += 1.0 * float64(score)
		total += 1.0
	}

	totalScore := int64(math.Ceil(weightedSum / total))
	if klog.V(5).Enabled() {
		klog.InfoS("Resources and score",
			"podName", pod.Name, "nodeName", nodeName, "score", totalScore)
	}

	return totalScore, nil
}

func (fit *NodeCRDResourceFit) ScoreExtensions() framework.ScoreExtensions {
	return nil
}
