/*
 * Copyright 2022 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package podfingerprint

import (
	"fmt"
	"strings"
)

// NamespacedName is a Namespace/Name pair
type NamespacedName struct {
	Namespace string
	Name      string
}

func (nn NamespacedName) GetNamespace() string {
	return nn.Namespace
}

func (nn NamespacedName) GetName() string {
	return nn.Name
}

func (nn NamespacedName) String() string {
	return nn.Namespace + "/" + nn.Name
}

// Tracer tracks the actions needed to compute a fingerprint
type Tracer interface {
	// Start is called when the Fingerprint is initialized
	Start(numPods int)
	// Add is called when a new pod data is fed to the fingerprint,
	// respecting the invocation order (no reordering done)
	Add(namespace, name string)
	// Sign is called when the fingerprint is asked for the signature
	Sign(computed string)
	// Check is called when the fingerprint is asked to check
	Check(expected string)
}

// NullTracer implements the Tracer interface and does nothing
type NullTracer struct{}

func (nt NullTracer) Start(numPods int)          {}
func (nt NullTracer) Add(namespace, name string) {}
func (nt NullTracer) Sign(computed string)       {}
func (nt NullTracer) Check(expected string)      {}

// Status represents the working status of a Fingerprint
type Status struct {
	FingerprintExpected string           `json:"fingerprintExpected,omitempty"`
	FingerprintComputed string           `json:"fingerprintComputed,omitempty"`
	Pods                []NamespacedName `json:"pods,omitempty"`
	NodeName            string           `json:"nodeName,omitempty"`
}

func MakeStatus(nodeName string) Status {
	return Status{
		NodeName: nodeName,
	}
}

func (st *Status) Start(numPods int) {
	st.Pods = make([]NamespacedName, 0, numPods)
}

func (st *Status) Add(namespace, name string) {
	st.Pods = append(st.Pods, NamespacedName{
		Namespace: namespace,
		Name:      name,
	})
}

func (st *Status) Sign(computed string) {
	st.FingerprintComputed = computed
}

func (st *Status) Check(expected string) {
	st.FingerprintExpected = expected
}

// Repr represents the Status as compact yet human-friendly string
func (st Status) Repr() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("> processing node %q\n", st.NodeName))
	sb.WriteString(fmt.Sprintf("> processing %d pods\n", len(st.Pods)))
	for _, pod := range st.Pods {
		sb.WriteString("+ " + pod.Namespace + "/" + pod.Name + "\n")
	}

	sb.WriteString("= " + st.FingerprintComputed + "\n")
	if st.FingerprintExpected != "" {
		sb.WriteString("V " + st.FingerprintExpected + "\n")
	}

	return sb.String()
}

func (st Status) Clone() Status {
	pods := make([]NamespacedName, len(st.Pods))
	copy(pods, st.Pods)
	ret := Status{
		FingerprintExpected: st.FingerprintExpected,
		FingerprintComputed: st.FingerprintComputed,
		Pods:                pods,
		NodeName:            st.NodeName,
	}
	return ret
}

type TracingFingerprint struct {
	Fingerprint
	tracer Tracer
}

// NewTracingFingerprint creates a empty Fingerprint.
// The size parameter is a hint for the expected size of the pod set. Use 0 if you don't know.
// Values of size < 0 are ignored.
// The tracer parameter attach a tracer to this fingerprint. Client code can peek inside the
// fingerprint parameters, for debug purposes.
func NewTracingFingerprint(size int, tracer Tracer) *TracingFingerprint {
	tf := &TracingFingerprint{}
	tf.Fingerprint.Reset(size)
	tf.tracer = tracer
	return tf
}

// AddPod adds a pod to the pod set.
func (tf *TracingFingerprint) AddPod(pod PodIdentifier) error {
	tf.tracer.Add(pod.GetNamespace(), pod.GetName())
	return tf.Fingerprint.AddPod(pod)
}

// AddPod add a pod by its namespace/name pair to the pod set.
func (tf *TracingFingerprint) Add(namespace, name string) error {
	tf.tracer.Add(namespace, name)
	return tf.Fingerprint.Add(namespace, name)
}

// Sum computes the fingerprint of the *current* pod set as slice of bytes.
// It is legal to keep adding pods after calling Sum, but the fingerprint of
// the set will change. The output of Sum is guaranteed to be stable if the
// content of the pod set is stable.
func (tf *TracingFingerprint) Sum() []byte {
	return tf.Fingerprint.Sum()
}

// Sign computes the pod set fingerprint as string.
// The string should be considered a opaque identifier and checked only for
// equality, or fed into Check
func (tf *TracingFingerprint) Sign() string {
	sign := tf.Fingerprint.Sign()
	tf.tracer.Sign(sign)
	return sign
}

// Check verifies if the provided fingerprint matches the current pod set.
// Returns nil if the verification holds, error otherwise, or if the fingerprint
// string is malformed.
func (tf *TracingFingerprint) Check(sign string) error {
	tf.tracer.Check(sign)
	return tf.Fingerprint.Check(sign)
}
