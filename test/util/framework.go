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

package util

import (
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

// NewFramework is a variant version of st.NewFramework - with extra PluginConfig slice as input.
func NewFramework(fns []st.RegisterPluginFunc, cfgs []config.PluginConfig, profileName string, opts ...runtime.Option) (framework.Framework, error) {
	registry := runtime.Registry{}
	plugins := &config.Plugins{}
	for _, f := range fns {
		f(&registry, plugins)
	}
	profile := &config.KubeSchedulerProfile{
		SchedulerName: profileName,
		Plugins:       plugins,
		PluginConfig:  cfgs,
	}
	return runtime.NewFramework(registry, profile, opts...)
}
