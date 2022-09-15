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

package v1beta2

import (
	conversion "k8s.io/apimachinery/pkg/conversion"
	config "sigs.k8s.io/scheduler-plugins/apis/config"
)

// Convert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs is an manually generated conversion function.
func Convert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(in *NodeResourceTopologyMatchArgs, out *config.NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	if in.ScoringStrategy != nil {
		if err := autoConvert_v1beta2_ScoringStrategy_To_config_ScoringStrategy(in.ScoringStrategy, &out.ScoringStrategy, s); err != nil {
			return err
		}
	}
	return nil
}

// Convert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs is an manually generated conversion function.
func Convert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs(in *config.NodeResourceTopologyMatchArgs, out *NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	out.ScoringStrategy = new(ScoringStrategy)
	if err := autoConvert_config_ScoringStrategy_To_v1beta2_ScoringStrategy(&in.ScoringStrategy, out.ScoringStrategy, s); err != nil {
		return err
	}
	return nil
}
