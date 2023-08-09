/*
Copyright 2021 The Kubernetes Authors.

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

package reclaimidleresource

import (
	"strconv"

	schedulingv1 "k8s.io/api/scheduling/v1"
)

const (
	AnnotationKeyPrefix                     = "reclaim-idle-resource.scheduling.x-k8s.io/"
	AnnotationKeyMinimumPreemptablePriority = AnnotationKeyPrefix + "minimum-preemptable-priority"
	AnnotationKeyTolerationSeconds          = AnnotationKeyPrefix + "toleration-seconds"
	AnnotationKeyResourceType               = AnnotationKeyPrefix + "resource-type"
	AnnotationKeyResourceIdleSeconds        = AnnotationKeyPrefix + "resource-idle-seconds"
	AnnotationKeyResourceIdleUsageThreshold = AnnotationKeyPrefix + "resource-idle-usage-threshold"
)

// Policy holds reclaimidleresource policy configuration.  Each property values are annotated in the target PriorityClass resource.
// Example:
//
//	kind: PriorityClass
//	  metadata:
//	  name: toleration-policy-sample
//	  annotation:
//	    reclaim-idle-resource.scheduling.x-k8s.io/minimum-preemptable-priority: "10000"
//	    reclaim-idle-resource.scheduling.x-k8s.io/toleration-seconds: "3600"
//	    reclaim-idle-resource.scheduling.x-k8s.io/resource-type: "gpu"
//	    reclaim-idle-resource.scheduling.x-k8s.io/resource-idle-seconds: "3600"
//	    reclaim-idle-resource.scheduling.x-k8s.io/resource-idle-usage-threshold: "0"

type Policy struct {
	// MinimumPreemptablePriority specifies the minimum priority value that can preempt this priority class.
	// It defaults to the PriorityClass's priority value + 1 if not set, which means pods that have a higher priority value can preempt it.
	MinimumPreemptablePriority int32

	// TolerationSeconds specifies how long this priority class can tolerate preemption
	// by priorities lower than MinimumPreemptablePriority.
	// It defaults to zero if not set. Zero value means the pod will be preempted immediately. i.e., no toleration at all.
	// If it's set to a positive value, the duration will be honored.
	// If it's set to a negative value, the pod can be tolerated forever - i.e., pods with priority
	// lower than MinimumPreemptablePriority won't be able to preempt it.
	// This value affects scheduled pods only (no effect on nominated pods).
	TolerationSeconds int64

	// ResourceType refers to the type of resource that needs to be managed
	ResourceType string

	// ResourceIdleSeconds specifies how long the priority class can tolerate the resource to be idle.
	ResourceIdleSeconds int64

	// ResourceIdleUsageThreshold refers to actual idle usage to be considered. Defaults to 0
	ResourceIdleUsageThreshold float64
}

func parseReclaimIdleResourcesPolicy(
	pc schedulingv1.PriorityClass,
) (*Policy, error) {
	policy := &Policy{}

	minimumPreemptablePriorityStr, ok := pc.Annotations[AnnotationKeyMinimumPreemptablePriority]
	if !ok {
		policy.MinimumPreemptablePriority = pc.Value + 1 // default value
	} else {
		minimumPreemptablePriority, err := strconv.ParseInt(minimumPreemptablePriorityStr, 10, 32)
		if err != nil {
			return nil, err
		}
		policy.MinimumPreemptablePriority = int32(minimumPreemptablePriority)
	}

	tolerationSecondsStr, ok := pc.Annotations[AnnotationKeyTolerationSeconds]
	if !ok {
		policy.TolerationSeconds = 0 // default value
	} else {
		tolerationSeconds, err := strconv.ParseInt(tolerationSecondsStr, 10, 64)
		if err != nil {
			return nil, err
		}
		policy.TolerationSeconds = tolerationSeconds
	}

	resourceTypeStr, ok := pc.Annotations[AnnotationKeyResourceType]
	if !ok {
		policy.ResourceType = "cpu" // default value
	} else {
		policy.ResourceType = resourceTypeStr
	}

	resourceIdleSecondsStr, ok := pc.Annotations[AnnotationKeyResourceIdleSeconds]
	if !ok {
		policy.ResourceIdleSeconds = 0 // default value
	} else {
		resourceIdleSeconds, err := strconv.ParseInt(resourceIdleSecondsStr, 10, 64)
		if err != nil {
			return nil, err
		}
		policy.ResourceIdleSeconds = resourceIdleSeconds
	}

	resourceThresholdUsageStr, ok := pc.Annotations[AnnotationKeyResourceIdleUsageThreshold]
	if !ok {
		policy.ResourceIdleUsageThreshold = 0.0 // default value
	} else {
		resourceIdleUsageThreshold, err := strconv.ParseFloat(resourceThresholdUsageStr, 64)
		if err != nil {
			return nil, err
		}
		policy.ResourceIdleUsageThreshold = resourceIdleUsageThreshold
	}

	return policy, nil
}
