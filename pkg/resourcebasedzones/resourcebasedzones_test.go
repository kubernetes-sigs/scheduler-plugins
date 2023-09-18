package resourcebasedzones

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	fakepgclientset "sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned/fake"
	"sigs.k8s.io/scheduler-plugins/pkg/store"

	// st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutil "sigs.k8s.io/scheduler-plugins/test/util"
)

func TestLess(t *testing.T) {
	now := time.Now()
	times := make([]time.Time, 0)
	for _, d := range []time.Duration{0, 1, 2, 3, -2, -1} {
		times = append(times, now.Add(d*time.Second))
	}
	// st.NewFakeFilterPlugin()
	cs := fakepgclientset.NewSimpleClientset()

	for _, pgInfo := range []struct {
		createTime time.Time
		pgNme      string
		ns         string
		minMember  int32
	}{
		{
			createTime: times[2],
			pgNme:      "pg1",
			ns:         "namespace1",
			minMember:  1,
		},
		{
			createTime: times[3],
			pgNme:      "pg2",
			ns:         "namespace2",
			minMember:  2,
		},
		{
			createTime: times[4],
			pgNme:      "pg3",
			ns:         "namespace2",
			minMember:  16,
		},
		{
			createTime: times[5],
			pgNme:      "pg4",
			ns:         "namespace2",
			minMember:  32,
		},
	} {
		pg := testutil.MakePG(pgInfo.pgNme, pgInfo.ns, pgInfo.minMember, &pgInfo.createTime, nil)
		cs.Tracker().Add(pg)
	}
}

func TestFilter(t *testing.T) {
	now := time.Now()
	times := make([]time.Time, 0)
	for _, d := range []time.Duration{0, 1, 2, 3, -2, -1} {
		times = append(times, now.Add(d*time.Second))
	}
	// st.NewFakeFilterPlugin()
	cs := fakepgclientset.NewSimpleClientset()

	for _, pgInfo := range []struct {
		createTime time.Time
		pgNme      string
		ns         string
		minMember  int32
	}{
		{
			createTime: times[2],
			pgNme:      "pg1",
			ns:         "namespace1",
			minMember:  1,
		},
		{
			createTime: times[3],
			pgNme:      "pg2",
			ns:         "namespace2",
			minMember:  2,
		},
		{
			createTime: times[4],
			pgNme:      "pg3",
			ns:         "namespace2",
			minMember:  16,
		},
		{
			createTime: times[5],
			pgNme:      "pg4",
			ns:         "namespace2",
			minMember:  32,
		},
	} {
		pg := testutil.MakePG(pgInfo.pgNme, pgInfo.ns, pgInfo.minMember, &pgInfo.createTime, nil)
		cs.Tracker().Add(pg)
	}

	// case for node selector not in a free zone
	// case for label selector for zone
	// case for mpi-launcher
	// case for regular job - all free
	// case for regular job - only 1/3 free zones
	// case for high priority - 1 zone free, 2 zones occupied with no-prioririty - free zone should be chosen
}

func TestScore(t *testing.T) {
	plugin := ZoneResource{
		PriorityZones: []string{"a", "b", "c", "d", "e", "f", "g"},
	}

	tests := []struct {
		name     string
		zone     string
		expScore int64
	}{
		{name: "Zone A should return 100", zone: "a", expScore: 100},
		{name: "Zone B should return 90", zone: "b", expScore: 90},
		{name: "Zone C should return 80", zone: "c", expScore: 80},
		{name: "Zone D should return 70", zone: "d", expScore: 70},
		{name: "Zone Not found should return 0", zone: "idc", expScore: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := plugin.score(tt.zone)
			if res != tt.expScore {
				t.Errorf("expected score %d for zone %s, got %d",
					tt.expScore, tt.zone, res)
			}
		})
	}
}

func TestIsReady(t *testing.T) {
	tests := []struct {
		name       string
		conditions []corev1.NodeCondition
		expResult  bool
	}{
		{
			name: "Node is ready - condition true",
			conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
			expResult: true,
		},
		{
			name: "Node not ready - condition false",
			conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			},
			expResult: false,
		},
		{
			name: "Node not ready - condition unknown",
			conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionUnknown,
				},
			},
			expResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := isReady(tt.conditions)
			if res != tt.expResult {
				t.Errorf("expected node ready %t, got %t", tt.expResult, res)
			}
		})
	}
}

func TestIsMaster(t *testing.T) {
	tests := []struct {
		name      string
		labels    map[string]string
		expResult bool
	}{
		{
			name: "Node is master",
			labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
			expResult: true,
		},
		{
			name:      "Node is not master",
			labels:    map[string]string{},
			expResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := isMaster(tt.labels)
			if res != tt.expResult {
				t.Errorf("expected master to be %t, got %t", tt.expResult, res)
			}
		})
	}
}

func TestIsService(t *testing.T) {
	tests := []struct {
		name      string
		labels    map[string]string
		expResult bool
	}{
		{
			name: "Node is services node",
			labels: map[string]string{
				"habana.ai/services": "true",
			},
			expResult: true,
		},
		{
			name:      "Node is not services node - no labels",
			labels:    map[string]string{},
			expResult: false,
		},
		{
			name: "Node is not services node",
			labels: map[string]string{
				"some-label": "some-value",
			},
			expResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := isService(tt.labels)
			if res != tt.expResult {
				t.Errorf("expected node to be %t, got %t", tt.expResult, res)
			}
		})
	}
}

func TestZoneFreeAsc(t *testing.T) {
	zones := map[string]zoneInfo{
		"a": {
			freeCards: 8,
			ToPreempt: 8,
		},
		"b": {
			freeCards: 2,
			ToPreempt: 0,
		},
		"c": {
			freeCards: 30,
			ToPreempt: 10,
		},
	}

	ordered := zoneFreeAsc(zones)

	expOrder := []string{"b", "a", "c"}

	if !reflect.DeepEqual(ordered, expOrder) {
		t.Errorf("expected order %v, got %v", expOrder, ordered)
	}

}

func TestZoneLessPreempt(t *testing.T) {
	zones := map[string]zoneInfo{
		"a": {
			freeCards: 8,
			ToPreempt: 8,
		},
		"b": {
			freeCards: 2,
			ToPreempt: 0,
		},
		"c": {
			freeCards: 30,
			ToPreempt: 10,
		},
		"d": {
			freeCards: 30,
			ToPreempt: 3,
		},
	}

	ordered := zoneLessPreempt(zones)

	expOrder := []string{"b", "d", "a", "c"}

	if !reflect.DeepEqual(ordered, expOrder) {
		t.Errorf("expected order %v, got %v", expOrder, ordered)
	}
}

func TestShouldClean(t *testing.T) {
	timeNow = func() time.Time {
		return time.Now().Add(-5 * time.Minute)
	}
	now := timeNow()
	tests := []struct {
		name      string
		pgName    string
		ts        time.Time
		expResult bool
	}{
		{
			name:      "Under 5 minutes should be false",
			pgName:    "pg1",
			ts:        now.Add(1 * time.Minute),
			expResult: false,
		},
		{
			name:      "Exactly 5 minutes should be false",
			pgName:    "pg2",
			ts:        now.Add(5 * time.Minute),
			expResult: false,
		},
		{
			name:      "Older than 5 minutes should be true",
			pgName:    "pg3",
			ts:        now.Add(8 * time.Minute),
			expResult: true,
		},
	}

	zr := ZoneResource{
		cleanupInterval: 5,
		store:           store.NewInMemoryStore(),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Add pg to store
			zr.store.Add(tt.pgName, "a", tt.ts)

			res := zr.shouldClean(tt.pgName)
			if res != tt.expResult {
				t.Errorf("expected %t, got %t", tt.expResult, res)
			}
		})
	}
}
