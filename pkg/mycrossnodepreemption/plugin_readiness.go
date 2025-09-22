package mycrossnodepreemption

import (
	"context"
	"time"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// waitForPluginReadiness waits for all informers to sync and until a usable node is found.
func (pl *MyCrossNodePreemption) waitForPluginReadiness(
	ctx context.Context, podsInf, nodesInf, cmsInf, rsInf, ssInf, dsInf, jobInf cache.SharedIndexInformer,
) {
	ok := cache.WaitForCacheSync(ctx.Done(),
		podsInf.HasSynced, nodesInf.HasSynced, cmsInf.HasSynced,
		rsInf.HasSynced, ssInf.HasSynced, dsInf.HasSynced, jobInf.HasSynced,
	)
	if !ok {
		klog.InfoS("Cache sync aborted (context done)")
		return
	}

	// Start a background watcher that waits for a usable node.
	// We immediately return so the caller can continue even if no usable nodes exist yet.
	go func() {
		t := time.NewTicker(PluginReadinessInterval)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				// Do NOT activate blocked pods here; only when a usable node is found.
				return

			case <-t.C:
				nodes, err := pl.getNodes()
				if err != nil {
					klog.V(MyV).InfoS("cache warm-up watcher: getNodes error; retrying", "err", err)
					continue
				}
				usable := false
				for _, n := range nodes {
					if isNodeUsable(n) {
						usable = true
						break
					}
				}
				if !usable {
					klog.V(MyV).InfoS("cache warm-up: waiting for a usable node")
					continue
				}

				// We have at least one usable node—mark warm and (optionally) unblock.
				pl.CachesWarm.Store(true)
				klog.InfoS("caches ready and usable node(s) detected")

				// Avoid a surge in Every@PreEnqueue; the idle nudge will trickle them.
				if !(optimizeEvery() && optimizeAtPreEnqueue()) {
					pl.activatePods(pl.BlockedWhileActive, false, 0) // activate all currently blocked
				}
				pl.startLoops(ctx)

				return
			}
		}
	}()

	// Return immediately; no usable nodes yet is fine — only unblock once watcher confirms usability.
}
