package sysched

//Todos
// 1. system call list for an unconfined pod (no seccomp profiles)
// 2. how to handle input of wieghts
// 3. Adding a readme file
// 4. Adding a scheduler-config file

import (
	"context"
	"math/rand"
	"strings"

	"github.com/containers/common/pkg/seccomp"
	v1 "k8s.io/api/core/v1"
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
	handle       framework.Handle
	clientSet    v1alpha1.SPOV1Alpha1Interface
	HostToPods   map[string][]*v1.Pod
	HostSyscalls map[string]map[string]bool
	CritSyscalls map[string][]string
	ExSAvg       float64
	ExSAvgCount  int64
}

var _ framework.ScorePlugin = &SySched{}

// Name is the name of the plugin used in Registry and configurations.
const Name = "SySched"

func setSubtract(hostsyscalls map[string]bool, podsyscalls []string) []string {
	var syscalls []string
	hsyscalls := make(map[string]bool)

	for k, v := range hostsyscalls {
		hsyscalls[k] = v
	}

	for _, s := range podsyscalls {
		_, ok := hsyscalls[s]
		if ok {
			hsyscalls[s] = false
		}
	}
	for s := range hsyscalls {
		if hsyscalls[s] {
			syscalls = append(syscalls, s)
		}
	}

	return syscalls
}

func union(syscalls map[string]bool, newsyscalls []string) {
	for _, s := range newsyscalls {
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

func getCRDandNamespace(localhostProfile string) (string, string) {
	// this function get the local profile relative path
	// e.g., localhost/operator/default/httpd-seccomp.json
	if localhostProfile == "" {
		return "", ""
	}

	parts := strings.Split(localhostProfile, "/")
	if len(parts) != 4 {
		return "", ""
	}
	ns := parts[2]

	file_parts := strings.Split(parts[3], ".")
	if len(file_parts) != 2 {
		return ns, ""
	}
	crd_name := file_parts[0]

	return ns, crd_name
}

func (sc *SySched) getSyscalls(pod *v1.Pod) ([]string, error) {
	//FIXME: if the system call list empty, i.e., a pod does
	// not have a seccomp profile or spo annotation, return all
	// available system calls
	var r []string
	if pod.ObjectMeta.Annotations == nil {
		return r, nil
	}

	// this loop looks for all annoations in a pod
	// and get the secommp security context related
	// annotations, extract the seccomp crd name from
	// an annotation, get the crd using the name, and
	// merge system calls from multiple crds (if any)
	// or system calls from mutiple action types
	for k, v := range pod.ObjectMeta.Annotations {
		// looks for annotation related to the seccomp
		if strings.Contains(k, "seccomp.security.alpha.kubernetes.io") {
			ns, crd_name := getCRDandNamespace(v)
			//klog.Debug("namespace and name = ", ns, crd_name)
			if ns == "" || crd_name == "" {
				continue
			}

			// extract a seccomp crd using namespace and crd name
			// crd name is the json file without extension in the
			// annotion.

			profile, err := sc.clientSet.Profiles().Get(crd_name, ns, metav1.GetOptions{})
			if err != nil {
				continue
			}

			//profile := raw_profile.(*v1beta1.SeccompProfile)
			syscall_categories := profile.Spec.Syscalls

			// need to merge the syscalls in the syscall categories
			// from multiple relevant actions, e.g., allow, log, notify
			// todo: do we need to include the SCMP_ACT_TRACE --> ActTrace
			// FIXME: what are the categories that we should consider?
			for _, element := range syscall_categories {
				if element.Action == seccomp.ActAllow || element.Action == seccomp.ActLog {
					//|| element.Action == seccomp.ActNotify {

					r = UnionList(r, element.Names) //merge syscalls with the result
				}
			}
		}
	}

	//klog.Info("system calls for pod = ", r)
	return r, nil
}

// Name returns name of the plugin. It is used in logs, etc.
func (sc *SySched) Name() string {
	return Name
}

func (sc *SySched) calcScore(syscalls []string) int {
	tot_crit := 0

	//FIXME: weight for critical/cve syscalls
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

	podsyscalls, _ := sc.getSyscalls(pod)
	if len(podsyscalls) == 0 {
		return int64(rand.Intn(2)), nil
	}

	_, hostsyscalls := sc.getHostSyscalls(nodeIP)
	if hostsyscalls == nil {
		return 0, nil
	}

	diffsyscalls := setSubtract(hostsyscalls, podsyscalls)
	totalDiffs := sc.calcScore(diffsyscalls)

	// add the difference existing pods will see if new Pod is added into this host
	newhostsyscalls := make(map[string]bool)
	for k, v := range hostsyscalls {
		newhostsyscalls[k] = v
	}

	union(newhostsyscalls, podsyscalls)
	for _, p := range sc.HostToPods[nodeIP] {
		podsyscalls, _ = sc.getSyscalls(p)
		diffsyscalls = setSubtract(newhostsyscalls, podsyscalls)
		totalDiffs += sc.calcScore(diffsyscalls)
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

// New initializes a new plugin and returns it.
func New(_ runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	sc := SySched{handle: handle}
	sc.HostToPods = make(map[string][]*v1.Pod)
	sc.HostSyscalls = make(map[string]map[string]bool)
	//sc.CritSyscalls = make(map[string][]string)
	sc.ExSAvg = 0
	sc.ExSAvgCount = 1

	v1beta1.AddToScheme(scheme.Scheme)

	clientSet, err := v1alpha1.NewForConfig(handle.KubeConfig())
	if err != nil {
		return nil, err
	}

	sc.clientSet = clientSet

	podInformer := handle.SharedInformerFactory().Core().V1().Pods()

	klog.Info("Setting up pod event handlers")
	podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			//AddFunc:    sc.podAdded,
			UpdateFunc: sc.podUpdated,
			DeleteFunc: sc.podDeleted,
		},
	)

	return &sc, nil
}
