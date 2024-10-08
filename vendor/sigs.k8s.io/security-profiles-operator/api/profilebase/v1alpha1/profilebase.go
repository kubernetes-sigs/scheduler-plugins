/*
Copyright 2020 The Kubernetes Authors.

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

package v1alpha1

import (
	rcommonv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	secprofnodestatusv1alpha1 "sigs.k8s.io/security-profiles-operator/api/secprofnodestatus/v1alpha1"
)

// StatusBase contains common attributes for a profile's status.
type StatusBase struct {
	rcommonv1.ConditionedStatus `json:",inline"`
	Status                      secprofnodestatusv1alpha1.ProfileState `json:"status,omitempty"`
}

type StatusBaseUser interface {
	metav1.Object
	runtime.Object
	// GetStatusBase gets an instance of the Base status that the
	// profiles share
	GetStatusBase() *StatusBase
	// DeepCopyToStatusBaseIf exposes an interface to call deep copy
	// on this StatusBaseUser interface
	DeepCopyToStatusBaseIf() StatusBaseUser
	// Sets the needed status that's specific to the profile
	// implementation
	SetImplementationStatus()
}
