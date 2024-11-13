package resource

import (
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	v1 "k8s.io/api/core/v1"
)

type FakeResource struct {
	AddPodFunc    func(*v1.Pod, v1alpha1.IOBandwidth) error
	RemovePodFunc func(*v1.Pod) error
}

func (f *FakeResource) Name() string {
	return "FakeResource"
}

func (f *FakeResource) AddPod(pod *v1.Pod, request v1alpha1.IOBandwidth) error {
	return f.AddPodFunc(pod, request)
}

func (f *FakeResource) RemovePod(pod *v1.Pod) error {
	return f.RemovePodFunc(pod)
}

func (f *FakeResource) PrintInfo() {
}
