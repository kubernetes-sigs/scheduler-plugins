/*
Copyright 2022 The Kubernetes Authors.

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

package topologicalsort

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	networkawareutil "sigs.k8s.io/scheduler-plugins/pkg/networkaware/util"

	agv1alpha "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup/v1alpha1"
)

const (
	// Name : name of plugin used in the plugin registry and configurations.
	Name = "TopologicalSort"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agv1alpha.AddToScheme(scheme))
}

// TopologicalSort : Sort pods based on their AppGroup and corresponding microservice dependencies
type TopologicalSort struct {
	client.Client
	handle     framework.Handle
	namespaces []string
}

var _ framework.QueueSortPlugin = &TopologicalSort{}

// Name : returns the name of the plugin.
func (ts *TopologicalSort) Name() string {
	return Name
}

// getArgs : returns the arguments for the TopologicalSort plugin.
func getArgs(obj runtime.Object) (*pluginconfig.TopologicalSortArgs, error) {
	TopologicalSortArgs, ok := obj.(*pluginconfig.TopologicalSortArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type TopologicalSortArgs, got %T", obj)
	}

	return TopologicalSortArgs, nil
}

// New : create an instance of a TopologicalSort plugin
func New(_ context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(4).InfoS("Creating new instance of the TopologicalSort plugin")

	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}

	client, err := client.New(handle.KubeConfig(), client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}

	pl := &TopologicalSort{
		Client:     client,
		handle:     handle,
		namespaces: args.Namespaces,
	}
	return pl, nil
}

// Less is the function used by the activeQ heap algorithm to sort pods.
// 1) Sort Pods based on their AppGroup and corresponding service topology graph.
// 2) Otherwise, follow the strategy of the in-tree QueueSort Plugin (PrioritySort Plugin)
func (ts *TopologicalSort) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
	p1AppGroup := networkawareutil.GetPodAppGroupLabel(pInfo1.Pod)
	p2AppGroup := networkawareutil.GetPodAppGroupLabel(pInfo2.Pod)

	// If pods do not belong to an AppGroup, or being to different AppGroups, follow vanilla QoS Sort
	if p1AppGroup != p2AppGroup || len(p1AppGroup) == 0 {
		klog.V(4).InfoS("Pods do not belong to the same AppGroup CR", "p1AppGroup", p1AppGroup, "p2AppGroup", p2AppGroup)
		s := &queuesort.PrioritySort{}
		return s.Less(pInfo1, pInfo2)
	}

	// Pods belong to the same appGroup, get the CR
	klog.V(6).InfoS("Pods belong to the same AppGroup CR", "p1 name", pInfo1.Pod.Name, "p2 name", pInfo2.Pod.Name, "appGroup", p1AppGroup)
	agName := p1AppGroup
	appGroup := ts.findAppGroupTopologicalSort(agName)

	// Get labels from both pods
	labelsP1 := pInfo1.Pod.GetLabels()
	labelsP2 := pInfo2.Pod.GetLabels()

	// Binary search to find both order index since topology list is ordered by Workload Name
	orderP1 := networkawareutil.FindPodOrder(appGroup.Status.TopologyOrder, labelsP1[agv1alpha.AppGroupSelectorLabel])
	orderP2 := networkawareutil.FindPodOrder(appGroup.Status.TopologyOrder, labelsP2[agv1alpha.AppGroupSelectorLabel])

	klog.V(6).InfoS("Pod order values", "p1 order", orderP1, "p2 order", orderP2)

	// Lower is better
	return orderP1 <= orderP2
}

func (ts *TopologicalSort) findAppGroupTopologicalSort(agName string) *agv1alpha.AppGroup {
	klog.V(6).InfoS("namespaces: %s", ts.namespaces)
	for _, namespace := range ts.namespaces {
		klog.V(6).InfoS("appGroup CR", "namespace", namespace, "name", agName)
		// AppGroup couldn't be placed in several namespaces simultaneously
		appGroup := &agv1alpha.AppGroup{}
		err := ts.Get(context.TODO(), client.ObjectKey{
			Namespace: namespace,
			Name:      agName,
		}, appGroup)
		if err != nil {
			klog.V(4).InfoS("Cannot get AppGroup from AppGroupNamespaceLister:", "error", err)
			continue
		}
		if appGroup != nil {
			return appGroup
		}
	}
	return nil
}
