package mycrossnodepreemption

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PluginName is the name of the plugin used in the plugin registry and configurations.
const PluginName = "MyCrossNodePreemption"

// NewPlugin initializes a new plugin and returns it.
func NewPlugin(ctx context.Context, args runtime.Object, fh framework.Handle) (framework.Plugin, error) {
	return New(ctx, args, fh)
}
