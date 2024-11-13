package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	diskiov1alpha1 "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/scheduler-plugins/pkg/diskioaware/normalizer"
)

func getModelPluginBytes(modelName string) ([]byte, error) {
	if len(modelName) == 0 {
		return nil, fmt.Errorf("file name is empty")
	}
	p, err := os.ReadFile(fmt.Sprintf("../../pkg/diskioaware/sampleplugin/%s/%v.so", modelName, modelName))
	if err != nil {
		return nil, fmt.Errorf("failed to %s.so: %v", modelName, err)
	}
	return p, nil
}

func composeDiskModelConfig(url string) (string, error) {
	if len(url) == 0 {
		return "", fmt.Errorf("url is empty")
	}
	config := normalizer.PlConfig{
		Vendor: "Intel",
		Model:  "P4510",
		URL:    url,
	}

	pl := &normalizer.PlList{config}
	// Marshal the struct to JSON
	jsonData, err := json.Marshal(pl)
	if err != nil {
		return "", err
	}

	// Write the JSON data to a file
	f, err := os.CreateTemp("/tmp", "diskVendors")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err = f.Write(jsonData); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// NodeDiskDevice wrapper
type nddWrapper struct{ *diskiov1alpha1.NodeDiskDevice }

func MakeNodeDiskDevice(namespace, name string) *nddWrapper {
	nt := &diskiov1alpha1.NodeDiskDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return &nddWrapper{nt}
}

func (nd *nddWrapper) Name(name string) *nddWrapper {
	nd.NodeDiskDevice.Name = name
	return nd
}

func (nd *nddWrapper) Namespace(namespace string) *nddWrapper {
	nd.NodeDiskDevice.Namespace = namespace
	return nd
}

func (nd *nddWrapper) Spec(spec diskiov1alpha1.NodeDiskDeviceSpec) *nddWrapper {
	nd.NodeDiskDevice.Spec = spec
	return nd
}

func (nd *nddWrapper) Status(status diskiov1alpha1.NodeDiskDeviceStatus) *nddWrapper {
	nd.NodeDiskDevice.Status = status
	return nd
}

func (nd *nddWrapper) Obj() *diskiov1alpha1.NodeDiskDevice {
	return nd.NodeDiskDevice
}

func cleanupNodeDiskDevices(ctx context.Context, client versioned.Interface, ndd []*diskiov1alpha1.NodeDiskDevice) {
	for _, n := range ndd {
		err := client.DiskioV1alpha1().NodeDiskDevices(n.Namespace).Delete(ctx, n.Name, metav1.DeleteOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to clean up NodeDiskDevice",
				"NodeDiskDevice", n)
		}
	}
	klog.Infof("cleaned up NodeDiskDevice %d objects", len(ndd))
}

func cleanupNodeDiskIOStats(ctx context.Context, client versioned.Interface, ns string, names []string) {
	for _, name := range names {
		err := client.DiskioV1alpha1().NodeDiskIOStatses(ns).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to clean up NodeDiskIOStats", "NodeDiskIOStats", name)
		}
	}
	klog.Infof("cleaned up NodeDiskIOStats %d objects", len(names))
}

func createNodeDiskDevices(ctx context.Context, client versioned.Interface, ndds []*diskiov1alpha1.NodeDiskDevice) error {
	for _, ndd := range ndds {
		_, err := client.DiskioV1alpha1().NodeDiskDevices(ndd.Namespace).Create(ctx, ndd, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}
