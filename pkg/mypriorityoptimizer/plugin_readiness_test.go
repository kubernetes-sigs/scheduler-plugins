// plugin_readiness_test.go
package mypriorityoptimizer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

func newTestPodInformer() cache.SharedIndexInformer {
	lw := &cache.ListWatch{
		ListFunc: func(_ metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{}, nil
		},
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return watch.NewFake(), nil
		},
	}
	return cache.NewSharedIndexInformer(lw, &v1.Pod{}, 0, cache.Indexers{})
}

func TestIsCacheReady_NoInformers_ReturnsTrue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := isCacheReady(ctx); !got {
		t.Fatalf("isCacheReady() = %v, want true", got)
	}
}

func TestIsCacheReady_AllNilInformers_ReturnsTrue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Non-empty slice but all nil → funcs slice is empty.
	// cache.WaitForCacheSync(...no funcs...) returns true.
	if got := isCacheReady(ctx, nil, nil); !got {
		t.Fatalf("isCacheReady(nil informers) = %v, want true", got)
	}
}

func TestIsCacheReady_ContextCanceled_ReturnsFalse(t *testing.T) {
	inf := newTestPodInformer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := isCacheReady(ctx, inf); got {
		t.Fatalf("isCacheReady(canceled ctx) = %v, want false", got)
	}
}

func TestWaitForUsableNode_ContextCanceled_ReturnsFalse(t *testing.T) {
	pl := &SharedState{}

	withReadinessInterval(1*time.Millisecond, func() {
		withReadinessHooks(
			func(_ *SharedState) ([]*v1.Node, error) {
				return []*v1.Node{newNode("nodeA")}, nil
			},
			func(_ *v1.Node) bool { return true },
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				if got := pl.waitForUsableNode(ctx); got {
					t.Fatalf("waitForUsableNode(canceled ctx) = %v, want false", got)
				}
			},
		)
	})
}

func TestWaitForUsableNode_EventuallyFindsUsableNode(t *testing.T) {
	pl := &SharedState{}

	withReadinessInterval(1*time.Millisecond, func() {
		var calls atomic.Int32
		withReadinessHooks(
			func(_ *SharedState) ([]*v1.Node, error) {
				calls.Add(1)
				// First tick: only unusable nodes; second tick: usable.
				if calls.Load() == 1 {
					return []*v1.Node{newNode("nodeA")}, nil
				}
				return []*v1.Node{newNode("nodeB")}, nil
			},
			func(n *v1.Node) bool { return n != nil && n.Name == "nodeB" },
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				defer cancel()

				if got := pl.waitForUsableNode(ctx); !got {
					t.Fatalf("waitForUsableNode() = %v, want true", got)
				}
				if calls.Load() < 2 {
					t.Fatalf("expected getNodesForReadiness to be called at least twice, got %d", calls.Load())
				}
			},
		)
	})
}

func TestPluginReadiness_InformerSyncCanceled_DoesNotMarkReady(t *testing.T) {
	pl := &SharedState{}
	pl.BlockedWhileActive = newSafePodSet("blocked")
	pl.PluginReady.Store(false)

	inf := newTestPodInformer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pl.pluginReadiness(ctx, inf)
	if pl.PluginReady.Load() {
		t.Fatalf("PluginReady = true, want false when informers never synced")
	}
}

func TestPluginReadiness_WarmupCanceled_DoesNotMarkReady(t *testing.T) {
	pl := &SharedState{}
	pl.BlockedWhileActive = newSafePodSet("blocked")
	pl.PluginReady.Store(false)

	// Ensure isCacheReady passes (no informers), but warmup delay gets canceled.
	oldDelay := cacheWarmupDelay
	cacheWarmupDelay = 250 * time.Millisecond
	t.Cleanup(func() { cacheWarmupDelay = oldDelay })

	withReadinessInterval(1*time.Millisecond, func() {
		withReadinessHooks(
			func(_ *SharedState) ([]*v1.Node, error) { return []*v1.Node{newNode("nodeA")}, nil },
			func(_ *v1.Node) bool { return true },
			func() {
				ctx, cancel := context.WithCancel(context.Background())
				// Cancel quickly so we hit the warmup cancellation path.
				cancel()

				pl.pluginReadiness(ctx)
				if pl.PluginReady.Load() {
					t.Fatalf("PluginReady = true, want false when warmup is canceled")
				}
			},
		)
	})
}

func TestPluginReadiness_UsableNodeNeverFound_DoesNotMarkReady(t *testing.T) {
	pl := &SharedState{}
	pl.BlockedWhileActive = newSafePodSet("blocked")
	pl.PluginReady.Store(false)

	oldDelay := cacheWarmupDelay
	cacheWarmupDelay = 0
	t.Cleanup(func() { cacheWarmupDelay = oldDelay })

	withReadinessInterval(1*time.Millisecond, func() {
		withReadinessHooks(
			func(_ *SharedState) ([]*v1.Node, error) { return []*v1.Node{newNode("nodeA")}, nil },
			func(_ *v1.Node) bool { return false },
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()

				pl.pluginReadiness(ctx)
				if pl.PluginReady.Load() {
					t.Fatalf("PluginReady = true, want false when no usable node is found")
				}
			},
		)
	})
}

func TestPluginReadiness_Success_MarksReady(t *testing.T) {
	pl := &SharedState{}
	pl.BlockedWhileActive = newSafePodSet("blocked")
	pl.PluginReady.Store(false)

	// Avoid starting background loops in this test.
	withMode(ModeManualBlocking, true, func() {
		oldDelay := cacheWarmupDelay
		cacheWarmupDelay = 0
		t.Cleanup(func() { cacheWarmupDelay = oldDelay })

		withReadinessInterval(1*time.Millisecond, func() {
			withReadinessHooks(
				func(_ *SharedState) ([]*v1.Node, error) { return []*v1.Node{newNode("nodeA")}, nil },
				func(_ *v1.Node) bool { return true },
				func() {
					ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
					defer cancel()

					pl.pluginReadiness(ctx)
					if !pl.PluginReady.Load() {
						t.Fatalf("PluginReady = false, want true on success")
					}
				},
			)
		})
	})
}
