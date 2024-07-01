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

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"reflect"
	"sort"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/generated/clientset/versioned"
	common "github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientretry "k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
)

type IORequest struct {
	Rbps      string `json:"rbps"`
	Wbps      string `json:"wbps"`
	BlockSize string `json:"blockSize"`
}

func RequestStrToQuantity(reqStr string) (v1alpha1.IOBandwidth, error) {
	q := v1alpha1.IOBandwidth{}
	err := json.Unmarshal([]byte(reqStr), &q)
	if err != nil {
		return v1alpha1.IOBandwidth{}, fmt.Errorf("unmarshal request error: %v", err)
	}
	q.Total = q.Read.DeepCopy()
	q.Total.Add(q.Write)
	return q, nil
}

func GetNodeIOStatus(ctx context.Context, client versioned.Interface, n string) (*v1alpha1.NodeDiskIOStats, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes configmap client cannot be nil")
	}
	if len(n) == 0 {
		return nil, fmt.Errorf("node name cannot be empty")
	}
	obj, err := client.DiskioV1alpha1().NodeDiskIOStatses(common.CRNameSpace).Get(ctx,
		common.GetCRName(n, common.NodeDiskIOInfoCRSuffix),
		v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func CreateNodeIOStatus(ctx context.Context, client versioned.Interface, node string, pl []string) error {
	nodeStatusInfo := &v1alpha1.NodeDiskIOStats{
		TypeMeta: v1.TypeMeta{
			APIVersion: common.APIVersion,
			Kind:       common.NodeIOStatusCR,
		},
		ObjectMeta: v1.ObjectMeta{
			Namespace: common.CRNameSpace,
			Name:      common.GetCRName(node, common.NodeDiskIOInfoCRSuffix),
		},
		Spec: v1alpha1.NodeDiskIOStatsSpec{
			NodeName:     node,
			ReservedPods: pl,
		},
		Status: v1alpha1.NodeDiskIOStatsStatus{},
	}

	_, err := client.DiskioV1alpha1().NodeDiskIOStatses(common.CRNameSpace).Create(ctx, nodeStatusInfo, v1.CreateOptions{})
	if err != nil {
		klog.Error("CreateNodeIOStatus fails: ", err)
		return err
	}
	return nil
}

func UpdateNodeIOStatus(ctx context.Context, client versioned.Interface, node string, pl []string) error {

	return clientretry.RetryOnConflict(common.UpdateBackoff, func() error {
		sts, err := GetNodeIOStatus(ctx, client, node)
		if err != nil {
			return err
		}
		sts.Spec.ReservedPods = pl
		_, err = client.DiskioV1alpha1().NodeDiskIOStatses(common.CRNameSpace).Update(context.TODO(), sts, v1.UpdateOptions{})
		if err != nil {
			klog.Error("UpdateNodeIOStatus fails: ", err)
			return err
		}
		return nil
	})
}

func ComparePodList(pl1, pl2 []string) bool {
	sort.Strings(pl1)
	sort.Strings(pl2)
	return reflect.DeepEqual(pl1, pl2)
}

func HashObject(obj interface{}) uint64 {
	if obj == nil {
		return 0
	}
	hash := fnv.New32a()

	hashutil.DeepHashObject(hash, obj)
	return uint64(hash.Sum32())
}
