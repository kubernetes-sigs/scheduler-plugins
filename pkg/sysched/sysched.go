package sysched

import (
	"context"
        "fmt"
	"math/rand"
        "sync"
	//"bufio"
        //"os"
        //"io/ioutil"
        "strings"
	//"strconv"
        //"encoding/json"

	v1 "k8s.io/api/core/v1"
        //"k8s.io/apimachinery/pkg/api/errors"
        //"k8s.io/apimachinery/pkg/fields"
        metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
        "k8s.io/apimachinery/pkg/watch"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
        "k8s.io/kubernetes/pkg/scheduler/framework/plugins/helper"
        //"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/klog/v2"
	//"sigs.k8s.io/scheduler-plugins/pkg/generated/clientset/versioned"
	//"k8s.io/apimachinery/pkg/runtime/schema"
        //"k8s.io/apimachinery/pkg/runtime/serializer"
	//"k8s.io/client-go/kubernetes"
        "k8s.io/client-go/kubernetes/scheme"
        //"k8s.io/client-go/rest"
        //"k8s.io/client-go/tools/clientcmd"
        //"k8s.io/client-go/util/homedir"
        //"k8s.io/klog/v2"
	"github.com/containers/common/pkg/seccomp"
        "sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
	"sigs.k8s.io/scheduler-plugins/pkg/sysched/clientset/v1alpha1"
)

var VolumeRoot = "/vagrant/"

type SySched struct{
    handle framework.Handle
    clientSet v1alpha1.SPOV1Alpha1Interface
    volumeRoot string
    HostToPods map[string] []*v1.Pod
    HostSyscalls map[string] map[string]bool
    CritSyscalls map[string][]string
    ExSAvg float64
    ExSAvgCount int64
}

var _ framework.ScorePlugin = &SySched{}

// Name is the name of the plugin used in Registry and configurations.
//const Name = names.SySched
const Name = "SySched"

/*
func getMatchingFileName(dir string, podname string) string {
    files, err := ioutil.ReadDir(dir)
    if err != nil {
        klog.Info(err)
    }
    for _, f := range files {
        //klog.Info(f.Name())
        //if matchName(podname, f.Name()) {
        if podname == f.Name() {
            return f.Name()
        }
    }
    return ""
}
*/

func setSubtract(hostsyscalls map[string] bool, podsyscalls []string) []string {
    var syscalls []string
    hsyscalls := make(map[string] bool)

    for k, v := range hostsyscalls {
        hsyscalls[k] = v
    }

    for _,s := range podsyscalls {
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
	for _,s := range newsyscalls {
		syscalls[s] = true
	}
}

func remove(s []*v1.Pod, i int) []*v1.Pod {
    s[i] = s[len(s)-1]
    return s[:len(s)-1]
}

/*
//FIXME: most likely will have to use container image names
// rather than pod names
func matchName(n string, n1 string) bool {
    if len(n1) > len(n) {
        return false
    }
    for i := 0; i < len(n1); i++ {
        if n1[i] != n[i] {
            return false
        }
    }
    return true
}*/

/*func (sc *SySched) getSyscalls(name string) ([]string, error) {
    filename := getMatchingFileName(sc.volumeRoot, name)
    return sc.readSyscalls(filename)
}*/

/*func (sc *SySched) readSyscalls(filename string) ([]string, error) {
    var syscalls []string

    klog.Info("reading file:", filename)
    f, err := os.Open(sc.volumeRoot + filename)
    if err != nil {
        klog.Error(err)
        return syscalls, err
    }

    defer f.Close()

    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
        //fmt.Println(scanner.Text())
        s := scanner.Text()
        syscalls = append(syscalls, s)
    }

    if err := scanner.Err(); err != nil {
        klog.Error(err)
        return syscalls, err
    }

    return syscalls, nil
}*/

/*
func (sc *SySched) readSyscalls(filename string) ([]string, error) {
    var syscalls []string

    klog.Info("reading file:", filename)
    //s, err := ioutil.ReadFile(sc.volumeRoot + "seccomp/" + filename)
    s, err := ioutil.ReadFile(filename)
    if err != nil {
        klog.Error(err)
        return syscalls, err
    }

    klog.Info("middle of readSyscalls function")

    var result map[string]interface{}
    json.Unmarshal(s,&result)
    if _, err := result["syscalls"]; !err {
        return syscalls, nil
    }
    r := result["syscalls"].([]interface{})
    r1 := r[0].(map[string]interface{})
    r2 := r1["names"].([]interface{})
    for _, value := range r2 {
        syscalls = append(syscalls, value.(string))
    }
    klog.Info(syscalls)
    return syscalls, nil
}


func (sc *SySched) getSyscalls(pod *v1.Pod) ([]string, error) {
    klog.Info("start getSyscalls function")
    //klog.Info("pod.annotations = ", pod.ObjectMeta.Annotations)
    //klog.Info("pod.spec = ", pod.Spec)
    //klog.Info("pod.spec.SecurityContext of containers = ", pod.Spec.Containers[0].SecurityContext)
    var m_SecurityContext = pod.Spec.Containers[0].SecurityContext


    //if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != "Localhost" {
    if m_SecurityContext == nil || m_SecurityContext.SeccompProfile == nil || m_SecurityContext.SeccompProfile.Type != "Localhost" {
        var r []string
        return r, nil
    }
    klog.Info("middle of getSyscalls function")
    //s := strings.Split(*pod.Spec.SecurityContext.SeccompProfile.LocalhostProfile, "/")
    //s := strings.Split(*m_SecurityContext.SeccompProfile.LocalhostProfile, "/")
    //m_str := []string{"/var/lib/kubelet/seccomp", *m_SecurityContext.SeccompProfile.LocalhostProfile}
    s := "/var/lib/kubelet/seccomp/" + *m_SecurityContext.SeccompProfile.LocalhostProfile
    klog.Info("before calling readSyscalls function = ", s)
    //return sc.readSyscalls(s[len(s)-1])
    return sc.readSyscalls(s)
}
*/

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

func getCRDandNamespace(localhostProfile string)(string, string) {
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


func (sc *SySched)getSyscalls(pod *v1.Pod) ([]string, error) {
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
	for k, v:= range pod.ObjectMeta.Annotations {
		// looks for annotation related to the seccomp
		if strings.Contains(k, "seccomp.security.alpha.kubernetes.io") {
			ns, crd_name := getCRDandNamespace(v)
			//klog.Info("namespace and name = ", ns, crd_name)
			if ns == "" || crd_name == "" {
				continue
			}
			//fmt.Printf("ns = %s, name = %s\n", ns, crd_name)
			//fmt.Printf("\n")

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
			for _, element := range syscall_categories {
				if element.Action == seccomp.ActAllow || element.Action == seccomp.ActLog {
					//|| element.Action == seccomp.ActNotify {

					r = UnionList(r, element.Names) //merge syscalls with the result
				}
			}
		}
	}

	//fmt.Printf("Len = %d\n", len(r))
	klog.Info("system calls for pod = ", r)
	return r, nil
}

// Name returns name of the plugin. It is used in logs, etc.
func (sc *SySched) Name() string {
    return Name
}

/*func (sc *SySched) getExSAvg() int {
    tot := 0
    for _,s := range sc.HostToExS {
        tot += s
    }
    if len(sc.HostToExS) > 0 {
        return int(tot/len(sc.HostToExS))
    }
    return 1
}*/

func (sc *SySched) calcScore(syscalls []string) int {
    tot_crit := 0
    for _, s := range syscalls {
        /*for _, c := range sc.CritSyscalls["critical_syscalls"] {
            if s == c {
                tot_crit += 1
            }
        }
        for _, c := range sc.CritSyscalls["cves_confine"] {
            if s == c {
                tot_crit += 1
            }
        }
        for _, c := range sc.CritSyscalls["cves_temporal"] {
            if s == c {
                tot_crit += 1
            }
        }*/
        /*for _, c := range sc.CritSyscalls["crit_cves_combined"] {
            if s == c {
                tot_crit += 1
            }
        }*/
        for _, c := range sc.CritSyscalls["W"] {
            //sys := strings.Split(c,":")[0]
	    //W, err := strconv.Atoi(strings.Split(c,":")[1])
	    //if err != nil {
            //    klog.Error(err)
            //    return 0
            //}
            if s == c {
                tot_crit += 1
            }
        }
    }
    //NOTE: weight for critical/cve syscalls
    W := 100
    score := len(syscalls) - tot_crit
    score = score + W*tot_crit
    klog.Info(score, " ", tot_crit)
    return score
}

/*func (sc *SySched) calcScore(syscalls []string) int {
    tot_crit := 0
    tot_weighted_score := 0
    //exsavg := sc.getExSAvg()
    for _, s := range syscalls {
        for _, c := range sc.CritSyscalls["W"] {
            sys := strings.Split(c,":")[0]
	    W, err := strconv.ParseFloat(strings.Split(c,":")[1], 64)
	    if err != nil {
                klog.Error(err)
                return 0
            }
            if s == sys {
                tot_crit += 1
                tot_weighted_score += int(W*sc.ExSAvg)
            }
        }
    }
    score := len(syscalls) - tot_crit
    score = score + tot_weighted_score
    klog.Info("calcScore: ", score, " ", tot_crit, " ", tot_weighted_score)
    return score
}*/

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
    //klog.Info("New pod name: ", pod.Name, " " , "diffs: ", diffsyscalls, nodeName, nodeIP)
    //totalDiffs := len(diffsyscalls)
    totalDiffs := sc.calcScore(diffsyscalls)

    // add the difference existing pods will see if new Pod is added into this host
    // NOTE: do we want union here or just diff against new podsyscall list?
    newhostsyscalls := make(map[string] bool)
    for k,v := range hostsyscalls {
        newhostsyscalls[k] = v
    }
    union(newhostsyscalls, podsyscalls)
    for _, p := range sc.HostToPods[nodeIP] {
       podsyscalls, _ = sc.getSyscalls(p)
       diffsyscalls = setSubtract(newhostsyscalls, podsyscalls)
       //totalDiffs += len(diffsyscalls)
       totalDiffs += sc.calcScore(diffsyscalls)
    }
    sc.ExSAvg = sc.ExSAvg + (float64(totalDiffs) - sc.ExSAvg)/float64(sc.ExSAvgCount)
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

func (sc *SySched) getHostSyscalls(nodeIP string) (int,map[string] bool) {
    count := 0
    //klog.Info("getHostSyscalls: ", nodeIP)
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
    return count,h
}

func (sc *SySched) updateHostSyscalls(pod *v1.Pod) {
    //klog.Info("start updateHostSyscalls:", pod.Status.HostIP)
    syscall, _ := sc.getSyscalls(pod)
    union(sc.HostSyscalls[pod.Status.HostIP], syscall)
    //klog.Info("end updateHostSyscalls:", pod.Status.HostIP)
}

func (sc *SySched) addPod(pod *v1.Pod) {
	//klog.Info("start of addPod function")
        nodeIP := pod.Status.HostIP
        name := pod.Name

	_, ok := sc.HostToPods[nodeIP]
	if !ok {
		sc.HostToPods[nodeIP] = make([]*v1.Pod,0)
		sc.HostToPods[nodeIP] = append(sc.HostToPods[nodeIP], pod)
                sc.HostSyscalls[nodeIP] = make(map[string] bool)
		sc.updateHostSyscalls(pod)
                //klog.Info(sc.HostToPods)
                //klog.Info(sc.getHostSyscalls(nodeIP))
		return
	}
        for _, p := range sc.HostToPods[nodeIP] {
            if p.Name == name {
                return
            }
        }
	sc.HostToPods[nodeIP] = append(sc.HostToPods[nodeIP], pod)
        sc.updateHostSyscalls(pod)
        //klog.Info(sc.HostToPods)
        //klog.Info(sc.getHostSyscalls(nodeIP))
	//klog.Info("end of addPod function")
	return
}

func (sc *SySched) recomputeHostSyscalls(pods []*v1.Pod) map[string] bool {
    syscalls := make(map[string] bool)
    for _,p := range pods {
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
            //klog.Info(sc.HostToPods, " ", nodeIP)
            c, _ := sc.getHostSyscalls(nodeIP)
            klog.Info("remaining syscalls: ", c, " ", nodeIP)
            return
        }
    }
    //klog.Infof("removePod: Pod %s in host %s not found", pod.Name, nodeIP)
    return
}

func (sc *SySched) WaitForPod(podCh <-chan watch.Event) error {
        for {
                event, ok := <-podCh
                if !ok {
			klog.Info("event not ok")
                        return fmt.Errorf("event not ok")
                }
                switch event.Object.(type) {
                case *v1.Pod:
                        // POD changed
                        var pod *v1.Pod = event.Object.(*v1.Pod)
                        //klog.Infof("sysched pod update received: %s %s/%s %s %s", event.Type, pod.Namespace, pod.Name, pod.Status.Phase, pod.Status.HostIP)
                        switch event.Type {
                        //case watch.Added:
                        //    klog.Info("pod added")
                        case watch.Modified:
                                if pod.Status.Phase == v1.PodSucceeded {
                                        // finished
					//klog.Info("pod success")
                                        //sc.removePod(pod.Name, pod.Status.HostIP)
                                        sc.removePod(pod)
				} else if (pod.Status.Phase == v1.PodPending && pod.Status.HostIP != "") {
					//klog.Info("pod pending with IP assigned")
                                        sc.addPod(pod)
				//} else if (pod.Status.Phase == v1.PodRunning) {
					//klog.Info("pod running")
                                //        sc.addPod(pod)
                                } else if pod.Status.Phase == v1.PodFailed {
                                        if pod.Status.Message != "" {
						klog.Infof("pod failed: %s", pod.Status.Message)
                                        }
			                klog.Infof("pod failed: pod.Status.Message unknown", )
                                }

                        case watch.Deleted:
			        //klog.Info("pod deleted")
                                sc.removePod(pod)

                        //case watch.Error:
			        //klog.Info("pod watcher failed")
                        }

                /*case *v1.Event:
                        // Event received
                        podEvent := event.Object.(*v1.Event)
                        klog.Infof("sysched event received: %s %s/%s %s/%s %s", event.Type, podEvent.Namespace, podEvent.Name, podEvent.InvolvedObject.Namespace, podEvent.InvolvedObject.Name, podEvent.Message)
                        if event.Type == watch.Added {
                        }
                */
                }
        }
}

func (sc *SySched) WatchPod(namespace string, stopChannel chan struct{}) (<-chan watch.Event, error) {
        //podSelector, err := fields.ParseSelector("metadata.name=" + name)
        //if err != nil {
        //        return nil, err
        //}
	var timeout int64 = 180
        options := metav1.ListOptions{
                //FieldSelector: podSelector.String(),
                Watch:         true,
                TimeoutSeconds: &timeout,
        }
        podWatch, err := sc.handle.ClientSet().CoreV1().Pods(namespace).Watch(context.TODO(), options)
        if err != nil {
                return nil, err
        }

        //eventSelector, _ := fields.ParseSelector("involvedObject.name=" + name)
        //eventWatch, err := sc.handle.ClientSet().CoreV1().Events(namespace).Watch(context.TODO(), metav1.ListOptions{
                //FieldSelector: eventSelector.String(),
        //        Watch:         true,
        //})
        //if err != nil {
        //        podWatch.Stop()
        //        return nil, err
        //}

        eventCh := make(chan watch.Event, 30)
        var wg sync.WaitGroup
        wg.Add(1)
        //wg.Add(2)
        go func() {
                defer close(eventCh)
                wg.Wait()
        }()

        /*go func() {
                defer eventWatch.Stop()
                defer wg.Done()
                for {
                        select {
                        case <-stopChannel:
                                return
                        case eventEvent, ok := <-eventWatch.ResultChan():
                                if !ok {
                                        return
                                }
                                eventCh <- eventEvent
                        }
                }
        }()*/

        go func() {
                defer podWatch.Stop()
                defer wg.Done()
                for {
			//klog.Info("watching")
                        select {
                        case <-stopChannel:
			        klog.Info("stopChannel exited")
                                return

                        case podEvent, ok := <-podWatch.ResultChan():
                                if !ok {
			                klog.Info("podEvent not ok")
                                        return
                                }
                                eventCh <- podEvent
                        }
                }
        }()

        return eventCh, nil
}


// New initializes a new plugin and returns it.
func New(_ runtime.Object, handle framework.Handle) (framework.Plugin, error) {
    stopChannel := make(chan struct{})
    sc := SySched{handle: handle, volumeRoot: VolumeRoot}
    sc.HostToPods = make(map[string] []*v1.Pod)
    sc.HostSyscalls = make(map[string] map[string]bool)
    sc.CritSyscalls = make(map[string][]string)
    sc.ExSAvg = 0
    sc.ExSAvgCount = 1

    v1beta1.AddToScheme(scheme.Scheme)
    clientSet, err := v1alpha1.NewForConfig(handle.KubeConfig())
    if err != nil {
	return nil, err
    }

    sc.clientSet = clientSet


    /*s, err := ioutil.ReadFile(sc.volumeRoot + "perf/syscall-vulnerable.json")
    if err != nil {
        klog.Error(err)
        return &sc, nil
    }
    json.Unmarshal(s,&sc.CritSyscalls)*/

    go func() {
        defer close(stopChannel)
	klog.Info("starting watching thread")
        namespace := "default"
	for {
            podCh, err := sc.WatchPod(namespace, stopChannel)
            if err != nil {
                klog.Info("resetting watcher")
            } else {
                err = sc.WaitForPod(podCh)
            }
        }
    }()
    //klog.Info("end of New function")
    return &sc, nil
}
