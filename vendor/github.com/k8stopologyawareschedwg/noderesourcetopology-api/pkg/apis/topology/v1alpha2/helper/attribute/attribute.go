/*
Copyright 2023 The Kubernetes Authors.

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

package attribute

import (
	"github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
)

// Get looks for attribute by its name in the provided AtributeList.
// If found, returns the value of the attribute and an explicit boolean value equals true
// Otherwise, returns the zero value and explicit boolean value equals false.
// In case there are duplicates in the attribute list, returns the first attribute from the left
// found in iteration order, iterating on the attribute list treating it as a golang slice.
func Get(attributes v1alpha2.AttributeList, name string) (v1alpha2.AttributeInfo, bool) {
	attr := findAttrByName(attributes, name)
	if attr == nil {
		return v1alpha2.AttributeInfo{}, false
	}
	return *attr, true
}

// Insert updates or adds an attribute to the provided AttributeList, preserving the existing order.
// The attribute name must be non-zero, the value can be zero.
// If the attribute is already present, its value is overridden with the given one; otherwise
// adds a new attribute preserving as much as possible the current ordering between attributes.
// Returns the updated AttributeList.
func Insert(attributes v1alpha2.AttributeList, attr v1alpha2.AttributeInfo) v1alpha2.AttributeList {
	if attr.Name == "" {
		return attributes
	}
	attrs := attributes.DeepCopy()
	existingAttr := findAttrByName(attrs, attr.Name)
	if existingAttr == nil {
		return append(attrs, attr)
	}
	existingAttr.Value = attr.Value
	return attrs
}

// Merge joins `updated` into `existing`, preserving as much as possible the current ordering between attributes.
// Returns the merged attribute list, which includes the union of the elements from `existing` and `updated`.
// If an element is present in both lists, takes the value present in `updated`.
func Merge(existing, updated v1alpha2.AttributeList) v1alpha2.AttributeList {
	ret := existing.DeepCopy()
	delta := v1alpha2.AttributeList{}
	for _, attr := range updated {
		if existingAttr := findAttrByName(ret, attr.Name); existingAttr != nil {
			existingAttr.Value = attr.Value
		} else {
			delta = append(delta, attr)
		}
	}
	return append(ret, delta...)
}

// DeleteAll removes from the given AttributeList all the elements whose name matches the given `name`.
// Returns a new AttributeList with all the attributes removed, preserving as much as possible the current
// ordering between attributes.
func DeleteAll(attributes v1alpha2.AttributeList, name string) v1alpha2.AttributeList {
	ret := make(v1alpha2.AttributeList, 0, len(attributes))
	for _, attr := range attributes {
		if attr.Name == name {
			continue
		}
		ret = append(ret, attr)
	}
	return ret
}

func findAttrByName(attributes v1alpha2.AttributeList, name string) *v1alpha2.AttributeInfo {
	for idx := range attributes {
		attr := &attributes[idx]
		if attr.Name == name {
			return attr
		}
	}
	return nil
}
