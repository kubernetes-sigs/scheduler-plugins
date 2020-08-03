package fixednode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	informersfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
)

func TestScheduledRecord(t *testing.T) {
	tests := []struct {
		Input *appsv1.StatefulSet
		Want  map[string]string
	}{
		{
			Input: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			Want: map[string]string{},
		},
		{
			Input: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						fixedNodeRecordKey: `{"pod1":"node1"}`,
					},
				},
			},
			Want: map[string]string{
				"pod1": "node1",
			},
		},
		{
			Input: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						fixedNodeRecordKey: `{"pod1":"node1",}`,
					},
				},
			},
			Want: map[string]string{},
		},
		{
			Input: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						fixedNodeRecordKey: `{"pod1":"node1","pod2":"node2","pod1":"node3"}`,
					},
				},
			},
			Want: map[string]string{
				"pod2": "node2",
				"pod1": "node3",
			},
		},
	}

	for _, c := range tests {
		t.Run("", func(t *testing.T) {
			result := newScheduleRecords(c.Input)
			assert.Equal(t, c.Want, result)
		})

	}
}

func TestFixedNode_PostBind(t *testing.T) {
	clientSet := fake.NewSimpleClientset()

	informers := informersfactory.NewSharedInformerFactory(clientSet, 0)

	stsLister := informers.Apps().V1().StatefulSets().Lister()

	fixednode := FixedNode{
		statefulSetLister: stsLister,
		clientSet:         clientSet,
	}

	stsSet := []*appsv1.StatefulSet{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-test",
				Namespace: "test",
				Annotations: map[string]string{
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-prd",
				Namespace: "prd",
				Annotations: map[string]string{
					fixedNodeRecordKey: `{"app-prd-0":"node0","app-prd-1":"node1"}`,
				},
			},
		},
	}

	for _, sts := range stsSet {

		informers.Apps().V1().StatefulSets().Informer().GetIndexer().Add(sts)

		if _, err := clientSet.AppsV1().StatefulSets(sts.Namespace).Create(context.TODO(), sts, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		Name     string
		Pod      *corev1.Pod
		NodeName string
		Want     string
	}{
		{
			Name: "case1",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "app-test-0",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-test"},
					},
				},
			},
			NodeName: "node0",
			Want:     "",
		},
		{
			Name: "case2",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test",
					Name:      "app-test-2",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-test"},
					},

					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},
			NodeName: "node2",
			Want:     "node2",
		},
		{
			Name: "case3",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-0",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-prd"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			NodeName: "node0",
			Want:     "node0",
		},
		{
			Name: "case4",

			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-1",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-prd"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			NodeName: "node1",

			Want: "node1",
		},
	}

	for _, c := range tests {

		t.Run(c.Name, func(t *testing.T) {

			fixednode.PostBind(context.TODO(), nil, c.Pod, c.NodeName)

			sts, err := clientSet.AppsV1().StatefulSets(c.Pod.Namespace).Get(context.TODO(), c.Pod.OwnerReferences[0].Name, metav1.GetOptions{})
			if err != nil {
				t.Error(err)
				return
			}

			scheduledRecords := newScheduleRecords(sts)

			nodeName, _ := scheduledRecords[c.Pod.Name]
			assert.Equal(t, c.Want, nodeName)
		})
	}
}

func
TestFixedNode_Filter(t *testing.T) {
	clientSet := fake.NewSimpleClientset()

	informers := informersfactory.NewSharedInformerFactory(clientSet, 0)

	stsInformer := informers.Apps().V1().StatefulSets().Informer()
	stsLister := informers.Apps().V1().StatefulSets().Lister()

	fixednode := FixedNode{
		statefulSetLister: stsLister,
		clientSet:         clientSet,
	}

	stsSet := []*appsv1.StatefulSet{
		{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-dev",
				Namespace: "dev",
			},
		},
		{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "app-prd",
				Namespace: "prd",
				Annotations: map[string]string{
					fixedNodeRecordKey: `{"app-prd-0":"node0","app-prd-1":"node1"}`,
				},
			},
		},
	}

	for _, sts := range stsSet {
		if err := stsInformer.GetIndexer().Add(sts); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		Name string
		Pod  *corev1.Pod
		Node *corev1.Node
		Want framework.Code
	}{
		{
			Name: "case1",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "dev",
					Name:      "app-dev-0",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-dev"},
					},
				},
				Spec: corev1.PodSpec{},
			},
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			Want: framework.Success,
		},
		{
			Name: "case2",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "dev",
					Name:      "app-dev-0",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-dev"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},
			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			Want: framework.Success,
		},
		{
			Name: "case3",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-0",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-prd"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node0"},
			},
			Want: framework.Success,
		},
		{
			Name: "case4",

			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-1",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-prd"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node0"},
			},
			Want: framework.Unschedulable,
		},
		{
			Name: "case5",

			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-1",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: ownerResource, Name: "app-prd"},
					},
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			Want: framework.Success,
		},
		{
			Name: "case6",
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "prd",
					Name:      "app-prd-1",
					Labels: map[string]string{
						fixedNodeEnableKey: "true",
					},
				},
			},

			Node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			Want: framework.Error,
		},
	}

	for _, c := range tests {

		t.Run(c.Name, func(t *testing.T) {
			nodeInfo := schedulernodeinfo.NewNodeInfo()
			nodeInfo.SetNode(c.Node)

			status := fixednode.Filter(nil, nil, c.Pod, nodeInfo)

			assert.Equal(t, c.Want, status.Code())
		})
	}

}
