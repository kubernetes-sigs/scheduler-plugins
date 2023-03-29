package sysched

//TODO:
// 1. weighted mechanism: i) extending SPO, ii) overload existing SPO, iii) CRD
// 2. handling the restart of scheduler to restore states

import (
	"fmt"
	"context"
	"strings"
	"path"
	"math"

	"github.com/containers/common/pkg/seccomp"
	v1 "k8s.io/api/core/v1"
	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/helper"
	"sigs.k8s.io/scheduler-plugins/pkg/sysched/clientset/v1alpha1"
	"sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
)

type SySched struct {
	handle			framework.Handle
	clientSet		v1alpha1.SPOV1Alpha1Interface
	HostToPods		map[string][]*v1.Pod
	HostSyscalls		map[string]map[string]bool
	CritSyscalls		map[string][]string
	ExSAvg			float64
	ExSAvgCount		int64
	SyscallCRDNamespace	string
	FullSyscallProfile	string
	WeightedSyscallProfile	string
}

var _ framework.ScorePlugin = &SySched{}

// Name is the name of the plugin used in Registry and configurations.
const Name = "SySched"

func setSubtract(hostSyscalls map[string]bool, podSyscalls []string) []string {
	var syscallDiffs []string
	syscalls := make(map[string]bool)

	// copying content from hostSyscalls to newHostSyscalls
	for k, v := range hostSyscalls {
		syscalls[k] = v
	}

	// remove a syscall from syscalls by setting the value to
	// false if the syscall is present in podSyscalls list
	for _, s := range podSyscalls {
		_, ok := syscalls[s]
		if ok {
			syscalls[s] = false
		}
	}
	// now the syscall difference is the list of syscalls that
	// still contain true values in the syscalls map
	for s := range syscalls {
		if syscalls[s] {
			syscallDiffs = append(syscallDiffs, s)
		}
	}

	return syscallDiffs
}

// copying syscalls from the newSyscalls list to the syscalls map
func union(syscalls map[string]bool, newSyscalls []string) {
	for _, s := range newSyscalls {
		syscalls[s] = true
	}
}

func remove(s []*v1.Pod, i int) []*v1.Pod {
	if len(s) == 0 {
		return nil
	}
	s[i] = s[len(s)-1]

	return s[:len(s)-1]
}

func UnionList(a, b []string) []string {
	m := make(map[string]bool)

	for _, item := range a {
		m[item] = true
	}

	for _, item := range b {
		if _, ok := m[item]; !ok {
			a = append(a, item)
		}
	}

	return a
}

// extracts filename and namespace from the relative seccomp
// profile path with the following formats
// e.g., localhost/operator/<namespace>/<filename>.json OR
// e.g., operator/<namespace>/<filename>.json
func getCRDandNamespace(localhostProfile string) (string, string) {
        if localhostProfile == "" {
                return "", ""
        }

        parts := strings.Split(localhostProfile, "/")
        if len(parts) < 3 {
                return "", ""
        }

	// namespace is the second item in the path
	// e.g.., operator/<namespace>/<filename>.json
	ns := parts[1]

	// namespace is the third item in the path
	// localhost/operator/<namespace>/<filename>.json
	if parts[0] == "localhost" {
		ns = parts[2]
	}

        // get filename without extension
        crdName := strings.TrimSuffix(parts[len(parts)-1], path.Ext(parts[len(parts)-1]))

        return ns, crdName
}

// fetch the system call list from a SPO seccomp profile CRD in a given namespace
func (sc *SySched) readSPOProfileCRD(crdName string, namespace string) ([]string, error) {
	var syscalls []string

	if crdName == "" || namespace == "" {
		return syscalls, nil
	}

	// extract a seccomp SPO crd using namespace and crd name
	profile, err := sc.clientSet.Profiles().Get(crdName, namespace, metav1.GetOptions{})

	if err != nil {
		return syscalls, nil
	}

	syscallCategories := profile.Spec.Syscalls

	// need to merge the syscalls in the syscall categories
	// from multiple relevant actions, e.g., allow, log, notify
	for _, element := range syscallCategories {
		// NOTE: should we consider the rest categories, e.g., notify, trace?
		// SCMP_ACT_TRACE --> ActTrace, seccomp.ActNotify
		if element.Action == seccomp.ActAllow || element.Action == seccomp.ActLog {
			syscalls = UnionList(syscalls, element.Names)
		}
	}

	return syscalls, nil
}

// obtains the system call list for a pod from the pod's seccomp profile
// SPO is used to generate and input the seccomp profile to a pod
// If a pod does not have a SPO seccomp profile, then an unconfined
// system call set is return for the pod
func (sc *SySched) getSyscalls(pod *v1.Pod) ([]string, error) {
	var r []string

	// read the seccomp profile from the security context of a pod
	podSC := pod.Spec.SecurityContext
	if podSC != nil && podSC.SeccompProfile != nil && podSC.SeccompProfile.Type == "Localhost" {
		profilePath := *podSC.SeccompProfile.LocalhostProfile
		ns, crdName := getCRDandNamespace(profilePath)
		//klog.Info("namespace and profile name from pod sc = ", ns, " ", crdName)

		syscalls, _ := sc.readSPOProfileCRD(crdName, ns)

		if len(syscalls) > 0 {
			r = UnionList(r, syscalls)
		}
	}

	// read the seccomp profile from container security context and merge them
	for _, container := range pod.Spec.Containers {
		conSC := container.SecurityContext
		if conSC != nil && conSC.SeccompProfile != nil && conSC.SeccompProfile.Type == "Localhost" {
			profilePath := *conSC.SeccompProfile.LocalhostProfile
			ns, crdName := getCRDandNamespace(profilePath)
			//klog.Info("namespace and profile name from pod sc = ", ns, " ", crdName)

			syscalls, _ := sc.readSPOProfileCRD(crdName, ns)

			if len(syscalls) > 0 {
				r = UnionList(r, syscalls)
			}
		}
	}

	// SPO seccomp profiles are sometimes automatically annotated to a pod
	if pod.ObjectMeta.Annotations != nil {
		// there could be multiple SPO seccomp profile annotations for a pod
		// merge all profiles to obtain the syscal set for a pod
		for k, v := range pod.ObjectMeta.Annotations {
			// looks for annotation related to the seccomp
			if strings.Contains(k, "seccomp.security.alpha.kubernetes.io") {
				ns, crdName := getCRDandNamespace(v)
				//klog.Debug("namespace and name = ", ns, crdName)

				syscalls, _ := sc.readSPOProfileCRD(crdName, ns)

				// look for next annotation if the profile is empty
				if len(syscalls) == 0 {
					continue
				}

				// merge if there are multiple profiles
				r = UnionList(r, syscalls)
			}
		}
	}

	// if a pod does not have a seccomp profile specified, return unconfined syscall set
	if len(r) == 0 {
		r, _ = sc.getUnconfinedSyscalls()
	}

	//klog.Info("number of system calls for pod = ", len(r))
	return r, nil
}

// Returns all available system calls by reading the full SPO seccomp profile CRD
func (sc *SySched) getUnconfinedSyscalls() ([]string, error) {
	var r []string

	// extract a seccomp crd using namespace and crd name
	r, _ = sc.readSPOProfileCRD(sc.FullSyscallProfile, sc.SyscallCRDNamespace)

	// klog.Info("unconfined system calls for pod = ", r)
	return r, nil
}

// Name returns name of the plugin. It is used in logs, etc.
func (sc *SySched) Name() string {
	return Name
}

func (sc *SySched) calcScore(syscalls []string) int {
	tot_crit := 0

	// TODO: weight for critical/cve syscalls
	W := 1
	score := len(syscalls) - tot_crit
	score = score + W*tot_crit
	klog.Info(score, " ", tot_crit)

	return score
}

// Score invoked at the score extension point.
func (sc *SySched) Score(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	node, err := sc.handle.ClientSet().CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, nil
	}

	nodeIP := node.Status.Addresses[0].Address

	podSyscalls, _ := sc.getSyscalls(pod)

	// NOTE: this condition is true only when a pod does not
	// have a syscall profile, or the unconfined syscall is
	// not set. We return a large number (INT64_MAX) as score.
	if len(podSyscalls) == 0 {
		return math.MaxInt64, nil
	}

	_, hostSyscalls := sc.getHostSyscalls(nodeIP)
	// when a host or node does not have any pods
	// running, the extraneous syscall score is zero
	if hostSyscalls == nil {
		return 0, nil
	}

	diffSyscalls := setSubtract(hostSyscalls, podSyscalls)
	totalDiffs := sc.calcScore(diffSyscalls)

	// add the difference existing pods will see if new Pod is added into this host
	newHostSyscalls := make(map[string]bool)
	for k, v := range hostSyscalls {
		newHostSyscalls[k] = v
	}

	union(newHostSyscalls, podSyscalls)
	for _, p := range sc.HostToPods[nodeIP] {
		podSyscalls, _ = sc.getSyscalls(p)
		diffSyscalls = setSubtract(newHostSyscalls, podSyscalls)
		totalDiffs += sc.calcScore(diffSyscalls)
	}

	sc.ExSAvg = sc.ExSAvg + (float64(totalDiffs)-sc.ExSAvg)/float64(sc.ExSAvgCount)
	sc.ExSAvgCount += 1

	klog.Info("ExSAvg: ", sc.ExSAvg)
	klog.Info("Score: ", totalDiffs, " ", pod.Name, " ", nodeName)

	return int64(totalDiffs), nil
}

func (sc *SySched) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	klog.Info("Original scores: ", scores, " ", pod.Name)
	ret := helper.DefaultNormalizeScore(framework.MaxNodeScore, true, scores)
	klog.Info("Normalized scores: ", scores, " ", pod.Name)

	return ret
}

// ScoreExtensions of the Score plugin.
func (sc *SySched) ScoreExtensions() framework.ScoreExtensions {
	return sc
}

func (sc *SySched) getHostSyscalls(nodeIP string) (int, map[string]bool) {
	count := 0
	h, ok := sc.HostSyscalls[nodeIP]
	if !ok {
		klog.Infof("getHostSyscalls: no nodeIP %s", nodeIP)
		return count, nil
	}
	for s := range h {
		if h[s] {
			count++
		}
	}
	return count, h
}

func (sc *SySched) updateHostSyscalls(pod *v1.Pod) {
	syscall, _ := sc.getSyscalls(pod)
	union(sc.HostSyscalls[pod.Status.HostIP], syscall)
}

func (sc *SySched) addPod(pod *v1.Pod) {
	nodeIP := pod.Status.HostIP
	name := pod.Name

	_, ok := sc.HostToPods[nodeIP]
	if !ok {
		sc.HostToPods[nodeIP] = make([]*v1.Pod, 0)
		sc.HostToPods[nodeIP] = append(sc.HostToPods[nodeIP], pod)
		sc.HostSyscalls[nodeIP] = make(map[string]bool)
		sc.updateHostSyscalls(pod)
		return
	}

	for _, p := range sc.HostToPods[nodeIP] {
		if p.Name == name {
			return
		}
	}

	sc.HostToPods[nodeIP] = append(sc.HostToPods[nodeIP], pod)
	sc.updateHostSyscalls(pod)

	return
}

func (sc *SySched) recomputeHostSyscalls(pods []*v1.Pod) map[string]bool {
	syscalls := make(map[string]bool)

	for _, p := range pods {
		syscall, _ := sc.getSyscalls(p)
		union(syscalls, syscall)
	}

	return syscalls
}

func (sc *SySched) removePod(pod *v1.Pod) {
	nodeIP := pod.Status.HostIP

	_, ok := sc.HostToPods[nodeIP]
	if !ok {
		klog.Infof("removePod: Host %s not yet cached", nodeIP)

		return
	}
	for i, p := range sc.HostToPods[nodeIP] {
		if p.Name == pod.Name {
			sc.HostToPods[nodeIP] = remove(sc.HostToPods[nodeIP], i)
			sc.HostSyscalls[nodeIP] = sc.recomputeHostSyscalls(sc.HostToPods[nodeIP])
			c, _ := sc.getHostSyscalls(nodeIP)
			klog.Info("remaining syscalls: ", c, " ", nodeIP)

			return
		}
	}

	return
}

/*
func (sc *SySched) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
}*/

func (sc *SySched) podUpdated(old, new interface{}) {
	pod := old.(*v1.Pod)
	klog.Infof(
		"POD UPDATED. %s/%s %s",
		pod.Namespace, pod.Name, pod.Status.Phase,
	)

	if pod.Status.Phase == v1.PodSucceeded {
		// finished
		sc.removePod(pod)
	} else if pod.Status.Phase == v1.PodPending && pod.Status.HostIP != "" {
		//klog.Info("pod pending with IP assigned")
		sc.addPod(pod)
		//} else if (pod.Status.Phase == v1.PodRunning) {
		//klog.Info("pod running")
		//sc.addPod(pod)
	} else if pod.Status.Phase == v1.PodFailed {
		if pod.Status.Message != "" {
			klog.Infof("pod failed: %s", pod.Status.Message)
		}
		klog.Infof("pod failed: pod.Status.Message unknown")
	}
}

func (sc *SySched) podDeleted(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
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
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	sc := SySched{handle: handle}
	sc.HostToPods = make(map[string][]*v1.Pod)
	sc.HostSyscalls = make(map[string]map[string]bool)
	//sc.CritSyscalls = make(map[string][]string)
	sc.ExSAvg = 0
	sc.ExSAvgCount = 1

	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}

	// get the syscall CRD namespace name and CRD names
	// for full and weighted system call profiles
	sc.SyscallCRDNamespace = args.SySchedCRDNamespace
	sc.FullSyscallProfile = args.SySchedFullCRDName


	v1beta1.AddToScheme(scheme.Scheme)

	clientSet, err := v1alpha1.NewForConfig(handle.KubeConfig())
	if err != nil {
		return nil, err
	}

	sc.clientSet = clientSet

	podInformer := handle.SharedInformerFactory().Core().V1().Pods()

	//klog.Info("Setting up pod event handlers")
	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			//AddFunc:    sc.podAdded,
			UpdateFunc: sc.podUpdated,
			DeleteFunc: sc.podDeleted,
		},
	)

	return &sc, nil
}
