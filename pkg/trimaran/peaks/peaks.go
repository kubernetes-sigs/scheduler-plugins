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

package peaks

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"github.com/paypal/load-watcher/pkg/watcher"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/apis/config"
	cfgv1 "sigs.k8s.io/scheduler-plugins/apis/config/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	Name = "Peaks"
)

type Peaks struct {
	handle    framework.Handle
	collector *trimaran.Collector
	args      *config.PeaksArgs
}

type PowerModel struct {
	K0 float64 `json:"k0"`
	K1 float64 `json:"k1"`
	K2 float64 `json:"k2"`
	// Power = K0 + K1 * e ^(K2 * x) : where x is utilisation
	// Idle power od node will be K0 - K1
}

var _ framework.ScorePlugin = &Peaks{}
var maxPower = 0.0
var clusterPowerModel map[string]PowerModel

func (pl *Peaks) Name() string {
	return Name
}

func initNodePowerModels() error {
	data, err := os.ReadFile(os.Getenv("NODE_POWER_MODEL"))
	if err != nil {
		klog.ErrorS(err, "Unable to read power model from configMap")
		return err
	}
	if err = json.Unmarshal(data, &clusterPowerModel); err != nil {
		klog.ErrorS(err, "Unable to unmarshal power model from configMap value")
		return err
	}
	return nil
}

func New(_ context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(4).InfoS("Peaks plugin Input config %+v\n", obj)

	args, ok := obj.(*config.PeaksArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type PeaksArgs, got %T", obj)
	}
	collector, err := trimaran.NewCollector(&config.TrimaranSpec{WatcherAddress: args.WatcherAddress})
	if err != nil {
		return nil, err
	}
	err = initNodePowerModels()
	if err != nil {
		return nil, err
	}
	pl := &Peaks{
		handle:    handle,
		collector: collector,
		args:      args,
	}
	return pl, nil
}

func (pl *Peaks) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	score := framework.MinNodeScore

	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	metrics, _ := pl.collector.GetNodeMetrics(nodeName)
	if metrics == nil {
		klog.ErrorS(nil, "Failed to get metrics for node; using minimum score", "nodeName", nodeName)
		return score, nil
	}

	var curPodCPUUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += PredictUtilisation(&container)
	}
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
	}

	var nodeCPUUtilPercent float64
	var cpuMetricFound bool
	for _, metric := range metrics {
		if metric.Type == watcher.CPU {
			if metric.Operator == watcher.Average || metric.Operator == watcher.Latest {
				nodeCPUUtilPercent = metric.Value
				cpuMetricFound = true
			}
		}
	}
	if !cpuMetricFound {
		klog.ErrorS(nil, "Cpu metric not found in node metrics for nodeName", nodeName)
		return score, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	var predictedCPUUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage)) / nodeCPUCapMillis
	}
	if predictedCPUUsage > 100 {
		return score, framework.NewStatus(framework.Success, "")
	} else {
		klog.InfoS("Node :", nodeName, ", Node cpu usage current :", nodeCPUUtilPercent, ", predicted :", predictedCPUUsage)
		jumpInPower := getPowerJumpForUtilisation(nodeCPUUtilPercent, predictedCPUUsage, getPowerModel(nodeName))
		var score int64 = int64(getMaxPower() / jumpInPower)
		klog.InfoS("Node: ", nodeName, " Jump in power: ", int64(jumpInPower*math.Pow(10, 15)), " score: ", score)

		return int64(jumpInPower * math.Pow(10, 15)), framework.NewStatus(framework.Success, "")
	}
}

func (pl *Peaks) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *Peaks) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	minCost, maxCost := getMinMaxScores(scores)
	if minCost == 0 && maxCost == 0 {
		return framework.NewStatus(framework.Success, "")
	}
	var normCost float64
	for i := range scores {
		if maxCost != minCost {
			normCost = float64(framework.MaxNodeScore) * float64(scores[i].Score-minCost) / float64(maxCost-minCost)
			scores[i].Score = framework.MaxNodeScore - int64(normCost)
		} else {
			normCost = float64(scores[i].Score - minCost)
			scores[i].Score = framework.MaxNodeScore - int64(normCost)
		}
	}
	return framework.NewStatus(framework.Success, "")
}

func getMinMaxScores(scores framework.NodeScoreList) (int64, int64) {
	var max int64 = math.MinInt64 // Set to min value
	var min int64 = math.MaxInt64 // Set to max value

	for _, nodeScore := range scores {
		if nodeScore.Score > max {
			max = nodeScore.Score
		}
		if nodeScore.Score < min {
			min = nodeScore.Score
		}
	}
	// return min and max scores
	return min, max
}

func PredictUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Cpu().MilliValue())))
	} else if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
		return container.Resources.Limits.Cpu().MilliValue()
	} else {
		return cfgv1.DefaultRequestsMilliCores
	}
}

func getPowerJumpForUtilisation(x, p float64, m PowerModel) float64 {
	return m.K1 * (math.Exp(m.K2*p) - math.Exp(m.K2*x))
}

func getMaxPower() float64 {
	if maxPower != 0.0 {
		return maxPower
	}
	for _, model := range clusterPowerModel {
		if maxPower < model.K0 {
			maxPower = model.K0
		}
	}
	return maxPower
}

func getPowerModel(nodeName string) PowerModel {
	power_model, ok := clusterPowerModel[nodeName]
	if ok {
		return power_model
	}
	return PowerModel{0, 0, 0}
}
