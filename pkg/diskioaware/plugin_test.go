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

package diskioaware

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	v1 "k8s.io/api/core/v1"
	rs "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	rt "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/diskio"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/normalizer"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/resource"
)

var s map[string]v1alpha1.DiskDevice = map[string]v1alpha1.DiskDevice{
	"sda": {
		Name:   "sda",
		Vendor: "v1",
		Model:  "m1",
		Type:   "emptyDir",
		Capacity: v1alpha1.IOBandwidth{
			Total: rs.MustParse("100Mi"),
			Read:  rs.MustParse("50Mi"),
			Write: rs.MustParse("50Mi"),
		},
	},
}

func makeNode(cs kubernetes.Interface, node *v1.Node) (*v1.Node, error) {
	return cs.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
}

func makeNodes(cs kubernetes.Interface, prefix string, numNodes int) ([]*v1.Node, error) {
	nodes := make([]*v1.Node, numNodes)
	for i := 0; i < numNodes; i++ {
		nodeName := fmt.Sprintf("%v-%d", prefix, i)
		n := &v1.Node{}
		n.SetName(nodeName)
		node, err := makeNode(cs, n)
		if err != nil {
			return nodes[:], err
		}
		nodes[i] = node
	}
	return nodes[:], nil
}

func TestNew(t *testing.T) {
	type fields struct {
		args runtime.Object // plugin args
		opts []rt.Option    // framework handle options
	}
	client := fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	opts := []rt.Option{
		rt.WithClientSet(client),
		rt.WithInformerFactory(informerFactory),
		rt.WithKubeConfig(&rest.Config{}),
	}

	tests := []struct {
		name    string
		fields  *fields
		wantErr bool
	}{
		{
			name: "nil plugin args",
			fields: &fields{
				args: nil,
				opts: opts,
			},
			wantErr: true,
		},
		{
			name: "Unknown score strategy",
			fields: &fields{
				args: &config.DiskIOArgs{
					DiskIOModelConfig: "/etc",
					ScoreStrategy:     "ABC",
				},
				opts: opts,
			},
			wantErr: true,
		},
		{
			name: "Unknown DiskIOModelConfig",
			fields: &fields{
				args: &config.DiskIOArgs{
					DiskIOModelConfig: "/etc/ABC",
					ScoreStrategy:     "MostAllocated",
				},
				opts: opts,
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fh, err := rt.NewFramework(ctx, nil, nil, tt.fields.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(ctx, tt.fields.args, fh); (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDiskIO_Filter(t *testing.T) {

	type args struct {
		ctx      context.Context
		state    *framework.CycleState
		pod      *v1.Pod
		nodeInfo *framework.NodeInfo
	}
	c := fake.NewSimpleClientset()
	rh := diskio.New()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	rh.(resource.CacheHandle).AddCacheNodeInfo("node1", s)
	resource.IoiContext = &resource.ResourceIOContext{
		NsWhiteList: []string{"ns1"},
	}
	nodeIOI := st.MakeNode().Name("node1").Obj()
	niIOI := framework.NewNodeInfo()
	niIOI.SetNode(nodeIOI)

	nodeIOI2 := st.MakeNode().Name("node2").Obj()
	niIOI2 := framework.NewNodeInfo()
	niIOI2.SetNode(nodeIOI2)

	nm := normalizer.NewNormalizerManager(baseModelDir, maxRetries)
	err = nm.LoadPlugin(context.TODO(), normalizer.PlConfig{
		Vendor: "v1",
		Model:  "m1",
		URL:    "file://./sampleplugin/foo/foo.so",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args args
		want *framework.Status
	}{
		{
			name: "Pod namespace in whitelist",
			args: args{
				ctx:   context.TODO(),
				state: nil,
				pod:   st.MakePod().Name("pod1").Namespace("ns1").Obj(),
			},
			want: framework.NewStatus(framework.Success),
		},
		{
			name: "Success to schedule Best Effort pod",
			args: args{
				ctx:      context.TODO(),
				state:    framework.NewCycleState(),
				pod:      st.MakePod().Name("pod1").Namespace("default").Obj(),
				nodeInfo: niIOI,
			},
			want: framework.NewStatus(framework.Success),
		},
		{
			name: "Success to schedule Best Effort pod (node not registed)",
			args: args{
				ctx:      context.TODO(),
				state:    framework.NewCycleState(),
				pod:      st.MakePod().Name("pod1").Namespace("default").Obj(),
				nodeInfo: niIOI2,
			},
			want: framework.NewStatus(framework.Success),
		},
		{
			name: "Success to schedule Garanteed pod",
			args: args{
				ctx:   context.TODO(),
				state: framework.NewCycleState(),
				pod: st.MakePod().Name("pod1").Namespace("default").Annotations(map[string]string{
					"blockio.kubernetes.io/resources": "{\"rbps\": \"30Mi\", \"wbps\": \"20Mi\", \"blocksize\": \"4k\"}",
				}).Obj(),
				nodeInfo: niIOI,
			},
			want: framework.NewStatus(framework.Success),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := &DiskIO{
				rh:         rh,
				scorer:     nil,
				nm:         nm,
				nodeLister: nil,
			}
			if got := ps.Filter(tt.args.ctx, tt.args.state, tt.args.pod, tt.args.nodeInfo); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DiskIO.Filter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiskIO_Score(t *testing.T) {
	type args struct {
		ctx      context.Context
		state    *framework.CycleState
		pod      *v1.Pod
		nodeName string
	}
	c := fake.NewSimpleClientset()
	rh := diskio.New()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	rh.(resource.CacheHandle).AddCacheNodeInfo("node1", s)

	scorer, err := getScorer("MostAllocated")
	if err != nil {
		t.Fatal(err)
	}
	resource.IoiContext = &resource.ResourceIOContext{
		NsWhiteList: []string{"ns1"},
	}
	cs := framework.NewCycleState()

	cs.Write(framework.StateKey(stateKeyPrefix+"node1"), &stateData{request: v1alpha1.IOBandwidth{
		Total: rs.MustParse("50Mi"),
		Read:  rs.MustParse("25Mi"),
		Write: rs.MustParse("25Mi"),
	}, nodeSupportIOI: true})

	cs2 := framework.NewCycleState()

	cs2.Write(framework.StateKey(stateKeyPrefix+"node1"), &stateData{nodeSupportIOI: false})
	tests := []struct {
		name  string
		args  args
		want  int64
		want1 *framework.Status
	}{
		{
			name: "Pod namespace in whitelist",
			args: args{
				pod: st.MakePod().Name("pod1").Namespace("ns1").Obj(),
			},
			want:  100,
			want1: framework.NewStatus(framework.Success),
		},
		{
			name: "Pod namespace not in whitelist (Guaranteed)",
			args: args{
				pod:      st.MakePod().Name("pod2").Namespace("default").Obj(),
				state:    cs,
				ctx:      context.TODO(),
				nodeName: "node1",
			},
			want:  50,
			want1: framework.NewStatus(framework.Success),
		},
		{
			name: "Pod namespace not in whitelist (Best Effort)",
			args: args{
				pod:      st.MakePod().Name("pod2").Namespace("default").Obj(),
				state:    cs2,
				ctx:      context.TODO(),
				nodeName: "node1",
			},
			want:  100,
			want1: framework.NewStatus(framework.Success),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := &DiskIO{
				rh:     rh,
				scorer: scorer,
			}
			got, got1 := ps.Score(tt.args.ctx, tt.args.state, tt.args.pod, tt.args.nodeName)
			if got != tt.want {
				t.Errorf("DiskIO.Score() got = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(got1, tt.want1) {
				t.Errorf("DiskIO.Score() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

func TestDiskIO_Reserve(t *testing.T) {
	type args struct {
		ctx      context.Context
		state    *framework.CycleState
		pod      *v1.Pod
		nodeName string
	}
	c := fake.NewSimpleClientset()
	rh := diskio.New()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	rh.(resource.CacheHandle).AddCacheNodeInfo("node1", s)
	informerFactory := informers.NewSharedInformerFactory(c, 0)
	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Second)
	resource.IoiContext = &resource.ResourceIOContext{
		NsWhiteList: []string{"ns1"},
		Reservedpod: map[string][]string{
			"node1": {},
		},
		PodRequests: make(map[string]v1alpha1.IOBandwidth),
		Queue:       workqueue.NewNamedRateLimitingQueue(rateLimiter, "diskioaware"),
	}
	cs := framework.NewCycleState()

	cs.Write(framework.StateKey(stateKeyPrefix+"node1"), &stateData{request: v1alpha1.IOBandwidth{
		Total: rs.MustParse("50Mi"),
		Read:  rs.MustParse("25Mi"),
		Write: rs.MustParse("25Mi"),
	}, nodeSupportIOI: true})

	nodeIOI := st.MakeNode().Name("node1").Obj()
	niIOI := framework.NewNodeInfo()
	niIOI.SetNode(nodeIOI)

	tests := []struct {
		name string
		args args
		want *framework.Status
	}{
		{
			name: "pod in white list",
			args: args{
				ctx:      context.TODO(),
				state:    cs,
				pod:      st.MakePod().Name("pod1").Namespace("ns1").Obj(),
				nodeName: "node1",
			},
			want: framework.NewStatus(framework.Success),
		},
		{
			name: "Reserve resource for pod",
			args: args{
				ctx:      context.TODO(),
				state:    cs,
				pod:      st.MakePod().Name("pod2").Namespace("default").Obj(),
				nodeName: "node1",
			},
			want: framework.NewStatus(framework.Success),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := &DiskIO{
				rh:         rh,
				nodeLister: informerFactory.Core().V1().Nodes().Lister(),
			}
			if got := ps.Reserve(tt.args.ctx, tt.args.state, tt.args.pod, tt.args.nodeName); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DiskIO.Reserve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiskIO_Unreserve(t *testing.T) {
	type args struct {
		ctx      context.Context
		state    *framework.CycleState
		pod      *v1.Pod
		nodeName string
	}
	c := fake.NewSimpleClientset()
	rh := diskio.New()
	err := rh.Run(resource.NewExtendedCache(), c)
	if err != nil {
		t.Fatal(err)
	}
	rh.(resource.CacheHandle).AddCacheNodeInfo("node1", s)
	rateLimiter := workqueue.NewItemExponentialFailureRateLimiter(time.Second, 5*time.Second)
	resource.IoiContext = &resource.ResourceIOContext{
		NsWhiteList: []string{"ns1"},
		Reservedpod: map[string][]string{
			"node1": {"pod1"},
		},
		PodRequests: map[string]v1alpha1.IOBandwidth{
			"pod1": {
				Total: rs.MustParse("50Mi"),
				Read:  rs.MustParse("25Mi"),
				Write: rs.MustParse("25Mi"),
			},
		},
		Queue: workqueue.NewNamedRateLimitingQueue(rateLimiter, "diskioaware"),
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "pod in white list",
			args: args{
				ctx:      context.TODO(),
				pod:      st.MakePod().Name("pod1").Namespace("ns1").Obj(),
				nodeName: "node1",
			},
		},
		{
			name: "Unreserve resource for pod",
			args: args{
				ctx:      context.TODO(),
				pod:      st.MakePod().Name("pod1").UID("pod1").Namespace("default").Obj(),
				nodeName: "node1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := &DiskIO{
				rh: rh,
			}
			ps.Unreserve(tt.args.ctx, tt.args.state, tt.args.pod, tt.args.nodeName)
		})
	}
}

func TestDiskIO_ClearStateData(t *testing.T) {
	type fields struct {
		state  *framework.CycleState
		lister corelisters.NodeLister
	}

	// create nodes
	c := fake.NewSimpleClientset()
	nodes, err := makeNodes(c, "N", 3)
	if err != nil {
		t.Fatal("failed to create node: ", err)
	}
	informerFactory := informers.NewSharedInformerFactory(c, 0)
	stop := make(chan struct{})
	defer close(stop)
	informerFactory.Start(stop)
	ninformer := informerFactory.Core().V1().Nodes()
	informerFactory.WaitForCacheSync(stop)
	for _, n := range nodes {
		ninformer.Informer().GetIndexer().Add(n)
	}

	// add data in cycle state
	cs := &framework.CycleState{}
	for _, n := range nodes {
		cs.Write(framework.StateKey(stateKeyPrefix+n.Name), &stateData{})
	}

	tests := []struct {
		name   string
		fields *fields
	}{
		{
			name: "Remove state data for all nodes",
			fields: &fields{
				state:  cs,
				lister: ninformer.Lister(),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearStateData(tt.fields.state, tt.fields.lister)
			for _, n := range nodes {
				got, err := getStateData(tt.fields.state, stateKeyPrefix+n.Name)
				if got != nil {
					t.Fatalf("failed to clear all nodes state data %v, err: %v", got, err)
				}
			}
		})
	}
}
