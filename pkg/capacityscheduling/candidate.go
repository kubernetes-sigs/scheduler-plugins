/*
Copyright 2020 The Kubernetes Authors.

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

package capacityscheduling

import extenderv1 "k8s.io/kube-scheduler/extender/v1"

type candidate struct {
	victims *extenderv1.Victims
	name    string
}

// Victims returns s.victims.
func (c *candidate) Victims() *extenderv1.Victims {
	return c.victims
}

// Name returns s.name.
func (c *candidate) Name() string {
	return c.name
}
