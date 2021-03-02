package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paypal/load-watcher/pkg/watcher"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config/v1beta1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran/targetloadpacking"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestTargetNodePackingPlugin(t *testing.T) {
	registry := fwkruntime.Registry{targetloadpacking.Name: targetloadpacking.New}
	metrics := watcher.WatcherMetrics{
		Window: watcher.Window{},
		Data: watcher.Data{
			NodeMetricsMap: map[string]watcher.NodeMetrics{
				"node-1": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    10,
							Operator: watcher.Latest,
						},
					},
				},
				"node-2": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    60,
							Operator: watcher.Latest,
						},
					},
				},
				"node-3": {
					Metrics: []watcher.Metric{
						{
							Type:     watcher.CPU,
							Value:    0,
							Operator: watcher.Latest,
						},
					},
				},
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		bytes, err := json.Marshal(metrics)
		assert.Nil(t, err)
		resp.Write(bytes)
	}))

	defer server.Close()
	profile := schedapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedapi.Plugins{
			Score: &schedapi.PluginSet{
				Enabled: []schedapi.Plugin{
					{Name: targetloadpacking.Name},
				},
				Disabled: []schedapi.Plugin{
					{Name: "*"},
				},
			},
		},
		PluginConfig: []schedapi.PluginConfig{
			{
				Name: targetloadpacking.Name,
				Args: &config.TargetLoadPackingArgs{
					WatcherAddress:            server.URL,
					TargetUtilization:         v1beta1.DefaultTargetUtilizationPercent,
					DefaultRequestsMultiplier: v1beta1.DefaultRequestsMultiplier,
				},
			},
		},
	}

	testCtx := util.InitTestSchedulerWithOptions(
		t,
		testutils.InitTestMaster(t, "sched-trimaran", nil),
		true,
		scheduler.WithProfiles(profile),
		scheduler.WithFrameworkOutOfTreeRegistry(registry),
	)

	defer testutils.CleanupTest(t, testCtx)

	cs, ns := testCtx.ClientSet, testCtx.NS.Name

	var nodes []*v1.Node
	nodeNames := []string{"node-1", "node-2", "node-3"}
	for i := 0; i < len(nodeNames); i++ {
		node := st.MakeNode().Name(nodeNames[i]).Label("node", nodeNames[i]).Obj()
		node.Status.Allocatable = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}
		node.Status.Capacity = v1.ResourceList{
			v1.ResourcePods:   *resource.NewQuantity(32, resource.DecimalSI),
			v1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(256, resource.DecimalSI),
		}
		node, err := cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		assert.Nil(t, err)
		nodes = append(nodes, node)
	}

	var newPods []*v1.Pod
	podNames := []string{"pod-1", "pod-2"}
	containerCPU := []int64{300, 100}
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(imageutils.GetPauseImageName()).Obj()
		pod.Spec.Containers[0].Resources = v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(containerCPU[i], resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(50, resource.DecimalSI),
			},
		}
		newPods = append(newPods, pod)
	}

	for i := range newPods {
		t.Logf("Creating Pod %q", newPods[i].Name)
		_, err := cs.CoreV1().Pods(ns).Create(context.TODO(), newPods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", newPods[i].Name, err)
		}
	}

	expected := [2]string{nodeNames[0], nodeNames[0]}
	for i := range newPods {
		err := wait.Poll(1*time.Second, 10*time.Second, func() (bool, error) {
			return podScheduled(cs, newPods[i].Namespace, newPods[i].Name), nil
		})
		assert.Nil(t, err)

		pod, err := cs.CoreV1().Pods(ns).Get(context.TODO(), newPods[i].Name, metav1.GetOptions{})
		assert.Nil(t, err)
		assert.Equal(t, expected[i], pod.Spec.NodeName)
	}

}
