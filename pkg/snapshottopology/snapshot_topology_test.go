/*
Copyright 2026 The Kubernetes Authors.

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
	"testing"

	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapshotlisters "github.com/kubernetes-csi/external-snapshotter/client/v8/listers/volumesnapshot/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	regionKey = "topology.kubernetes.io/region"
	zoneKey   = "topology.kubernetes.io/zone"
)

// TestNodeMatchesTopology covers the Filter extension point's core decision:
// does a candidate node's labels satisfy the snapshot's NodeAffinity terms?
func TestNodeMatchesTopology(t *testing.T) {
	testcases := map[string]struct {
		nodeLabels map[string]string
		terms      []v1.TopologySelectorTerm
		want       bool
	}{
		// With zero terms there is nothing to OR over, so no node "matches".
		// The backward-compatible "empty topology => schedulable anywhere"
		// guarantee is enforced one level up in Filter, which short-circuits
		// to Success before calling nodeMatchesTopology when terms is empty.
		"no terms matches nothing (Filter short-circuits before this)": {
			nodeLabels: map[string]string{zoneKey: "us-west-2a"},
			terms:      nil,
			want:       false,
		},
		"node zone in the term values matches": {
			nodeLabels: map[string]string{zoneKey: "us-west-2a"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a", "us-west-2b"}},
				},
			}},
			want: true,
		},
		"node zone not in the term values does not match": {
			nodeLabels: map[string]string{zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a", "us-west-2b"}},
				},
			}},
			want: false,
		},
		"missing label key on node does not match": {
			nodeLabels: map[string]string{regionKey: "us-west-2"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: false,
		},
		"multiple expressions in a term are ANDed - all satisfied": {
			nodeLabels: map[string]string{regionKey: "us-west-2", zoneKey: "us-west-2a"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: regionKey, Values: []string{"us-west-2"}},
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: true,
		},
		"multiple expressions in a term are ANDed - one unsatisfied": {
			nodeLabels: map[string]string{regionKey: "us-west-2", zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{{
				MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: regionKey, Values: []string{"us-west-2"}},
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				},
			}},
			want: false,
		},
		"multiple terms are ORed - second term matches": {
			nodeLabels: map[string]string{zoneKey: "us-west-2c"},
			terms: []v1.TopologySelectorTerm{
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				}},
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2c"}},
				}},
			},
			want: true,
		},
		"multiple terms are ORed - none match": {
			nodeLabels: map[string]string{zoneKey: "us-west-2d"},
			terms: []v1.TopologySelectorTerm{
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2a"}},
				}},
				{MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
					{Key: zoneKey, Values: []string{"us-west-2c"}},
				}},
			},
			want: false,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			if got := nodeMatchesTopology(tc.nodeLabels, tc.terms); got != tc.want {
				t.Errorf("nodeMatchesTopology() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetPVCName verifies extraction of the PVC name from a pod volume,
// which gates whether PreFilter inspects a volume at all.
func TestGetPVCName(t *testing.T) {
	testcases := map[string]struct {
		vol  v1.Volume
		want string
	}{
		"pvc volume returns claim name": {
			vol: v1.Volume{
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
						ClaimName: "ebs-claim",
					},
				},
			},
			want: "ebs-claim",
		},
		"generic ephemeral volume returns pod-derived claim name": {
			vol: v1.Volume{
				Name: "data",
				VolumeSource: v1.VolumeSource{
					Ephemeral: &v1.EphemeralVolumeSource{},
				},
			},
			want: "app-data",
		},
		"non-pvc volume returns empty": {
			vol: v1.Volume{
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
			},
			want: "",
		},
	}

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}}
	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			if got := getPVCName(pod, tc.vol); got != tc.want {
				t.Errorf("getPVCName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// zoneSelectorTerm builds a single-key zone TopologySelectorTerm.
func zoneSelectorTerm(values ...string) v1.TopologySelectorTerm {
	return v1.TopologySelectorTerm{
		MatchLabelExpressions: []v1.TopologySelectorLabelRequirement{
			{Key: zoneKey, Values: values},
		},
	}
}

// TestFilterMultiPVC verifies that when a pod has multiple snapshot-sourced
// PVCs, the node must satisfy EVERY PVC's term set (AND across PVCs), not just
// one of them. Flattening all terms into a single OR-ed set would wrongly admit
// a node that satisfies only one PVC's snapshot topology.
func TestFilterMultiPVC(t *testing.T) {
	testcases := map[string]struct {
		termSets    [][]v1.TopologySelectorTerm
		nodeLabels  map[string]string
		wantSuccess bool
	}{
		"single PVC, node in zone -> schedulable": {
			termSets:    [][]v1.TopologySelectorTerm{{zoneSelectorTerm("us-west-2a")}},
			nodeLabels:  map[string]string{zoneKey: "us-west-2a"},
			wantSuccess: true,
		},
		"single PVC, node in other zone -> rejected": {
			termSets:    [][]v1.TopologySelectorTerm{{zoneSelectorTerm("us-west-2a")}},
			nodeLabels:  map[string]string{zoneKey: "us-west-2b"},
			wantSuccess: false,
		},
		"two PVCs in different zones, node matches only one -> rejected": {
			termSets: [][]v1.TopologySelectorTerm{
				{zoneSelectorTerm("us-west-2a")},
				{zoneSelectorTerm("us-west-2b")},
			},
			nodeLabels:  map[string]string{zoneKey: "us-west-2a"},
			wantSuccess: false,
		},
		"two PVCs in the same zone, node matches both -> schedulable": {
			termSets: [][]v1.TopologySelectorTerm{
				{zoneSelectorTerm("us-west-2a")},
				{zoneSelectorTerm("us-west-2a")},
			},
			nodeLabels:  map[string]string{zoneKey: "us-west-2a"},
			wantSuccess: true,
		},
		"one PVC allows two zones, other pins one; node in the shared zone -> schedulable": {
			termSets: [][]v1.TopologySelectorTerm{
				{zoneSelectorTerm("us-west-2a", "us-west-2b")},
				{zoneSelectorTerm("us-west-2b")},
			},
			nodeLabels:  map[string]string{zoneKey: "us-west-2b"},
			wantSuccess: true,
		},
	}

	pl := &SnapshotTopology{}
	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			state := framework.NewCycleState()
			state.Write(stateKey, &preFilterState{termSets: tc.termSets})

			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(&v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: tc.nodeLabels},
			})

			status := pl.Filter(context.Background(), state, &v1.Pod{}, nodeInfo)
			if got := status.IsSuccess(); got != tc.wantSuccess {
				t.Errorf("Filter() success = %v, want %v (status: %v)", got, tc.wantSuccess, status)
			}
		})
	}
}

// --- PreFilter test helpers ---

func newIndexer(objs ...interface{}) cache.Indexer {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, o := range objs {
		_ = idx.Add(o)
	}
	return idx
}

func newTestPlugin(pvcs []*v1.PersistentVolumeClaim, scs []*storagev1.StorageClass, snaps []*snapv1.VolumeSnapshot, contents []*snapv1.VolumeSnapshotContent) *SnapshotTopology {
	pvcObjs := make([]interface{}, len(pvcs))
	for i, o := range pvcs {
		pvcObjs[i] = o
	}
	scObjs := make([]interface{}, len(scs))
	for i, o := range scs {
		scObjs[i] = o
	}
	snapObjs := make([]interface{}, len(snaps))
	for i, o := range snaps {
		snapObjs[i] = o
	}
	contentObjs := make([]interface{}, len(contents))
	for i, o := range contents {
		contentObjs[i] = o
	}
	return &SnapshotTopology{
		pvcLister:             corelisters.NewPersistentVolumeClaimLister(newIndexer(pvcObjs...)),
		scLister:              storagelisters.NewStorageClassLister(newIndexer(scObjs...)),
		snapshotLister:        snapshotlisters.NewVolumeSnapshotLister(newIndexer(snapObjs...)),
		snapshotContentLister: snapshotlisters.NewVolumeSnapshotContentLister(newIndexer(contentObjs...)),
	}
}

func storageClass(name string, mode storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, VolumeBindingMode: &mode}
}

func snapshotPVC(name, scName, snapName, apiGroup string) *v1.PersistentVolumeClaim {
	sc := scName
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       v1.PersistentVolumeClaimSpec{StorageClassName: &sc},
	}
	if snapName != "" {
		grp := apiGroup
		pvc.Spec.DataSource = &v1.TypedLocalObjectReference{APIGroup: &grp, Kind: "VolumeSnapshot", Name: snapName}
	}
	return pvc
}

func boundSnapshot(name, contentName string) *snapv1.VolumeSnapshot {
	return &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Status:     &snapv1.VolumeSnapshotStatus{BoundVolumeSnapshotContentName: &contentName},
	}
}

func contentWithAffinity(name string, terms ...v1.TopologySelectorTerm) *snapv1.VolumeSnapshotContent {
	return &snapv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       snapv1.VolumeSnapshotContentSpec{NodeAffinity: terms},
	}
}

func podWithPVCs(claimNames ...string) *v1.Pod {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app"}}
	for i, cn := range claimNames {
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
			Name:         "v" + string(rune('a'+i)),
			VolumeSource: v1.VolumeSource{PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: cn}},
		})
	}
	return pod
}

const testPodUID = "pod-uid"

// podWithEphemeral returns a pod owning a single generic ephemeral volume named
// "eph" (its PVC name is therefore "app-eph").
func podWithEphemeral() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app", UID: testPodUID},
		Spec: v1.PodSpec{Volumes: []v1.Volume{{
			Name:         "eph",
			VolumeSource: v1.VolumeSource{Ephemeral: &v1.EphemeralVolumeSource{}},
		}}},
	}
}

// ownedByPod adds the controller owner reference that ephemeral.VolumeIsForPod
// requires to accept a PVC as belonging to the pod.
func ownedByPod(pvc *v1.PersistentVolumeClaim) *v1.PersistentVolumeClaim {
	controller := true
	pvc.OwnerReferences = []metav1.OwnerReference{{
		Kind: "Pod", Name: "app", UID: testPodUID, Controller: &controller,
	}}
	return pvc
}

// TestPreFilter exercises the PVC->snapshot->content resolution, WFFC gating,
// DataSource APIGroup/Kind check, and Skip-vs-Success outcome, asserting how
// many per-PVC term sets get cached.
func TestPreFilter(t *testing.T) {
	wffc := storageClass("wffc", storagev1.VolumeBindingWaitForFirstConsumer)
	immediate := storageClass("immediate", storagev1.VolumeBindingImmediate)
	terms := []v1.TopologySelectorTerm{zoneSelectorTerm("us-west-2a")}

	testcases := map[string]struct {
		pvcs        []*v1.PersistentVolumeClaim
		scs         []*storagev1.StorageClass
		snaps       []*snapv1.VolumeSnapshot
		contents    []*snapv1.VolumeSnapshotContent
		pod         *v1.Pod
		wantSkip    bool
		wantTermSet int
	}{
		"no volumes": {
			pod:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app"}},
			wantSkip: true,
		},
		"wffc pvc without datasource": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("c", "wffc", "", "")},
			scs:      []*storagev1.StorageClass{wffc},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"immediate binding is skipped": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("c", "immediate", "snap", snapshotAPIGroup)},
			scs:      []*storagev1.StorageClass{immediate},
			snaps:    []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents: []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"wrong api group is skipped": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("c", "wffc", "snap", "example.com")},
			scs:      []*storagev1.StorageClass{wffc},
			snaps:    []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents: []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"already-bound pvc is skipped": {
			pvcs: []*v1.PersistentVolumeClaim{func() *v1.PersistentVolumeClaim {
				p := snapshotPVC("c", "wffc", "snap", snapshotAPIGroup)
				p.Spec.VolumeName = "pv-1"
				return p
			}()},
			scs:      []*storagev1.StorageClass{wffc},
			snaps:    []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents: []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"snapshot not yet bound is skipped": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("c", "wffc", "snap", snapshotAPIGroup)},
			scs:      []*storagev1.StorageClass{wffc},
			snaps:    []*snapv1.VolumeSnapshot{{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "snap"}}},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"content with empty nodeAffinity is skipped": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("c", "wffc", "snap", snapshotAPIGroup)},
			scs:      []*storagev1.StorageClass{wffc},
			snaps:    []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents: []*snapv1.VolumeSnapshotContent{contentWithAffinity("content")},
			pod:      podWithPVCs("c"),
			wantSkip: true,
		},
		"single snapshot pvc yields one term set": {
			pvcs:        []*v1.PersistentVolumeClaim{snapshotPVC("c", "wffc", "snap", snapshotAPIGroup)},
			scs:         []*storagev1.StorageClass{wffc},
			snaps:       []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents:    []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:         podWithPVCs("c"),
			wantTermSet: 1,
		},
		"ephemeral volume owned by pod yields one term set": {
			pvcs:        []*v1.PersistentVolumeClaim{ownedByPod(snapshotPVC("app-eph", "wffc", "snap", snapshotAPIGroup))},
			scs:         []*storagev1.StorageClass{wffc},
			snaps:       []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents:    []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:         podWithEphemeral(),
			wantTermSet: 1,
		},
		"ephemeral volume not owned by pod is skipped": {
			pvcs:     []*v1.PersistentVolumeClaim{snapshotPVC("app-eph", "wffc", "snap", snapshotAPIGroup)},
			scs:      []*storagev1.StorageClass{wffc},
			snaps:    []*snapv1.VolumeSnapshot{boundSnapshot("snap", "content")},
			contents: []*snapv1.VolumeSnapshotContent{contentWithAffinity("content", terms...)},
			pod:      podWithEphemeral(),
			wantSkip: true,
		},
		"two snapshot pvcs yield two term sets": {
			pvcs: []*v1.PersistentVolumeClaim{
				snapshotPVC("c1", "wffc", "snap1", snapshotAPIGroup),
				snapshotPVC("c2", "wffc", "snap2", snapshotAPIGroup),
			},
			scs: []*storagev1.StorageClass{wffc},
			snaps: []*snapv1.VolumeSnapshot{
				boundSnapshot("snap1", "content1"),
				boundSnapshot("snap2", "content2"),
			},
			contents: []*snapv1.VolumeSnapshotContent{
				contentWithAffinity("content1", terms...),
				contentWithAffinity("content2", zoneSelectorTerm("us-west-2b")),
			},
			pod:         podWithPVCs("c1", "c2"),
			wantTermSet: 2,
		},
	}

	for name, tc := range testcases {
		t.Run(name, func(t *testing.T) {
			pl := newTestPlugin(tc.pvcs, tc.scs, tc.snaps, tc.contents)
			state := framework.NewCycleState()

			_, status := pl.PreFilter(context.Background(), state, tc.pod, nil)
			if !status.IsSuccess() && status.Code() != fwk.Skip {
				t.Fatalf("PreFilter() unexpected status: %v", status)
			}
			gotSkip := status.Code() == fwk.Skip
			if gotSkip != tc.wantSkip {
				t.Fatalf("PreFilter() skip = %v, want %v (status: %v)", gotSkip, tc.wantSkip, status)
			}
			if tc.wantSkip {
				return
			}
			s, err := state.Read(stateKey)
			if err != nil {
				t.Fatalf("expected state written, got err: %v", err)
			}
			if got := len(s.(*preFilterState).termSets); got != tc.wantTermSet {
				t.Errorf("cached term sets = %d, want %d", got, tc.wantTermSet)
			}
		})
	}
}
