/*
Copyright 2025 The Kubernetes Authors.

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

package mycrossnodepreemption

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// Name of the plugin used in the plugin registry and configurations.
	Name = "MyCrossNodePreemption"
)

// MyCrossNodePreemption is an improved PostFilter plugin with efficient preemption algorithms.
type MyCrossNodePreemption struct {
	fh        framework.Handle
	podLister corelisters.PodLister
	args      *MyCrossNodePreemptionArgs
	logger    klog.Logger
}

// MyCrossNodePreemptionArgs holds configuration for the plugin.
type MyCrossNodePreemptionArgs struct {
	MaxCandidates       int32                  `json:"maxCandidates,omitempty"`
	EnableBranchCutting bool                   `json:"enableBranchCutting,omitempty"`
	ConsiderPDBs        bool                   `json:"considerPDBs,omitempty"`
	ScoreWeights        ScoreWeights           `json:"scoreWeights,omitempty"`
	TimeoutDuration     time.Duration          `json:"timeoutDuration,omitempty"`
}

// ScoreWeights defines weights for different scoring criteria.
type ScoreWeights struct {
	Priority  float64 `json:"priority,omitempty"`
	Resources float64 `json:"resources,omitempty"`
	Topology  float64 `json:"topology,omitempty"`
}

// PreemptionCandidate represents a preemption candidate with enhanced metadata.
type PreemptionCandidate struct {
	NodeName    string
	Victims     []*v1.Pod
	Score       float64
	ResourceCost float64
	Priority    int32
}

// CandidateSelector implements intelligent candidate selection algorithms.
type CandidateSelector struct {
	logger klog.Logger
	args   *MyCrossNodePreemptionArgs
}

var _ framework.PostFilterPlugin = &MyCrossNodePreemption{}

// Name returns name of the plugin.
func (pl *MyCrossNodePreemption) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(ctx context.Context, args runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", Name)
	
	// Set default arguments
	pluginArgs := &MyCrossNodePreemptionArgs{
		MaxCandidates:       100,
		EnableBranchCutting: true,
		ConsiderPDBs:        true,
		ScoreWeights: ScoreWeights{
			Priority:  0.4,
			Resources: 0.3,
			Topology:  0.3,
		},
		TimeoutDuration: 5 * time.Second,
	}

	// Parse custom arguments if provided
	if args != nil {
		// For now, we'll skip argument parsing to avoid complexity
		// In a full implementation, we'd parse the args here
		logger.V(4).Info("Plugin arguments provided but parsing skipped for simplicity")
	}

	logger.Info("🚀 MyCrossNodePreemption plugin initialized", 
		"maxCandidates", pluginArgs.MaxCandidates,
		"branchCutting", pluginArgs.EnableBranchCutting,
		"considerPDBs", pluginArgs.ConsiderPDBs)

	return &MyCrossNodePreemption{
		fh:        fh,
		podLister: fh.SharedInformerFactory().Core().V1().Pods().Lister(),
		args:      pluginArgs,
		logger:    logger,
	}, nil
}

// PostFilter invoked at the postFilter extension point.
func (pl *MyCrossNodePreemption) PostFilter(ctx context.Context, state *framework.CycleState, 
	pod *v1.Pod, m framework.NodeToStatusMap) (*framework.PostFilterResult, *framework.Status) {
	
	startTime := time.Now()
	pl.logger.Info("🔍 Starting cross-node preemption analysis", 
		"pod", klog.KObj(pod), 
		"unschedulableNodes", "multiple")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, pl.args.TimeoutDuration)
	defer cancel()

	nodeName, status := pl.preempt(ctx, state, pod, m)
	
	duration := time.Since(startTime)
	pl.logger.Info("⏱️ Preemption analysis completed", 
		"pod", klog.KObj(pod),
		"duration", duration,
		"result", nodeName != "",
		"selectedNode", nodeName)

	if !status.IsSuccess() {
		return nil, status
	}

	// This happens when the pod is not eligible for preemption or no viable candidates found.
	if nodeName == "" {
		pl.logger.V(4).Info("❌ No viable preemption candidates found", "pod", klog.KObj(pod))
		return nil, framework.NewStatus(framework.Unschedulable)
	}

	pl.logger.Info("✅ Preemption candidate selected", 
		"pod", klog.KObj(pod), 
		"targetNode", nodeName)

	return &framework.PostFilterResult{}, framework.NewStatus(framework.Success)
}

// preempt implements the main preemption logic with optimizations.
func (pl *MyCrossNodePreemption) preempt(ctx context.Context, state *framework.CycleState, 
	pod *v1.Pod, m framework.NodeToStatusMap) (string, *framework.Status) {
	
	nodeLister := pl.fh.SnapshotSharedLister().NodeInfos()

	// Fetch the latest version of the pod
	latestPod, err := pl.podLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		pl.logger.Error(err, "Failed to get updated preemptor pod", "pod", klog.KObj(pod))
		return "", framework.AsStatus(err)
	}

	// 1) Ensure the preemptor is eligible to preempt other pods (simplified check)
	if latestPod.Spec.Priority == nil || *latestPod.Spec.Priority <= 0 {
		pl.logger.V(4).Info("Pod has no priority for preemption", "pod", klog.KObj(latestPod))
		return "", nil
	}

	// 2) Find preemption candidates using optimized algorithm
	candidates, status := pl.findOptimizedCandidates(ctx, state, latestPod, m, nodeLister)
	if !status.IsSuccess() {
		return "", status
	}

	if len(candidates) == 0 {
		pl.logger.V(4).Info("No preemption candidates found", "pod", klog.KObj(latestPod))
		return "", framework.NewStatus(framework.Unschedulable, "no preemption candidates found")
	}

	pl.logger.Info("📊 Found preemption candidates", 
		"pod", klog.KObj(latestPod),
		"candidateCount", len(candidates))

	// 3) Interaction with extenders omitted for simplicity

	// 4) Select the best candidate using multi-criteria optimization
	bestCandidate := pl.selectBestCandidate(candidates, latestPod)
	if bestCandidate == nil {
		pl.logger.V(4).Info("No suitable candidate after optimization", "pod", klog.KObj(latestPod))
		return "", nil
	}

	pl.logger.Info("🎯 Selected best preemption candidate", 
		"pod", klog.KObj(latestPod),
		"targetNode", bestCandidate.NodeName,
		"victimCount", len(bestCandidate.Victims),
		"score", bestCandidate.Score)

	// 5) Perform preparation work before nominating the selected candidate
	pl.logger.Info("🎯 Preparing candidate for nomination", 
		"pod", klog.KObj(latestPod),
		"targetNode", bestCandidate.NodeName,
		"victimCount", len(bestCandidate.Victims))

	// Delete victim pods to make room for the preemptor
	err = pl.deleteVictimPods(ctx, bestCandidate.Victims, latestPod)
	if err != nil {
		pl.logger.Error(err, "Failed to delete victim pods", 
			"pod", klog.KObj(latestPod),
			"targetNode", bestCandidate.NodeName)
		return "", framework.AsStatus(err)
	}

	pl.logger.Info("✅ Successfully deleted victim pods for preemption", 
		"pod", klog.KObj(latestPod),
		"targetNode", bestCandidate.NodeName,
		"deletedVictims", len(bestCandidate.Victims))

	return bestCandidate.NodeName, nil
}

// deleteVictimPods deletes the victim pods to make room for the preemptor
func (pl *MyCrossNodePreemption) deleteVictimPods(ctx context.Context, victims []*v1.Pod, preemptor *v1.Pod) error {
	if len(victims) == 0 {
		return nil
	}

	pl.logger.Info("🗑️ Starting victim pod deletion", 
		"preemptor", klog.KObj(preemptor),
		"victimCount", len(victims))

	clientset := pl.fh.ClientSet()
	var errs []error

	for _, victim := range victims {
		pl.logger.Info("🗑️ Deleting victim pod", 
			"preemptor", klog.KObj(preemptor),
			"victim", klog.KObj(victim),
			"victimNode", victim.Spec.NodeName,
			"victimPriority", getPodPriority(victim))

		// Delete the victim pod
		err := clientset.CoreV1().Pods(victim.Namespace).Delete(ctx, victim.Name, 
			metav1.DeleteOptions{
				GracePeriodSeconds: &[]int64{0}[0], // Immediate deletion for preemption
			})
		
		if err != nil {
			pl.logger.Error(err, "Failed to delete victim pod", 
				"preemptor", klog.KObj(preemptor),
				"victim", klog.KObj(victim))
			errs = append(errs, err)
		} else {
			pl.logger.Info("✅ Successfully deleted victim pod", 
				"preemptor", klog.KObj(preemptor),
				"victim", klog.KObj(victim))
		}
	}

	if len(errs) > 0 {
		pl.logger.Error(errs[0], "Some victim pods failed to delete", 
			"preemptor", klog.KObj(preemptor),
			"failedCount", len(errs),
			"totalVictims", len(victims))
		return errs[0] // Return the first error
	}

	pl.logger.Info("🎉 All victim pods deleted successfully", 
		"preemptor", klog.KObj(preemptor),
		"deletedCount", len(victims))

	return nil
}
