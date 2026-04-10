/*
 * +-------------------------------------------------------------------+
 * | Copyright (c) 2025, 2026 IBM Corp.                                |
 * | SPDX-License-Identifier: Apache-2.0                               |
 * +-------------------------------------------------------------------+
 */

package spyre_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	spyreconst "github.com/ibm-aiu/spyre-operator/const"
	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/scheduler-plugins/pkg/spyre"
)

var _ = Describe("Common functions", func() {

	var err error
	var ap *spyre.SpyrePlugin
	ctx := context.Background()

	BeforeEach(func() {
		spyreClient, err = spyreclient.NewClient(ctx, cfg)
		Expect(err).To(BeNil())
		Expect(spyreClient).NotTo((BeNil()))
		k8sClient, err = client.New(cfg, client.Options{})
		Expect(err).NotTo(HaveOccurred())
		ap = &spyre.SpyrePlugin{}
		ap.ExportSetSpyreClient(spyreClient)
		ap.ExportSetK8sClient(k8sClient)
	})

	Context("SpyreNodeState handling", func() {

		nsName := "test-ns"
		var ns *spyrev1alpha1.SpyreNodeState
		var status *spyrev1alpha1.SpyreNodeStateStatus

		BeforeEach(func() {
			ns = &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
				},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					SpyreInterfaces: spyrev1alpha1.SpyreInterfaces{
						{PciAddress: "pf1", NumVfs: 1, Vfs: []string{"vf1"}},
						{PciAddress: "pf2", NumVfs: 1, Vfs: []string{"vf2"}},
						{PciAddress: "pf3", NumVfs: 1, Vfs: []string{"vf3"}},
						{PciAddress: "pf4", NumVfs: 2, Vfs: []string{"vf41", "vf42"}},
						{PciAddress: "pf5", NumVfs: 2, Vfs: []string{"vf51", "vf52"}},
						{PciAddress: "pf6", NumVfs: 2, Vfs: []string{"vf61", "vf62"}},
						{PciAddress: "pf7", NumVfs: 0},
						{PciAddress: "pf8", NumVfs: 0},
					},
				},
			}
			status = &spyrev1alpha1.SpyreNodeStateStatus{
				AllocationList: []spyrev1alpha1.Allocation{{DeviceList: []string{"pf2"}}, {DeviceList: []string{"vf2"}}},
				Reservations: map[string]spyrev1alpha1.Reservation{
					"spyre_pf": {DeviceSets: [][]string{{"pf3"}}},
					"spyre_vf": {DeviceSets: [][]string{{"vf3"}}},
				},
				UnhealthyDevices: []spyrev1alpha1.UnhealthyDevice{
					{ID: "pf4", State: "REMOVED"},
					{ID: "vf41", State: "REMOVED"},
					{ID: "vf42", State: "REMOVED"},
					{ID: "vf51", State: "IN_ERROR"},
					{ID: "pf6", State: "IN_ERROR"},
					{ID: "pf8", State: "IN_ERROR"},
				},
			}
			_, err := spyreClient.Create(ctx, ns)
			Expect(err).To(Succeed())
		})

		AfterEach(func() {
			err := spyreClient.Delete(ctx, nsName, &client.DeleteOptions{})
			Expect(err).Should(Succeed())
		})

		It("can check device availability without SpyreNodeState status", func() {
			ns, err := spyreClient.Get(ctx, nsName)
			Expect(err).Should(Succeed())
			for _, d := range ns.Spec.SpyreInterfaces {
				Expect(spyre.IsAvailable(ns, d.PciAddress)).To(BeTrue())
				for _, vf := range d.Vfs {
					Expect(spyre.IsAvailable(ns, vf)).To(BeTrue())
				}
			}
		})

		DescribeTable("device availability check with health info in SpyreNodeState status", func(devName string, expected bool) {
			ns.Status = *status
			ns, err = spyreClient.UpdateStatus(ctx, ns, true)
			Expect(err).Should(Succeed())
			Expect(spyre.IsAvailable(ns, devName)).Should(Equal(expected), devName)
		},
			Entry(nil, "pf1", true),
			Entry(nil, "vf1", true),
			Entry(nil, "pf2", false),  // allocated
			Entry(nil, "vf2", false),  // allocated
			Entry(nil, "pf3", false),  // reserved
			Entry(nil, "vf3", false),  // reserved
			Entry(nil, "pf4", false),  // unhealthy
			Entry(nil, "vf41", false), // unhealthy
			Entry(nil, "vf42", false), // unhealthy
			Entry(nil, "pf5", true),
			Entry(nil, "vf51", false), // unhealthy
			Entry(nil, "vf52", true),
			Entry(nil, "pf6", false), // unhealthy
			Entry(nil, "vf61", true),
			Entry(nil, "vf62", true),
			Entry(nil, "pf7", true),
			Entry(nil, "pf8", false),
		)
	})

	Context("selecting device with spyreClient", Ordered, func() {
		// enable envtest, write SpyreNodeState, then test SelectDevices().

		BeforeAll(func() {
			setupTestenv()
		})

		AfterAll(func() {
			err := testEnv.Stop()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			nsList, err := spyreClient.List(ctx, &client.ListOptions{})
			Expect(err).To(BeNil())
			for _, ns := range nsList.Items {
				err := spyreClient.Delete(ctx, ns.Name, &client.DeleteOptions{})
				Expect(err).To(BeNil())
			}
		})

		It("can generate controller-runtime log", func() {
			Expect(logBuf.String()).Should(ContainSubstring("controller-runtime.test-env"))
		})

		It("can select appropriate devices", func() {
			prepareSpyreNodeState([]string{"0000:1f:00.0"}, [][]string{{"0000:36:00.0"}}, []spyrev1alpha1.Pod{})
			numReqDevs := int64(4)
			nodeName := "node1"
			devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_pf", numReqDevs, nodeName)
			Expect(err).To(BeNil())
			Expect(len(devs)).Should(BeNumerically("==", 4))
		})

		It("can select spyre_pf and spyre_vf devices without pcitopo", func() {
			ctx := context.Background()
			node := "node1"
			s := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{
					Name: node,
				},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName:        node,
					SpyreInterfaces: getSpyreIf(),
				},
			}
			_, err := spyreClient.Create(ctx, s)
			Expect(err).To(BeNil())

			numReqDevs := int64(4)
			devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_pf", numReqDevs, node)
			Expect(err).To(BeNil())
			Expect(len(devs)).Should(BeNumerically("==", numReqDevs))
			devs, _, err = spyre.SelectDevices(ap, ctx, false, "spyre_vf", numReqDevs, node)
			Expect(err).To(BeNil())
			Expect(len(devs)).Should(BeNumerically("==", numReqDevs))
		})

		It("shows error at selecting spyre_pf_tier0 without pcitopo", func() {
			ctx := context.Background()
			node := "node1"
			s := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{
					Name: node,
				},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName:        node,
					SpyreInterfaces: getSpyreIf(),
				},
			}
			_, err := spyreClient.Create(ctx, s)
			Expect(err).To(BeNil())

			numReqDevs := int64(4)
			_, _, err = spyre.SelectDevices(ap, ctx, false, "spyre_pf_tier0", numReqDevs, node)
			Expect(err).Should(MatchError("unable to select 4 spyre_pf_tier0 device(s) due to pcitopo error: unexpected end of JSON input"))
		})

		Context("select devices", func() {
			DescribeTable("spyre_pf_tier0", func(numDevs int, allocList []string, reserveList [][]string) {
				prepareSpyreNodeState(allocList, reserveList, []spyrev1alpha1.Pod{})
				numReqDevs := int64(numDevs)
				nodeName := "node1"
				devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_pf_tier0", numReqDevs, nodeName)
				if numDevs <= 2 {
					Expect(err).To(BeNil())
					Expect(len(devs)).Should(BeNumerically("==", numDevs))
				} else {
					Expect(err.Error()).Should(Or(
						HavePrefix("not enough devices remain"),
						HavePrefix("no enough device exists")))
				}
			},
				Entry("1 device can be selected", 1, []string{"0000:1f:00.0"}, [][]string{{"0000:36:00.0"}}),
				Entry("2 devices can be selected", 2, []string{"0000:1f:00.0"}, [][]string{{"0000:36:00.0"}}),
				Entry("3 devices cannot be selected", 3, []string{"0000:1f:00.0"}, [][]string{{"0000:36:00.0"}}),
				Entry("many devices cannot be selected under no availableDeviceGroup situation",
					12, []string{
						"0000:1f:00.0", "0000:1c:00.0", "0000:20:00.0",
						"0000:36:00.0", "0000:39:00.0", "0000:3d:00.0",
						"0000:9c:00.0", "0000:9d:00.0", "0000:b9:00.0",
					}, [][]string{{}}),
			)

			DescribeTable("spyre_vf_tier0", func(numDevs int, allocList []string, reserveList [][]string, expectedDevs []string) {
				prepareSpyreNodeState(allocList, reserveList, []spyrev1alpha1.Pod{})
				numReqDevs := int64(numDevs)
				nodeName := "node1"
				devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_vf_tier0", numReqDevs, nodeName)
				if expectedDevs != nil {
					Expect(err).To(BeNil())
					Expect(devs).Should(ContainElements(expectedDevs))
				} else {
					Expect(err.Error()).Should(Or(
						HavePrefix("not enough devices remain"),
						HavePrefix("no enough device exists")))
				}
			},
				Entry("1 device can be selected", 1, []string{"0000:1f:00.1"}, [][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2"}),
				Entry("2 devices can be selected from smallest tier0", 2,
					[]string{"0000:1f:00.1"}, [][]string{{"0000:36:00.1"}},
					[]string{"0000:9c:00.2", "0000:9d:00.2"}),
				Entry("2 devices can be selected from available tier0", 2,
					[]string{
						"0000:1f:00.1", "0000:9c:00.1", "0000:9c:00.2", "0000:9c:00.3", "0000:9c:00.4",
						"0000:9d:00.1", "0000:9d:00.2", "0000:9d:00.3", "0000:9d:00.4"},
					[][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2", "0000:1f:00.2"}),
				Entry("3 devices cannot be selected", 3,
					[]string{"0000:1f:00.1", "0000:1f:00.2", "0000:1f:00.3", "0000:1f:00.4"},
					[][]string{{"0000:36:00.1", "0000:36:00.2", "0000:36:00.3", "0000:36:00.4"}},
					nil),
				Entry("do not allocate VFs from single PF", 4,
					[]string{
						"0000:1f:00.1", "0000:1f:00.2", "0000:1f:00.3", "0000:1f:00.4",
						"0000:20:00.1", "0000:20:00.2", "0000:20:00.3", "0000:20:00.4",
						"0000:36:00.1", "0000:36:00.2", "0000:36:00.3", "0000:36:00.4",
						"0000:39:00.1", "0000:39:00.2", "0000:39:00.3", "0000:39:00.4",
						"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3", "0000:3d:00.4",
						"0000:9c:00.1", "0000:9c:00.2", "0000:9c:00.3", "0000:9c:00.4",
						"0000:9d:00.1", "0000:9d:00.2", "0000:9d:00.3", "0000:9d:00.4"},
					[][]string{{"0000:b9:00.1", "0000:b9:00.2", "0000:b9:00.3", "0000:b9:00.4"}},
					nil),
			)

			DescribeTable("spyre_vf_tier1", func(numDevs int, allocList []string, reserveList [][]string, expectedDevs []string) {
				prepareSpyreNodeState(allocList, reserveList, []spyrev1alpha1.Pod{})
				numReqDevs := int64(numDevs)
				nodeName := "node1"
				devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_vf_tier1", numReqDevs, nodeName)
				if expectedDevs != nil {
					Expect(err).To(BeNil())
					Expect(devs).Should(ContainElements(expectedDevs))
				} else {
					Expect(err.Error()).Should(Or(
						HavePrefix("not enough devices remain"),
						HavePrefix("no enough device exists")))
				}
			},
				Entry("1 device can be selected", 1, []string{"0000:1f:00.1"}, [][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2"}),
				Entry("2 devices can be selected", 2,
					[]string{"0000:1f:00.1"}, [][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2", "0000:36:00.2"}),
				Entry("2 devices can be selected with many allocated devices", 2,
					[]string{
						"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3", "0000:1c:00.4",
						"0000:1f:00.1", "0000:1f:00.2", "0000:1f:00.3", "0000:1f:00.4",
						"0000:20:00.1", "0000:20:00.2", "0000:20:00.3", "0000:20:00.4",
						"0000:36:00.1", "0000:36:00.2", "0000:36:00.3", "0000:36:00.4",
						"0000:39:00.1", "0000:39:00.2", "0000:39:00.3", "0000:39:00.4"},
					[][]string{{"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3", "0000:3d:00.4"}},
					[]string{"0000:9c:00.2", "0000:b9:00.2"},
				),
				Entry("do not allocate VFs from single PF", 4,
					[]string{
						"0000:1f:00.1", "0000:1f:00.2", "0000:1f:00.3", "0000:1f:00.4",
						"0000:20:00.1", "0000:20:00.2", "0000:20:00.3", "0000:20:00.4",
						"0000:36:00.1", "0000:36:00.2", "0000:36:00.3", "0000:36:00.4",
						"0000:39:00.1", "0000:39:00.2", "0000:39:00.3", "0000:39:00.4",
						"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3", "0000:3d:00.4",
						"0000:9c:00.1", "0000:9c:00.2", "0000:9c:00.3", "0000:9c:00.4",
						"0000:9d:00.1", "0000:9d:00.2", "0000:9d:00.3", "0000:9d:00.4"},
					[][]string{{"0000:b9:00.1", "0000:b9:00.2", "0000:b9:00.3", "0000:b9:00.4"}},
					nil),
			)

			DescribeTable("spyre_vf_tier2", func(numDevs int, allocList []string, reserveList [][]string, expectedDevs []string) {
				nodeList := prepareUnbalancedSpyreNodeState(allocList, reserveList, []spyrev1alpha1.Pod{})
				numReqDevs := int64(numDevs)

				_, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_vf_tier2", numReqDevs, nodeList[0])
				Expect(err).NotTo(BeNil()) // must be error, but panic must not occur.
				devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_vf_tier2", numReqDevs, nodeList[1])

				if expectedDevs != nil {
					Expect(err).To(BeNil())
					Expect(devs).Should(ContainElements(expectedDevs))
				} else {
					Expect(err.Error()).Should(Or(
						HavePrefix("not enough devices remain"),
						HavePrefix("no enough device exists")))
				}

			},
				Entry("2 devices can be selected", 2,
					[]string{"0000:1f:00.1"},
					[][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2", "0000:9c:00.2"}),
				Entry("4 devices can be selected", 4,
					[]string{},
					[][]string{{"0000:36:00.1"}},
					[]string{"0000:1c:00.2", "0000:9c:00.2"}),
			)

		})

		It("can cleanup orphan reservations", func() {
			oneQuant, err := resource.ParseQuantity("1")
			Expect(err).To(BeNil())
			twoQuant, err := resource.ParseQuantity("2")
			Expect(err).To(BeNil())
			tenMillQuant, err := resource.ParseQuantity("10m")
			Expect(err).To(BeNil())
			tenMegaQuant, err := resource.ParseQuantity("10Mi")
			Expect(err).To(BeNil())
			resName := spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName
			ns := createNewNamespace(ctx, k8sClient)
			allocList := []string{}
			reserveList := [][]string{
				{"0000:1f:00.0", "0000:1c:00.0"},
				{"0000:36:00.0", "0000:39:00.0"},
				{"0000:9c:00.0", "0000:9d:00.0"},
				{"0000:20:00.0"},
			}
			pSched := []spyrev1alpha1.Pod{
				{Namespace: ns, Name: "p1"},
				{Namespace: ns, Name: "p2"},
				{Namespace: ns, Name: "p3"},
				{Namespace: ns, Name: "p4"},
			}
			prepareSpyreNodeState(allocList, reserveList, pSched)

			names := []string{"p1", "p2", "p3"}
			qs := []resource.Quantity{twoQuant, twoQuant, oneQuant}
			for i := range names {
				err = k8sClient.Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: ns},
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
										corev1.ResourceName(resName): qs[i]},
								},
							},
						},
					},
				})
				Expect(err).To(BeNil())
			}
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: pSched[0].Name, Namespace: ns},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "c1",
							Image: "image",
							Resources: corev1.ResourceRequirements{
								Limits: map[corev1.ResourceName]resource.Quantity{
									corev1.ResourceName(resName): twoQuant},
							}}}}}
			nodeState, err := spyre.CleanupOrphanReservation(ap, ctx, "node1", p)
			Expect(err).To(BeNil())
			r := nodeState.Status.Reservations[spyreconst.PfResourceName]
			Expect(r.PodsUnderScheduling).Should(HaveLen(2))
			Expect(r.DeviceSets).Should(HaveLen(2))
			Expect(r.DeviceSets[0]).Should(HaveLen(2))
			Expect(r.DeviceSets[1]).Should(HaveLen(1))
		})
	})

	Context("common function", Ordered, func() {

		BeforeAll(func() {
			setupTestenv()
		})

		It("can detect Pods requesting SPYREs", func() {
			oneQuant, _ := resource.ParseQuantity("1")
			resName := spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Namespace: "ns1"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "c1",
						Image:   "i1",
						Command: []string{"hello"},
						Resources: corev1.ResourceRequirements{
							Requests: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceName(resName): oneQuant},
							Limits: map[corev1.ResourceName]resource.Quantity{
								corev1.ResourceName(resName): oneQuant},
						},
					}},
				},
			}
			m, err := spyre.GetNumRequestedDevices(p)
			// sum, rName
			Expect(err).To(BeNil())
			Expect(m).Should(HaveKey(spyreconst.PfResourceName))
			Expect(m[spyreconst.PfResourceName]).Should(Equal(1))
		})

		It("can count zero spyre request", func() {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "n1", Namespace: "ns1"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "c1",
						Image:   "i1",
						Command: []string{"hello"},
					}},
				},
			}
			m, err := spyre.GetNumRequestedDevices(p)
			// sum, rName
			Expect(err).To(BeNil())
			Expect(m).Should(HaveLen(0))
		})

		It("can create available device groups", func() {
			node := "node1"
			resourceName := "spyre_pf"
			pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-1.json"))
			Expect(err).To(BeNil())
			s := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{
					Name: node,
				},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName: node,
					Pcitopo:  string(pcitopo),
				},
			}
			s.Status.AllocationList = []spyrev1alpha1.Allocation{{DeviceList: []string{"0000:1f:00.0"}}}
			s.Status.Reservations = map[string]spyrev1alpha1.Reservation{
				resourceName: {DeviceSets: [][]string{{"0000:36:00.0"}}}}
			groups, err := spyre.DeviceTree(s, false, resourceName)
			Expect(err).To(BeNil())
			expectedElementGroups := SatisfyAny(
				ContainElements("0000:1a:00.0", "0000:1c:00.0", "0000:1d:00.0", "0000:1e:00.0"),
				ContainElements("0000:3d:00.0", "0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"))
			Expect(groups[0][0]).Should(expectedElementGroups)
			Expect(groups[0][1]).Should(expectedElementGroups)
		})

		Context("Pod-privilege-aware remaining device count", func() {
			s := &spyrev1alpha1.SpyreNodeState{
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
						{PciAddress: "0000:1c:00.0", NumVfs: 2,
							Health: spyrev1alpha1.SpyreHealthy,
							Vfs:    []string{"0000:1c:00.1", "0000:1c:00.2"}},
						{PciAddress: "0000:1f:00.0", NumVfs: 2,
							Health: spyrev1alpha1.SpyreHealthy,
							Vfs:    []string{"0000:1f:00.1", "0000:1f:00.2"}},
					},
				},
				Status: spyrev1alpha1.SpyreNodeStateStatus{
					AllocationList: []spyrev1alpha1.Allocation{{DeviceList: []string{"0000:1c:00.1"}}},
				},
			}

			It("only counts .1 VF devices for privileged Pod", func() {
				isPrivileged := true
				nDev, err := spyre.GetNumRemainingDevices(ap, ctx, s, isPrivileged, "spyre_vf")
				Expect(err).To(BeNil())
				Expect(nDev).Should(BeNumerically("==", 1))
			})
			It("both counts .1 and .2 VF devices for non-privileged Pod", func() {
				isPrivileged := false
				nDev, err := spyre.GetNumRemainingDevices(ap, ctx, s, isPrivileged, "spyre_vf")
				Expect(err).To(BeNil())
				Expect(nDev).Should(BeNumerically("==", 2))
			})
		})

		Context("cardmgmt Pod", func() {

			var ap *spyre.SpyrePlugin
			const opNs = "spyre-operator"
			nodeState := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName: nodeName,
					SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
						{PciAddress: "01.0", NumVfs: 2, Vfs: []string{"01.1", "01.2"}, Health: spyrev1alpha1.SpyreHealthy},
						{PciAddress: "02.0", NumVfs: 2, Vfs: []string{"02.1", "02.2"}, Health: spyrev1alpha1.SpyreHealthy},
						{PciAddress: "03.0", NumVfs: 2, Vfs: []string{"03.1", "03.2"}, Health: spyrev1alpha1.SpyreHealthy},
						{PciAddress: "04.0", NumVfs: 2, Vfs: []string{"04.1", "04.2"}, Health: spyrev1alpha1.SpyreHealthy},
					},
				},
			}

			DescribeTable("card management and its runner Pod detection", func(p *corev1.Pod, isCardMgmt bool, isRunner bool) {
				Expect(spyre.IsCardManagementPod(p, opNs)).Should(Equal(isCardMgmt))
				Expect(spyre.IsCardManagementRunnerPod(p, opNs)).Should(Equal(isRunner))
			},
				Entry("cardmgmt-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"app": "cardmgmt"},
					Namespace: opNs}},
					true, false),
				Entry("cmrunner-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"aiucardmanager": "1"},
					Namespace: opNs}},
					false, true),
				Entry("cardmgmt-nons", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"app": "cardmgmt"},
					Namespace: "abc"}},
					false, false),
				Entry("cmrunner-nons", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"aiucardmanager": "1"},
					Namespace: "abc"}},
					false, false),
				Entry("nolabel-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"app": "foo"},
					Namespace: opNs}},
					false, false),
				Entry("nolabel-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"apaiucardmanagerp": "foo"},
					Namespace: opNs}},
					false, false),
				Entry("nolabel-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Labels:    map[string]string{"foo": "cardmgmt"},
					Namespace: opNs}},
					false, false),
				Entry("nolabel-ns", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Namespace: opNs}},
					false, false),
			)

			DescribeTable("device selection for device cardmgmt Pod", func(labels map[string]string, numPfs, numVfs int, expectedError error) {

				Expect(err).To(BeNil())
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "p1", Namespace: opNs,
						Labels: labels,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{},
								},
							},
						},
					},
				}
				if numPfs > 0 {
					q, err := resource.ParseQuantity(strconv.Itoa(numPfs))
					Expect(err).To(BeNil())
					pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(spyreconst.ResourcePrefix+"/"+spyreconst.PfResourceName)] = q
				}
				if numVfs > 0 {
					q, err := resource.ParseQuantity(strconv.Itoa(numVfs))
					Expect(err).To(BeNil())
					pod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(spyreconst.ResourcePrefix+"/"+spyreconst.VfResourceName)] = q
				}
				devs, err := spyre.SelectDevicesForCardManagement(ap, pod, nodeState, nodeName)
				if expectedError == nil {
					Expect(err).To(BeNil())
					if numPfs > 0 {
						Expect(devs[spyreconst.PfResourceName]).Should(HaveLen(numPfs))
					}
					if numVfs > 0 {
						Expect(devs[spyreconst.VfResourceName]).Should(HaveLen(numVfs))
					}
				} else {
					Expect(err).Should(Equal(expectedError))
				}
			},
				Entry("accept exact numbers", map[string]string{"app": "cardmgmt"}, 4, 4, nil),
				Entry("accept less numbers", map[string]string{"app": "cardmgmt"}, 3, 3, nil),

				Entry("accept unbalanced numbers", map[string]string{"app": "cardmgmt"}, 4, 3, nil),
				Entry("deny exceed numbers", map[string]string{"app": "cardmgmt"}, 5, 5,
					errors.New("not enough devices remain: 4/5 PFs and 4/5 VFs available/requested")),
				Entry("accept PF runner Pod", map[string]string{"aiucardmanager": "1"}, 1, 0, nil),
				Entry("accept VF runner Pod", map[string]string{"aiucardmanager": "1"}, 0, 1, nil),
				Entry("accept user Pod", nil, 4, 0, nil),
			)
		})
	})

	Context("device tree with PFs and isolated-VFs", func() {

		const nodeZ = "node-z"

		BeforeEach(func() {
			topoZ, err := os.ReadFile(filepath.Join("testdata", "topo-z-1.json"))
			Expect(err).To(BeNil())

			s := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{Name: nodeZ},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName: nodeZ,
					Pcitopo:  string(topoZ),
					SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
						{PciAddress: "0001:00:00.0", NumVfs: 0}, // PF
						{PciAddress: "0002:00:00.0", NumVfs: 0}, // PF
					},
					SpyreSSAInterfaces: []spyrev1alpha1.SpyreSSAInterface{
						{PciAddress: "0003:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
						{PciAddress: "0004:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
					},
				},
			}
			_, err = spyreClient.Create(ctx, s)
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			_ = spyreClient.Delete(ctx, nodeZ, &client.DeleteOptions{})
		})

		It("builds correct PF device tree", func() {
			s, err := spyreClient.GetSpyreNodeState(ctx, nodeZ)
			Expect(err).To(BeNil())

			tree, err := spyre.DeviceTree(s, false, "spyre_pf")
			Expect(err).To(BeNil())

			Expect(tree[0][0]).Should(HaveLen(2))
			Expect(tree[0][0]).Should(ContainElements("0001:00:00.0", "0002:00:00.0"))
		})

		It("builds correct isolated-VF device tree", func() {
			s, err := spyreClient.GetSpyreNodeState(ctx, nodeZ)
			Expect(err).To(BeNil())

			tree, err := spyre.DeviceTree(s, false, "spyre_vf")
			Expect(err).To(BeNil())

			Expect(tree[0][0]).Should(HaveLen(2))
			Expect(tree[0][0]).Should(ContainElements("0003:00:00.0", "0004:00:00.0"))
		})
	})

	Context("hasSSADevice function behavior", func() {

		DescribeTable("should return correct result based on VF types",
			func(hasRegularVFs bool, hasSSAVFs bool, expectedResult bool, description string) {
				// Create SpyreNodeState based on test parameters
				var spyresInterfaces []spyrev1alpha1.SpyreInterface
				var spyresSSAInterfaces []spyrev1alpha1.SpyreSSAInterface

				if hasRegularVFs {
					spyresInterfaces = []spyrev1alpha1.SpyreInterface{
						{PciAddress: "0001:00:00.0", NumVfs: 2, Vfs: []string{"0001:00:00.1", "0001:00:00.2"}},
					}
				} else {
					spyresInterfaces = []spyrev1alpha1.SpyreInterface{
						{PciAddress: "0001:00:00.0", NumVfs: 0}, // PF without VFs
					}
				}

				if hasSSAVFs {
					spyresSSAInterfaces = []spyrev1alpha1.SpyreSSAInterface{
						{PciAddress: "0003:00:00.0", Health: spyrev1alpha1.SpyreHealthy},
						{PciAddress: "0004:00:00.0", Health: spyrev1alpha1.SpyreHealthy},
					}
				}

				nodeState := &spyrev1alpha1.SpyreNodeState{
					Spec: spyrev1alpha1.SpyreNodeStateSpec{
						SpyreInterfaces:    spyresInterfaces,
						SpyreSSAInterfaces: spyresSSAInterfaces,
					},
				}

				// Test hasSSADevice function
				result := spyre.HasSSADevice(nodeState)
				Expect(result).To(Equal(expectedResult), description)
			},
			Entry("both regular VFs and SSA VFs exist", true, true, false, "should return false when both exist - regular VFs get priority"),
			Entry("only SSA VFs exist", false, true, true, "should return true when only SSA VFs exist"),
			Entry("only regular VFs exist", true, false, false, "should return false when only regular VFs exist"),
			Entry("no VFs exist", false, false, false, "should return false when no VFs exist"),
		)

		It("should document the expected behavior: regular VFs have higher priority", func() {
			// This test documents the current behavior:
			// When both regular VFs and SSA VFs exist, regular VFs are preferred
			// This ensures deterministic behavior even in unexpected scenarios

			nodeState := &spyrev1alpha1.SpyreNodeState{
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
						{PciAddress: "0001:00:00.0", NumVfs: 2, Vfs: []string{"0001:00:00.1", "0001:00:00.2"}}, // PF with regular VFs
					},
					SpyreSSAInterfaces: []spyrev1alpha1.SpyreSSAInterface{
						{PciAddress: "0003:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
						{PciAddress: "0004:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
					},
				},
			}

			// Verify the logic: hasSSA && !hasRegularVFs
			hasSSA := len(nodeState.Spec.SpyreSSAInterfaces) > 0
			hasRegularVFs := false
			for _, pf := range nodeState.Spec.SpyreInterfaces {
				if len(pf.Vfs) > 0 {
					hasRegularVFs = true
					break
				}
			}

			Expect(hasSSA).To(BeTrue(), "Should have SSA interfaces")
			Expect(hasRegularVFs).To(BeTrue(), "Should have regular VFs")

			// The result should be false because both exist
			result := hasSSA && !hasRegularVFs
			Expect(result).To(BeFalse(), "hasSSADevice should return false when both exist")
		})
	})

	Context("device tree only containing isolated-VFs", func() {

		const nodeZ = "node-z"

		BeforeEach(func() {
			topoZ, err := os.ReadFile(filepath.Join("testdata", "topo-z-2.json"))
			Expect(err).To(BeNil())

			s := &spyrev1alpha1.SpyreNodeState{
				ObjectMeta: metav1.ObjectMeta{Name: nodeZ},
				Spec: spyrev1alpha1.SpyreNodeStateSpec{
					NodeName: nodeZ,
					Pcitopo:  string(topoZ),
					SpyreSSAInterfaces: []spyrev1alpha1.SpyreSSAInterface{
						{PciAddress: "0001:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
						{PciAddress: "0002:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
						{PciAddress: "0003:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
						{PciAddress: "0004:00:00.0", Health: spyrev1alpha1.SpyreHealthy}, // isolated VF
					},
				},
			}
			_, err = spyreClient.Create(ctx, s)
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			_ = spyreClient.Delete(ctx, nodeZ, &client.DeleteOptions{})
		})

		It("can select 4 VFs", func() {
			numDevs := 4
			numReqDevs := int64(numDevs)
			devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_vf", numReqDevs, nodeZ)
			Expect(err).To(BeNil())
			Expect(len(devs)).Should(BeNumerically("==", numDevs))
			Expect(devs).Should(ContainElements(
				"0001:00:00.0", "0002:00:00.0", "0003:00:00.0", "0004:00:00.0"))
		})

	})

	Context("5-PF peer2 topology", func() {

		const nodeName = "z1" // defined in spyrens-z-1.json

		BeforeEach(func() {
			f, err := os.Open(filepath.Join("testdata", "spyrens-z-1.json"))
			Expect(err).To(BeNil())
			defer f.Close()
			d, err := io.ReadAll(f)
			Expect(err).To(BeNil())
			s := &spyrev1alpha1.SpyreNodeState{}
			err = json.Unmarshal(d, s)
			Expect(err).To(BeNil())
			s, err = spyreClient.Create(ctx, s)
			Expect(err).To(BeNil())
			s.Status.AllocationList = []spyrev1alpha1.Allocation{
				{
					DeviceList: []string{"0001:00:00.0"},
					Pod: &spyrev1alpha1.Pod{
						Name:      "p1",
						Namespace: "n1",
					},
					ResourcePool: "spyre_pf",
				}}
			_, err = spyreClient.UpdateStatus(ctx, s, true)
			Expect(err).To(BeNil())
		})

		AfterEach(func() {
			_ = spyreClient.Delete(ctx, nodeName, &client.DeleteOptions{})
		})

		It("builds correct PF device tree", func() {
			numDevs := 4
			numReqDevs := int64(numDevs)
			devs, _, err := spyre.SelectDevices(ap, ctx, false, "spyre_pf", numReqDevs, nodeName)
			Expect(err).To(BeNil())
			Expect(len(devs)).Should(BeNumerically("==", numDevs))
			Expect(devs).Should(ContainElements(
				"0002:00:00.0", "0004:00:00.0", "0005:00:00.0", "0003:00:00.0"))
		})
	})
})

func createNewNamespace(ctx context.Context, k8sClient client.Client) string {
	namespace := "ns" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	ns := &corev1.Namespace{}
	ns.Name = namespace
	err := k8sClient.Create(ctx, ns)
	Expect(err).To(BeNil())
	return namespace
}

// getSpyreIf returns SpyreInterface list for test/data/topo.json.
// this will be updated in future test update for tier2 requests.
func getSpyreIf() []spyrev1alpha1.SpyreInterface {
	return []spyrev1alpha1.SpyreInterface{
		{PciAddress: "0000:1c:00.0", NumVfs: 4,
			Vfs: []string{"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3", "0000:1c:00.4"}},
		{PciAddress: "0000:1f:00.0", NumVfs: 4,
			Vfs: []string{"0000:1f:00.1", "0000:1f:00.2", "0000:1f:00.3", "0000:1f:00.4"}},
		{PciAddress: "0000:20:00.0", NumVfs: 4,
			Vfs: []string{"0000:20:00.1", "0000:20:00.2", "0000:20:00.3", "0000:20:00.4"}},
		{PciAddress: "0000:36:00.0", NumVfs: 4,
			Vfs: []string{"0000:36:00.1", "0000:36:00.2", "0000:36:00.3", "0000:36:00.4"}},
		{PciAddress: "0000:39:00.0", NumVfs: 4,
			Vfs: []string{"0000:39:00.1", "0000:39:00.2", "0000:39:00.3", "0000:39:00.4"}},
		{PciAddress: "0000:3d:00.0", NumVfs: 4,
			Vfs: []string{"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3", "0000:3d:00.4"}},
		{PciAddress: "0000:9c:00.0", NumVfs: 4,
			Vfs: []string{"0000:9c:00.1", "0000:9c:00.2", "0000:9c:00.3", "0000:9c:00.4"}},
		{PciAddress: "0000:9d:00.0", NumVfs: 4,
			Vfs: []string{"0000:9d:00.1", "0000:9d:00.2", "0000:9d:00.3", "0000:9d:00.4"}},
		{PciAddress: "0000:b9:00.0", NumVfs: 4,
			Vfs: []string{"0000:b9:00.1", "0000:b9:00.2", "0000:b9:00.3", "0000:b9:00.4"}},
	}
}

func prepareSpyreNodeState(allocList []string, reserveList [][]string, pus []spyrev1alpha1.Pod) {
	var err error
	ctx := context.Background()

	nodeList := []string{"node1", "node2"}

	//          +------------+
	//          |            |
	//     +----+---+        +--+
	//     |        |        |  |
	//  +--+--+  +--+--+  +--+  |
	//  |  |  |  |  |  |  |  |  |
	//  1c 1f 20 36 39 3d 9c 9d b9
	pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo.json"))

	Expect(err).To(BeNil())
	for _, node := range nodeList {
		s := &spyrev1alpha1.SpyreNodeState{
			ObjectMeta: metav1.ObjectMeta{
				Name: node,
			},
			Spec: spyrev1alpha1.SpyreNodeStateSpec{
				NodeName:        node,
				Pcitopo:         string(pcitopo),
				SpyreInterfaces: getSpyreIf(),
			},
		}
		s, err = spyreClient.Create(ctx, s)
		Expect(err).To(BeNil())
		ns := "ns" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
		s.Status.AllocationList = []spyrev1alpha1.Allocation{{
			DeviceList: allocList,
			Pod:        &spyrev1alpha1.Pod{Name: "p0", Namespace: ns}}}
		s.Status.Reservations = map[string]spyrev1alpha1.Reservation{"spyre_pf": {
			DeviceSets:          reserveList,
			PodsUnderScheduling: pus}}
		s, err = spyreClient.UpdateStatus(ctx, s, true)
		Expect(err).To(BeNil())
		Expect(len(s.Status.AllocationList[0].DeviceList)).Should(BeNumerically("==", len(allocList)))
		Expect(len(s.Status.Reservations["spyre_pf"].DeviceSets[0])).Should(BeNumerically("==", len(reserveList[0])))
	}
	nsList, err := spyreClient.List(ctx, &client.ListOptions{})
	Expect(err).To(BeNil())
	Expect(len(nsList.Items)).Should(Equal(len(nodeList)))

}

func prepareUnbalancedSpyreNodeState(allocList []string, reserveList [][]string, pus []spyrev1alpha1.Pod) []string {
	var err error
	ctx := context.Background()

	nodeList := []string{"node1", "node2"}

	// node1: small-size (no tier2 relations)
	// not a target of allocList and reserveList
	pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-2.json"))
	Expect(err).To(BeNil())
	s := &spyrev1alpha1.SpyreNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeList[0],
		},
		Spec: spyrev1alpha1.SpyreNodeStateSpec{
			NodeName: nodeList[0],
			Pcitopo:  string(pcitopo),
			SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
				{PciAddress: "0000:1a:00.0", NumVfs: 2,
					Vfs: []string{"0000:1a:00.1", "0000:1a:00.2"}},
				{PciAddress: "0000:1b:00.0", NumVfs: 4,
					Vfs: []string{"0000:1b:00.1", "0000:1b:00.2"}},
				{PciAddress: "0000:2a:00.0", NumVfs: 2,
					Vfs: []string{"0000:2a:00.1", "0000:2a:00.2"}},
				{PciAddress: "0000:2b:00.0", NumVfs: 4,
					Vfs: []string{"0000:2b:00.1", "0000:2b:00.2"}},
			},
		},
	}
	_, err = spyreClient.Create(ctx, s)
	Expect(err).To(BeNil())

	// node2: large-size
	//          +------------+
	//          |            |
	//     +----+---+        +--+
	//     |        |        |  |
	//  +--+--+  +--+--+  +--+  |
	//  |  |  |  |  |  |  |  |  |
	//  1c 1f 20 36 39 3d 9c 9d b9
	pcitopo, err = os.ReadFile(filepath.Join("testdata", "topo.json"))
	Expect(err).To(BeNil())

	s = &spyrev1alpha1.SpyreNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeList[1],
		},
		Spec: spyrev1alpha1.SpyreNodeStateSpec{
			NodeName:        nodeList[1],
			Pcitopo:         string(pcitopo),
			SpyreInterfaces: getSpyreIf(),
		},
	}
	s, err = spyreClient.Create(ctx, s)
	Expect(err).To(BeNil())
	ns := "ns" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	s.Status.AllocationList = []spyrev1alpha1.Allocation{{
		DeviceList: allocList,
		Pod:        &spyrev1alpha1.Pod{Name: "p0", Namespace: ns}}}
	s.Status.Reservations = map[string]spyrev1alpha1.Reservation{"spyre_pf": {
		DeviceSets:          reserveList,
		PodsUnderScheduling: pus}}
	s, err = spyreClient.UpdateStatus(ctx, s, true)
	Expect(err).To(BeNil())
	Expect(len(s.Status.AllocationList[0].DeviceList)).Should(BeNumerically("==", len(allocList)))
	Expect(len(s.Status.Reservations["spyre_pf"].DeviceSets[0])).Should(BeNumerically("==", len(reserveList[0])))

	nsList, err := spyreClient.List(ctx, &client.ListOptions{})
	Expect(err).To(BeNil())
	Expect(len(nsList.Items)).Should(Equal(len(nodeList)))

	return nodeList
}

func setupTestenv() {
	crdPath := filepath.Join("..", "..", "config", "crd", "external")
	_, err := os.Stat(crdPath)
	Expect(err).To(BeNil(), "%v not exist", crdPath)
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

}
