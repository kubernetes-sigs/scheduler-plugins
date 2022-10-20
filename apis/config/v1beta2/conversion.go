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
	"unsafe"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

// This file stores all necessary manual conversion bits, to leave zz_generated*.go intact after code generation.

func Convert_v1beta2_CoschedulingArgs_To_config_CoschedulingArgs(in *CoschedulingArgs, out *config.CoschedulingArgs, s conversion.Scope) error {
	return autoConvert_v1beta2_CoschedulingArgs_To_config_CoschedulingArgs(in, out, s)
}

func Convert_v1beta2_LoadVariationRiskBalancingArgs_To_config_LoadVariationRiskBalancingArgs(in *LoadVariationRiskBalancingArgs, out *config.LoadVariationRiskBalancingArgs, s conversion.Scope) error {
	if err := autoConvert_v1beta2_LoadVariationRiskBalancingArgs_To_config_LoadVariationRiskBalancingArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	if err := Convert_v1beta2_MetricProviderSpec_To_config_MetricProviderSpec(&in.MetricProvider, &out.TrimaranSpec.MetricProvider, s); err != nil {
		return err
	}
	return v1.Convert_Pointer_string_To_string(&in.WatcherAddress, &out.TrimaranSpec.WatcherAddress, s)
}

func Convert_config_LoadVariationRiskBalancingArgs_To_v1beta2_LoadVariationRiskBalancingArgs(in *config.LoadVariationRiskBalancingArgs, out *LoadVariationRiskBalancingArgs, s conversion.Scope) error {
	if err := autoConvert_config_LoadVariationRiskBalancingArgs_To_v1beta2_LoadVariationRiskBalancingArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	if err := Convert_config_MetricProviderSpec_To_v1beta2_MetricProviderSpec(&in.TrimaranSpec.MetricProvider, &out.MetricProvider, s); err != nil {
		return err
	}
	return v1.Convert_string_To_Pointer_string(&in.TrimaranSpec.WatcherAddress, &out.WatcherAddress, s)
}

func Convert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(in *NodeResourceTopologyMatchArgs, out *config.NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	if err := autoConvert_v1beta2_NodeResourceTopologyMatchArgs_To_config_NodeResourceTopologyMatchArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	out.ScoringStrategy = *(*config.ScoringStrategy)(unsafe.Pointer(in.ScoringStrategy))
	return nil
}

func Convert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs(in *config.NodeResourceTopologyMatchArgs, out *NodeResourceTopologyMatchArgs, s conversion.Scope) error {
	if err := autoConvert_config_NodeResourceTopologyMatchArgs_To_v1beta2_NodeResourceTopologyMatchArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	out.ScoringStrategy = (*ScoringStrategy)(unsafe.Pointer(&in.ScoringStrategy))
	return nil
}

func Convert_v1beta2_TargetLoadPackingArgs_To_config_TargetLoadPackingArgs(in *TargetLoadPackingArgs, out *config.TargetLoadPackingArgs, s conversion.Scope) error {
	if err := autoConvert_v1beta2_TargetLoadPackingArgs_To_config_TargetLoadPackingArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	if err := Convert_v1beta2_MetricProviderSpec_To_config_MetricProviderSpec(&in.MetricProvider, &out.TrimaranSpec.MetricProvider, s); err != nil {
		return err
	}
	return v1.Convert_Pointer_string_To_string(&in.WatcherAddress, &out.TrimaranSpec.WatcherAddress, s)
}

func Convert_config_TargetLoadPackingArgs_To_v1beta2_TargetLoadPackingArgs(in *config.TargetLoadPackingArgs, out *TargetLoadPackingArgs, s conversion.Scope) error {
	if err := autoConvert_config_TargetLoadPackingArgs_To_v1beta2_TargetLoadPackingArgs(in, out, s); err != nil {
		return err
	}
	// Manual conversions.
	if err := Convert_config_MetricProviderSpec_To_v1beta2_MetricProviderSpec(&in.TrimaranSpec.MetricProvider, &out.MetricProvider, s); err != nil {
		return err
	}
	return v1.Convert_string_To_Pointer_string(&in.TrimaranSpec.WatcherAddress, &out.WatcherAddress, s)
}
