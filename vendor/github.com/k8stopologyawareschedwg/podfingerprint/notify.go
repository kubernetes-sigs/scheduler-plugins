/*
 * Copyright 2023 Red Hat, Inc.
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

var completionSink chan<- Status

// SetCompletionSink sets the process-global completion destination
func SetCompletionSink(ch chan<- Status) {
	completionSink = ch
}

// MarkCompleted sets a podfingerprint Status as completed.
// When a Status is completed, it means the Status and its fingerprint is considered sealed,
// so from now on only idempotent, non-state altering actions (like Repr(), Sign(), Check())
// are expected to be performed. Applications which use the Tracing fingerprint can use this
// function to clearly signal this completion point and as another option to pass the Status
// to other subssystems.
func MarkCompleted(st Status) error {
	if completionSink == nil { // nothing to do
		return nil
	}
	completionSink <- st.Clone()
	return nil
}

// CleanCompletionSink should be used only in testing
func CleanCompletionSink() {
	completionSink = nil
}
