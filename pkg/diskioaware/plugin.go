/*
Copyright 2024 The Kubernetes Authors.

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

package diskioaware

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	externalinformer "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/informers/externalversions"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/validation"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/diskio"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/normalizer"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/utils"
)

type DiskIO struct {
	rh         resource.Handle
	scorer     Scorer
	nm         *normalizer.NormalizerManager
	nodeLister corelisters.NodeLister
}

const (
	Name           = "DiskIO"
	stateKeyPrefix = "DiskIO-"
	maxRetries     = 2
	workers        = 2
	baseModelDir   = "/tmp"
)

var _ = framework.FilterPlugin(&DiskIO{})
var _ = framework.ScorePlugin(&DiskIO{})
var _ = framework.ReservePlugin(&DiskIO{})

type stateData struct {
	request        v1alpha1.IOBandwidth
	nodeSupportIOI bool
}

func (d *stateData) Clone() framework.StateData {
	return d
}

// New initializes a new plugin and returns it.
func New(ctx context.Context, configuration runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	d := &DiskIO{
		rh:         diskio.New(),
		nodeLister: handle.SharedInformerFactory().Core().V1().Nodes().Lister(),
	}
	args, ok := configuration.(*config.DiskIOArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type DiskIOArgs, got %T", args)
	}

	// validate args
	if err := validation.ValidateDiskIOArgs(nil, args); err != nil {
		return nil, err
	}

	// load disk vendor normalize functions
	d.nm = normalizer.NewNormalizerManager(baseModelDir, maxRetries)
	go d.nm.Run(ctx, args.DiskIOModelConfig, workers)

	//initialize scorer
	scorer, err := getScorer(args.ScoreStrategy)
	if err != nil {
		return nil, err
	}
	d.scorer = scorer

	//initialize disk IO resource handler
	err = d.rh.Run(resource.NewExtendedCache(), handle.ClientSet())
	if err != nil {
		return nil, err
	}

	ratelimiter := workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Second)
	resource.IoiContext, err = resource.NewContext(ratelimiter, args.NSWhiteList, handle)
	if err != nil {
		return nil, err
	}
	go resource.IoiContext.RunWorkerQueue(ctx)

	//initialize event handling
	podLister := handle.SharedInformerFactory().Core().V1().Pods().Lister()
	nsLister := handle.SharedInformerFactory().Core().V1().Namespaces().Lister()
	eh := resource.NewIOEventHandler(d.rh, handle, podLister, nsLister, d.nm)

	podInformer := handle.SharedInformerFactory().Core().V1().Pods().Informer()
	iof := externalinformer.NewSharedInformerFactory(resource.IoiContext.VClient, time.Minute)
	err = eh.BuildEvtHandler(ctx, podInformer, iof)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (ps *DiskIO) Name() string {
	return Name
}

// Filter invoked at the filter extension point.
func (ps *DiskIO) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if resource.IoiContext.InNamespaceWhiteList(pod.Namespace) {
		return framework.NewStatus(framework.Success)
	}
	node := nodeInfo.Node()
	nodeName := node.Name
	exist := ps.rh.(resource.CacheHandle).NodeRegistered(nodeName)
	if !exist {
		if ps.rh.(resource.CacheHandle).IsIORequired(pod.Annotations) {
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("node %v without disk io aware support cannot schedule disk io aware workload", nodeName))
		} else {
			state.Write(framework.StateKey(stateKeyPrefix+nodeName), &stateData{nodeSupportIOI: false})
			return framework.NewStatus(framework.Success)
		}
	}
	model, err := ps.rh.(resource.CacheHandle).GetDiskNormalizeModel(nodeName)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	normalizeFunc, err := ps.nm.GetNormalizer(model)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	key := pod.Annotations[common.DiskIOAnnotation]
	reqStr, err := normalizeFunc(key)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	request, err := utils.RequestStrToQuantity(reqStr)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	ok, err := ps.rh.(resource.CacheHandle).CanAdmitPod(nodeName, request)
	if !ok {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	state.Write(framework.StateKey(stateKeyPrefix+nodeName), &stateData{request: request, nodeSupportIOI: true})
	return framework.NewStatus(framework.Success)
}

// Score invoked at the score extension point.
func (ps *DiskIO) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	if resource.IoiContext.InNamespaceWhiteList(pod.Namespace) {
		return framework.MaxNodeScore, framework.NewStatus(framework.Success)
	}
	score := int64(0)
	sd, err := getStateData(state, stateKeyPrefix+nodeName)
	if err != nil {
		return framework.MinNodeScore, framework.NewStatus(framework.Unschedulable, err.Error())
	}
	// BE pod schedule to node without IOI support
	if !sd.nodeSupportIOI {
		score = framework.MaxNodeScore
	} else {
		s, err := ps.scorer.Score(nodeName, sd, ps.rh)
		if err != nil {
			return framework.MinNodeScore, framework.NewStatus(framework.Unschedulable, err.Error())
		} else {
			score = s
		}
	}
	return score, framework.NewStatus(framework.Success)
}

// ScoreExtensions of the Score plugin.
func (ps *DiskIO) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// Reserve is the functions invoked by the framework at "reserve" extension point.
func (ps *DiskIO) Reserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	if resource.IoiContext.InNamespaceWhiteList(pod.Namespace) {
		return framework.NewStatus(framework.Success)
	}
	sd, err := getStateData(state, stateKeyPrefix+nodeName)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	if !sd.nodeSupportIOI {
		return framework.NewStatus(framework.Success)
	}
	err = ps.rh.(resource.CacheHandle).AddPod(pod, nodeName, sd.request)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	ps.rh.(resource.CacheHandle).PrintCacheInfo()
	request := sd.request
	clearStateData(state, ps.nodeLister)
	err = resource.IoiContext.AddPod(pod, nodeName, request)
	if err != nil {
		return framework.NewStatus(framework.Unschedulable, err.Error())
	}
	return framework.NewStatus(framework.Success)
}

func (ps *DiskIO) Unreserve(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) {
	if resource.IoiContext.InNamespaceWhiteList(pod.Namespace) {
		return
	}
	err := ps.rh.(resource.CacheHandle).RemovePod(pod, nodeName)
	if err != nil {
		klog.Error("Unreserve pod error: ", err.Error())
	}
	err = resource.IoiContext.RemovePod(pod, nodeName)
	if err != nil {
		klog.Errorf("fail to remove pod in ReservedPod: %v", err)
	}

}

func (ps *DiskIO) EventsToRegister() []framework.ClusterEvent {
	// To register a custom event, follow the naming convention at:
	// https://git.k8s.io/kubernetes/pkg/scheduler/eventhandlers.go#L410-L422
	ce := []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.All},
		{Resource: framework.GVK(fmt.Sprintf("nodediskdevices.v1alpha1.%v", scheduling.GroupName)), ActionType: framework.Add | framework.Delete},
		{Resource: framework.GVK(fmt.Sprintf("nodediskiostatses.v1alpha1.%v", scheduling.GroupName)), ActionType: framework.Update},
	}
	return ce
}

// read by reserve and score
func getStateData(cs *framework.CycleState, key string) (*stateData, error) {
	state, err := cs.Read(framework.StateKey(key))
	if err != nil {
		return nil, err
	}
	s, ok := state.(*stateData)
	if !ok {
		return nil, errors.New("unable to convert state into stateData")
	}
	return s, nil
}

func clearStateData(state *framework.CycleState, nodeLister corelisters.NodeLister) {
	// delete all cycle states for all nodes
	nodes, err := nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Get nodes error: %v", err)
	}
	for _, node := range nodes {
		deleteStateData(state, stateKeyPrefix+node.Name)
	}
}

func deleteStateData(cs *framework.CycleState, key string) {
	cs.Delete(framework.StateKey(key))
}
