/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	spyreconst "github.com/ibm-aiu/spyre-operator/const"
	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/scheduler-plugins/pkg/spyre"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Filter Plugin", func() {

	var err error
	var ap *spyre.SpyrePlugin
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		spyreClient, err = spyreclient.NewClient(ctx, cfg)
		Expect(err).To(BeNil())
		Expect(spyreClient).NotTo((BeNil()))

		ap = &spyre.SpyrePlugin{}
		ap.ExportSetSpyreClient(spyreClient)
		ap.ExportSetK8sClient(k8sClient)

	})

	Context("pf", func() {

		var nodeList = []string{"node1", "node2"}

		BeforeEach(func() {

			appNs := createNewNamespace(ctx, k8sClient)

			for _, node := range nodeList {
				s := &spyrev1alpha1.SpyreNodeState{
					ObjectMeta: metav1.ObjectMeta{
						Name: node,
					},
					Spec: spyrev1alpha1.SpyreNodeStateSpec{
						NodeName: node,
						SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
							{PciAddress: "00:01", NumVfs: 1},
							{PciAddress: "00:02", NumVfs: 1},
							{PciAddress: "00:03", NumVfs: 1},
						},
					},
				}
				s, err = spyreClient.Create(ctx, s)
				Expect(err).To(BeNil())
				s.Status.AllocationList = []spyrev1alpha1.Allocation{{DeviceList: []string{"00:02"}}}
				s.Status.Reservations = map[string]spyrev1alpha1.Reservation{"spyre_pf": {
					PodsUnderScheduling: []spyrev1alpha1.Pod{{Namespace: appNs, Name: "p1"}},
					DeviceSets:          [][]string{{"00:03"}},
				}}
				s, err = spyreClient.UpdateStatus(ctx, s, true)
				Expect(len(s.Status.AllocationList[0].DeviceList)).Should(BeNumerically("==", 1))
			}
			nsList, err := spyreClient.List(ctx, &client.ListOptions{})
			Expect(err).To(BeNil())
			Expect(len(nsList.Items)).Should(Equal(len(nodeList)))

			oneQuant, err := resource.ParseQuantity("1")
			Expect(err).To(BeNil())
			tenMillQuant, err := resource.ParseQuantity("10m")
			Expect(err).To(BeNil())
			tenMegaQuant, err := resource.ParseQuantity("10Mi")
			Expect(err).To(BeNil())
			err = k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: appNs},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Requests: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName("cpu"):    tenMillQuant,
									corev1.ResourceName("memory"): tenMegaQuant,
								},
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName("cpu"):    tenMillQuant,
									corev1.ResourceName("memory"): tenMegaQuant,
								},
							},
						},
						{
							Name:  "c2",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName(spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName): oneQuant},
							},
						},
					},
				},
			})
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			for _, node := range nodeList {
				err := spyreClient.Delete(ctx, node, &client.DeleteOptions{})
				Expect(err).To(BeNil())
			}
		})

		It("reads the number of requested devices", func() {
			oneQuant, err := resource.ParseQuantity("1")
			Expect(err).To(BeNil())
			twoQuant, err := resource.ParseQuantity("2")
			Expect(err).To(BeNil())
			tenMillQuant, err := resource.ParseQuantity("10m")
			Expect(err).To(BeNil())
			tenMegaQuant, err := resource.ParseQuantity("10Mi")
			Expect(err).To(BeNil())
			pf := spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "p1",
					Namespace: "n1",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "c1",
							Resources: corev1.ResourceRequirements{
								Requests: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName("cpu"):    tenMillQuant,
									corev1.ResourceName("memory"): tenMegaQuant,
								},
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName("cpu"):    tenMillQuant,
									corev1.ResourceName("memory"): tenMegaQuant,
								},
							},
						},
						{Name: "c2"},
					},
				},
				Status: corev1.PodStatus{},
			}
			m, s := spyre.GetNumRequestedDevices(p)
			Expect(s).To(BeNil())
			Expect(m[spyreconst.PfResourceName]).Should(BeNumerically("==", 0))
			Expect(m[spyreconst.VfResourceName]).Should(BeNumerically("==", 0))

			p.Spec.Containers[1].Resources = corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceName(pf): oneQuant},
				Limits:   map[corev1.ResourceName]resource.Quantity{corev1.ResourceName(pf): oneQuant},
			}
			m, s = spyre.GetNumRequestedDevices(p)
			Expect(s).To(BeNil())
			Expect(m[spyreconst.PfResourceName]).Should(BeNumerically("==", 1))

			p.Spec.Containers[1].Resources = corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceName(pf): twoQuant},
				Limits:   map[corev1.ResourceName]resource.Quantity{corev1.ResourceName(pf): twoQuant},
			}
			m, s = spyre.GetNumRequestedDevices(p)
			Expect(s).To(BeNil())
			Expect(m[spyreconst.PfResourceName]).Should(BeNumerically("==", 2))

			p.Spec.Containers[1].Resources = corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceName("example.com/dev"): oneQuant},
				Limits:   map[corev1.ResourceName]resource.Quantity{corev1.ResourceName("example.com/dev"): oneQuant},
			}
			m, s = spyre.GetNumRequestedDevices(p)
			Expect(s).To(BeNil())
			Expect(m[spyreconst.PfResourceName]).Should(BeNumerically("==", 0))
		})

		It("can extract number of remaining devices from SpyreNodeState", func() {
			nodeState, err := spyreClient.GetSpyreNodeState(ctx, "node1")
			Expect(err).To(BeNil())
			n, err := spyre.GetNumRemainingDevices(ap, ctx, nodeState, false, "spyre_pf")
			Expect(err).To(BeNil())
			Expect(n).Should(BeNumerically("==", 1))
		})

	})

	Context("vf", func() {

		node := "node"
		var ctx context.Context
		spyrepolName := "spyreclusterpolicy"
		BeforeEach(func() {
			ctx = context.Background()
			p := &spyrev1alpha1.SpyreClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: spyrepolName},
			}
			p, err := spyreClient.CreateSpyreClusterPolicy(ctx, p)
			Expect(err).To(BeNil())
			p.Status.Namespace = "spyre-operator"
			p.Status.State = spyrev1alpha1.Ready
			_, err = spyreClient.UpdateSpyreClusterPolicyStatus(ctx, p, true)
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			ctx = context.Background()
			err := spyreClient.DeleteSpyreClusterPolicy(ctx, spyrepolName, &client.DeleteOptions{})
			Expect(err).To(BeNil())

		})

		DescribeTable("2 PF x 4 VF with single node", func(allocated []string, reserved []string, numRequest int, isSuccess bool) {

			spyreIfList := []spyrev1alpha1.SpyreInterface{
				{PciAddress: "00:01.0", NumVfs: 4, Vfs: []string{"00:01.1", "00:01.2", "00:01.3", "00:01.4"}},
				{PciAddress: "00:02.0", NumVfs: 4, Vfs: []string{"00:02.1", "00:02.2", "00:02.3", "00:02.4"}},
			}
			var alloc []spyrev1alpha1.Allocation
			var reserve map[string]spyrev1alpha1.Reservation
			if len(allocated) > 0 {
				alloc = []spyrev1alpha1.Allocation{{
					DeviceList:   allocated,
					Pod:          &spyrev1alpha1.Pod{Name: "n", Namespace: "n"},
					ResourcePool: "spyre_vf",
				}}
			}
			if len(reserved) > 0 {
				reserve = map[string]spyrev1alpha1.Reservation{"spyre_vf": {
					PodsUnderScheduling: []spyrev1alpha1.Pod{{Name: "n", Namespace: "n"}},
					DeviceSets:          [][]string{reserved},
				}}
			}
			pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-2.json"))
			Expect(err).To(BeNil())
			createSpyreNodeState(spyreClient, ctx, spyreIfList, node, string(pcitopo), alloc, reserve)
			namespace := createNewNamespace(ctx, k8sClient)
			q, err := resource.ParseQuantity(strconv.Itoa(numRequest))
			Expect(err).To(BeNil())
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName(spyreconst.ResourcePrefix + "/" + spyreconst.VfResourceName): q},
							},
						},
					},
				}}
			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
			status := spyre.ExportFilter(ap, ctx, p, node)
			Expect(status.IsSuccess()).Should(BeTrue())
		},
			Entry("alloc=0, reserve=0, req=1",
				nil, nil, 1, true),
			Entry("alloc=2, reserve=0, req=2",
				[]string{"00:01.1", "00:01.2"}, nil, 2, true),
			Entry("alloc=0, reserve=2, req=2",
				nil, []string{"00:01.3", "00:01.4"}, 2, true),
		)

		DescribeTable("select appropriate node for tierX requests", func(allocated []string, success bool) {

			pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-2.json"))
			Expect(err).To(BeNil())

			spyreIfList := []spyrev1alpha1.SpyreInterface{
				{PciAddress: "0000:1a:00.0", NumVfs: 1},
				{PciAddress: "0000:1b:00.0", NumVfs: 1},
				{PciAddress: "0000:2a:00.0", NumVfs: 1},
				{PciAddress: "0000:2b:00.0", NumVfs: 1},
			}
			var alloc []spyrev1alpha1.Allocation
			if len(allocated) > 0 {
				alloc = []spyrev1alpha1.Allocation{{
					DeviceList:   allocated,
					Pod:          &spyrev1alpha1.Pod{Name: "n", Namespace: "n"},
					ResourcePool: "spyre_pf",
				}}
			}

			createSpyreNodeState(spyreClient, ctx, spyreIfList, node, string(pcitopo), alloc, nil)

			// create request
			namespace := createNewNamespace(ctx, k8sClient)
			q, err := resource.ParseQuantity(strconv.Itoa(2))
			Expect(err).To(BeNil())
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName("ibm.com/spyre_pf_tier0"): q},
							},
						},
					},
				}}
			nodeInfo := framework.NewNodeInfo()
			nodeInfo.SetNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
			status := spyre.ExportFilter(ap, ctx, p, node)
			if success {
				Expect(status.IsSuccess()).Should(BeTrue())
			} else {
				Expect(status.IsSuccess()).Should(BeFalse())
				Expect(status.Reasons()[0]).Should(ContainSubstring("not enough devices remain"))
			}
		},
			Entry("enough device but not enough tier0", []string{"0000:1b:00.0", "0000:2a:00.0"}, false),
			Entry("enough tier0", nil, true),
		)

		AfterEach(func() {
			err := spyreClient.Delete(ctx, node, &client.DeleteOptions{})
			Expect(err).To(BeNil())
		})
	})

	Context("cardmgmt", Ordered, func() {

		spyrepolName := "spyreclusterpolicy"
		nodes := []string{"abc-worker-1", "abc-worker-2", "def-worker-1", "def-worker-2"}

		BeforeAll(func() {

			ctx := context.Background()

			// create SpyreNodeState for each node
			pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-2.json"))
			Expect(err).To(BeNil())
			spyreIfList := []spyrev1alpha1.SpyreInterface{
				{PciAddress: "00:01.0", NumVfs: 4, Vfs: []string{"00:01.1", "00:01.2", "00:01.3", "00:01.4"}},
				{PciAddress: "00:02.0", NumVfs: 4, Vfs: []string{"00:02.1", "00:02.2", "00:02.3", "00:02.4"}},
			}
			for _, node := range nodes {
				createSpyreNodeState(spyreClient, ctx, spyreIfList, node, string(pcitopo), nil, nil)
			}
		})

		AfterAll(func() {
			ctx := context.Background()
			err := spyreClient.DeleteAll(ctx)
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			ctx = context.Background()
			err := spyreClient.DeleteSpyreClusterPolicy(ctx, spyrepolName, &client.DeleteOptions{})
			Expect(err).To(BeNil())
		})

		DescribeTable("device allocation based on cardmgmt and spyrefilter", func(
			resourceName string, spyrefilter string, expected []bool) {

			ctx = context.Background()

			cardmgmtEnabled := len(spyrefilter) > 0
			pol := &spyrev1alpha1.SpyreClusterPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: spyrepolName},
				Spec: spyrev1alpha1.SpyreClusterPolicySpec{
					CardManagement: spyrev1alpha1.CardManagementSpec{
						Enabled: cardmgmtEnabled,
						Config: &spyrev1alpha1.CardManagementConfig{
							SpyreFilter: &spyrefilter,
						},
					},
				},
			}
			pol, err := spyreClient.CreateSpyreClusterPolicy(ctx, pol)
			Expect(err).To(BeNil())
			pol.Status.Namespace = "spyre-operator"
			pol.Status.State = spyrev1alpha1.Ready
			_, err = spyreClient.UpdateSpyreClusterPolicyStatus(ctx, pol, true)
			Expect(err).To(BeNil())

			oneQuant, err := resource.ParseQuantity("1")
			Expect(err).To(BeNil())
			pUser := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName(spyreconst.ResourcePrefix + "/" + resourceName): oneQuant},
							},
						},
					},
				},
			}
			pCardmgmt := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cardmgmt",
					Namespace: pol.Status.Namespace,
					Labels: map[string]string{
						spyreconst.CardManagementLabelKey: spyreconst.CardManagementLabelValue},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName(spyreconst.ResourcePrefix + "/" + resourceName): oneQuant},
							},
						},
					},
				},
			}
			for idx, node := range nodes {
				nodeInfo := framework.NewNodeInfo()
				nodeInfo.SetNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})
				// user Pod: varies on spyrefilter
				status := spyre.ExportFilter(ap, ctx, pUser, node)
				if expected[idx] {
					Expect(status.IsSuccess()).Should(BeTrue())
				} else {
					Expect(status.IsRejected()).Should(BeTrue())
				}

				// cardmgmt Pod: always permissive
				status = spyre.ExportFilter(ap, ctx, pCardmgmt, node)
				Expect(status.IsSuccess()).Should(BeTrue())
			}

		},
			Entry("PF - all nodes are managed", spyreconst.PfResourceName, ".", []bool{false, false, false, false}),
			Entry("PF - some nodes are managed", spyreconst.PfResourceName, "worker-1", []bool{false, true, false, true}),
			Entry("PF - regexp with pipe", spyreconst.PfResourceName, "abc-worker-1|def-worker-2", []bool{false, true, true, false}),
			Entry("PF - one node is managed", spyreconst.PfResourceName, "abc-worker-1", []bool{false, true, true, true}),
			Entry("PF - no node is managed", spyreconst.PfResourceName, "", []bool{true, true, true, true}),
			Entry("VF - all nodes are managed", spyreconst.VfResourceName, ".", []bool{true, true, true, true}),
			Entry("VF - some nodes are managed", spyreconst.VfResourceName, "worker-1", []bool{true, true, true, true}),
			Entry("VF - one node is managed", spyreconst.VfResourceName, "abc-worker-1", []bool{true, true, true, true}),
			Entry("VF - no node is managed", spyreconst.VfResourceName, "", []bool{true, true, true, true}),
		)
	})
})

func createSpyreNodeState(spyreClient *spyreclient.SpyreClient, ctx context.Context, spyreIfList []spyrev1alpha1.SpyreInterface,
	node string, pcitopo string, allocations []spyrev1alpha1.Allocation, reservations map[string]spyrev1alpha1.Reservation) {

	var err error

	s := &spyrev1alpha1.SpyreNodeState{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec:       spyrev1alpha1.SpyreNodeStateSpec{NodeName: node, SpyreInterfaces: spyreIfList},
	}
	if len(pcitopo) > 0 {
		s.Spec.Pcitopo = pcitopo
	}
	s, err = spyreClient.Create(ctx, s)
	Expect(err).To(BeNil())
	if allocations != nil {
		s.Status.AllocationList = allocations
	}
	if reservations != nil {
		s.Status.Reservations = reservations
	}
	_, err = spyreClient.UpdateStatus(ctx, s, true)
	Expect(err).To(BeNil())

	_, err = spyreClient.List(ctx, &client.ListOptions{})
	Expect(err).To(BeNil())

}
