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

package resource

import (
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

// ExtendedResource specifies extended resources's aggregation methods
// It is inteneded to be triggered by Pod/Node events
type ExtendedResource interface {
	Name() string
	AddPod(pod *v1.Pod, request v1alpha1.IOBandwidth) error
	RemovePod(pod *v1.Pod) error
	PrintInfo()
}
