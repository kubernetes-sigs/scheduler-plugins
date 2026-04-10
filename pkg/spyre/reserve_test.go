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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	spyrev1alpha1 "github.com/ibm-aiu/spyre-operator/api/v1alpha1"
	spyreconst "github.com/ibm-aiu/spyre-operator/const"
	spyreclient "github.com/ibm-aiu/spyre-operator/pkg/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/pkg/spyre"
)

const (
	nodeName     = "node1"
	spyrepolName = "spyreclusterpolicy"
)

var _ = Describe("Reserve", Ordered, func() {

	var err error
	var ap *spyre.SpyrePlugin
	var opNs string
	var appNs string
	ctx := context.Background()

	BeforeAll(func() {
		spyreClient, err = spyreclient.NewClient(ctx, cfg)
		Expect(err).To(BeNil())
		Expect(spyreClient).NotTo((BeNil()))
		ap = &spyre.SpyrePlugin{}
		ap.ExportSetSpyreClient(spyreClient)
		ap.ExportSetK8sClient(k8sClient)
	})

	Context("reservation", func() {

		Context("no spyre request", func() {

			It("does nothing for non-spyre Pod", func() {
				p := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "container",
								Image:   "image",
								Command: []string{"run.sh"},
							},
						},
					},
				}
				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())
			})
		})

		Context("basic pf functionalities", func() {

			BeforeEach(func() {

				opNs = createNewNamespace(ctx, k8sClient)
				appNs = createNewNamespace(ctx, k8sClient)
				Expect(err).To(BeNil())

				path := filepath.Join("testdata", "spyrens-1.json")
				alloc := []spyrev1alpha1.Allocation{{
					DeviceList: []string{"0000:1a:00.0"},
					Pod:        &spyrev1alpha1.Pod{Name: "p0", Namespace: appNs}}}
				reserve := map[string]spyrev1alpha1.Reservation{"spyre_pf": {
					PodsUnderScheduling: []spyrev1alpha1.Pod{{Name: "p1", Namespace: appNs}},
					DeviceSets:          [][]string{{"0000:1c:00.0"}},
				},
				}
				err = configureSpyreNodeState(ctx, opNs, appNs, path, alloc, reserve)
				p := buildPod("p1", appNs, "spyre_pf", 1)
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())

				ctx = context.Background()
				pol := &spyrev1alpha1.SpyreClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: spyrepolName},
				}
				pol, err := spyreClient.CreateSpyreClusterPolicy(ctx, pol)
				Expect(err).To(BeNil())
				pol.Status.Namespace = "aiu-operator"
				pol.Status.State = spyrev1alpha1.Ready
				_, err = spyreClient.UpdateSpyreClusterPolicyStatus(ctx, pol, true)
				Expect(err).To(BeNil())

			})

			AfterEach(func() {
				spyreClient.DeleteAll(ctx)
				ctx = context.Background()
				err := spyreClient.DeleteSpyreClusterPolicy(ctx, spyrepolName, &client.DeleteOptions{})
				Expect(err).To(BeNil())
			})

			Specify("request values are accessible even if they are not defined", func() {
				p := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "container",
								Image:   "image",
								Command: []string{"run.sh"},
							},
						},
					},
				}
				n := p.Spec.Containers[0].Resources.Requests[spyreconst.PfResourceName]
				numPfReq, ok := n.AsInt64()
				Expect(ok).Should(BeTrue())
				n, err = resource.ParseQuantity("0")
				Expect(err).To(BeNil())
				zero, ok := n.AsInt64()
				Expect(ok).Should(BeTrue())
				Expect(numPfReq).Should(Equal(zero))
				n = p.Spec.Containers[0].Resources.Requests[spyreconst.VfResourceName]
				numVfReq, ok := n.AsInt64()
				Expect(ok).Should(BeTrue())
				Expect(numVfReq).Should(Equal(zero))

			})

			DescribeTable("policy-aware reservation", func(resourceName string) {

				By("prepare Pod for scheduling")
				p := buildPod("px", appNs, resourceName, 2)
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())
				p2 := &corev1.Pod{}
				err = k8sClient.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: appNs}, p2)
				Expect(err).To(BeNil())

				By("reserve devices")
				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())

				By("ensure reservation")
				s, err := spyreClient.Get(ctx, nodeName)
				Expect(err).To(BeNil())

				if resourceName == "spyre_pf" {
					// "spyre_pf" is already reserved in BeforeEach(), and "p" requests two SPYREs at once
					// -> deviceSets should be: [["36"], ["xx", "yy"]] (length: 2)
					Expect(len(s.Status.Reservations[resourceName].DeviceSets)).Should(Equal(2))
					Expect(len(s.Status.Reservations[resourceName].DeviceSets[1])).Should(Equal(2))
				} else if strings.HasPrefix(resourceName, "spyre_pf") {
					// "spyre_pf_xx" must be mapped to an 1-element slice containing two SPYREs.
					Expect(len(s.Status.Reservations[resourceName].DeviceSets)).Should(Equal(1))
					Expect(len(s.Status.Reservations[resourceName].DeviceSets[0])).Should(Equal(2))
				}
			},
				Entry("plain spyre_pf", "spyre_pf"),
				Entry("spyre_pf_tier0", "spyre_pf_tier0"),
			)

			It("can reserve specific spyre_pf", func() {

				deviceId := "0000:41:00.0"
				resourceName := "spyre_pf_" + strings.ReplaceAll(deviceId, ":", "_")

				By("prepare Pod for scheduling")
				p := buildPod("px", appNs, resourceName, 1)
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())
				p2 := &corev1.Pod{}
				err = k8sClient.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: appNs}, p2)
				Expect(err).To(BeNil())

				By("reserve devices")
				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())

				By("ensure reservation")
				s, err := spyreClient.Get(ctx, nodeName)
				Expect(err).To(BeNil())

				resv, exists := s.Status.Reservations[resourceName]
				Expect(exists).Should(BeTrue())
				Expect(resv.DeviceSets).Should(HaveExactElements([][]string{{deviceId}}))

			})

			It("can reserve re-entered stuck Pod", func() {
				s, err := spyreClient.Get(ctx, "node1")
				Expect(err).To(BeNil())
				s.Status.AllocationList = []spyrev1alpha1.Allocation{}
				s.Status.Reservations = map[string]spyrev1alpha1.Reservation{
					"spyre_pf": {
						PodsUnderScheduling: []spyrev1alpha1.Pod{{Name: "p2", Namespace: appNs}},
						DeviceSets: [][]string{{
							"0000:1a:00.0", "0000:1c:00.0", "0000:1d:00.0", "0000:1e:00.0",
							"0000:3d:00.0", "0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"}},
					},
				}

				_, err = spyreClient.UpdateStatus(ctx, s, true)
				Expect(err).To(BeNil())
				q, err := resource.ParseQuantity("8")
				Expect(err).To(BeNil())

				p2 := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + spyreconst.PfResourceName): q},
								}}}},
					Status: corev1.PodStatus{Phase: "Pending"},
				}
				err = k8sClient.Create(ctx, p2)
				Expect(err).To(BeNil())
				err = k8sClient.Get(ctx, client.ObjectKey{Namespace: appNs, Name: "p2"}, p2)
				Expect(err).To(BeNil())

				By("reserve devices")
				err = spyre.ReserveDevices(ap, ctx, p2, "node1")
				Expect(err).To(BeNil())

			})
		})

		Context("pf allocation for various topologies", func() {

			BeforeEach(func() {
				opNs = createNewNamespace(ctx, k8sClient)
				appNs = createNewNamespace(ctx, k8sClient)
				Expect(err).To(BeNil())
				ctx = context.Background()
				pol := &spyrev1alpha1.SpyreClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: spyrepolName},
				}
				pol, err := spyreClient.CreateSpyreClusterPolicy(ctx, pol)
				Expect(err).To(BeNil())
				pol.Status.Namespace = "aiu-operator"
				pol.Status.State = spyrev1alpha1.Ready
				_, err = spyreClient.UpdateSpyreClusterPolicyStatus(ctx, pol, true)
				Expect(err).To(BeNil())
			})

			AfterEach(func() {
				spyreClient.DeleteAll(ctx)
				ctx = context.Background()
				err := spyreClient.DeleteSpyreClusterPolicy(ctx, spyrepolName, &client.DeleteOptions{})
				Expect(err).To(BeNil())
			})

			It("can reserve best 2/4 SPYREs", func() {

				path := filepath.Join("testdata", "spyrens-1.json")
				alloc := []spyrev1alpha1.Allocation{{
					DeviceList: []string{"0000:1a:00.0"},
					Pod:        &spyrev1alpha1.Pod{Name: "p0", Namespace: appNs}}}
				reserve := map[string]spyrev1alpha1.Reservation{"spyre_pf": {
					PodsUnderScheduling: []spyrev1alpha1.Pod{{Name: "p1", Namespace: appNs}},
					DeviceSets:          [][]string{{"0000:1c:00.0"}},
				},
				}
				err = configureSpyreNodeState(ctx, opNs, appNs, path, alloc, reserve)
				Expect(err).To(BeNil())
				p := buildPod("p1", appNs, "spyre_pf", 1)
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())

				devs := []int{2, 4}
				for idx, d := range devs {
					p := buildPod(fmt.Sprintf("px%d", idx), appNs, "spyre_pf", d)
					err = k8sClient.Create(ctx, p)
					Expect(err).To(BeNil())
					p2 := &corev1.Pod{}
					err = k8sClient.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: appNs}, p2)
					Expect(err).To(BeNil())

					By("reserve devices")
					err = spyre.ReserveDevices(ap, ctx, p, nodeName)
					Expect(err).To(BeNil())
				}

				By("ensure reservation")
				s, err := spyreClient.Get(ctx, nodeName)
				Expect(err).To(BeNil())

				// DeviceSets[0] is created at BeforeEach().
				// group 1: [1c], 1d, 1e
				// group 2: 3d, 3f, 40, 41
				// two requests should use 1d, 1e
				// four requests should use group 2
				Expect(s.Status.Reservations["spyre_pf"].DeviceSets[1]).Should(ContainElements("0000:1d:00.0", "0000:1e:00.0"))
				Expect(len(s.Status.Reservations["spyre_pf"].DeviceSets[2])).Should(Equal(4))
			})

			It("can reserve 6 SPYREs in 33-33 tree", func() {

				path := filepath.Join("testdata", "spyrens-3.json")
				alloc := []spyrev1alpha1.Allocation{}
				reserve := map[string]spyrev1alpha1.Reservation{}
				err = configureSpyreNodeState(ctx, opNs, appNs, path, alloc, reserve)
				Expect(err).To(BeNil())

				p := buildPod("px1", appNs, "spyre_pf_tier1", 6)
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())

				By("reserve devices")
				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())

				By("ensure reservation")
				s, err := spyreClient.Get(ctx, nodeName)
				Expect(err).To(BeNil())

				Expect(s.Status.Reservations["spyre_pf_tier1"].DeviceSets[0]).Should(
					ContainElements(
						"0000:29:00.0", "0000:2a:00.0", "0000:2b:00.0",
						"0000:3a:00.0", "0000:3b:00.0", "0000:3c:00.0"))
			})

			DescribeTable("uniformly reserves device from tier1/2 group",
				func(resourceName string, numDev int, alloc []spyrev1alpha1.Allocation, expectedTree [][][]string) {

					By("prepare SpyreNodeState")
					// spyrens-2.json
					// +----------- tier1 ----------+    +----------- tier1 -----------+
					// +--- tier0 --+  +--- tier0 --+    +--- tier0 --+   +--- tier0 --+
					// [2e] [2f] [30]  [3f] [40] [41]    [af] [b0] [b1]   [c1] [c2] [c3]
					path := filepath.Join("testdata", "spyrens-2.json")

					reserve := map[string]spyrev1alpha1.Reservation{}

					err = configureSpyreNodeState(ctx, opNs, appNs, path, alloc, reserve)
					Expect(err).To(BeNil())

					By("prepare Pod for scheduling")
					p := buildPod("px", appNs, resourceName, numDev)
					err = k8sClient.Create(ctx, p)
					Expect(err).To(BeNil())
					p2 := &corev1.Pod{}
					err = k8sClient.Get(ctx, client.ObjectKey{Name: p.Name, Namespace: appNs}, p2)
					Expect(err).To(BeNil())

					By("reserve devices")
					err = spyre.ReserveDevices(ap, ctx, p, nodeName)
					Expect(err).To(BeNil())

					By("ensure reservation")
					s, err := spyreClient.Get(ctx, nodeName)
					Expect(err).To(BeNil())

					tree, err := spyre.DeviceTree(s, false, resourceName)
					Expect(err).To(BeNil())

					Expect(tree).Should(Equal(expectedTree))

				},
				Entry("tier1x1-0000", "spyre_pf_tier1", 1,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x1-0300", "spyre_pf_tier1", 1,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"}},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x1-3330", "spyre_pf_tier1", 1,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{
						"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0",
						"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0",
						"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"}},
					[][][]string{
						{
							{},
							{}},
						{
							{},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x2-0000", "spyre_pf_tier1", 2,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x2-3000", "spyre_pf_tier1", 2,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{
						"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0",
					},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"}},
					[][][]string{
						{
							{},
							{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x2-3300", "spyre_pf_tier1", 2,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{
						"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0",
						"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"}},
					[][][]string{
						{
							{},
							{}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x4-0000", "spyre_pf_tier1", 4,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:30:00.0"},
							{"0000:41:00.0"}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x4-0001", "spyre_pf_tier1", 4,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:c2:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"},
					},
					[][][]string{
						{
							{"0000:30:00.0"},
							{"0000:41:00.0"}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier1x4-0200", "spyre_pf_tier1", 4,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:40:00.0", "0000:41:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier1"},
					},
					[][][]string{
						{
							{"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0"},
							{"0000:3f:00.0"}},
						{
							{"0000:b1:00.0"},
							{"0000:c3:00.0"},
						}}),
				Entry("tier2x1-0000", "spyre_pf_tier2", 1,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x1-0300", "spyre_pf_tier2", 1,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier2"}},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{}},
						{
							{"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x1-3330", "spyre_pf_tier2", 1,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{
						"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0",
						"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0",
						"0000:af:00.0", "0000:b0:00.0", "0000:b1:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier2"}},
					[][][]string{
						{
							{},
							{}},
						{
							{},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x2-0000", "spyre_pf_tier2", 2,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:3f:00.0", "0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x2-3000", "spyre_pf_tier2", 2,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{
						"0000:2e:00.0", "0000:2f:00.0", "0000:30:00.0",
					},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier2"}},
					[][][]string{
						{
							{},
							{"0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x4-0000", "spyre_pf_tier2", 4,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x4-0001", "spyre_pf_tier2", 4, //
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:c2:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier2"},
					},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{"0000:40:00.0", "0000:41:00.0"}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c3:00.0"},
						}}),
				Entry("tier2x4-0200", "spyre_pf_tier2", 4,
					[]spyrev1alpha1.Allocation{{DeviceList: []string{"0000:40:00.0", "0000:41:00.0"},
						Pod: &spyrev1alpha1.Pod{Name: "px", Namespace: appNs}, ResourcePool: "spyre_pf_tier2"},
					},
					[][][]string{
						{
							{"0000:2f:00.0", "0000:30:00.0"},
							{}},
						{
							{"0000:b0:00.0", "0000:b1:00.0"},
							{"0000:c2:00.0", "0000:c3:00.0"},
						}}),
				Entry("tier2x8-0000", "spyre_pf_tier2", 8,
					[]spyrev1alpha1.Allocation{},
					[][][]string{
						{
							{"0000:30:00.0"},
							{"0000:41:00.0"}},
						{
							{"0000:b1:00.0"},
							{"0000:c3:00.0"},
						}}),
			)
		})

		Context("in 8 VFs", func() {

			BeforeEach(func() {

				opNs = createNewNamespace(ctx, k8sClient)
				appNs = createNewNamespace(ctx, k8sClient)
				pcitopo, err := os.ReadFile(filepath.Join("testdata", "topo-1.json"))
				Expect(err).To(BeNil())

				s := &spyrev1alpha1.SpyreNodeState{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
					Spec: spyrev1alpha1.SpyreNodeStateSpec{
						NodeName: nodeName,
						SpyreInterfaces: []spyrev1alpha1.SpyreInterface{
							{PciAddress: "0000:1a:00.0", NumVfs: 4,
								Vfs: []string{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3", "0000:1a:00.4"}},
							{PciAddress: "0000:1c:00.0", NumVfs: 4,
								Vfs: []string{"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3", "0000:1c:00.4"}},
							{PciAddress: "0000:1d:00.0", NumVfs: 4,
								Vfs: []string{"0000:1d:00.1", "0000:1d:00.2", "0000:1d:00.3", "0000:1d:00.4"}},
							{PciAddress: "0000:1e:00.0", NumVfs: 4,
								Vfs: []string{"0000:1e:00.1", "0000:1e:00.2", "0000:1e:00.3", "0000:1e:00.4"}},
							{PciAddress: "0000:3d:00.0", NumVfs: 4,
								Vfs: []string{"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3", "0000:3d:00.4"}},
							{PciAddress: "0000:3f:00.0", NumVfs: 4,
								Vfs: []string{"0000:3f:00.1", "0000:3f:00.2", "0000:3f:00.3", "0000:3f:00.4"}},
							{PciAddress: "0000:40:00.0", NumVfs: 4,
								Vfs: []string{"0000:40:00.1", "0000:40:00.2", "0000:40:00.3", "0000:40:00.4"}},
							{PciAddress: "0000:41:00.0", NumVfs: 4,
								Vfs: []string{"0000:41:00.1", "0000:41:00.2", "0000:41:00.3", "0000:41:00.4"}},
						},
						Pcitopo: string(pcitopo),
					},
				}
				_, err = spyreClient.Create(ctx, s)
				Expect(err).To(BeNil())

				oneQuant, err := resource.ParseQuantity("1")
				Expect(err).To(BeNil())
				err = k8sClient.Create(ctx, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
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

				ctx = context.Background()
				p := &spyrev1alpha1.SpyreClusterPolicy{
					ObjectMeta: metav1.ObjectMeta{Name: spyrepolName},
				}
				p, err = spyreClient.CreateSpyreClusterPolicy(ctx, p)
				Expect(err).To(BeNil())
				p.Status.Namespace = opNs
				p.Status.State = spyrev1alpha1.Ready
				_, err = spyreClient.UpdateSpyreClusterPolicyStatus(ctx, p, true)
				Expect(err).To(BeNil())
			})

			AfterEach(func() {
				ctx = context.Background()
				err := spyreClient.DeleteSpyreClusterPolicy(ctx, spyrepolName, &client.DeleteOptions{})
				Expect(err).To(BeNil())
			})

			It("can reserve specific VF", func() {

				oneQuant, err := resource.ParseQuantity("1")
				Expect(err).To(BeNil())

				dev := "0000:41:00.0"
				r := fmt.Sprintf("%s_%s", spyreconst.VfResourceName,
					strings.ReplaceAll(dev, ":", "_"))

				p := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "px", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + r): oneQuant},
								}}}}}

				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())
				s, err := spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				reservedPrefix := strings.Split(s.Status.Reservations[r].DeviceSets[0][0], ".")[0]
				reservedSuffix := strings.Split(s.Status.Reservations[r].DeviceSets[0][0], ".")[1]
				devPrefix := strings.Split(dev, ".")[0]
				Expect(reservedPrefix).Should(Equal(devPrefix))
				Expect(reservedSuffix).ShouldNot(Equal("0"))
			})

			It("can show error when user requests allocated VF", func() {

				oneQuant, err := resource.ParseQuantity("1")
				Expect(err).To(BeNil())
				dev := "0000:41:00.0"
				r := fmt.Sprintf("%s_%s", spyreconst.VfResourceName,
					strings.ReplaceAll(dev, ":", "_"))

				s, err := spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				devList := []string{"0000:41:00.1", "0000:41:00.2", "0000:41:00.3", "0000:41:00.4"}
				s.Status.AllocationList = []spyrev1alpha1.Allocation{{
					DeviceList:   devList,
					Pod:          &spyrev1alpha1.Pod{Name: "v1", Namespace: appNs},
					ResourcePool: spyreconst.VfResourceName,
				}}
				_, err = spyreClient.UpdateStatus(ctx, s, true)
				Expect(err).To(BeNil())

				p := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "px", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + r): oneQuant},
								}}}}}

				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).Should(Equal(
					fmt.Errorf("device unavailable: %s", r)))
				s, err = spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				Expect(s.Status.AllocationList[0].Pod.Namespace).Should(Equal(appNs))
				Expect(s.Status.AllocationList[0].Pod.Name).Should(Equal("v1"))
				Expect(s.Status.AllocationList[0].DeviceList).Should(ContainElements(devList))

			})

			It("can show error when user requests reserved VF", func() {

				oneQuant, err := resource.ParseQuantity("1")
				Expect(err).To(BeNil())
				fourQuant, err := resource.ParseQuantity("4")
				Expect(err).To(BeNil())

				dev := "0000:41:00.0"
				r := fmt.Sprintf("%s_%s", spyreconst.VfResourceName,
					strings.ReplaceAll(dev, ":", "_"))

				s, err := spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				devSets := [][]string{{"0000:41:00.1", "0000:41:00.2", "0000:41:00.3", "0000:41:00.4"}}
				s.Status.Reservations = map[string]spyrev1alpha1.Reservation{
					r: {
						DeviceSets:          devSets,
						PodsUnderScheduling: []spyrev1alpha1.Pod{{Namespace: appNs, Name: "v1"}},
					}}
				_, err = spyreClient.UpdateStatus(ctx, s, true)
				Expect(err).To(BeNil())

				p := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "v1", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + r): fourQuant},
								}}}}}
				err = k8sClient.Create(ctx, p)
				Expect(err).To(BeNil())
				p = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "px", Namespace: appNs},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "c1",
								Image: "image",
								Resources: corev1.ResourceRequirements{
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + r): oneQuant},
								}}}}}

				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).Should(Equal(
					fmt.Errorf("device unavailable: %s", r)))
				s, err = spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				Expect(s.Status.Reservations[r].PodsUnderScheduling[0].Name).Should(Equal("v1"))
				Expect(s.Status.Reservations[r].DeviceSets).Should(Equal(devSets))
			})

			DescribeTable("reserve VF device requested by card-management Pods", func(labels map[string]string, numReqDevs int, hasDotOneVf bool) {

				q, err := resource.ParseQuantity(strconv.Itoa(numReqDevs))
				Expect(err).To(BeNil())
				p := &corev1.Pod{
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
									Limits: map[corev1.ResourceName]resource.Quantity{
										corev1.ResourceName(spyreconst.ResourcePrefix + "/" + spyreconst.VfResourceName): q},
								},
							},
						},
					}}
				err = spyre.ReserveDevices(ap, ctx, p, nodeName)
				Expect(err).To(BeNil())
				s, err := spyreClient.GetSpyreNodeState(ctx, nodeName)
				Expect(err).To(BeNil())
				ds := s.Status.Reservations["spyre_vf"].DeviceSets
				Expect(ds).Should(HaveLen(1))

				Expect(ds[0]).Should(HaveLen(numReqDevs))

				if hasDotOneVf {
					Expect(s.Status.Reservations["spyre_vf"].DeviceSets[0][0]).Should(HaveSuffix(".1"))
				} else {
					Expect(s.Status.Reservations["spyre_vf"].DeviceSets[0][0]).ShouldNot(HaveSuffix(".1"))
				}

			},
				Entry("card-management Pod", map[string]string{"app": "cardmgmt"}, 8, true),
				Entry("card-management Pod (request smaller than existing)", map[string]string{"app": "cardmgmt"}, 7, true),
				Entry("PF VF runner Pod", map[string]string{"aiucardmanager": "1"}, 1, true),
				Entry("user Pod", nil, 4, false),
			)

			DescribeTable("reserve devices",
				func(numReq int, alloc []string, reserve [][]string, expected error) {

					// configure allocation/reservation
					s, err := spyreClient.GetSpyreNodeState(ctx, nodeName)
					Expect(err).To(BeNil())
					if len(alloc) > 0 {
						s.Status.AllocationList = []spyrev1alpha1.Allocation{{
							DeviceList:   alloc,
							Pod:          &spyrev1alpha1.Pod{Name: "p", Namespace: "a"},
							ResourcePool: spyreconst.VfResourceName,
						}}
					}

					if len(reserve) > 0 {

						pSched := make([]spyrev1alpha1.Pod, 0, len(reserve))
						for idx, r := range reserve {
							q, err := resource.ParseQuantity(strconv.Itoa(len(r)))
							Expect(err).To(BeNil())
							pName := fmt.Sprintf("pod%d", idx)
							pSched = append(pSched, spyrev1alpha1.Pod{Name: pName, Namespace: appNs})
							pReserve := &corev1.Pod{
								ObjectMeta: metav1.ObjectMeta{Name: pName, Namespace: appNs},
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
							err = k8sClient.Create(ctx, pReserve)
							Expect(err).To(BeNil())
						}
						s.Status.Reservations = map[string]spyrev1alpha1.Reservation{
							spyreconst.VfResourceName: {
								DeviceSets:          reserve,
								PodsUnderScheduling: pSched,
							},
						}
					}

					_, err = spyreClient.UpdateStatus(ctx, s, true)
					Expect(err).To(BeNil())
					s, err = spyreClient.GetSpyreNodeState(ctx, nodeName)
					Expect(err).To(BeNil())
					Expect(len(s.Status.Reservations[spyreconst.VfResourceName].DeviceSets)).Should(Equal(len(reserve)))

					q, err := resource.ParseQuantity(strconv.Itoa(numReq))
					Expect(err).To(BeNil())
					p := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: opNs},
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

					err = spyre.ReserveDevices(ap, ctx, p, nodeName)
					s, e2 := spyreClient.GetSpyreNodeState(ctx, nodeName)
					Expect(e2).To(BeNil())
					ds := s.Status.Reservations[spyreconst.VfResourceName].DeviceSets
					if expected == nil {
						// success case should create a new deviceSets
						Expect(err).To(BeNil(), "req=%d, alloc=%d, reserve=%d", numReq, len(alloc), len(reserve))
						Expect(ds[len(ds)-1]).Should(HaveLen(numReq))

						// assert no double-booking
						all := make([]string, 0, 64)
						for _, a := range s.Status.AllocationList {
							all = append(all, a.DeviceList...)

						}
						for _, d := range s.Status.Reservations[spyreconst.VfResourceName].DeviceSets {
							all = append(all, d...)
						}
						slices.Sort(all)
						unique := make([]string, len(all))
						copy(unique, all)
						unique = slices.Compact(unique)
						Expect(unique).Should(HaveLen(len(all)), "double booking: %v", all)

						if numReq <= 8 {
							// assert all VFs come from different PF
							devs := make([]string, 0, 8)
							for _, d := range ds[len(ds)-1] {
								devs = append(devs, strings.Split(d, ".")[0])
							}
							slices.Sort(devs)
							unique := make([]string, len(devs))
							copy(unique, devs)
							unique = slices.Compact(unique)
							Expect(unique).Should(HaveLen(len(devs)), "multiple VFs from same PF: %v", devs)
						}
					} else {
						// error cases should preserve initial reserves.
						Expect(err).To(Equal(expected), "req=%d, alloc=%d, reserve=%v", numReq, len(alloc), reserve)
						Expect(ds).Should(HaveLen(len(reserve)))
					}
				},

				Entry(nil, 1, []string{"0000:1a:00.1"}, [][]string{{"0000:1c:00.1"}}, nil),
				Entry(nil, 4, []string{"0000:1a:00.1"}, [][]string{{"0000:1c:00.1"}}, nil),
				Entry(nil, 8, []string{"0000:1a:00.1"}, [][]string{{"0000:1c:00.1"}}, nil),
				Entry(nil, 8, []string{}, [][]string{{"0000:1c:00.1"}}, nil),
				Entry(nil, 8,
					[]string{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3",
						"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3",
						"0000:1d:00.1", "0000:1d:00.2", "0000:1d:00.3",
						"0000:1e:00.1", "0000:1e:00.3", "0000:1e:00.4",
						"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3",
						"0000:3f:00.3", "0000:3f:00.4", "0000:3f:00.1",
						"0000:40:00.1", "0000:40:00.2", "0000:40:00.3",
						"0000:41:00.1", "0000:41:00.2", "0000:41:00.3"}, [][]string{}, nil),
				Entry(nil, 8, []string{},
					[][]string{
						{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3",
							"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3",
							"0000:1d:00.1", "0000:1d:00.2", "0000:1d:00.3"},
						{"0000:1e:00.1", "0000:1e:00.3", "0000:1e:00.4"},
						{"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3",
							"0000:3f:00.3", "0000:3f:00.4", "0000:3f:00.1",
							"0000:40:00.1", "0000:40:00.2", "0000:40:00.3",
							"0000:41:00.1", "0000:41:00.2", "0000:41:00.3"}}, nil),
				Entry(nil, 8,
					[]string{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3",
						"0000:1c:00.1", "0000:1c:00.2", "0000:1c:00.3",
						"0000:1d:00.1", "0000:1d:00.2", "0000:1d:00.3"},
					[][]string{
						{"0000:1e:00.1", "0000:1e:00.3", "0000:1e:00.4"},
						{"0000:3d:00.1", "0000:3d:00.2", "0000:3d:00.3",
							"0000:3f:00.3", "0000:3f:00.4", "0000:3f:00.1",
							"0000:40:00.1", "0000:40:00.2", "0000:40:00.3",
							"0000:41:00.1", "0000:41:00.2", "0000:41:00.3"}}, nil),
				Entry(nil, 8,
					[]string{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3"},
					[][]string{{"0000:1a:00.4"}},
					fmt.Errorf("failed to reserve enough devices: num_requested: 8, num_remaining: 7")),
				Entry(nil, 8,
					[]string{"0000:1a:00.1", "0000:1a:00.2", "0000:1a:00.3", "0000:1a:00.4"},
					[][]string{},
					fmt.Errorf("failed to reserve enough devices: num_requested: 8, num_remaining: 7")),
				Entry(nil, 8, []string{},
					[][]string{{"0000:1a:00.1"}, {"0000:1a:00.2", "0000:1a:00.3"}, {"0000:1a:00.4"}},
					fmt.Errorf("failed to reserve enough devices: num_requested: 8, num_remaining: 7")),
				Entry(nil, 8, []string{"0000:1a:00.1"},
					[][]string{{"0000:1a:00.2", "0000:1a:00.3"}, {"0000:1a:00.4"}},
					fmt.Errorf("failed to reserve enough devices: num_requested: 8, num_remaining: 7")),
				Entry(nil, 9, []string{}, [][]string{},
					fmt.Errorf("failed to reserve enough devices: num_requested: 9, num_remaining: 8")),
			)

			AfterEach(func() {
				spyreClient.DeleteAll(ctx)
			})
		})

	})
})

// configureSpyreNodeState creates SpyreNodeState for a node.
// It first unmarshals a JSON file into an SpyreNodeState object, and let the given
// namespace, nodeName, allocation and reservation to the object.
func configureSpyreNodeState(ctx context.Context, opNs, appNs, path string,
	alloc []spyrev1alpha1.Allocation, reserve map[string]spyrev1alpha1.Reservation) error {

	f, err := os.Open(path)
	Expect(err).To(BeNil())
	defer f.Close()
	d, err := io.ReadAll(f)
	Expect(err).To(BeNil())
	s := &spyrev1alpha1.SpyreNodeState{}
	err = json.Unmarshal(d, s)
	Expect(err).To(BeNil())
	s.Name = nodeName
	s.Namespace = opNs
	s.Spec.NodeName = nodeName
	s, err = spyreClient.Create(ctx, s)
	Expect(err).To(BeNil())
	s.Status.AllocationList = alloc
	s.Status.Reservations = reserve
	s, err = spyreClient.UpdateStatus(ctx, s, true)
	Expect(err).To(BeNil())
	Expect(s.Status.AllocationList).Should(HaveLen(len(alloc)))
	Expect(s.Status.Reservations).Should(HaveLen(len(reserve)))

	nsList, err := spyreClient.List(ctx, &client.ListOptions{})
	Expect(err).To(BeNil())
	Expect(nsList.Items).Should(HaveLen(1))

	return nil
}

// buildPod returns Pod which allocate devices for resource.
func buildPod(name string, namespace string, resourceName string, numDev int) *corev1.Pod {

	rr := make(map[corev1.ResourceName]resource.Quantity, 1)
	q, err := resource.ParseQuantity(fmt.Sprintf("%d", numDev))
	Expect(err).To(BeNil())
	k := corev1.ResourceName(spyreconst.ResourcePrefix + "/" + resourceName)
	rr[k] = q

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{},
			Name:        name,
			Namespace:   namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:      "c1",
					Image:     "image",
					Command:   []string{"run.sh"},
					Resources: corev1.ResourceRequirements{Limits: rr}},
			},
		},
	}
}
