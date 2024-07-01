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

package main

import (
	"encoding/json"
	"fmt"

	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/api/diskio/v1alpha1"
	"github.com/intel/cloud-resource-scheduling-and-isolation/pkg/iodriver"
	"k8s.io/apimachinery/pkg/api/resource"
)

type fooNormalizer struct{}

func (n fooNormalizer) Name() string {
	return "Intel P4510 NVMe Disk"
}

// ioRequest example: {"rbps": "30Mi", "wbps": "20Mi", "blocksize": "4k"}
func (n fooNormalizer) EstimateRequest(ioReq string) (string, error) {
	var req = &iodriver.IORequest{}
	var resp = &v1alpha1.IOBandwidth{}

	if len(ioReq) == 0 {
		resp = &v1alpha1.IOBandwidth{
			Read:  iodriver.MinDefaultIOBW,
			Write: iodriver.MinDefaultIOBW,
			Total: iodriver.MinDefaultTotalIOBW,
		}
	} else {
		err := json.Unmarshal([]byte(ioReq), req)
		if err != nil {
			return "", err
		}
		resp, err = normalize(req)
		if err != nil {
			return "", err
		}
	}
	normalized, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

// customized normalization method
func normalize(ioRequest *iodriver.IORequest) (*v1alpha1.IOBandwidth, error) {
	r := resource.MustParse(ioRequest.Rbps)
	w := resource.MustParse(ioRequest.Wbps)
	bs := ioRequest.BlockSize

	diskinfo := iodriver.GetFakeDevice()
	bsIdx := iodriver.GetSliceIdx(ioRequest.BlockSize, iodriver.BlockSize)
	if bsIdx == -1 {
		return nil, fmt.Errorf("unsupported block size")
	}
	rRatio := diskinfo.ReadRatio[bs]
	wRatio := diskinfo.WriteRatio[bs]

	rbpsValue, _ := r.AsInt64()
	wbpsValue, _ := w.AsInt64()

	rout := float64(rbpsValue) * rRatio / iodriver.Mi
	wout := float64(wbpsValue) * wRatio / iodriver.Mi

	return &v1alpha1.IOBandwidth{
		Read:  resource.MustParse(fmt.Sprintf("%fMi", rout)),
		Write: resource.MustParse(fmt.Sprintf("%fMi", wout)),
		Total: resource.MustParse(fmt.Sprintf("%fMi", rout+wout)),
	}, nil
}

// Exported as a symbol named "Normalizer"
var Normalizer fooNormalizer
