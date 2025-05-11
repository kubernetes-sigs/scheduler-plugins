package resourcepolicy

import (
	"strings"

	"github.com/KunWuLuan/resourcepolicyapi/pkg/apis/scheduling/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type keyStr string
type labelKeysValue string
type multiLevelPodSet []map[string]map[labelKeysValue]sets.Set[keyStr]

func genLabelKeysValue(strs []string) labelKeysValue {
	return labelKeysValue(strings.Join(strs, "/"))
}

// ret: valid, labelValue
func genLabelKeyValueForPod(rp *v1alpha1.ResourcePolicy, pod *v1.Pod) (bool, labelKeysValue) {
	if len(rp.Spec.MatchLabelKeys) == 0 {
		return true, "/"
	}
	value := ""
	for _, key := range rp.Spec.MatchLabelKeys {
		if v, ok := pod.Labels[key]; !ok {
			return false, ""
		} else {
			value += v
		}
	}
	return true, labelKeysValue(value)
}

func GetKeyStr(obj metav1.ObjectMeta) keyStr {
	return keyStr(obj.Namespace + "/" + obj.Name)
}

const ManagedByResourcePolicyAnnoKey = "scheduling.x-k8s.io/managed-by-resourcepolicy"
const ManagedByResourcePolicyIndexKey = "metadata.annotations[scheduling.x-k8s.io/managed-by-resourcepolicy]"
const ResourcePolicyPreFilterStateKey = "scheduling.x-k8s.io/resourcepolicy-prefilter-state"

type ResourcePolicyPreFilterState struct {
	matchedInfo *resourcePolicyInfo

	maxCount       []int
	currentCount   []int
	resConsumption []*framework.Resource
	maxConsumption []*framework.Resource

	labelKeyValue labelKeysValue
	podRes        *framework.Resource

	nodeSelectos []labels.Selector
}

func (r *ResourcePolicyPreFilterState) Clone() framework.StateData {
	ret := &ResourcePolicyPreFilterState{
		matchedInfo: r.matchedInfo,

		maxCount:       SliceCopyInt(r.maxCount),
		currentCount:   SliceCopyInt(r.currentCount),
		resConsumption: SliceCopyRes(r.resConsumption),
		maxConsumption: SliceCopyRes(r.maxConsumption),

		labelKeyValue: r.labelKeyValue,
		podRes:        r.podRes,
	}
	return ret
}

func GetManagedResourcePolicy(p *v1.Pod) keyStr {
	return keyStr(p.Annotations[ManagedByResourcePolicyAnnoKey])
}

func equalResource(res1, res2 *framework.Resource) bool {
	if res1 == nil && res2 == nil {
		return true
	}
	if res1 == nil && res2 != nil || res1 != nil && res2 == nil {
		return false
	}
	if res1.MilliCPU != res2.MilliCPU ||
		res1.Memory != res2.Memory ||
		res1.EphemeralStorage != res2.EphemeralStorage ||
		res1.AllowedPodNumber != res2.AllowedPodNumber {
		return false
	}
	if len(res1.ScalarResources) != len(res2.ScalarResources) {
		return false
	}
	for k, v := range res1.ScalarResources {
		if v2, ok := res2.ScalarResources[k]; !ok || v != v2 {
			return false
		}
	}
	return true
}

func addResources(res1, res2 *framework.Resource) {
	if res1 == nil || res2 == nil {
		return
	}
	res1.MilliCPU += res2.MilliCPU
	res1.Memory += res2.Memory
	res1.EphemeralStorage += res2.EphemeralStorage
	res1.AllowedPodNumber += res2.AllowedPodNumber
	for k, v := range res2.ScalarResources {
		if _, ok := res1.ScalarResources[k]; !ok {
			res1.ScalarResources[k] = v
		} else {
			res1.ScalarResources[k] += v
		}
	}
}

func subResources(res1, res2 *framework.Resource) {
	if res1 == nil || res2 == nil {
		return
	}
	res1.MilliCPU -= res2.MilliCPU
	if res1.MilliCPU < 0 {
		res1.MilliCPU = 0
	}
	res1.Memory -= res2.Memory
	if res1.Memory < 0 {
		res1.Memory = 0
	}
	res1.EphemeralStorage -= res2.EphemeralStorage
	if res1.EphemeralStorage < 0 {
		res1.EphemeralStorage = 0
	}
	res1.AllowedPodNumber -= res2.AllowedPodNumber
	if res1.AllowedPodNumber < 0 {
		res1.AllowedPodNumber = 0
	}
	for k, v := range res2.ScalarResources {
		if _, ok := res1.ScalarResources[k]; ok {
			res1.ScalarResources[k] -= v
			if res1.ScalarResources[k] < 0 {
				res1.ScalarResources[k] = 0
			}
		}
	}
}

func lessThan(res1, res2 *framework.Resource) (bool, string) {
	if res1.MilliCPU > 0 && res1.MilliCPU >= res2.MilliCPU {
		return false, "cpu"
	}
	if res1.Memory > 0 && res1.Memory >= res2.Memory {
		return false, "memory"
	}
	if res1.EphemeralStorage > 0 && res1.EphemeralStorage >= res2.EphemeralStorage {
		return false, "ephemeral-storage"
	}
	for k, v := range res1.ScalarResources {
		if v2, ok := res2.ScalarResources[k]; !ok || ok && v > 0 && v >= v2 {
			return false, string(k)
		}
	}
	return true, ""
}

func SliceCopyInt(src []int) []int {
	dst := make([]int, len(src))
	copy(dst, src)
	return dst
}

func SliceCopyRes(src []*framework.Resource) []*framework.Resource {
	dst := make([]*framework.Resource, len(src))
	for i := range src {
		dst[i] = src[i].Clone()
	}
	return dst
}
