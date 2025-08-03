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

package validation

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

var (
	supportNodeResourcesMode sets.Set[string]
	validScoringStrategy     sets.Set[string]
)

func init() {
	supportNodeResourcesMode = sets.New[string](
		string(config.Least),
		string(config.Most),
	)

	validScoringStrategy = sets.New[string](
		string(config.MostAllocated),
		string(config.BalancedAllocation),
		string(config.LeastAllocated),
		string(config.LeastNUMANodes),
	)
}

func ValidateNodeResourceTopologyMatchArgs(path *field.Path, args *config.NodeResourceTopologyMatchArgs) error {
	var allErrs field.ErrorList
	scoringStrategyTypePath := path.Child("scoringStrategy.type")
	if err := validateScoringStrategyType(args.ScoringStrategy.Type, scoringStrategyTypePath); err != nil {
		allErrs = append(allErrs, err)
	}

	return allErrs.ToAggregate()
}

func validateScoringStrategyType(scoringStrategy config.ScoringStrategyType, path *field.Path) *field.Error {
	if !validScoringStrategy.Has(string(scoringStrategy)) {
		return field.Invalid(path, scoringStrategy, "invalid ScoringStrategyType")
	}
	return nil
}

func validateResources(resources []schedconfig.ResourceSpec, p *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i, resource := range resources {
		if resource.Weight <= 0 {
			msg := fmt.Sprintf("resource weight of %v should be a positive value, got :%v", resource.Name, resource.Weight)
			allErrs = append(allErrs, field.Invalid(p.Index(i).Child("weight"), resource.Weight, msg))
		}
	}
	return allErrs
}

func validateNodeResourcesModeType(mode config.ModeType, path *field.Path) *field.Error {
	if !supportNodeResourcesMode.Has(string(mode)) {
		return field.Invalid(path, mode, "invalid support ModeType")
	}
	return nil
}

func ValidateNodeResourcesAllocatableArgs(args *config.NodeResourcesAllocatableArgs, path *field.Path) error {
	var allErrs field.ErrorList
	if args.Resources != nil {
		allErrs = append(allErrs, validateResources(args.Resources, path.Child("resources"))...)
	}
	if err := validateNodeResourcesModeType(args.Mode, path.Child("mode")); err != nil {
		allErrs = append(allErrs, err)
	}
	if len(allErrs) == 0 {
		return nil
	}
	return allErrs.ToAggregate()
}

func ValidateCoschedulingArgs(args *config.CoschedulingArgs, _ *field.Path) error {
	var allErrs field.ErrorList
	if args.PermitWaitingTimeSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("permitWaitingTimeSeconds"),
			args.PermitWaitingTimeSeconds, "must be greater than 0"))
	}
	if args.PodGroupBackoffSeconds < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("podGroupBackoffSeconds"),
			args.PodGroupBackoffSeconds, "must be greater than 0"))
	}
	if len(allErrs) == 0 {
		return nil
	}
	return allErrs.ToAggregate()
}
