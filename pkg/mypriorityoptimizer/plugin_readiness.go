// plugin_readiness.go
package mypriorityoptimizer

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// -------------------------
// Test hooks
// --------------------------

var (
	cacheWarmupDelay            = CacheWarmupSettleDelay
	readinessUsableNodeInterval = PluginReadinessUsableNodeInterval
	isNodeUsableForReadiness    = isNodeUsable
	getNodesForReadiness        = func(pl *SharedState) ([]*v1.Node, error) { return pl.getNodes() }
)

// pluginReadiness waits for all informers to sync and until a usable node is found.
func (pl *SharedState) pluginReadiness(ctx context.Context, informers ...cache.SharedIndexInformer) {
	label := "Plugin Readiness"
	klog.InfoS(msg(label, InfoWaitingForInformers))
	if !isCacheReady(ctx, informers...) {
		klog.Error(msg(label, ErrInformersCanceledOrTimedOut.Error()))
		return
	}
	klog.InfoS(msg(label, InfoInformersSynced))

	// Context-aware warmup delay.
	if cacheWarmupDelay > 0 {
		select {
		case <-ctx.Done():
			klog.Error(msg(label, "cache warmup canceled"))
			return
		case <-time.After(cacheWarmupDelay):
			// proceed
		}
	}

	// Wait for a usable node
	if !pl.waitForUsableNode(ctx) {
		klog.Error(msg(label, ErrWaitForUsableNodeCanceledOrTimedOut.Error()))
		return
	}

	// Finalize readiness (mark ready, optionally unblock, start loops)
	pl.PluginReady.Store(true)
	klog.InfoS(msg(label, InfoPluginReady))

	// Snapshot  plugin configuration
	_ = pl.persistPluginConfig(ctx)

	// Activate all currently blocked pods
	pl.activatePods(pl.BlockedWhileActive, false, -1)

	// Start optimization loops (periodic / interlude / nudge)
	pl.startLoops(ctx)
}

func isCacheReady(ctx context.Context, informers ...cache.SharedIndexInformer) bool {
	if len(informers) == 0 {
		return true
	}

	funcs := make([]cache.InformerSynced, 0, len(informers))
	for _, inf := range informers {
		if inf == nil {
			continue
		}
		funcs = append(funcs, inf.HasSynced)
	}

	return cache.WaitForCacheSync(ctx.Done(), funcs...)
}

func (pl *SharedState) waitForUsableNode(ctx context.Context) bool {
	label := "Wait for Usable Node"

	// Use overrideable interval (tests can make this tiny).
	t := time.NewTicker(readinessUsableNodeInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			nodes, err := getNodesForReadiness(pl)
			if err != nil {
				klog.V(MyV).InfoS(msg(label, InfoGetNodesFailed), "err", err)
				continue
			}
			usable := false
			for _, n := range nodes {
				if isNodeUsableForReadiness(n) {
					usable = true
					break
				}
			}
			if !usable {
				klog.V(MyV).InfoS(msg(label, InfoNoUsableNodes))
				continue
			}
			klog.InfoS(msg(label, InfoUsableNodeFound))
			return true
		}
	}
}
