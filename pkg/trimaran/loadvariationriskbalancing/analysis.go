/*
Copyright 2021 The Kubernetes Authors.

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

package loadvariationriskbalancing

import (
	"math"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

/*
Calculation of risk score for resources given measured data
*/

// computeScore : compute score given usage statistics
// - risk = [ average + margin * stDev^{1/sensitivity} ] / 2
// - score = ( 1 - risk ) * maxScore
func computeScore(rs *trimaran.ResourceStats, margin float64, sensitivity float64) float64 {
	if rs.Capacity <= 0 {
		klog.ErrorS(nil, "Invalid resource capacity", "capacity", rs.Capacity)
		return 0
	}

	// make sure values are within bounds
	rs.Req = math.Max(rs.Req, 0)
	rs.UsedAvg = math.Max(math.Min(rs.UsedAvg, rs.Capacity), 0)
	rs.UsedStdev = math.Max(math.Min(rs.UsedStdev, rs.Capacity), 0)

	// calculate average and deviation factors
	mu, sigma := trimaran.GetMuSigma(rs)

	// apply root power
	if sensitivity >= 0 {
		sigma = math.Pow(sigma, 1/sensitivity)
	}
	// apply multiplier
	sigma *= margin
	sigma = math.Max(math.Min(sigma, 1), 0)

	// evaluate overall risk factor
	risk := (mu + sigma) / 2
	klog.V(6).InfoS("Evaluating risk factor", "mu", mu, "sigma", sigma, "margin", margin, "sensitivity", sensitivity, "risk", risk)
	return (1. - risk) * float64(framework.MaxNodeScore)
}
