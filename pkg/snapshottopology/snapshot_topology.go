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

package snapshottopology

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"

	snapshotclient "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	snapshotinformers "github.com/kubernetes-csi/external-snapshotter/client/v8/informers/externalversions"
	snapshotlisters "github.com/kubernetes-csi/external-snapshotter/client/v8/listers/volumesnapshot/v1"
)

const Name = "SnapshotTopology"
const snapshotAPIGroup = "snapshot.storage.k8s.io"
const cacheSyncTimeout = time.Minute

// SnapshotTopology is a scheduler plugin that filters nodes based on
// VolumeSnapshotContent.spec.nodeAffinity (KEP-5943).
type SnapshotTopology struct {
	pvcLister             corelisters.PersistentVolumeClaimLister
	scLister              storagelisters.StorageClassLister
	snapshotLister        snapshotlisters.VolumeSnapshotLister
	snapshotContentLister snapshotlisters.VolumeSnapshotContentLister
}

var _ fwk.PreFilterPlugin = &SnapshotTopology{}
var _ fwk.FilterPlugin = &SnapshotTopology{}
var _ fwk.EnqueueExtensions = &SnapshotTopology{}

func (pl *SnapshotTopology) Name() string {
	return Name
}

func New(ctx context.Context, _ runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Creating SnapshotTopology plugin")

	kubeFactory := handle.SharedInformerFactory()

	snapClient, err := snapshotclient.NewForConfig(handle.KubeConfig())
	if err != nil {
		return nil, fmt.Errorf("creating snapshot clientset: %w", err)
	}
	snapFactory := snapshotinformers.NewSharedInformerFactory(snapClient, 0)

	snapshotInformer := snapFactory.Snapshot().V1().VolumeSnapshots()
	snapshotContentInformer := snapFactory.Snapshot().V1().VolumeSnapshotContents()

	// Ensure informers are registered before starting.
	_ = snapshotInformer.Informer()
	_ = snapshotContentInformer.Informer()
	snapFactory.Start(ctx.Done())

	// if the snapshot.storage.k8s.io CRDs are not
	// installed show clear error.
	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()
	for informer, synced := range snapFactory.WaitForCacheSync(syncCtx.Done()) {
		if !synced {
			return nil, fmt.Errorf("informer %v failed to sync within %s (are the snapshot.storage.k8s.io CRDs installed?)", informer, cacheSyncTimeout)
		}
		logger.V(4).Info("SnapshotTopology: informer synced", "informer", informer)
	}

	plugin := &SnapshotTopology{
		pvcLister:             kubeFactory.Core().V1().PersistentVolumeClaims().Lister(),
		scLister:              kubeFactory.Storage().V1().StorageClasses().Lister(),
		snapshotLister:        snapshotInformer.Lister(),
		snapshotContentLister: snapshotContentInformer.Lister(),
	}
	return plugin, nil
}

// EventsToRegister returns events that could make a previously unschedulable
// pod schedulable: a new/updated Node with matching topology labels, or a
// VolumeSnapshot/VolumeSnapshotContent change that binds the snapshot or
// populates/updates its NodeAffinity.
func (pl *SnapshotTopology) EventsToRegister(_ context.Context) ([]fwk.ClusterEventWithHint, error) {
	snapshotGVK := fmt.Sprintf("volumesnapshots.v1.%s", snapshotAPIGroup)
	contentGVK := fmt.Sprintf("volumesnapshotcontents.v1.%s", snapshotAPIGroup)
	return []fwk.ClusterEventWithHint{
		{Event: fwk.ClusterEvent{Resource: fwk.Node, ActionType: fwk.Add | fwk.Update}},
		{Event: fwk.ClusterEvent{Resource: fwk.EventResource(snapshotGVK), ActionType: fwk.Add | fwk.Update}},
		{Event: fwk.ClusterEvent{Resource: fwk.EventResource(contentGVK), ActionType: fwk.Add | fwk.Update}},
	}, nil
}

// stateKey is used to store/retrieve precomputed topology data in CycleState.
const stateKey = Name

// preFilterState holds the topology constraints collected during PreFilter.
type preFilterState struct {
	// termSets holds one NodeAffinity term-set per snapshot-sourced PVC. Each
	// set is an independent constraint: a node must satisfy EVERY set (AND
	// across PVCs), while within a set the terms are ORed. Flattening all PVCs'
	// terms into one OR-ed list would wrongly accept a node that satisfies only
	// one PVC's snapshot topology. If empty, Filter is a no-op.
	termSets [][]v1.TopologySelectorTerm
}

func (s *preFilterState) Clone() fwk.StateData {
	return s
}

// PreFilter resolves each WaitForFirstConsumer, snapshot-sourced PVC to its
// VolumeSnapshotContent and caches the recorded NodeAffinity terms in CycleState.
// Snapshots whose content has no NodeAffinity contribute no constraint.
func (pl *SnapshotTopology) PreFilter(ctx context.Context, state fwk.CycleState, pod *v1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	logger := klog.FromContext(ctx)
	var termSets [][]v1.TopologySelectorTerm

	logger.V(5).Info("PreFilter evaluating pod", "pod", klog.KObj(pod), "volumes", len(pod.Spec.Volumes))

	for _, vol := range pod.Spec.Volumes {
		pvcName := getPVCName(pod, vol)
		if pvcName == "" {
			continue
		}

		pvc, err := pl.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
		if err != nil {
			logger.V(4).Info("PreFilter skipping PVC that could not be retrieved", "pvc", pvcName, "err", err)
			continue
		}

		if vol.Ephemeral != nil {
			if err := ephemeral.VolumeIsForPod(pod, pvc); err != nil {
				logger.V(4).Info("PreFilter skipping ephemeral PVC not owned by pod", "pvc", pvcName, "err", err)
				continue
			}
		}

		if pvc.Spec.VolumeName != "" {
			continue
		}

		if !pl.isWaitForFirstConsumer(pvc) {
			continue
		}
		ds := pvc.Spec.DataSource
		if ds == nil || ds.Kind != "VolumeSnapshot" || ds.APIGroup == nil || *ds.APIGroup != snapshotAPIGroup {
			continue
		}

		snapshotName := ds.Name
		logger.V(5).Info("PreFilter resolving snapshot for PVC", "pvc", pvcName, "snapshot", snapshotName)

		snapshot, err := pl.snapshotLister.VolumeSnapshots(pod.Namespace).Get(snapshotName)
		if err != nil {
			logger.V(4).Info("PreFilter skipping snapshot that could not be retrieved", "snapshot", snapshotName, "err", err)
			continue
		}

		if snapshot.Status == nil || snapshot.Status.BoundVolumeSnapshotContentName == nil {
			logger.V(4).Info("PreFilter: snapshot not yet bound to content", "snapshot", snapshotName)
			continue
		}

		contentName := *snapshot.Status.BoundVolumeSnapshotContentName
		content, err := pl.snapshotContentLister.Get(contentName)
		if err != nil {
			logger.V(4).Info("PreFilter skipping content that could not be retrieved", "content", contentName, "err", err)
			continue
		}

		if len(content.Spec.NodeAffinity) == 0 {
			logger.V(4).Info("PreFilter: content has no nodeAffinity, no constraint", "content", contentName)
			continue
		}

		logger.V(4).Info("PreFilter found snapshot topology", "content", contentName, "terms", len(content.Spec.NodeAffinity))
		// Keep each PVC's terms as their own set; they must all be satisfied.
		termSets = append(termSets, content.Spec.NodeAffinity)
	}

	logger.V(5).Info("PreFilter cached topology term sets", "pod", klog.KObj(pod), "sets", len(termSets))
	if len(termSets) == 0 {
		// No snapshot topology constraints apply to this pod; skip Filter.
		return nil, fwk.NewStatus(fwk.Skip)
	}
	state.Write(stateKey, &preFilterState{termSets: termSets})
	return nil, fwk.NewStatus(fwk.Success)
}

// PreFilterExtensions returns nil since we don't need AddPod/RemovePod.
func (pl *SnapshotTopology) PreFilterExtensions() fwk.PreFilterExtensions {
	return nil
}

// Filter checks whether the candidate node satisfies the cached snapshot
// topology constraints.
func (pl *SnapshotTopology) Filter(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	s, err := state.Read(stateKey)
	if err != nil {
		return fwk.NewStatus(fwk.Success)
	}

	pfs := s.(*preFilterState)
	if len(pfs.termSets) == 0 {
		return fwk.NewStatus(fwk.Success)
	}

	node := nodeInfo.Node()
	if node == nil {
		return fwk.AsStatus(fmt.Errorf("node not found"))
	}

	// Each set is one snapshot-sourced PVC's constraint; the node must satisfy
	// all of them (a volume restored from each snapshot must be provisionable
	// on this node).
	for _, terms := range pfs.termSets {
		if !nodeMatchesTopology(node.Labels, terms) {
			return fwk.NewStatus(fwk.UnschedulableAndUnresolvable,
				fmt.Sprintf("node %q does not satisfy snapshot topology constraints", node.Name))
		}
	}
	return fwk.NewStatus(fwk.Success)
}

// nodeMatchesTopology returns true if the node's labels satisfy at least one
// TopologySelectorTerm. Terms are OR'd; within a term, MatchLabelExpressions
// are AND'd (each expression's key must exist on the node and the node's value
// for that key must be in the expression's Values list).
func nodeMatchesTopology(nodeLabels map[string]string, terms []v1.TopologySelectorTerm) bool {
	for _, term := range terms {
		if termMatches(nodeLabels, term) {
			return true
		}
	}
	return false
}

func termMatches(nodeLabels map[string]string, term v1.TopologySelectorTerm) bool {
	for _, expr := range term.MatchLabelExpressions {
		nodeVal, exists := nodeLabels[expr.Key]
		if !exists {
			return false
		}
		if !valuesContain(expr.Values, nodeVal) {
			return false
		}
	}
	return true
}

func valuesContain(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// getPVCName returns the PVC name backing a Volume, resolving both a direct
// PersistentVolumeClaim reference and a generic ephemeral volume (whose PVC name
// is derived from the pod and volume names). Returns "" for any other source.
func getPVCName(pod *v1.Pod, vol v1.Volume) string {
	switch {
	case vol.PersistentVolumeClaim != nil:
		return vol.PersistentVolumeClaim.ClaimName
	case vol.Ephemeral != nil:
		return ephemeral.VolumeClaimName(pod, &vol)
	default:
		return ""
	}
}

// isWaitForFirstConsumer checks if the PVC's StorageClass uses
// WaitForFirstConsumer volume binding mode.
func (pl *SnapshotTopology) isWaitForFirstConsumer(pvc *v1.PersistentVolumeClaim) bool {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false
	}
	sc, err := pl.scLister.Get(*pvc.Spec.StorageClassName)
	if err != nil {
		return false
	}
	if sc.VolumeBindingMode == nil {
		return false
	}
	return *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
}
