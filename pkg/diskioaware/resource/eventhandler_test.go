package resource

import (
	"reflect"
	"testing"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned/fake"
	utils "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakekubeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func int64Ptr(i int64) *int64 { return &i }

func TestIOEventHandler_DeletePod(t *testing.T) {
	client := fakekubeclientset.NewSimpleClientset()
	vclient := fake.NewSimpleClientset()
	nn := "testNode"
	rl := workqueue.DefaultControllerRateLimiter()
	type fields struct {
		coreClient   kubernetes.Interface
		vClient      versioned.Interface
		reservedPods map[string][]string
	}
	type args struct {
		obj interface{}
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "obj cannot be convertd to *v1.Pod",
			fields: fields{
				coreClient:   client,
				vClient:      vclient,
				reservedPods: map[string][]string{},
			},
			args: args{
				obj: corev1.Node{},
			},
			wantErr: true,
		},
		{
			name: "success",
			fields: fields{
				coreClient: client,
				vClient:    vclient,
				reservedPods: map[string][]string{
					nn: {"12345678"},
				},
			},
			args: args{
				obj: st.MakePod().Name("pod1").Node(nn).UID("12345678").Namespace("default").Obj(),
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			IoiContext = &ResourceIOContext{
				Reservedpod: tt.fields.reservedPods,
				Queue:       workqueue.NewNamedRateLimitingQueue(rl, "test"),
			}
			var gotRemovePod *corev1.Pod
			fh := &FakeHandle{}
			fh.Run(&FakeCache{
				GetCacheFunc: func(string) ExtendedResource {
					return &FakeResource{
						RemovePodFunc: func(pod *corev1.Pod) error {
							gotRemovePod = pod
							return nil
						},
					}
				},
			}, tt.fields.coreClient)
			eh := &IOEventHandler{
				cache:     fh,
				vClient:   tt.fields.vClient,
				nm:        nil,
				podLister: nil,
				nsLister:  nil,
			}
			eh.DeletePod(tt.args.obj)
			if !tt.wantErr {
				if !reflect.DeepEqual(tt.args.obj, gotRemovePod) {
					t.Errorf("DeletePod() failed, gotRemovePod is %v, pod to delete is %v", gotRemovePod, tt.args.obj)
				}
				v, err := IoiContext.GetReservedPods(nn)
				if err != nil {
					t.Fatal(err)
				}
				if len(v) != 0 {
					t.Errorf("DeletePod() failed, pod %v is not deleted in Podlist", tt.args.obj.(*corev1.Pod).Name)
				}
			}
		})
	}
}

func TestIOEventHandler_UpdateNodeDiskIOStats(t *testing.T) {
	client := fakekubeclientset.NewSimpleClientset()
	vclient := fake.NewSimpleClientset()
	nn := "testNode"

	type fields struct {
		coreClient   kubernetes.Interface
		vClient      versioned.Interface
		reservedPods map[string][]string
	}
	type args struct {
		oldObj interface{}
		newObj interface{}
	}
	tests := []struct {
		name       string
		fields     fields
		args       args
		wantChange bool
	}{
		{
			name: "status is nil",
			args: args{
				oldObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.CRNameSpace,
						Name:      nn + utils.NodeDiskIOInfoCRSuffix,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
				},
				newObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.CRNameSpace,
						Name:      nn + utils.NodeDiskIOInfoCRSuffix,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
				},
			},
			fields: fields{
				coreClient: client,
				vClient:    vclient,
				reservedPods: map[string][]string{
					nn: nil,
				},
			},
			wantChange: false,
		},
		{
			name: "old obj is wrong",
			args: args{
				oldObj: &corev1.Pod{},
				newObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.CRNameSpace,
						Name:      nn + utils.NodeDiskIOInfoCRSuffix,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
					Status: v1alpha1.NodeDiskIOStatsStatus{},
				},
			},
			fields: fields{
				coreClient:   client,
				vClient:      vclient,
				reservedPods: map[string][]string{},
			},
			wantChange: false,
		},
		{
			name: "new obj is wrong",
			args: args{
				newObj: &corev1.Pod{},
				oldObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.CRNameSpace,
						Name:      nn + utils.NodeDiskIOInfoCRSuffix,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
					Status: v1alpha1.NodeDiskIOStatsStatus{},
				},
			},
			fields: fields{
				coreClient:   client,
				vClient:      vclient,
				reservedPods: map[string][]string{},
			},
			wantChange: false,
		},
		{
			name: "success",
			args: args{
				oldObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: utils.CRNameSpace,
						Name:      nn + utils.NodeDiskIOInfoCRSuffix,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
					Status: v1alpha1.NodeDiskIOStatsStatus{
						ObservedGeneration:   int64Ptr(0),
						AllocatableBandwidth: map[string]v1alpha1.IOBandwidth{},
					},
				},
				newObj: &v1alpha1.NodeDiskIOStats{
					TypeMeta: metav1.TypeMeta{
						APIVersion: utils.APIVersion,
					},
					ObjectMeta: metav1.ObjectMeta{
						Namespace:  utils.CRNameSpace,
						Name:       nn + utils.NodeDiskIOInfoCRSuffix,
						Generation: 1,
					},
					Spec: v1alpha1.NodeDiskIOStatsSpec{
						NodeName: nn,
					},
					Status: v1alpha1.NodeDiskIOStatsStatus{
						ObservedGeneration:   int64Ptr(1),
						AllocatableBandwidth: map[string]v1alpha1.IOBandwidth{},
					},
				},
			},
			fields: fields{
				coreClient: client,
				vClient:    vclient,
				reservedPods: map[string][]string{
					nn: nil,
				},
			},
			wantChange: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			IoiContext = &ResourceIOContext{
				Reservedpod: tt.fields.reservedPods,
			}
			var status *v1alpha1.NodeDiskIOStatsStatus
			fh := &FakeHandle{
				UpdateNodeIOStatusFunc: func(n string, nodeIoBw v1alpha1.NodeDiskIOStatsStatus) error {
					status = &nodeIoBw
					return nil
				},
			}
			fh.Run(&FakeCache{}, tt.fields.coreClient)
			eh := &IOEventHandler{
				cache:   fh,
				vClient: tt.fields.vClient,
			}
			eh.UpdateNodeDiskIOStats(tt.args.oldObj, tt.args.newObj)
			if tt.wantChange {
				got := *status
				want := tt.args.newObj.(*v1alpha1.NodeDiskIOStats).Status
				t.Logf("got: %v, want: %v", got, want)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("UpdateNodeDiskIOStats() failed, want %v, got %v", want, got)
				}
			}
		})
	}
}

func TestIOEventHandler_DeleteNodeStaticIOInfo(t *testing.T) {
	client := fakekubeclientset.NewSimpleClientset()
	vclient := fake.NewSimpleClientset()
	nn := "testNode"
	type fields struct {
		coreClient   kubernetes.Interface
		vClient      versioned.Interface
		reservedPods map[string][]string
	}
	type args struct {
		obj interface{}
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "success and node name does not exist in reserved pod",
			fields: fields{
				coreClient:   client,
				vClient:      vclient,
				reservedPods: map[string][]string{},
			},
			args: args{
				obj: &v1alpha1.NodeDiskDevice{
					Spec: v1alpha1.NodeDiskDeviceSpec{
						NodeName: "node1",
						Devices:  map[string]v1alpha1.DiskDevice{},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "success and remove node in reserved pod",
			fields: fields{
				coreClient: client,
				vClient:    vclient,
				reservedPods: map[string][]string{
					nn: nil,
				},
			},
			args: args{
				obj: &v1alpha1.NodeDiskDevice{
					Spec: v1alpha1.NodeDiskDeviceSpec{
						NodeName: nn,
						Devices:  map[string]v1alpha1.DiskDevice{},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "obj cannot be converted to *v1alpha1.NodeDiskDevice",
			fields: fields{
				coreClient:   client,
				vClient:      vclient,
				reservedPods: nil,
			},
			args: args{
				obj: corev1.Pod{},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			IoiContext = &ResourceIOContext{
				Reservedpod: tt.fields.reservedPods,
			}
			var gotNodeName string
			fh := &FakeHandle{
				DeleteCacheNodeInfoFunc: func(nn string) error {
					gotNodeName = nn
					return nil
				},
			}
			fh.Run(&FakeCache{}, tt.fields.coreClient)
			eh := &IOEventHandler{
				cache:   fh,
				vClient: tt.fields.vClient,
			}
			eh.DeleteDiskDevice(tt.args.obj)
			if !tt.wantErr {
				node := tt.args.obj.(*v1alpha1.NodeDiskDevice).Spec.NodeName
				if gotNodeName != node {
					t.Errorf("DeleteDiskDevice() failed in DeleteCacheNodeInfo, want %v, got %v", nn, gotNodeName)
					return
				}
				if _, err := IoiContext.GetReservedPods(node); err == nil {
					t.Errorf("DeleteDiskDevice() failed in IoiContext, node %s exists", nn)
				}
			}
		})
	}
}
