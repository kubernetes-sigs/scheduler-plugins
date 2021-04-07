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

package util

import (
	"testing"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	testutils "k8s.io/kubernetes/test/integration/util"
)

// InitTestSchedulerWithOptions initializes a test environment and creates a scheduler with default
// configuration and other options.
// TODO(Huang-Wei): refactor the same function in the upstream, and remove here.
func InitTestSchedulerWithOptions(
	t *testing.T,
	testCtx *testutils.TestContext,
	startScheduler bool,
	opts ...scheduler.Option,
) *testutils.TestContext {
	testCtx.InformerFactory = informers.NewSharedInformerFactory(testCtx.ClientSet, 0)

	var err error
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: testCtx.ClientSet.EventsV1(),
	})

	testCtx.Scheduler, err = scheduler.New(
		testCtx.ClientSet,
		testCtx.InformerFactory,
		profile.NewRecorderFactory(eventBroadcaster),
		testCtx.Ctx.Done(),
		opts...,
	)

	if err != nil {
		t.Fatalf("Couldn't create scheduler: %v", err)
	}

	eventBroadcaster.StartRecordingToSink(testCtx.Ctx.Done())

	testCtx.InformerFactory.Start(testCtx.Scheduler.StopEverything)
	testCtx.InformerFactory.WaitForCacheSync(testCtx.Scheduler.StopEverything)

	if startScheduler {
		go testCtx.Scheduler.Run(testCtx.Ctx)
	}

	return testCtx
}
