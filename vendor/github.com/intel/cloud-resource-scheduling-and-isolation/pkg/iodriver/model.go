/*
Copyright 2024 Intel Corporation

Licensed under the Apache License, Version 2.0 (the License);
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package iodriver

import (
	"encoding/json"
	"fmt"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ioRequest example: {"rbps": "30Mi", "wbps": "20Mi", "blocksize": "4k"}
func EstimateRequest(ioReq string) (*v1alpha1.IOBandwidth, error) {
	var req = &IORequest{}

	if len(ioReq) == 0 {
		return &v1alpha1.IOBandwidth{
			Read:  MinDefaultIOBW,
			Write: MinDefaultIOBW,
			Total: MinDefaultTotalIOBW,
		}, nil
	}

	err := json.Unmarshal([]byte(ioReq), req)
	if err != nil {
		return nil, err
	}
	resp, err := normalize(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// customized normalization method
func normalize(ioRequest *IORequest) (*v1alpha1.IOBandwidth, error) {
	r := resource.MustParse(ioRequest.Rbps)
	w := resource.MustParse(ioRequest.Wbps)
	bs := ioRequest.BlockSize

	diskinfo := GetFakeDevice()
	bsIdx := GetSliceIdx(ioRequest.BlockSize, BlockSize)
	if bsIdx == -1 {
		return nil, fmt.Errorf("unsupported block size")
	}
	rRatio := diskinfo.ReadRatio[bs]
	wRatio := diskinfo.WriteRatio[bs]

	rbpsValue, _ := r.AsInt64()
	wbpsValue, _ := w.AsInt64()

	rout := float64(rbpsValue) * rRatio / Mi
	wout := float64(wbpsValue) * wRatio / Mi

	return &v1alpha1.IOBandwidth{
		Read:  resource.MustParse(fmt.Sprintf("%fMi", rout)),
		Write: resource.MustParse(fmt.Sprintf("%fMi", wout)),
		Total: resource.MustParse(fmt.Sprintf("%fMi", rout+wout)),
	}, nil
}
