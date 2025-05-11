package resourcepolicy

import (
	"context"
	"fmt"
	"math"

	"github.com/KunWuLuan/resourcepolicyapi/pkg/apis/scheduling/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/pkg/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const Name = "ResourcePolicy"

type resourcePolicyPlugin struct {
	cache *resourcePolicyCache

	handle framework.Handle

	client client.Client

	schedulingCtx map[keyStr]*schedulingContext
}

var _ framework.PreFilterPlugin = &resourcePolicyPlugin{}
var _ framework.FilterPlugin = &resourcePolicyPlugin{}
var _ framework.ScorePlugin = &resourcePolicyPlugin{}
var _ framework.ReservePlugin = &resourcePolicyPlugin{}
var _ framework.PreBindPlugin = &resourcePolicyPlugin{}

func (rspp *resourcePolicyPlugin) Name() string {
	return Name
}

func New(ctx context.Context, args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	lh := klog.FromContext(ctx).WithValues("plugin", Name)
	lh.V(5).Info("creating new coscheduling plugin")

	podLister := handle.SharedInformerFactory().Core().V1().Pods().Lister()
	nodeLister := handle.SharedInformerFactory().Core().V1().Nodes().Lister()
	nodeInfoSnapshot := handle.SnapshotSharedLister().NodeInfos()
	rspCache := NewResourcePolicyCache(
		nodeLister,
		podLister,
		nodeInfoSnapshot,
	)

	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c, ccache, err := util.NewClientWithCachedReader(ctx, handle.KubeConfig(), scheme)
	if err != nil {
		return nil, err
	}

	rspInformer, _ := ccache.GetInformerForKind(ctx, v1alpha1.SchemeGroupVersion.WithKind("ResourcePolicy"))
	rspInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rsp, ok := obj.(*v1alpha1.ResourcePolicy)
			if !ok {
				return
			}
			rspCache.AddOrUpdateResPolicy(rsp)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			rsp, ok := newObj.(*v1alpha1.ResourcePolicy)
			if !ok {
				return
			}
			rspCache.AddOrUpdateResPolicy(rsp)
		},
		DeleteFunc: func(obj interface{}) {
			switch t := obj.(type) {
			case *v1alpha1.ResourcePolicy:
				rsp := t
				rspCache.DeleteResourcePolicy(rsp)
			case cache.DeletedFinalStateUnknown:
				rsp, ok := t.Obj.(*v1alpha1.ResourcePolicy)
				if !ok {
					return
				}
				rspCache.DeleteResourcePolicy(rsp)
			}
		},
	})

	handle.SharedInformerFactory().Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{})

	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddIndexers(cache.Indexers{
		ManagedByResourcePolicyIndexKey: func(obj interface{}) ([]string, error) {
			return []string{string(GetManagedResourcePolicy(obj.(*v1.Pod)))}, nil
		},
	})
	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *v1.Pod:
				return GetManagedResourcePolicy(t) != "" && t.Spec.NodeName != ""
			case cache.DeletedFinalStateUnknown:
				if pod, ok := t.Obj.(*v1.Pod); ok {
					return GetManagedResourcePolicy(pod) != "" && pod.Spec.NodeName != ""
				}
				return false
			default:
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pd, ok := obj.(*v1.Pod)
				if !ok {
					return
				}
				rspCache.AddOrUpdateBoundPod(pd)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pd, ok := newObj.(*v1.Pod)
				if !ok {
					return
				}
				rspCache.AddOrUpdateBoundPod(pd)
			},
			DeleteFunc: func(obj interface{}) {
				var pd *v1.Pod
				switch t := obj.(type) {
				case *v1.Pod:
					pd = t
				case cache.DeletedFinalStateUnknown:
					var ok bool
					pd, ok = t.Obj.(*v1.Pod)
					if !ok {
						return
					}
				}
				rspCache.DeleteBoundPod(pd)
			},
		},
	})

	plg := &resourcePolicyPlugin{
		cache: &resourcePolicyCache{
			pd2Rps: make(map[keyStr]keyStr),
			rps:    make(map[string]map[keyStr]*resourcePolicyInfo),
		},
		client: c,
		handle: handle,
	}
	return plg, nil
}

func (rspp *resourcePolicyPlugin) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	return []framework.ClusterEventWithHint{
		{},
	}, nil
}

func (rspp *resourcePolicyPlugin) PreFilter(ctx context.Context, state *framework.CycleState,
	pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	var matched *resourcePolicyInfo
	var schedCtx *schedulingContext
	var ok bool
	rspp.cache.processingLock.RLock()
	podKey := GetKeyStr(pod.ObjectMeta)
	if schedCtx, ok = rspp.schedulingCtx[podKey]; ok && schedCtx.matched != "" {
		matched = rspp.cache.getResourcePolicyInfoByKey(schedCtx.matched, pod.Namespace)
		if matched == nil || matched.rv != schedCtx.resourceVersion {
			schedCtx.matched = ""
			matched = nil
		}
	} else {
		rspp.schedulingCtx[podKey] = &schedulingContext{}
	}

	if matched == nil {
		resourcePoliciesInNamespace := rspp.cache.rps[pod.Namespace]
		if len(resourcePoliciesInNamespace) == 0 {
			rspp.cache.processingLock.RUnlock()
			return nil, framework.NewStatus(framework.Skip)
		}

		for _, rspi := range resourcePoliciesInNamespace {
			if rspi.podSelector.Matches(labels.Set(pod.Labels)) {
				if matched != nil {
					rspp.cache.processingLock.RUnlock()
					return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "multiple resourcePolicies match pod")
				}
				matched = rspi
			}
		}
	}
	rspp.cache.processingLock.RUnlock()

	if matched == nil {
		return nil, framework.NewStatus(framework.Skip, "no resourcePolicy matches pod")
	} else {
		schedCtx.matched = matched.ks
		schedCtx.resourceVersion = matched.rv
		schedCtx.unitIdx = 0
	}

	valid, labelKeyValue := genLabelKeyValueForPod(matched.policy, pod)
	if !valid {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "some labels not found in pod")
	}
	preFilterState := &ResourcePolicyPreFilterState{
		matchedInfo:   matched,
		podRes:        framework.NewResource(resource.PodRequests(pod, resource.PodResourcesOptions{})),
		labelKeyValue: labelKeyValue,

		currentCount:   make([]int, len(matched.nodeSelectors)),
		maxCount:       make([]int, len(matched.nodeSelectors)),
		maxConsumption: make([]*framework.Resource, len(matched.nodeSelectors)),
		resConsumption: make([]*framework.Resource, len(matched.nodeSelectors)),

		nodeSelectos: make([]labels.Selector, len(matched.nodeSelectors)),
	}
	for idx, count := range matched.assumedPodCount {
		preFilterState.currentCount[idx] = count[labelKeyValue]
	}
	for idx, consumption := range matched.assumedPodConsumption {
		preFilterState.resConsumption[idx] = consumption[labelKeyValue]
	}
	for idx, max := range matched.maxPodResources {
		preFilterState.maxConsumption[idx] = max.Clone()
	}
	copy(preFilterState.nodeSelectos, matched.nodeSelectors)
	for idx, max := range matched.policy.Spec.Units {
		if max.Max == nil {
			preFilterState.maxCount[idx] = math.MaxInt32
		} else {
			preFilterState.maxCount[idx] = int(*max.Max)
		}
	}
	state.Write(ResourcePolicyPreFilterStateKey, preFilterState)
	return nil, nil
}

func (rspp *resourcePolicyPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

type unitNotAvaiInfo struct {
	idx int
	res string
}

func findAvailableUnitForNode(nodeInfo *framework.NodeInfo, state *ResourcePolicyPreFilterState) (int, []unitNotAvaiInfo) {
	found := -1
	notValidIdx := []unitNotAvaiInfo{}
	for idx := range state.nodeSelectos {
		if !state.nodeSelectos[idx].Matches(labels.Set(nodeInfo.Node().Labels)) {
			continue
		}
		if state.currentCount[idx] >= state.maxCount[idx] {
			notValidIdx = append(notValidIdx, unitNotAvaiInfo{
				idx: idx,
				res: "pod",
			})
			continue
		}
		if lt, res := lessThan(state.resConsumption[idx], state.maxConsumption[idx]); lt {
			notValidIdx = append(notValidIdx, unitNotAvaiInfo{
				idx: idx,
				res: res,
			})
			continue
		}
		found = idx
		break
	}
	return found, notValidIdx
}

func (rspp *resourcePolicyPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	obj, err := state.Read(ResourcePolicyPreFilterStateKey)
	if err != nil {
		return nil
	}
	preFilterState, ok := obj.(*ResourcePolicyPreFilterState)
	if !ok {
		return framework.AsStatus(fmt.Errorf("cannot convert %T to ResourcePolicyPreFilterState", obj))
	}
	if avai, info := findAvailableUnitForNode(nodeInfo, preFilterState); avai == -1 {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("unit not available: %v", info))
	}
	return nil
}

func (rspp *resourcePolicyPlugin) Score(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) (int64, *framework.Status) {
	obj, err := state.Read(ResourcePolicyPreFilterStateKey)
	if err != nil {
		return 0, nil
	}
	preFilterState, ok := obj.(*ResourcePolicyPreFilterState)
	if !ok {
		return 0, framework.AsStatus(fmt.Errorf("cannot convert %T to ResourcePolicyPreFilterState", obj))
	}
	nodeInfo, err := rspp.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	if avai, info := findAvailableUnitForNode(nodeInfo, preFilterState); avai == -1 {
		return 0, framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("unit not available: %v", info))
	} else {
		return int64(100 - avai), nil
	}
}

func (rspp *resourcePolicyPlugin) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (rspp *resourcePolicyPlugin) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	nodeInfo, err := rspp.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}
	err = rspp.cache.Assume(state, pod, nodeInfo.Node())
	if err != nil {
		return framework.NewStatus(framework.Error, fmt.Sprintf("assuming pod %q: %v", pod.Name, err))
	}
	return nil
}

func (rspp *resourcePolicyPlugin) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	nodeInfo, err := rspp.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return
	}
	rspp.cache.Forget(state, pod, nodeInfo.Node())
}

func (rspp *resourcePolicyPlugin) PreBind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	return nil
}
