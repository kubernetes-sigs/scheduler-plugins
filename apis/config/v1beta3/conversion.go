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

package v1beta3

import (
	"unsafe"

	"k8s.io/apimachinery/pkg/conversion"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

// This file stores all necessary manual conversion bits, to leave zz_generated*.go intact after code generation.

func Convert_v1beta3_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(in *NodeResourceTopologyMatchArgs, out *config.NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	if err := autoConvert_v1beta3_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	out.ScoringStrategy = *(*config.ScoringStrategy)(unsafe.Pointer(in.ScoringStrategy))
	return nil
}

func Convert_config_NodeResourceTopologyMatchArgs_To_v1beta3_NodeResourceTopologyMatchArgs(in *config.NodeResourceTopologyMatchArgs, out *NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	if err := autoConvert_config_NodeResourceTopologyMatchArgs_To_v1beta3_NodeResourceTopologyMatchArgs(in, out, s); err != nil {
		return err
	}
	out.ScoringStrategy = (*ScoringStrategy)(unsafe.Pointer(&in.ScoringStrategy))
	return nil
}
