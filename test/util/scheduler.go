package util

import (
	"testing"

	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
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

	var podInformer coreinformers.PodInformer

	podInformer = testCtx.InformerFactory.Core().V1().Pods()
	var err error
	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{
		Interface: testCtx.ClientSet.EventsV1beta1().Events(""),
	})

	testCtx.Scheduler, err = scheduler.New(
		testCtx.ClientSet,
		testCtx.InformerFactory,
		podInformer,
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
