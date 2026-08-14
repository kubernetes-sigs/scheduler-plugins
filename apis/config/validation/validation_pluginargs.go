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
	"net/url"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	schedconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"

	"sigs.k8s.io/scheduler-plugins/apis/config"
)

var (
	supportNodeResourcesMode sets.Set[string]
	validScoringStrategy     sets.Set[string]
	validPreemptionMode      sets.Set[string]
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

	validPreemptionMode = sets.New[string](
		string(config.PreemptionDisabled),
		string(config.PreemptionEnabled),
	)
}

func ValidateNodeResourceTopologyMatchArgs(path *field.Path, args *config.NodeResourceTopologyMatchArgs) error {
	var allErrs field.ErrorList
	scoringStrategyTypePath := path.Child("scoringStrategy.type")
	if err := validateScoringStrategyType(args.ScoringStrategy.Type, scoringStrategyTypePath); err != nil {
		allErrs = append(allErrs, err)
	}

	if args.PreemptionMode != nil {
		preemptionModePath := path.Child("preemptionMode")
		if err := validatePreemptionMode(*args.PreemptionMode, preemptionModePath); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	return allErrs.ToAggregate()
}

func validateScoringStrategyType(scoringStrategy config.ScoringStrategyType, path *field.Path) *field.Error {
	if !validScoringStrategy.Has(string(scoringStrategy)) {
		return field.Invalid(path, scoringStrategy, "invalid ScoringStrategyType")
	}
	return nil
}

func validatePreemptionMode(mode config.PreemptionMode, path *field.Path) *field.Error {
	if !validPreemptionMode.Has(string(mode)) {
		return field.Invalid(path, mode, "invalid PreemptionMode")
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
	if args.PodGroupRejectPercentage < 0 || args.PodGroupRejectPercentage > 100 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("podGroupRejectPercentage"),
			args.PodGroupRejectPercentage, "must be between 0 and 100"))
	}
	if len(allErrs) == 0 {
		return nil
	}
	return allErrs.ToAggregate()
}

// ValidateHighLoadFilterArgs validates HighLoadFilter configuration.
func ValidateHighLoadFilterArgs(args *config.HighLoadFilterArgs, path *field.Path) error {
	var allErrs field.ErrorList

	thresholdsPath := path.Child("usageThresholds")
	if args.UsageThresholds.CPU < 0 || args.UsageThresholds.CPU > 100 {
		allErrs = append(allErrs, field.Invalid(thresholdsPath.Child("cpu"), args.UsageThresholds.CPU, "must be between 0 and 100"))
	}
	if args.UsageThresholds.Memory < 0 || args.UsageThresholds.Memory > 100 {
		allErrs = append(allErrs, field.Invalid(thresholdsPath.Child("memory"), args.UsageThresholds.Memory, "must be between 0 and 100"))
	}

	if args.MetricsUpdateIntervalSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("metricsUpdateIntervalSeconds"), args.MetricsUpdateIntervalSeconds, "must be greater than 0"))
	}
	if args.NodeMetricExpirationSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("nodeMetricExpirationSeconds"), args.NodeMetricExpirationSeconds, "must be greater than 0"))
	} else if args.NodeMetricExpirationSeconds < args.MetricsUpdateIntervalSeconds {
		allErrs = append(allErrs, field.Invalid(path.Child("nodeMetricExpirationSeconds"), args.NodeMetricExpirationSeconds, "must be greater than or equal to metricsUpdateIntervalSeconds"))
	}

	providerPath := path.Child("metricProvider")
	if args.WatcherAddress != "" {
		if !validHTTPURL(args.WatcherAddress) {
			allErrs = append(allErrs, field.Invalid(path.Child("watcherAddress"), args.WatcherAddress, "must be an absolute HTTP or HTTPS URL"))
		}
		if args.MetricProvider.Type != "" || args.MetricProvider.Address != "" || args.MetricProvider.Token != "" || args.MetricProvider.InsecureSkipVerify {
			allErrs = append(allErrs, field.Invalid(providerPath, "<configured>", "must be empty when watcherAddress is set"))
		}
		return allErrs.ToAggregate()
	}

	switch args.MetricProvider.Type {
	case config.HighLoadFilterKubernetesMetricsServer:
		if args.MetricProvider.Address != "" || args.MetricProvider.Token != "" || args.MetricProvider.InsecureSkipVerify {
			allErrs = append(allErrs, field.Invalid(providerPath, "<configured>", "address, token, and insecureSkipVerify are not supported for KubernetesMetricsServer"))
		}
	case config.HighLoadFilterPrometheus:
		if !validHTTPURL(args.MetricProvider.Address) {
			allErrs = append(allErrs, field.Invalid(providerPath.Child("address"), args.MetricProvider.Address, "must be an absolute HTTP or HTTPS URL"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(providerPath.Child("type"), args.MetricProvider.Type, []string{
			string(config.HighLoadFilterKubernetesMetricsServer),
			string(config.HighLoadFilterPrometheus),
		}))
	}

	return allErrs.ToAggregate()
}

func validHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}
