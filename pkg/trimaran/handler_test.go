package trimaran

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/util/wait"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	testClientSet "k8s.io/client-go/kubernetes/fake"

	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

type testSharedLister struct {
	nodes       []*v1.Node
	nodeInfos   []*framework.NodeInfo
	nodeInfoMap map[string]*framework.NodeInfo
}

func (f *testSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

func (f *testSharedLister) NodeInfos() framework.NodeInfoLister {
	return f
}

func (f *testSharedLister) List() ([]*framework.NodeInfo, error) {
	return f.nodeInfos, nil
}

func (f *testSharedLister) HavePodsWithAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) HavePodsWithRequiredAntiAffinityList() ([]*framework.NodeInfo, error) {
	return nil, nil
}

func (f *testSharedLister) Get(nodeName string) (*framework.NodeInfo, error) {
	return f.nodeInfoMap[nodeName], nil
}

func TestHandlerCacheCleanup(t *testing.T) {
	testNode := "node-1"
	pod1 := st.MakePod().Name("Pod-1").Obj()
	pod2 := st.MakePod().Name("Pod-2").Obj()
	pod3 := st.MakePod().Name("Pod-3").Obj()
	pod4 := st.MakePod().Name("Pod-4").Obj()

	tests := []struct {
		name              string
		podInfoList       []podInfo
		podToUpdate       string
		expectedCacheSize int
		expectedCachePods []string
	}{
		{
			name: "OnUpdate doesn't add unassigned pods",
			podInfoList: []podInfo{
				{Pod: pod1},
				{Pod: pod2},
				{Pod: pod3}},
			podToUpdate:       "Pod-4",
			expectedCacheSize: 1,
			expectedCachePods: []string{"Pod-4"},
		},
		{
			name: "cleanupCache doesn't delete newly added pods",
			podInfoList: []podInfo{
				{Pod: pod1},
				{Pod: pod2},
				{Pod: pod3},
				{Timestamp: time.Now(), Pod: pod4}},
			podToUpdate:       "Pod-5",
			expectedCacheSize: 2,
			expectedCachePods: []string{"Pod-4", "Pod-5"},
		},
		{
			name: "cleanupCache deletes old pods",
			podInfoList: []podInfo{
				{Timestamp: time.Now().Add(-5 * time.Minute), Pod: pod1},
				{Timestamp: time.Now().Add(-10 * time.Second), Pod: pod2},
				{Timestamp: time.Now().Add(-5 * time.Second), Pod: pod3},
			},
			expectedCacheSize: 2,
			expectedCachePods: []string{pod2.Name, pod3.Name},
		},
		{
			name:              "cleanupCache deletes empty node entry",
			podInfoList:       []podInfo{},
			expectedCacheSize: 0,
			expectedCachePods: []string{},
		},
		{
			name: "cleanupCache deletes old pods and node entry with empty cache",
			podInfoList: []podInfo{
				{Timestamp: time.Now().Add(-7 * time.Minute), Pod: pod1},
				{Timestamp: time.Now().Add(-6 * time.Minute), Pod: pod2},
				{Timestamp: time.Now().Add(-5 * time.Minute), Pod: pod3},
			},
			expectedCacheSize: 0,
			expectedCachePods: []string{},
		},
		{
			name: "cleanupCache of pods with timestamp values not in ascending order",
			podInfoList: []podInfo{
				{Timestamp: time.Now().Add(-6 * time.Minute), Pod: pod2},
				{Timestamp: time.Now().Add(-7 * time.Second), Pod: pod1},
				{Timestamp: time.Now().Add(-5 * time.Minute), Pod: pod3},
			},
			expectedCacheSize: 1,
			expectedCachePods: []string{pod1.Name},
		},
		{
			name: "cleanupCache keeps all pods",
			podInfoList: []podInfo{
				{Timestamp: time.Now(), Pod: pod1},
				{Timestamp: time.Now(), Pod: pod2},
				{Timestamp: time.Now(), Pod: pod3},
			},
			expectedCacheSize: 3,
			expectedCachePods: []string{pod1.Name, pod2.Name, pod3.Name},
		},
		{
			name: "cleanupCache keeps all pods",
			podInfoList: []podInfo{
				{Timestamp: time.Now(), Pod: pod1},
				{Timestamp: time.Now(), Pod: pod2},
				{Timestamp: time.Now(), Pod: pod3},
			},
			expectedCacheSize: 3,
			expectedCachePods: []string{pod1.Name, pod2.Name, pod3.Name},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New()
			p.ScheduledPodsCache[testNode] = append(p.ScheduledPodsCache[testNode], tt.podInfoList...)
			if strings.Compare(tt.name, "cleanupCache of pods with timestamp values not in ascending order") == 0 {
				// sort cache entries in ascending order of their timestamps
				cache := p.ScheduledPodsCache[testNode]
				sort.Slice(cache, func(i, j int) bool {
					return cache[i].Timestamp.Before(cache[j].Timestamp)
				})
				p.ScheduledPodsCache[testNode] = cache
			}
			if tt.podToUpdate != "" {
				pod := st.MakePod().Name(tt.podToUpdate).Obj()
				pod.Spec.NodeName = testNode
				oldPod := st.MakePod().Name(tt.podToUpdate).Obj()
				p.OnUpdate(oldPod, pod)
			}
			p.cleanupCache()
			if strings.Contains(tt.name, "cleanupCache deletes empty node entry") ||
				strings.Contains(tt.name, "cleanupCache deletes old pods and node entry with empty cache") {
				assert.Nil(t, p.ScheduledPodsCache[testNode])
				assert.Equal(t, tt.expectedCacheSize, len(p.ScheduledPodsCache[testNode]))
			} else {
				assert.NotNil(t, p.ScheduledPodsCache[testNode])
				assert.Equal(t, tt.expectedCacheSize, len(p.ScheduledPodsCache[testNode]))
				for i, v := range p.ScheduledPodsCache[testNode] {
					assert.Equal(t, tt.expectedCachePods[i], v.Pod.Name)
				}
			}
		})
	}
}

func TestHandlerCacheAddUpdateDelete(t *testing.T) {
	testNode := "node-1"
	podToUpdate := "Pod-1"
	expectedCacheSize := 1
	expectedCachePods := []string{podToUpdate}
	p := New()

	pod := st.MakePod().Name(podToUpdate).Obj()
	pod.Spec.NodeName = testNode
	p.OnAdd(pod, true)
	assert.NotNil(t, p.ScheduledPodsCache[testNode])
	assert.Equal(t, expectedCacheSize, len(p.ScheduledPodsCache[testNode]))
	assert.Equal(t, p.ScheduledPodsCache[testNode][0].Pod.Spec.NodeName, testNode)
	for i, v := range p.ScheduledPodsCache[testNode] {
		assert.Equal(t, expectedCachePods[i], v.Pod.Name)
	}
	fmt.Printf("Test OnAdd is success\n")

	newPod := st.MakePod().Name(podToUpdate).Obj()
	newPod.Spec.NodeName = ""
	p.OnUpdate(pod, newPod)
	assert.NotNil(t, p.ScheduledPodsCache[testNode])
	assert.Equal(t, expectedCacheSize, len(p.ScheduledPodsCache[testNode]))
	assert.Equal(t, p.ScheduledPodsCache[testNode][0].Pod.Spec.NodeName, testNode)
	for i, v := range p.ScheduledPodsCache[testNode] {
		assert.Equal(t, expectedCachePods[i], v.Pod.Name)
	}
	fmt.Printf("Test OnUpdate is success\n")

	p.OnDelete(newPod)
	assert.Nil(t, p.ScheduledPodsCache[newPod.Spec.NodeName])
	fmt.Printf("Test OnDelete success with no pod cache for given node name\n")

	p.OnDelete(pod)
	expectedCacheSize = 0
	expectedCachePods = []string{}
	assert.Nil(t, p.ScheduledPodsCache[testNode])
	assert.Equal(t, expectedCacheSize, len(p.ScheduledPodsCache[testNode]))
	fmt.Printf("Test OnDelete success with pod cache cleared for given node name\n")
}

func TestHandlerAddToHandle(t *testing.T) {

	registeredPlugins := []st.RegisterPluginFunc{
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
	}
	cs := testClientSet.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(cs, 0)
	snapshot := &testSharedLister{}
	fh, err := st.NewFramework(registeredPlugins, "default-scheduler", wait.NeverStop, runtime.WithClientSet(cs),
		runtime.WithInformerFactory(informerFactory), runtime.WithSnapshotSharedLister(snapshot))

	assert.Nil(t, err)
	p := New()
	p.AddToHandle(fh)
	assert.NotNil(t, p)
	fmt.Printf("Test AddToHandle success\n")
}
