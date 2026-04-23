/*
Copyright 2024 The Kubernetes Authors.

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

// Package energygradient implements a scheduler plugin that scores nodes
// based on energy gradients exposed via /egs/v1/gradient endpoints.
package energygradient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// EnergyGradient is a score plugin that favors nodes based on their
// energy gradient (marginal capacity, cost index, thermal headroom).
type EnergyGradient struct {
	handle framework.Handle
	client *http.Client
}

// Gradient represents the EGS gradient response.
type Gradient struct {
	ActorID                string      `json:"actor_id"`
	ActorType              string      `json:"actor_type"`
	Region                 string      `json:"region"`
	TS                     string      `json:"ts"`
	MarginalCapacityKW     float64     `json:"marginal_capacity_kw"`
	InstantaneousCostIndex float64     `json:"instantaneous_cost_index"`
	ThermodynamicPriority  float64     `json:"thermodynamic_priority"`
	Constraints            Constraints `json:"constraints"`
}

// Constraints represents physical constraints.
type Constraints struct {
	MaxDeltaKW         float64 `json:"max_delta_kw"`
	RampRateKWPerMin   float64 `json:"ramp_rate_kw_per_min"`
	ThermalHeadroomPct float64 `json:"thermal_headroom_pct"`
}

var _ framework.ScorePlugin = &EnergyGradient{}

// Name is the name of the plugin used in the Registry and configurations.
const Name = "EnergyGradient"

// AnnotationKey is the node annotation key for the EGS endpoint.
const AnnotationKey = "egs.ear-standard.org/endpoint"

// Name returns name of the plugin.
func (eg *EnergyGradient) Name() string {
	return Name
}

// Score invoked at the score extension point.
// Nodes with higher marginal capacity, lower cost, and higher thermal headroom score higher.
func (eg *EnergyGradient) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	node, err := eg.handle.ClientSet().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get node", "node", nodeName)
		return 0, framework.AsStatus(err)
	}

	endpoint, ok := node.Annotations[AnnotationKey]
	if !ok || endpoint == "" {
		// Node doesn't expose EGS gradient
		return 0, nil
	}

	gradient, err := eg.fetchGradient(endpoint)
	if err != nil {
		klog.ErrorS(err, "Failed to fetch gradient", "node", nodeName, "endpoint", endpoint)
		return 0, nil
	}

	// Score = marginal_capacity × (1 - cost_index) × thermal_headroom
	score := int64(gradient.MarginalCapacityKW *
		(1 - gradient.InstantaneousCostIndex) *
		(gradient.Constraints.ThermalHeadroomPct / 100))

	klog.V(4).InfoS("Scored node", "node", nodeName, "score", score,
		"marginal_kw", gradient.MarginalCapacityKW,
		"cost_index", gradient.InstantaneousCostIndex,
		"thermal_headroom", gradient.Constraints.ThermalHeadroomPct)

	return score, nil
}

// ScoreExtensions of the Score plugin.
func (eg *EnergyGradient) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// fetchGradient fetches the gradient from the EGS endpoint.
func (eg *EnergyGradient) fetchGradient(endpoint string) (*Gradient, error) {
	url := fmt.Sprintf("%s/egs/v1/gradient", endpoint)
	resp, err := eg.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var g Gradient
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return nil, err
	}

	return &g, nil
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &EnergyGradient{
		handle: h,
		client: &http.Client{Timeout: 2 * time.Second},
	}, nil
}
