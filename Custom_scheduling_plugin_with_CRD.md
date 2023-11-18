# How to build a new scheduling plugin

Various scheduling use cases require various combination of scheduling plugins. Sometimes in cooperation with specific controllers which bring external view of a cluster. In some cases it is practical to implement a scheduling plugin which utilizes CRs to read about the cluster state. Building a plugin with Filter and Scoring extension points reaching out for CRs carrying this external view. The upstream https://github.com/kubernetes-sigs/scheduler-plugins repository provides basic setting helping to implement your own scheduling plugin. Easy to extend. Easy to integrate a new plugin with the scheduling framework. Easy to build a new scheduler.

For illustrative purpose the new plugin (named `NodeCRDResourceFit`) is going to schedule pods based on resources reported through CRs.

## Prepare the development environment

The upstream https://github.com/kubernetes-sigs/scheduler-plugins repository is well prepared to provide the basic building blocks for a new plugin. From giving examples of existing plugins, wiring them with the scheduling framework to just running `make` to build all the pieces into a single binary for testing and production.

```
$ git clone https://github.com/kubernetes-sigs/scheduler-plugins
$ cd scheduler-plugins
$ make
```

After cloning the repository running the `make` command will pull the vendor directory and verifies that the code builds.

## Create a custom scheduling plugin

Each community plugin has a dedicated directory located under `/pkg` directory. You can explore any of these to see how the individual extension points (e.g. Filter, Score) are implemented.

```
$ mkdir pkg/nodecrdresourcefit
$ cd pkg/nodecrdresourcefit
$ touch node_crd_resource_fit.go
```

It is a good practice to also provide a `README.md`:

```
cat << EOF > README.md
Overview

Implementation of \`NodeCRDResourceFit\` plugin which reads available resources on nodes through CRs.
EOF
```

The plugin implementation can be split up into the following steps:
- Creating a plugin skeleton
- Building a CRD and clientset
- Implementing Filter and Score extension points

### Plugin skeleton

The plugin implementation can start with a very simple skeleton:

```go
package nodecrdresourcefit

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// NodeCRDResourceFit is a plugin which filters/scores nodes based on available
// resources on nodes provided through CRs
type NodeCRDResourceFit struct {
	handle framework.Handle
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
func NewNodeCRDResourceFit(fitArgs runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &NodeCRDResourceFit{
		handle: h,
	}, nil
}
```

**Facts**:

- The code imports `k8s.io/kubernetes/pkg/scheduler/framework` providing definition of the framework data types
- `NodeCRDResourceFit` defines `handle` field of `framework.Handle` which (once injected through `NewNodeCRDResourceFit`) provides handlers for accessing cluster snapshot, shared informers, etc.
- lines containing `framework.ScorePlugin` and `framework.FilterPlugin` make sure the Go compiler checks whether the plugin implements both the Filter and the Score extension point.
- `NodeCRDResourceFitName` constant with `Name()` method defines name of the plugin
- `NewNodeCRDResourceFit`: invoked when the scheduler registry is built and injected with the framework handle

When `go build sigs.k8s.io/scheduler-plugins/pkg/nodecrdresourcefit` is executed from the `scheduler-plugins` directory the Go compiler complains about missing `Filter` and `Score` methods:
```
# sigs.k8s.io/scheduler-plugins/pkg/nodecrdresourcefit
pkg/nodecrdresourcefit/node_crd_resource_fit.go:13:30: cannot convert &NodeCRDResourceFit{} (type *NodeCRDResourceFit) to type framework.ScorePlugin:
	*NodeCRDResourceFit does not implement framework.ScorePlugin (missing Name method)
pkg/nodecrdresourcefit/node_crd_resource_fit.go:14:31: cannot convert &NodeCRDResourceFit{} (type *NodeCRDResourceFit) to type framework.FilterPlugin:
	*NodeCRDResourceFit does not implement framework.FilterPlugin (missing Filter method)
```

The building errors are expected since the code specifies build-time checks for extension points which are not yet implemented:
```
var _ = framework.ScorePlugin(&NodeCRDResourceFit{})
var _ = framework.FilterPlugin(&NodeCRDResourceFit{})
```

### CRD and clientset

The https://github.com/kubernetes-sigs/scheduler-plugins repository already defines few CRDs. To create a new CRD the steps reduce into:
1. Creating a new data type(s) under [pkg/apis/scheduling/v1alpha1/types.go](https://github.com/kubernetes-sigs/scheduler-plugins/blob/abbdb9f6ff9fd56c99d525f70e9647da3032456b/pkg/apis/scheduling/v1alpha1/types.go):
   ```go
   // +genclient
   // +genclient:nonNamespaced
   // +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
   // +kubebuilder:resource:scope=Cluster

   // ClusterScopedResource is a collection of cluster scoped resources
   type ClusterScopedResource struct {
   	metav1.TypeMeta `json:",inline"`
   	// Standard object's metadata.
   	// +optional
   	metav1.ObjectMeta `json:"metadata,omitempty"`

   	// Specification of the desired behavior of the cluster scoped resource.
   	Spec ClusterScopedResourceSpec `json:"spec"`
   }

   // ClusterScopedResourceSpec specifies resources
   type ClusterScopedResourceSpec struct {
   	// +optional
   	ResourcesFilter map[v1.ResourceName]bool `json:"resourcesFilter,omitempty"`
   	// +optional
   	ResourcesScore map[v1.ResourceName]int64 `json:"resourcesScore,omitempty"`
   }

   // +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

   // ClusterScopedResourceList is a collection of ClusterScopedResource.
   type ClusterScopedResourceList struct {
   	metav1.TypeMeta `json:",inline"`
   	// Standard list metadata
   	// +optional
   	metav1.ListMeta `json:"metadata,omitempty"`

   	// Items is the list of ClusterScopedResource
   	Items []ClusterScopedResource `json:"items"`
   }
   ```

   **Facts**:
   - Notice the `+genclient` and other `+` directives. They are required by the code autogenerators
   - Each top data type requires `metav1.TypeMeta` and `metav1.ObjectMeta`, resp. `metav1.ListMeta` data type embedded (the data types provide `Name`, `Namespace` and other fields)

2. Generate clientset, informers and listers for the new CRD
   ```sh
   $ ./hack/update-codegen.sh
   ```
   Updated clientset, informers and listers are located under `pkg/generated` directory.
3. Initialize the clientset and informers in the `NewNodeCRDResourceFit` function:
   ```go
   import (
   	"context"
   	"fmt"

   	"k8s.io/apimachinery/pkg/runtime"
   	"k8s.io/client-go/tools/cache"
   	"k8s.io/klog/v2"
   	"k8s.io/kubernetes/pkg/scheduler/framework"

   	crdclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
   	crdinformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
   	v1alpha1schedulinginformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
   )
   ...
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
   ```
   **Facts**:
   - The list of imported packages need to be extended with new imports
   - The `NewNodeCRDResourceFit` function initializes a client (`crdclientset.NewForConfigOrDie`), starts an informer factory (`crdInformerFactory.Start`) and waits until the informer cache is synced (`cache.WaitForCacheSync`).


### Filter extension point

Every filter plugin must implement the [Filter method](https://github.com/kubernetes/kubernetes/blob/2433b083a928af7eed1ee9594e37ce6c742237d6/pkg/scheduler/framework/interface.go#L358-L372):

```go
package nodecrdresourcefit

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	crdclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	crdinformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions"
	v1alpha1schedulinginformers "sigs.k8s.io/scheduler-plugins/pkg/generated/informers/externalversions/scheduling/v1alpha1"
)

...

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
```

**Facts**:

- The list of imported packages need to be extended with new imports (including the newly generated informers)
- The `Filter` method retrieves a CRD object (CR) for a given node
- The custom logic for filtering nodes based on the CR lives here

### Score extension point

The same approach as in the `Filter` case holds for the `Score`:

```go
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
```

## Enable the plugin in the custom scheduler

Once the new scheduling plugin is implemented, it can be enabled.
The list of enabled plugins is located under `/cmd/scheduler/main.go`:
```go
command := app.NewSchedulerCommand(
  app.WithPlugin(capacityscheduling.Name, capacityscheduling.New),
  app.WithPlugin(coscheduling.Name, coscheduling.New),
  ...
)
```

Extending the list with `app.WithPlugin` will enable the plugin:
```
command := app.NewSchedulerCommand(
  app.WithPlugin(capacityscheduling.Name, capacityscheduling.New),
  app.WithPlugin(coscheduling.Name, coscheduling.New),
  ...
  app.WithPlugin(nodecrdresourcefit.NodeCRDResourceFitName, nodecrdresourcefit.NewNodeCRDResourceFit),
)
```

Running another `make` will produce a scheduler binary with the new scheduling plugin.

## Deploying the CRD and the scheduler

Manifest for the new `ClusterScopedResources` CRD can be generating using the https://github.com/kubernetes-sigs/controller-tools repository. You can buid the `controller-tools` binary from within the upstream repository:

```
$ git clone https://github.com/kubernetes-sigs/controller-tools
$ cd controller-tools
$ go build -o controller-gen ./cmd/controller-gen
```

Once the binary is accessible through the `PATH` env, you can generate the manifest:

```
$ cd scheduler-plugins
$ controller-gen crd paths=./pkg/apis/scheduling/... +output:stdout > crds.yaml
```

The command will generate manifests for all CRDs (including definitions of already existing types). So unless the goal is to enable all plugins, there's no need to apply all CRDs. Also be aware of https://github.com/kubernetes/enhancements/pull/1111.

```
$ # editing the crds.yaml and removing definitions of CRDs of plugins which are not to be enabled
$ # add "api-approved.kubernetes.io": "https://github.com/kubernetes/kubernetes/pull/78458" into the newly generated CRD
$ $ oc apply -f crds.yaml
customresourcedefinition.apiextensions.k8s.io/clusterscopedresources.scheduling.sigs.k8s.io created
#
```

After building the scheduler binary, the scheduler needs to be informed to enable the new plugin
for filtering and scoring with the following configuration:

```
$ cat scheduler-config.yaml
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: secondary-scheduler
  plugins:
    filter:
      enabled:
      - name: NodeCRDResourceFit
      disabled:
      - name: "*"
    score:
      enabled:
      - name: NodeCRDResourceFit
      disabled:
      - name: "*"
```

The new plugin also expects CRs to be available:
```
$ cat clusterscopedresources-cr.yaml
---
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: ClusterScopedResources
metadata:
  name: "REPLACE_ME_WITH_NODE_NAME"
spec:
  resourcesFilter:
    "company.net/memory": true
  resourcesScore:
    "company.net/memory": 55
---
apiVersion: scheduling.sigs.k8s.io/v1alpha1
kind: ClusterScopedResources
metadata:
  name: "REPLACE_ME_WITH_NODE_NAME"
spec:
  resourcesFilter:
    "company.net/memory": true
  resourcesScore:
    "company.net/memory": 55
$ oc apply -f clusterscopedresources-cr.yaml
```

The scheduler can be run from a CLI:

```
$ cd scheduler-plugins
$ make
$ ./bin/kube-scheduler --config pkg/nodecrdresourcefit/scheduler-config.yaml -v=5
```

The scheduler will schedule only pods which have the `schedulerName` field set to `secondary-scheduler`:

```
$ cat simple-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginx
spec:
  schedulerName: secondary-scheduler
  containers:
  - name: nginx
    image: nginx:1.14.2
    ports:
    - containerPort: 80
    resources:
      requests:
        company.net/memory: 100Mi
      limits:
        company.net/memory: 100Mi
$ oc apply -f simple-pod.yaml
```

Once the scheduler notices the pod the following logs will appear and the pod gets scheduled:
```
I0301 14:43:59.686874 3271115 eventhandlers.go:118] "Add event for unscheduled pod" pod="default/nginx"
I0301 14:43:59.686966 3271115 scheduling_queue.go:928] "About to try and schedule pod" pod="default/nginx"
I0301 14:43:59.686992 3271115 scheduler.go:443] "Attempting to schedule pod" pod="default/nginx"
I0301 14:43:59.687377 3271115 node_crd_resource_fit.go:67] "CRD for node not found" nodeName="AAA"
I0301 14:43:59.687425 3271115 node_crd_resource_fit.go:67] "CRD for node not found" nodeName="BBB"
I0301 14:43:59.687465 3271115 node_crd_resource_fit.go:67] "CRD for node not found" nodeName="CCC"
I0301 14:43:59.687482 3271115 node_crd_resource_fit.go:67] "CRD for node not found" nodeName="DDD"
I0301 14:43:59.687737 3271115 node_crd_resource_fit.go:90] "Scoring node" nodeName="EEE"
I0301 14:43:59.687762 3271115 node_crd_resource_fit.go:90] "Scoring node" nodeName="FFF"
I0301 14:43:59.687801 3271115 node_crd_resource_fit.go:132] "Resources and score" podName="nginx" nodeName="EEE" score=55
I0301 14:43:59.687840 3271115 node_crd_resource_fit.go:132] "Resources and score" podName="nginx" nodeName="FFF" score=55
I0301 14:43:59.688225 3271115 default_binder.go:52] "Attempting to bind pod to node" pod="default/nginx" node="FFF"
I0301 14:43:59.801841 3271115 cache.go:385] "Finished binding for pod, can be expired" pod="default/nginx"
I0301 14:43:59.801880 3271115 eventhandlers.go:161] "Delete event for unscheduled pod" pod="default/nginx"
I0301 14:43:59.801895 3271115 scheduler.go:621] "Successfully bound pod to node" pod="default/nginx" node="FFF" evaluatedNodes=6 feasibleNodes=2
I0301 14:43:59.801911 3271115 eventhandlers.go:186] "Add event for scheduled pod" pod="default/nginx"
```

