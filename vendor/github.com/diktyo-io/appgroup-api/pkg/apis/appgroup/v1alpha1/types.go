package v1alpha1

import (
	appgroupapi "github.com/diktyo-io/appgroup-api/pkg/apis/appgroup"
	_ "github.com/gogo/protobuf/gogoproto"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Constants for AppGroup
const (
	// AppGroupLabel is the default label of the AppGroup for the network-aware framework
	AppGroupLabel = appgroupapi.GroupName

	// AppGroupSelectorLabel is the default selector label for Pods belonging to a given Workload (e.g., workload = App-A)
	AppGroupSelectorLabel = AppGroupLabel + ".workload"

	// Topological Sorting algorithms supported by AppGroup
	AppGroupKahnSort        = "KahnSort"
	AppGroupTarjanSort      = "TarjanSort"
	AppGroupReverseKahn     = "ReverseKahn"
	AppGroupReverseTarjan   = "ReverseTarjan"
	AppGroupAlternateKahn   = "AlternateKahn"
	AppGroupAlternateTarjan = "AlternateTarjan"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:scope=Namespaced,shortName=ag

// AppGroup is a collection of Pods belonging to the same application.
// +protobuf=true
type AppGroup struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object's metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// AppGroupSpec defines the number of Pods and which Pods belong to the group.
	// +optional
	Spec AppGroupSpec `json:"spec,omitempty" protobuf:"bytes,2,opt,name=spec"`

	// AppGroupStatus defines the observed use.
	// +optional
	Status AppGroupStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// AppGroupSpec represents the template of a app group.
// +protobuf=true
type AppGroupSpec struct {
	// NumMembers defines the number of Pods belonging to the App Group
	// +kubebuilder:validation:Minimum=1
	// +required
	NumMembers int32 `json:"numMembers" protobuf:"bytes,1,opt,name=numMembers"`

	// The preferred Topology Sorting Algorithm
	// +required
	TopologySortingAlgorithm string `json:"topologySortingAlgorithm" protobuf:"bytes,2,opt,name=topologySortingAlgorithm"`

	// Workloads defines the workloads belonging to the group
	// +required
	Workloads AppGroupWorkloadList `json:"workloads" protobuf:"bytes,3,rep,name=workloads, casttype=AppGroupWorkloadList"`
}

// AppGroupWorkload represents the Workloads belonging to the App Group.
// +protobuf=true
type AppGroupWorkload struct {
	// Workload reference Info.
	// +required
	Workload AppGroupWorkloadInfo `json:"workload" protobuf:"bytes,1,opt,name=workload, casttype=AppGroupWorkloadInfo"`

	// Dependencies of the Workload.
	// +optional
	Dependencies DependenciesList `json:"dependencies,omitempty" protobuf:"bytes,2,opt,name=dependencies, casttype=DependenciesList"`
}

// AppGroupWorkloadInfo contains information about one workload.
// +protobuf=true
type AppGroupWorkloadInfo struct {
	// Kind of the workload, info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds"
	// +required
	Kind string `json:"kind" protobuf:"bytes,1,opt,name=kind"`

	// Name represents the workload, info: http://kubernetes.io/docs/user-guide/identifiers#names
	// +required
	Name string `json:"name" protobuf:"bytes,2,opt,name=name"`

	// Selector defines how to find Pods related to the Workload (key = workload). (e.g., workload=w1)
	// +required
	Selector string `json:"selector" protobuf:"bytes,3,opt,name=selector"`

	// ApiVersion defines the versioned schema of an object.
	//+optional
	APIVersion string `json:"apiVersion,omitempty" protobuf:"bytes,4,opt,name=apiVersion"`

	// Namespace of the workload
	//+optional
	Namespace string `json:"namespace,omitempty" protobuf:"bytes,5,opt,name=namespace"`
}

// AppGroupWorkloadList contains an array of Pod objects.
// +protobuf=true
type AppGroupWorkloadList []AppGroupWorkload

// DependenciesInfo contains information about one dependency.
// +protobuf=true
type DependenciesInfo struct {
	// Workload reference Info.
	// +required
	Workload AppGroupWorkloadInfo `json:"workload" protobuf:"bytes,1,opt,name=workload, casttype=AppGroupWorkloadInfo"`

	// MinBandwidth between workloads
	// +optional
	MinBandwidth resource.Quantity `json:"minBandwidth,omitempty" protobuf:"bytes,2,opt,name=minBandwidth"`

	// Max Network Cost between workloads
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Default=0
	// +kubebuilder:validation:Maximum=10000
	MaxNetworkCost int64 `json:"maxNetworkCost,omitempty" protobuf:"bytes,3,opt,name=maxNetworkCost"`
}

// DependenciesList contains an array of ResourceInfo objects.
// +protobuf=true
type DependenciesList []DependenciesInfo

// AppGroupStatus represents the current state of an AppGroup.
// +protobuf=true
type AppGroupStatus struct {
	// The number of actively running workloads (e.g., number of pods).
	// +optional
	// +kubebuilder:validation:Minimum=0
	RunningWorkloads int32 `json:"runningWorkloads,omitempty" protobuf:"bytes,1,opt,name=runningWorkloads"`

	// ScheduleStartTime of the group
	// +optional
	ScheduleStartTime metav1.Time `json:"scheduleStartTime,omitempty" protobuf:"bytes,2,opt,name=scheduleStartTime"`

	// TopologyCalculationTime of the group
	// +optional
	TopologyCalculationTime metav1.Time `json:"topologyCalculationTime,omitempty" protobuf:"bytes,3,opt,name=topologyCalculationTime"`

	// Topology order for TopSort plugin (QueueSort)
	// +optional
	TopologyOrder AppGroupTopologyList `json:"topologyOrder,omitempty" protobuf:"bytes,4,rep,name=topologyOrder,casttype=TopologyList"`
}

// AppGroupTopologyInfo represents the calculated order for a given Workload.
// +protobuf=true
type AppGroupTopologyInfo struct {
	// Workload reference Info.
	Workload AppGroupWorkloadInfo `json:"workload,omitempty" protobuf:"bytes,1,opt,name=workload, casttype=AppGroupWorkloadInfo"`

	// Topology index.
	Index int32 `json:"index,omitempty" protobuf:"bytes,2,opt,name=index"`
}

// TopologyList contains an array of workload indexes for the TopologySorting plugin.
// +protobuf=true
type AppGroupTopologyList []AppGroupTopologyInfo

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// AppGroupList is a collection of app groups.
type AppGroupList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`

	// Items is the list of AppGroup
	Items []AppGroup `json:"items"`
}
