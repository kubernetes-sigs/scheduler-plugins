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

/*
peaks package provides K8s scheduler plugin for best-fit variant of bin packing based on CPU utilization around a target load
It contains plugin for Score extension point.
*/

package peaks

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/paypal/load-watcher/pkg/watcher"
	v1 "k8s.io/api/core/v1"
	res "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"

	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	Name = "Peaks"
)

type Peaks struct {
	logger    klog.Logger
	handle    framework.Handle
	collector *trimaran.Collector
	args      *config.PeaksArgs
}

var _ framework.ScorePlugin = &Peaks{}

func (pl *Peaks) Name() string {
	return Name
}

func initNodePowerModels(powerModel map[string]config.PowerModel) error {
	fmt.Printf("args power model : %+v\n", powerModel)
	if len(powerModel) > 0 {
		return nil
	}
	data, err := os.ReadFile(os.Getenv("NODE_POWER_MODEL"))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&powerModel); err != nil {
		return err
	}
	return nil
}

func New(ctx context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)
	logger.V(4).Info("Peaks plugin Input config %+v", obj)

	args, ok := obj.(*config.PeaksArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type PeaksArgs, got %T", obj)
	}
	collector, err := trimaran.NewCollector(logger, &config.TrimaranSpec{WatcherAddress: args.WatcherAddress})
	if err != nil {
		return nil, err
	}

	err = initNodePowerModels(args.NodePowerModel)
	if err != nil {
		logger.Error(err, "Unable to create power model from the input configuration")
		return nil, err
	}
	pl := &Peaks{
		logger:    logger,
		handle:    handle,
		collector: collector,
		args:      args,
	}
	return pl, nil
}

func (pl *Peaks) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	logger := klog.FromContext(klog.NewContext(ctx, pl.logger)).WithValues("ExtensionPoint", "Score")
	score := framework.MinNodeScore

	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return score, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	metrics, _ := pl.collector.GetNodeMetrics(logger, nodeName)
	if metrics == nil {
		logger.Error(nil, "Failed to get metrics for node; using minimum score", "nodeName", nodeName)
		return score, nil
	}

	opts := resource.PodResourcesOptions{
		NonMissingContainerRequests: v1.ResourceList{
			v1.ResourceCPU: *res.NewMilliQuantity(
				schedutil.DefaultMilliCPURequest,
				res.DecimalSI,
			),
		},
	}

	reqs := resource.PodRequests(
		pod,
		opts,
	)

	quantity := reqs[v1.ResourceCPU]
	curPodCPUUsage := quantity.MilliValue()

	var nodeCPUUtilPercent float64
	var cpuMetricFound bool
	for _, metric := range metrics {
		if metric.Type == watcher.CPU {
			if metric.Operator == watcher.Average || metric.Operator == watcher.Latest {
				nodeCPUUtilPercent = metric.Value
				cpuMetricFound = true
				break
			}
		}
	}
	if !cpuMetricFound {
		logger.Error(nil, "Cpu metric not found in node metrics for nodeName", nodeName)
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
		logger.V(4).Info("Node :", nodeName, ", Node cpu usage current :", nodeCPUUtilPercent, ", predicted :", predictedCPUUsage)
		jumpInPower := getPowerJumpForUtilisation(nodeCPUUtilPercent, predictedCPUUsage, getPowerModel(nodeName, pl.args.NodePowerModel))
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

func getPowerJumpForUtilisation(x, p float64, m config.PowerModel) float64 {
	return m.K1 * (math.Exp(m.K2*p) - math.Exp(m.K2*x))
}

func getPowerModel(nodeName string, powerModelMap map[string]config.PowerModel) config.PowerModel {
	powerModel, ok := powerModelMap[nodeName]
	if ok {
		return powerModel
	}
	return config.PowerModel{0, 0, 0}
}
