package resource

import (
	"sync"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"k8s.io/client-go/kubernetes"
)

type FakeHandle struct {
	HandleBase
	client                  kubernetes.Interface
	UpdateNodeIOStatusFunc  func(n string, nodeIoBw v1alpha1.NodeDiskIOStatsStatus) error
	DeleteCacheNodeInfoFunc func(string) error
	NodePressureRatioFunc   func(string, v1alpha1.IOBandwidth) (float64, error)
	sync.RWMutex
}

// ioiresource.Handle interface
func (h *FakeHandle) Name() string {
	return "FakeHandle"
}

func (h *FakeHandle) Run(c ExtendedCache, cli kubernetes.Interface) error {
	h.EC = c
	h.client = cli
	return nil
}

func (h *FakeHandle) CanAdmitPod(string, v1alpha1.IOBandwidth) (bool, error) {
	return true, nil
}

func (h *FakeHandle) NodePressureRatio(n string, bw v1alpha1.IOBandwidth) (float64, error) {
	return h.NodePressureRatioFunc(n, bw)
}

func (h *FakeHandle) IsIORequired(annotations map[string]string) bool {
	return true
}

func (h *FakeHandle) AddCacheNodeInfo(string, map[string]v1alpha1.DiskDevice) {
}

func (h *FakeHandle) DeleteCacheNodeInfo(nodeName string) error {
	return h.DeleteCacheNodeInfoFunc(nodeName)
}

func (h *FakeHandle) UpdateCacheNodeStatus(n string, nodeIoBw v1alpha1.NodeDiskIOStatsStatus) error {
	return h.UpdateNodeIOStatusFunc(n, nodeIoBw)
}

func (h *FakeHandle) GetDiskNormalizeModel(string) (string, error) {
	return "", nil
}
