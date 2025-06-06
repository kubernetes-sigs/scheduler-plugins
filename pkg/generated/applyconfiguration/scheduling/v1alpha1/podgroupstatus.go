/*
Copyright The Kubernetes Authors.

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

// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedulingv1alpha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

// PodGroupStatusApplyConfiguration represents a declarative configuration of the PodGroupStatus type for use
// with apply.
type PodGroupStatusApplyConfiguration struct {
	Phase             *schedulingv1alpha1.PodGroupPhase `json:"phase,omitempty"`
	OccupiedBy        *string                           `json:"occupiedBy,omitempty"`
	Running           *int32                            `json:"running,omitempty"`
	Succeeded         *int32                            `json:"succeeded,omitempty"`
	Failed            *int32                            `json:"failed,omitempty"`
	ScheduleStartTime *v1.Time                          `json:"scheduleStartTime,omitempty"`
}

// PodGroupStatusApplyConfiguration constructs a declarative configuration of the PodGroupStatus type for use with
// apply.
func PodGroupStatus() *PodGroupStatusApplyConfiguration {
	return &PodGroupStatusApplyConfiguration{}
}

// WithPhase sets the Phase field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Phase field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithPhase(value schedulingv1alpha1.PodGroupPhase) *PodGroupStatusApplyConfiguration {
	b.Phase = &value
	return b
}

// WithOccupiedBy sets the OccupiedBy field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the OccupiedBy field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithOccupiedBy(value string) *PodGroupStatusApplyConfiguration {
	b.OccupiedBy = &value
	return b
}

// WithRunning sets the Running field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Running field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithRunning(value int32) *PodGroupStatusApplyConfiguration {
	b.Running = &value
	return b
}

// WithSucceeded sets the Succeeded field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Succeeded field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithSucceeded(value int32) *PodGroupStatusApplyConfiguration {
	b.Succeeded = &value
	return b
}

// WithFailed sets the Failed field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Failed field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithFailed(value int32) *PodGroupStatusApplyConfiguration {
	b.Failed = &value
	return b
}

// WithScheduleStartTime sets the ScheduleStartTime field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the ScheduleStartTime field is set to the value of the last call.
func (b *PodGroupStatusApplyConfiguration) WithScheduleStartTime(value v1.Time) *PodGroupStatusApplyConfiguration {
	b.ScheduleStartTime = &value
	return b
}
