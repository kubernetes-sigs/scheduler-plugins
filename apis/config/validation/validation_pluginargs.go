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
	"os"

	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/scheduler-plugins/apis/config"
)

var validScoringStrategy = sets.NewString(
	string(config.MostAllocated),
	string(config.BalancedAllocation),
	string(config.LeastAllocated),
	string(config.LeastNUMANodes),
)

var validDiskIOScoreStrategy = sets.NewString(
	string(config.MostAllocated),
	string(config.LeastAllocated),
)

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

func ValidateDiskIOArgs(path *field.Path, args *config.DiskIOArgs) error {
	var allErrs field.ErrorList
	scoreStrategyPath := path.Child("scoreStrategy")
	if err := validateDiskIOScoreStrategy(args.ScoreStrategy, scoreStrategyPath); err != nil {
		allErrs = append(allErrs, err)
	}
	nswlPath := path.Child("nsWhiteList")
	if err := validateDiskIOWhiteListNamespace(args.NSWhiteList, nswlPath); err != nil {
		allErrs = append(allErrs, err)
	}
	mcPath := path.Child("diskIOModelConfig")
	if err := validateDiskIOModelConfig(args.DiskIOModelConfig, mcPath); err != nil {
		allErrs = append(allErrs, err)
	}

	return allErrs.ToAggregate()
}

func validateDiskIOModelConfig(configPath string, path *field.Path) *field.Error {
	if _, err := os.Stat(configPath); err != nil {
		return field.Invalid(path, configPath, "invalid DiskIOModelConfig")
	}
	return nil
}

func validateDiskIOScoreStrategy(scoreStrategy string, path *field.Path) *field.Error {
	if !validDiskIOScoreStrategy.Has(string(scoreStrategy)) {
		return field.Invalid(path, scoreStrategy, "invalid ScoreStrategy")
	}
	return nil
}

func validateDiskIOWhiteListNamespace(nsWhiteList []string, path *field.Path) *field.Error {
	for _, ns := range nsWhiteList {
		errs := apimachineryvalidation.ValidateNamespaceName(ns, false)
		if len(errs) > 0 {
			return field.Invalid(path, ns, "invalid namespace format")
		}
	}
	return nil
}
