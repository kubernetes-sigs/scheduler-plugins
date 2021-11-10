/*
Copyright 2021 The Kubernetes Authors.

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

package log

import (
	"k8s.io/klog/v2"
)

const (
	// ObjectSchedVerboseAnnotation informs the scheduler that verbose logs,
	// if available, should be emitted when processing this pod
	ObjectSchedVerboseAnnotation = "experimental-verbose.scheduling.sigs.k8s.io"
)

var (
	// UpgradedLogLevel is the level we upgrade the verbosiness level if
	// the given object overrides the priority using the annotation.
	// We initialize with level which should be enabled most of the time,
	// but which is still possible to silence.
	// Client code can still override if needed
	UpgradedLogLevel klog.Level = 2
)

// KMetadata is a subset of the kubernetes k8s.io/apimachinery/pkg/apis/meta/v1.Object interface
// this interface may expand in the future, but will always be a subset of the
// kubernetes k8s.io/apimachinery/pkg/apis/meta/v1.Object interface
type KMetadata interface {
	klog.KMetadata
	GetAnnotations() map[string]string
}

// V reports whether verbosity at the call site is at least the requested level,
// or if the given pod opted in for verbose logging having added the annotation.
// In the latter case, the verbosiness is transparently bumped to UpgradedLogLevel.
func V(level klog.Level, obj KMetadata) klog.Verbose {
	if obj == nil {
		return klog.V(level)
	}
	anns := obj.GetAnnotations()
	if anns == nil {
		return klog.V(level)
	}
	if _, ok := anns[ObjectSchedVerboseAnnotation]; !ok {
		return klog.V(level)
	}
	return klog.V(UpgradedLogLevel)
}
