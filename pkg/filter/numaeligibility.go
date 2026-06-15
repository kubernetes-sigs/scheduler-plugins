package filter

import (
	"github.com/k8stopologyawareschedwg/numaplacement"
)

func IsNUMAEligible(pod *corev1.Pod) bool {
	return true
}

/
