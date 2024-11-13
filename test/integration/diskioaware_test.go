package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	diskioapi "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio"
	diskiov1alpha1 "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	scheconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware"
	"sigs.k8s.io/scheduler-plugins/test/util"
)

func TestDiskIOAwarePlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := clientset.NewForConfigOrDie(globalKubeConfig)
	extClient := versioned.NewForConfigOrDie(globalKubeConfig)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(diskiov1alpha1.AddToScheme(scheme))

	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == diskioapi.GroupName {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	// Create a test server to serve a module plugin
	p, err := getModelPluginBytes("foo")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(p)
	}))
	defer ts.Close()

	createNamespace(t, testCtx, common.CRNameSpace)
	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	// compose disk model config
	cp, err := composeDiskModelConfig(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(cp)

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles[0].Plugins.Filter.Enabled = append(cfg.Profiles[0].Plugins.Filter.Enabled, schedapi.Plugin{Name: diskioaware.Name})
	cfg.Profiles[0].Plugins.Score.Enabled = append(cfg.Profiles[0].Plugins.Score.Enabled, schedapi.Plugin{Name: diskioaware.Name})
	cfg.Profiles[0].Plugins.Reserve.Enabled = append(cfg.Profiles[0].Plugins.Reserve.Enabled, schedapi.Plugin{Name: diskioaware.Name})

	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: diskioaware.Name,
		Args: &scheconfig.DiskIOArgs{
			ScoreStrategy:     string(scheconfig.LeastAllocated),
			DiskIOModelConfig: cp,
			NSWhiteList:       []string{"kube-system"},
		},
	})

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{diskioaware.Name: diskioaware.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	t.Log("Init scheduler success")
	defer cleanupTest(t, testCtx)

	// Create nodes
	resList := map[v1.ResourceName]string{
		v1.ResourceCPU:    "64",
		v1.ResourceMemory: "128Gi",
	}
	nodes := []string{"fake-node-1", "fake-node-2"}
	for _, nodeName := range nodes {
		newNode := st.MakeNode().Name(nodeName).Label("node", nodeName).Capacity(resList).Obj()
		n, err := cs.CoreV1().Nodes().Create(testCtx.Ctx, newNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}

		t.Logf("Node %s created: %s", nodeName, formatObject(n))
	}

	_, err = cs.CoreV1().Nodes().List(testCtx.Ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("can't list nodes: %s", err.Error())
	}

	nodeDiskInfo1 := MakeNodeDiskDevice(common.CRNameSpace, fmt.Sprintf("%s-%s", nodes[0], common.NodeDiskDeviceCRSuffix)).Spec(
		diskiov1alpha1.NodeDiskDeviceSpec{
			NodeName: nodes[0],
			Devices: map[string]diskiov1alpha1.DiskDevice{
				"fakeDeviceId": {
					Name:   common.FakeDeviceID,
					Vendor: "Intel",
					Model:  "P4510",
					Type:   string(common.EmptyDir),
					Capacity: diskiov1alpha1.IOBandwidth{
						Total: resource.MustParse("2000Mi"),
						Read:  resource.MustParse("1000Mi"),
						Write: resource.MustParse("1000Mi"),
					},
				},
			},
		}).Status(diskiov1alpha1.NodeDiskDeviceStatus{}).Obj()

	nodeDiskInfo2 := MakeNodeDiskDevice(common.CRNameSpace, fmt.Sprintf("%s-%s", nodes[1], common.NodeDiskDeviceCRSuffix)).Spec(
		diskiov1alpha1.NodeDiskDeviceSpec{
			NodeName: nodes[1],
			Devices: map[string]diskiov1alpha1.DiskDevice{
				"fakeDeviceId": {
					Name:   common.FakeDeviceID,
					Vendor: "Intel",
					Model:  "P4510",
					Type:   string(common.EmptyDir),
					Capacity: diskiov1alpha1.IOBandwidth{
						Total: resource.MustParse("200Mi"),
						Read:  resource.MustParse("100Mi"),
						Write: resource.MustParse("100Mi"),
					},
				},
			},
		}).Status(diskiov1alpha1.NodeDiskDeviceStatus{}).Obj()

	pause := imageutils.GetPauseImageName()
	for _, tt := range []struct {
		name             string
		pods             []*v1.Pod
		nodeDiskIOInfoes []*diskiov1alpha1.NodeDiskDevice
		expectedPods     []string
		expectedNodes    []string
	}{
		{
			name: "Case1: enough BW to schedule two guaranteed pods on node which support diskio",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"30Mi\", \"wbps\": \"20Mi\", \"blocksize\": \"4k\"}"}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"30Mi\", \"wbps\": \"20Mi\", \"blocksize\": \"4k\"}"}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{
				nodeDiskInfo1,
				nodeDiskInfo2,
			},
			expectedPods: []string{
				"pod1",
				"pod2",
			},
			expectedNodes: []string{"fake-node-1"},
		},
		{
			name: "Case2: enough BW to schedule two best-effort pods on nodes which do not support diskio",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{
				nodeDiskInfo1,
			},
			expectedPods: []string{
				"pod1",
				"pod2",
			},
			expectedNodes: []string{"fake-node-2"},
		},
		{
			name: "Case3: enough BW to schedule two best effort pods on nodes which support diskio",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{
				nodeDiskInfo1,
				nodeDiskInfo2,
			},
			expectedPods: []string{
				"pod1",
				"pod2",
			},
			expectedNodes: []string{"fake-node-1", "fake-node-2"},
		},
		{
			name: "Case4: not enough BW to schedule two guaranteed pods on a node which supports diskio",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"2000Mi\", \"wbps\": \"2000Mi\", \"blocksize\": \"4k\"}"}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"3000Mi\", \"wbps\": \"2000Mi\", \"blocksize\": \"4k\"}"}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{
				nodeDiskInfo1,
				nodeDiskInfo2,
			},
			expectedPods:  []string{},
			expectedNodes: []string{},
		},
		{
			name: "Case5: schedule two guaranteed pods, one pod succeeds and one pod fails",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"1000Mi\", \"wbps\": \"1000Mi\", \"blocksize\": \"512\"}"}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"3000Mi\", \"wbps\": \"2000Mi\", \"blocksize\": \"4k\"}"}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{
				nodeDiskInfo1,
				nodeDiskInfo2,
			},
			expectedPods:  []string{"pod1"},
			expectedNodes: []string{"fake-node-1"},
		},
		{
			name: "Case6: schedule two guaranteed pods, but no node's disk io info is registered",
			pods: []*v1.Pod{
				st.MakePod().Namespace(ns).Name("pod1").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"10Mi\", \"wbps\": \"10Mi\", \"blocksize\": \"512\"}"}).Container(pause).Obj(),
				st.MakePod().Namespace(ns).Name("pod2").Annotations(map[string]string{
					common.DiskIOAnnotation: "{\"rbps\": \"30Mi\", \"wbps\": \"20Mi\", \"blocksize\": \"512\"}"}).Container(pause).Obj(),
			},
			nodeDiskIOInfoes: []*diskiov1alpha1.NodeDiskDevice{},
			expectedPods:     []string{},
			expectedNodes:    []string{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer cleanupNodeDiskDevices(testCtx.Ctx, extClient, tt.nodeDiskIOInfoes)
			defer cleanupNodeDiskIOStats(testCtx.Ctx, extClient,
				common.CRNameSpace,
				[]string{fmt.Sprintf("%s-%s", nodes[0], common.NodeDiskIOInfoCRSuffix),
					fmt.Sprintf("%s-%s", nodes[1],
						common.NodeDiskIOInfoCRSuffix)})
			defer cleanupPods(t, testCtx, tt.pods)

			t.Log("Step 1 - create the NodeDiskDevice CR...")
			// create NodeDiskDevice CRs
			if err := createNodeDiskDevices(testCtx.Ctx, extClient, tt.nodeDiskIOInfoes); err != nil {
				t.Fatal(err)
			}

			t.Log("Step 2 - create pods...")
			// create pods
			for _, pod := range tt.pods {
				_, err := cs.CoreV1().Pods(pod.Namespace).Create(testCtx.Ctx, pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create Pod %q: %v", pod.Name, err)
				}
				time.Sleep(2 * time.Second)
			}

			t.Logf("Step 3 -  Wait for expected status...")
			for _, p := range tt.pods {
				err := wait.PollUntilContextTimeout(testCtx.Ctx, 1*time.Second, 20*time.Second, false, func(_ context.Context) (bool, error) {
					return podScheduled(cs, ns, p.Name), nil
				})
				if err == nil {
					if !slices.Contains(tt.expectedPods, p.Name) {
						t.Errorf("pod %q should not be scheduled", p.Name)
						continue
					}
				} else {
					if slices.Contains(tt.expectedPods, p.Name) {
						t.Errorf("pod %q to be scheduled, error: %v", p.Name, err)
					} else {
						continue
					}
				}
				t.Logf("Pod %v scheduled to %v", p.Name, p.Spec.NodeName)

				nodeName, err := getNodeName(testCtx.Ctx, cs, ns, p.Name)
				if err != nil {
					t.Log(err)
				}
				if slices.Contains(tt.expectedNodes, nodeName) {
					t.Logf("Pod %q is on the expected node %s.", p.Name, nodeName)
				} else {
					t.Errorf("Pod %s is expected on node %s, but found on node %s",
						p.Name, tt.expectedNodes, nodeName)
				}
			}
			t.Logf("Case %v finished", tt.name)
		})
	}
}
