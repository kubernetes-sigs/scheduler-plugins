/*
Copyright 2023 The Kubernetes Authors.

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

package util

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

type PodGroupWrapper struct{ v1alpha1.PodGroup }

func MakePodGroup() *PodGroupWrapper {
	return &PodGroupWrapper{v1alpha1.PodGroup{}}
}

func (p *PodGroupWrapper) Obj() *v1alpha1.PodGroup {
	return &p.PodGroup
}

func (p *PodGroupWrapper) Name(s string) *PodGroupWrapper {
	p.SetName(s)
	return p
}

func (p *PodGroupWrapper) Namespace(s string) *PodGroupWrapper {
	p.SetNamespace(s)
	return p
}

func (p *PodGroupWrapper) MinMember(i int32) *PodGroupWrapper {
	p.Spec.MinMember = i
	return p
}

func (p *PodGroupWrapper) Time(t time.Time) *PodGroupWrapper {
	p.CreationTimestamp.Time = t
	return p
}

func (p *PodGroupWrapper) MinResources(resources map[v1.ResourceName]string) *PodGroupWrapper {
	res := make(v1.ResourceList)
	for name, value := range resources {
		res[name] = resource.MustParse(value)
	}
	p.PodGroup.Spec.MinResources = res
	return p
}

func (p *PodGroupWrapper) Phase(phase v1alpha1.PodGroupPhase) *PodGroupWrapper {
	p.Status.Phase = phase
	return p
}
