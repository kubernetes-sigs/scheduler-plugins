/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resourcepolicy

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/KunWuLuan/resourcepolicyapi/pkg/apis/scheduling/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/pkg/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/klog/v2"
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
	lh.V(2).Info("creating new resourcepolicy plugin")

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
	defer func() {
		ccache.Start(ctx)
		ccache.WaitForCacheSync(ctx)
		lh.V(2).Info("ResourcePolicyCache synced.")
	}()

	rspInformer, err := ccache.GetInformerForKind(ctx, v1alpha1.SchemeGroupVersion.WithKind("ResourcePolicy"))
	if err != nil {
		return nil, err
	}
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

	handle.SharedInformerFactory().Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			switch t := obj.(type) {
			case *v1.Node:
				node := t
				pods, err := handle.SharedInformerFactory().Core().V1().Pods().Informer().GetIndexer().ByIndex("spec.nodeName", node.Name)
				if err != nil {
					klog.Error(err, "get node pods failed")
					return
				}
				rspCache.processingLock.Lock()
				defer rspCache.processingLock.Unlock()

				podKeys := []string{}
				for _, i := range pods {
					pod := i.(*v1.Pod)
					rsp := GetManagedResourcePolicy(pod)
					if rsp == "" {
						continue
					}
					podKeys = append(podKeys, string(klog.KObj(pod).String()))
					rspCache.AddOrUpdateBoundPod(pod)
				}
				klog.InfoS("add node event", "processedPod", strings.Join(podKeys, ","))
			}
		},
	})

	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddIndexers(cache.Indexers{
		ManagedByResourcePolicyIndexKey: func(obj interface{}) ([]string, error) {
			return []string{string(GetManagedResourcePolicy(obj.(*v1.Pod)))}, nil
		},
		"spec.nodeName": func(obj interface{}) ([]string, error) {
			p := obj.(*v1.Pod)
			return []string{p.Spec.NodeName}, nil
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
				rspCache.processingLock.Lock()
				defer rspCache.processingLock.Unlock()
				rspCache.AddOrUpdateBoundPod(pd)
				klog.Info("add event for scheduled pod", "pod", klog.KObj(pd))
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pd, ok := newObj.(*v1.Pod)
				if !ok {
					return
				}
				rspCache.processingLock.Lock()
				defer rspCache.processingLock.Unlock()
				rspCache.AddOrUpdateBoundPod(pd)
				klog.Info("update event for scheduled pod", "pod", klog.KObj(pd))
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
		cache:         rspCache,
		client:        c,
		handle:        handle,
		schedulingCtx: map[keyStr]*schedulingContext{},
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
	logger := klog.FromContext(ctx).WithValues("pod", klog.KObj(pod))
	rspp.cache.processingLock.RLock()
	podKey := GetKeyStr(pod.ObjectMeta)
	if schedCtx, ok = rspp.schedulingCtx[podKey]; ok && schedCtx.matched != "" {
		matched = rspp.cache.getResourcePolicyInfoByKey(schedCtx.matched, pod.Namespace)
		if matched == nil || matched.rv != schedCtx.resourceVersion {
			schedCtx.matched = ""
			matched = nil
		}
	} else {
		schedCtx = &schedulingContext{}
		rspp.schedulingCtx[podKey] = schedCtx
	}

	if matched == nil {
		resourcePoliciesInNamespace := rspp.cache.rps[pod.Namespace]
		if len(resourcePoliciesInNamespace) == 0 {
			rspp.cache.processingLock.RUnlock()
			logger.V(2).Info("no resourcePolicy matches pod")
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
		logger.V(2).Info("no resourcePolicy matches pod")
		return nil, framework.NewStatus(framework.Skip, "no resourcePolicy matches pod")
	} else {
		logger = logger.WithValues("resourcePolicy", matched.ks)
		schedCtx.matched = matched.ks
		schedCtx.resourceVersion = matched.rv
		schedCtx.unitIdx = 0
	}
	matched.processingLock.Lock()
	for matched.processing {
		matched.cond.Wait()
	}
	matched.processingLock.Unlock()

	valid, labelKeyValue := genLabelKeyValueForPod(matched.policy, pod)
	if !valid {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, "some labels not found in pod")
	}
	preFilterState := &ResourcePolicyPreFilterState{
		matchedInfo:   matched,
		podRes:        framework.NewResource(resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})),
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
		if max == nil {
			continue
		}
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
	logger.V(2).Info("details of matched resource policy", "maxCount", preFilterState.maxCount, "currentCount", preFilterState.currentCount,
		"maxConsumption", resourceListToStr(preFilterState.maxConsumption), "resConsumption", resourceListToStr(preFilterState.resConsumption),
		"nodeSelectors", nodeSelectorsToStr(preFilterState.nodeSelectos))
	return nil, nil
}

func nodeSelectorsToStr(selectors []labels.Selector) string {
	res := []string{}
	for _, selector := range selectors {
		if selector == nil {
			continue
		}
		res = append(res, selector.String())
	}
	return strings.Join(res, "|")
}

func resourceListToStr(list []*framework.Resource) string {
	res := []string{}
	for _, r := range list {
		if r == nil {
			res = append(res, "nil")
			continue
		}
		res = append(res, fmt.Sprintf("cpu:%v,memory:%v,scalar:%+v", r.MilliCPU, r.Memory, r.ScalarResources))
	}
	return strings.Join(res, "|")
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
		if state.maxConsumption[idx] != nil && state.resConsumption[idx] != nil {
			res := state.resConsumption[idx].Clone()
			res.MilliCPU += state.podRes.MilliCPU
			res.Memory += state.podRes.Memory
			for k, v := range state.podRes.ScalarResources {
				res.AddScalar(k, v)
			}
			if gt, res := largeThan(res, state.maxConsumption[idx]); gt {
				notValidIdx = append(notValidIdx, unitNotAvaiInfo{
					idx: idx,
					res: res,
				})
				continue
			}
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
		if len(info) == 0 {
			return framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("not match any unit"))
		}
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
	obj, err := state.Read(ResourcePolicyPreFilterStateKey)
	if err != nil {
		return nil
	}
	preFilterState, ok := obj.(*ResourcePolicyPreFilterState)
	if !ok {
		return framework.AsStatus(fmt.Errorf("unable to convert state to ResourcePolicyPreFilterState"))
	}
	newPod := v1.Pod{}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := rspp.client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, &newPod)
		if err != nil {
			return err
		}
		if len(newPod.Annotations) == 0 {
			newPod.Annotations = make(map[string]string)
		}
		newPod.Annotations[ManagedByResourcePolicyAnnoKey] = string(preFilterState.matchedInfo.ks)
		return rspp.client.Update(ctx, &newPod)
	})
	if err != nil {
		return framework.AsStatus(fmt.Errorf("unable to get pod %v/%v", p.Namespace, p.Name))
	}
	return nil
}
