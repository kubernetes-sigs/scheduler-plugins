package resourcepolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	framework "k8s.io/kubernetes/pkg/scheduler/framework"
)

func TestFindAvailableUnitForNode(t *testing.T) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"region": "us-east-1",
			},
		},
	}

	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)

	state := &ResourcePolicyPreFilterState{
		nodeSelectos: []labels.Selector{
			labels.SelectorFromSet(labels.Set{"region": "us-east-1"}),
		},
		currentCount: []int{0},
		maxCount:     []int{1},
		resConsumption: []*framework.Resource{
			{MilliCPU: 500, Memory: 512},
		},
		maxConsumption: []*framework.Resource{
			{MilliCPU: 1000, Memory: 1024},
		},
		podRes: &framework.Resource{
			MilliCPU: 400, Memory: 256,
		},
	}

	// Test case 1
	idx, notValid := findAvailableUnitForNode(nodeInfo, state)
	assert.Equal(t, 0, idx)
	assert.Empty(t, notValid)

	// Test case 2: No matching node selector
	state.nodeSelectos[0] = labels.SelectorFromSet(labels.Set{"region": "us-west-1"})
	idx, notValid = findAvailableUnitForNode(nodeInfo, state)
	assert.Equal(t, -1, idx)
	assert.Empty(t, notValid)

	// Test case 3: Max count reached
	state.nodeSelectos[0] = labels.SelectorFromSet(labels.Set{"region": "us-east-1"})
	state.currentCount[0] = 1
	idx, notValid = findAvailableUnitForNode(nodeInfo, state)
	assert.Equal(t, -1, idx)
	assert.Len(t, notValid, 1)
	assert.Equal(t, "pod", notValid[0].res)

	// Test case 4: Resource consumption exceeds limit
	state.currentCount[0] = 0
	state.resConsumption[0] = &framework.Resource{
		MilliCPU: 900, Memory: 900,
	}
	idx, notValid = findAvailableUnitForNode(nodeInfo, state)
	assert.Equal(t, -1, idx)
	assert.Len(t, notValid, 1)
	assert.Equal(t, "cpu", notValid[0].res)

	// Test case 5: memory consumption exceeds limit
	state.currentCount[0] = 0
	state.resConsumption[0] = &framework.Resource{
		MilliCPU: 100, Memory: 900,
	}
	idx, notValid = findAvailableUnitForNode(nodeInfo, state)
	assert.Equal(t, -1, idx)
	assert.Len(t, notValid, 1)
	assert.Equal(t, "memory", notValid[0].res)
}
