// loop_interlude.go
package mypriorityoptimizer

import (
	"context"
	"time"
)

func (pl *SharedState) loopInterlude(ctx context.Context) {
	delay := OptimizeInterludeDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	checkInterval := OptimizeInterludeCheckInterval
	if checkInterval <= 0 {
		checkInterval = 250 * time.Millisecond
	}

	cfg := optimizeLoopConfig{
		Label:          "InterludeLoop",
		Interval:       checkInterval,
		InterludeDelay: delay, // require stability for this long
		CancelOnChange: true,  // cancel if pending set changes
	}
	// delegated through hook
	optimizeGlobalLoopFunc(pl, ctx, cfg)
}
