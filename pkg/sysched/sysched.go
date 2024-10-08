package sysched

import (
	"context"
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/containers/common/pkg/seccomp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/helper"
	"sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

type SySched struct {
	handle framework.Handle
	client client.Client
	// Maintain state of what pods on each node
	// Cached state from SharedLister does not hold system wide info of pods
	// scheduled by other schedulers
	HostToPods map[string][]*v1.Pod
	// Key: node name
	// Value: set of system call names
	HostSyscalls            map[string]sets.Set[string]
	ExSAvg                  float64
	ExSAvgCount             int64
	DefaultProfileNamespace string
	DefaultProfileName      string
	WeightedSyscallProfile  string
}

var _ framework.ScorePlugin = &SySched{}

// Name is the name of the plugin used in Registry and configurations.
const Name = "SySched"

// SPO annotation string
const SPO_ANNOTATION = "seccomp.security.alpha.kubernetes.io"

func remove(s []*v1.Pod, i int) []*v1.Pod {
	if len(s) == 0 {
		return nil
	}
	s[i] = s[len(s)-1]

	return s[:len(s)-1]
}

// extracts filename and namespace from the relative seccomp
// profile path with the following formats
// e.g., localhost/operator/<namespace>/<filename>.json OR
// e.g., operator/<namespace>/<filename>.json
// func getCRDandNamespace(localhostProfile string) (string, string) {
func parseNameNS(profilePath string) (string, string) {
	if profilePath == "" {
		return "", ""
	}

	parts := strings.Split(profilePath, "/")
	if len(parts) < 2 {
		return "", ""
	}

	ns := parts[len(parts)-2]

	// get filename without extension
	name := strings.TrimSuffix(parts[len(parts)-1], path.Ext(parts[len(parts)-1]))

	return ns, name
}

// fetch the system call list from a SPO seccomp profile CR in a given namespace
func (sc *SySched) readSPOProfileCR(name string, namespace string) (sets.Set[string], error) {
	syscalls := sets.New[string]()

	if name == "" || namespace == "" {
		return syscalls, nil
	}

	// extract a seccomp SPO profile CR using namespace and cr name
	seccompProfile := &v1beta1.SeccompProfile{}

	err := sc.client.Get(context.TODO(), client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}, seccompProfile)

	if err != nil {
		return syscalls, err
	}

	syscallCategories := seccompProfile.Spec.Syscalls

	// need to merge the syscalls in the syscall categories
	// from multiple relevant actions, e.g., allow, log, notify
	for _, element := range syscallCategories {
		// NOTE: should we consider the other categories, e.g., notify, trace?
		// SCMP_ACT_TRACE --> ActTrace, seccomp.ActNotify
		if element.Action == seccomp.ActAllow || element.Action == seccomp.ActLog {
			syscalls = syscalls.Union(sets.New[string](element.Names...))
		}
	}

	return syscalls, nil
}

// obtains the system call list for a pod from the pod's seccomp profile
// SPO is used to generate and input the seccomp profile to a pod
// If a pod does not have a SPO seccomp profile, then an unconfined
// system call set is return for the pod
func (sc *SySched) getSyscalls(pod *v1.Pod) sets.Set[string] {
	r := sets.New[string]()

	// read the seccomp profile from the security context of a pod
	podSC := pod.Spec.SecurityContext

	if podSC != nil && podSC.SeccompProfile != nil && podSC.SeccompProfile.Type == "Localhost" {
		if podSC.SeccompProfile.LocalhostProfile != nil {
			profilePath := *podSC.SeccompProfile.LocalhostProfile
			ns, name := parseNameNS(profilePath)

			if len(ns) > 0 && len(name) > 0 {
				syscalls, err := sc.readSPOProfileCR(name, ns)
				if err != nil {
					klog.ErrorS(err, "Failed to read syscall CR by parsing pod security context")
				}

				if len(syscalls) > 0 {
					r = r.Union(syscalls)
				}
			}
		}
	}

	// read the seccomp profile from container security context and merge them
	for _, container := range pod.Spec.Containers {
		conSC := container.SecurityContext
		if conSC != nil && conSC.SeccompProfile != nil && conSC.SeccompProfile.Type == "Localhost" {
			if conSC.SeccompProfile.LocalhostProfile != nil {
				profilePath := *conSC.SeccompProfile.LocalhostProfile
				ns, name := parseNameNS(profilePath)

				if len(ns) > 0 && len(name) > 0 {
					syscalls, err := sc.readSPOProfileCR(name, ns)
					if err != nil {
						klog.ErrorS(err, "Failed to read syscall CR by parsing container security context")
					}

					if len(syscalls) > 0 {
						r = r.Union(syscalls)
					}
				}
			}
		}
	}

	// SPO seccomp profiles are sometimes automatically annotated to a pod
	if pod.ObjectMeta.Annotations != nil {
		// there could be multiple SPO seccomp profile annotations for a pod
		// merge all profiles to obtain the syscal set for a pod
		for k, v := range pod.ObjectMeta.Annotations {
			// looks for annotation related to the seccomp
			if strings.Contains(k, SPO_ANNOTATION) {
				ns, name := parseNameNS(v)

				if len(ns) > 0 && len(name) > 0 {
					syscalls, err := sc.readSPOProfileCR(name, ns)

					if err != nil {
						klog.ErrorS(err, "Failed to read syscall CR by parsing pod annotation")
						continue
					}

					if len(syscalls) > 0 {
						r = r.Union(syscalls)
					}
				}
				break
			}
		}
	}

	// if a pod does not have a seccomp profile specified, return the set of all syscalls
	if len(r) == 0 {
		syscalls, err := sc.readSPOProfileCR(sc.DefaultProfileName, sc.DefaultProfileNamespace)
		if err != nil {
			klog.ErrorS(err, "Failed to read the CR of all syscalls")
		}

		if syscalls.Len() > 0 {
			r = r.Union(syscalls)
		}
	}

	return r
}

// Name returns name of the plugin. It is used in logs, etc.
func (sc *SySched) Name() string {
	return Name
}

func (sc *SySched) calcScore(syscalls sets.Set[string]) int {
	// Currently, score is not adjusted based on critical/cve syscalls.
	// NOTE: weight W is hardcoded for now
	// TODO: add critical/cve syscalls
	// TODO: adjust weight W for critical/cve syscalls
	totCrit := 0
	W := 1

	score := syscalls.Len() - totCrit
	score = score + W*totCrit
	klog.V(10).InfoS("Score: ", "score", score, "tot_crit", totCrit)

	return score
}

// Score invoked at the score extension point.
func (sc *SySched) Score(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	// Read directly from API server because cached state in SnapSharedLister not always up-to-date
	// especially during initial scheduler start.
	node, err := sc.handle.ClientSet().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, nil
	}

	podSyscalls := sc.getSyscalls(pod)

	// NOTE: this condition is true only when a pod does not
	// have a syscall profile, or the unconfined syscall is
	// not set. We return a large number (INT64_MAX) as score.
	if len(podSyscalls) == 0 {
		return math.MaxInt64, nil
	}

	_, hostSyscalls := sc.getHostSyscalls(node.Name)

	// when a host or node does not have any pods
	// running, the extraneous syscall score is zero
	if hostSyscalls == nil {
		return 0, nil
	}

	diffSyscalls := hostSyscalls.Difference(podSyscalls)
	totalDiffs := sc.calcScore(diffSyscalls)

	// add the difference existing pods will see if new Pod is added into this host
	newHostSyscalls := hostSyscalls.Clone()
	newHostSyscalls = newHostSyscalls.Union(podSyscalls)
	for _, p := range sc.HostToPods[node.Name] {
		podSyscalls = sc.getSyscalls(p)
		diffSyscalls = newHostSyscalls.Difference(podSyscalls)
		totalDiffs += sc.calcScore(diffSyscalls)
	}

	sc.ExSAvg = sc.ExSAvg + (float64(totalDiffs)-sc.ExSAvg)/float64(sc.ExSAvgCount)
	sc.ExSAvgCount += 1

	klog.V(10).Info("ExSAvg: ", sc.ExSAvg)
	klog.V(10).InfoS("Score: ", "totalDiffs", totalDiffs, "pod", pod.Name, "node", nodeName)

	return int64(totalDiffs), nil
}

func (sc *SySched) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	klog.V(10).InfoS("Original: ", "scores", scores, "pod", pod.Name)
	ret := helper.DefaultNormalizeScore(framework.MaxNodeScore, true, scores)
	klog.V(10).InfoS("Normalized: ", "scores", scores, "pod", pod.Name)

	return ret
}

// ScoreExtensions of the Score plugin.
func (sc *SySched) ScoreExtensions() framework.ScoreExtensions {
	return sc
}

func (sc *SySched) getHostSyscalls(nodeName string) (int, sets.Set[string]) {
	count := 0
	h, ok := sc.HostSyscalls[nodeName]
	if !ok {
		klog.V(5).Infof("getHostSyscalls: no nodeName %s", nodeName)
		return count, nil
	}
	return h.Len(), h
}

func (sc *SySched) updateHostSyscalls(pod *v1.Pod) {
	syscall := sc.getSyscalls(pod)
	sc.HostSyscalls[pod.Spec.NodeName] = sc.HostSyscalls[pod.Spec.NodeName].Union(syscall)
}

func (sc *SySched) addPod(pod *v1.Pod) {
	nodeName := pod.Spec.NodeName
	name := pod.Name

	_, ok := sc.HostToPods[nodeName]
	if !ok {
		sc.HostToPods[nodeName] = make([]*v1.Pod, 0)
		sc.HostToPods[nodeName] = append(sc.HostToPods[nodeName], pod)
		sc.HostSyscalls[nodeName] = sets.New[string]()
		sc.updateHostSyscalls(pod)
		return
	}

	for _, p := range sc.HostToPods[nodeName] {
		if p.Name == name {
			return
		}
	}

	sc.HostToPods[nodeName] = append(sc.HostToPods[nodeName], pod)
	sc.updateHostSyscalls(pod)

	return
}

func (sc *SySched) recomputeHostSyscalls(pods []*v1.Pod) sets.Set[string] {
	syscalls := sets.New[string]()

	for _, p := range pods {
		syscall := sc.getSyscalls(p)
		syscalls = syscalls.Union(syscall)
	}

	return syscalls
}

func (sc *SySched) removePod(pod *v1.Pod) {
	nodeName := pod.Spec.NodeName

	_, ok := sc.HostToPods[nodeName]
	if !ok {
		klog.V(5).Infof("removePod: Host %s not yet cached", nodeName)
		return
	}
	for i, p := range sc.HostToPods[nodeName] {
		if p.Name == pod.Name {
			sc.HostToPods[nodeName] = remove(sc.HostToPods[nodeName], i)
			sc.HostSyscalls[nodeName] = sc.recomputeHostSyscalls(sc.HostToPods[nodeName])
			c, _ := sc.getHostSyscalls(nodeName)
			klog.V(5).InfoS("remaining ", "syscalls", c, "node", nodeName)
			return
		}
	}

	return
}

func (sc *SySched) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)

	// Add already running pod to map
	// This is for when our scheduler comes up after other pods
	if pod.Status.Phase == v1.PodRunning {
		klog.V(10).Infof("POD ADDED: %s/%s phase: %s", pod.Namespace, pod.Name, pod.Status.Phase)
		sc.addPod(pod)
	}
}

func (sc *SySched) podUpdated(old, new interface{}) {
	pod := old.(*v1.Pod)

	// Pod has been assigned to node, now can add to our map
	if pod.Status.Phase == v1.PodPending && pod.Status.HostIP != "" {
		klog.V(10).Infof("POD UPDATED. %s/%s", pod.Namespace, pod.Name)
		sc.addPod(pod)
	}
}

func (sc *SySched) podDeleted(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.V(10).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	sc.removePod(pod)
}

// getArgs : returns the arguments for the SySchedArg plugin.
func getArgs(obj runtime.Object) (*pluginconfig.SySchedArgs, error) {
	SySchedArgs, ok := obj.(*pluginconfig.SySchedArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type SySchedArgs, got %T", obj)
	}

	return SySchedArgs, nil
}

// New initializes a new plugin and returns it.
func New(_ context.Context, obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	sc := SySched{handle: handle}
	sc.HostToPods = make(map[string][]*v1.Pod)
	sc.HostSyscalls = make(map[string]sets.Set[string])
	sc.ExSAvg = 0
	sc.ExSAvgCount = 1

	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}

	// get the default syscall profile CR namespace and name for all syscalls
	sc.DefaultProfileNamespace = args.DefaultProfileNamespace
	sc.DefaultProfileName = args.DefaultProfileName

	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	v1beta1.AddToScheme(scheme)

	client, err := client.New(handle.KubeConfig(), client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	sc.client = client

	podInformer := handle.SharedInformerFactory().Core().V1().Pods()

	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    sc.podAdded,
			UpdateFunc: sc.podUpdated,
			DeleteFunc: sc.podDeleted,
		},
	)

	return &sc, nil
}
