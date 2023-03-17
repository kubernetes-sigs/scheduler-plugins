package sysched

import (
	//"fmt"
	"strings"
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"context"
	"io/ioutil"
	"github.com/stretchr/testify/assert"
	restfake "k8s.io/client-go/rest/fake"
	restclient "k8s.io/client-go/rest"
	v1 "k8s.io/api/core/v1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
        "k8s.io/apimachinery/pkg/runtime/schema"
        "k8s.io/client-go/kubernetes/scheme"
	v1alpha1 "sigs.k8s.io/scheduler-plugins/pkg/sysched/clientset/v1alpha1"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	informers "k8s.io/client-go/informers"
)

var (
	spoResponse = map[string]interface{}{
	        "apiVersion": "security-profiles-operator.x-k8s.io/v1beta1",
	        "kind": "SeccompProfile",
	        "metadata": map[string]interface{} {
	                "annotations": map[string]interface{}{},
	                "name": "z-seccomp",
	                "namespace": "default",
	        },
	        "spec": map[string]interface{} {
	                "architectures": []string {"SCMP_ARCH_X86_64"},
	                "defaultAction": "SCMP_ACT_LOG",
	                "syscalls":[]map[string]interface{}{{
	                        "action":"SCMP_ACT_ALLOW",
	                        "names": []string {"clone","socket","getuid","setrlimit","nanosleep","sendto","setuid","getpgrp","mkdir","getegid","getsockname","clock_gettime","prctl","epoll_pwait","futex","link","ftruncate","access","gettimeofday","select","getsockopt","mmap","write","connect","capget","chmod","arch_prctl","wait4","brk","stat","getrlimit","fsync","chroot","recvfrom","newfstatat","setresgid","poll","lstat","listen","getpgid","sigreturn","setreuid","setgid","signaldeliver","recvmsg","bind","close","setsockopt","openat","container","getpeername","lseek","procexit","uname","statfs","utime","pipe","getcwd","chdir","execve","rt_sigaction","set_tid_address","dup","ioctl","munmap","rename","kill","getpid","alarm","umask","setresuid","exit_group","fstat","geteuid","mprotect","read","getppid","fchown","capset","rt_sigprocmask","accept","setgroups","open","set_robust_list","fchownat","unlink","getdents","fcntl","readlink","getgid","fchmod"},
	                },},
	        },
	}

	spoResponse1 = map[string]interface{}{
	        "apiVersion": "security-profiles-operator.x-k8s.io/v1beta1",
	        "kind": "SeccompProfile",
	        "metadata": map[string]interface{} {
	                "annotations": map[string]interface{}{},
	                "name": "x-seccomp",
	                "namespace": "default",
	        },
	        "spec": map[string]interface{} {
	                "architectures": []string {"SCMP_ARCH_X86_64"},
	                "defaultAction": "SCMP_ACT_LOG",
	                "syscalls":[]map[string]interface{}{{
	                        "action":"SCMP_ACT_ALLOW",
	                        "names": []string {"clone","socket","getuid","setrlimit","nanosleep","sendto","setuid","getpgrp","mkdir","getegid","getsockname","clock_gettime","prctl","epoll_pwait","futex","link","ftruncate","access","gettimeofday","select","getsockopt","mmap","write","connect","capget","chmod","arch_prctl","wait4","brk","stat","getrlimit","fsync","chroot","recvfrom","newfstatat","setresgid","poll","lstat","listen","getpgid","sigreturn","setreuid","setgid","signaldeliver","recvmsg","bind","close","setsockopt","openat","container","getpeername","lseek","procexit","uname","statfs","utime","pipe","getcwd","chdir","execve","rt_sigaction","set_tid_address","dup","ioctl","munmap","rename","kill","getpid","alarm","umask","setresuid","exit_group","fstat","geteuid","mprotect","read","getppid","fchown","capset","rt_sigprocmask","accept","setgroups","open","set_robust_list","fchownat","unlink","getdents","fcntl","readlink","getgid","dup3"},
	                },},
	        },
	}

	groupVersion = &schema.GroupVersion{Group: "security-profiles-operator.x-k8s.io",
					Version: "v1beta1"}
	aPIPath = "/apis"
	negotiatedSerializer = scheme.Codecs.WithoutConversion()

	restclientSPO = restfake.RESTClient{NegotiatedSerializer: negotiatedSerializer,
				GroupVersion: *groupVersion, VersionedAPIPath: aPIPath, Client: httpclient}
	spoclient = v1alpha1.SPOV1Alpha1Client{RestClient: &restclientSPO}

	// fake SPO Get return
	httpclient = restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
		//fmt.Printf(req.URL.String())
		podname := strings.Split(req.URL.String(), "/")[len(strings.Split(req.URL.String(), "/"))-1]
		//z-seccomp
		body, _ := json.Marshal(spoResponse)
		if podname == "x-seccomp" {
			body, _ = json.Marshal(spoResponse1)
		}
		return &http.Response{Body: ioutil.NopCloser(bytes.NewBuffer(body)), StatusCode: 200}, nil
	})

	registeredPlugins = []st.RegisterPluginFunc{
		st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
)


func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

func TestSetSubtract(t *testing.T) {
	hostsyscalls := make(map[string]bool)
        hostsyscalls["syscall1"] = true
        hostsyscalls["syscall2"] = true
        hostsyscalls["syscall3"] = true
        hostsyscalls["syscall4"] = true
        hostsyscalls["syscall5"] = true

        podsyscalls := []string{"syscall5", "syscall4"}

	s := setSubtract(hostsyscalls, podsyscalls)

	if len(s) != 3 {
		t.Errorf("Incorrect size")
	}
        if contains(s, "syscall1") == false {
		t.Errorf("Missing syscall1")
	}
        if contains(s, "syscall2") == false {
		t.Errorf("Missing syscall2")
	}
        if contains(s, "syscall3") == false {
		t.Errorf("Missing syscall3")
	}
        if contains(s, "syscall4") == true {
		t.Errorf("syscall4 not removed")
	}
        if contains(s, "syscall5") == true {
		t.Errorf("syscall3 not removed")
	}
}

func TestUnion(t *testing.T) {
	hostsyscalls := make(map[string]bool)
        hostsyscalls["syscall1"] = true
        hostsyscalls["syscall2"] = true
        hostsyscalls["syscall3"] = true

	podsyscalls := []string{"syscall4", "syscall5"}
	union(hostsyscalls, podsyscalls)

	if len(hostsyscalls) != 5 {
		t.Errorf("Incorrect size")
	}
	expected := make(map[string]bool)
	expected["syscall1"] = true
	expected["syscall2"] = true
	expected["syscall3"] = true
	expected["syscall4"] = true
	expected["syscall5"] = true

        for k, _ := range expected {
		if _, ok := hostsyscalls[k]; !ok {
			t.Errorf("missing element")
		}
	}
}

func TestRemove(t *testing.T) {
	pods := make([]*v1.Pod, 3)
	pods[0] = st.MakePod().Name("Pod-1").Obj()
	pods[1] = st.MakePod().Name("Pod-2").Obj()
	pods[2] = st.MakePod().Name("Pod-3").Obj()
	p := remove(pods, 2)
	if len(p) != 2 {
		t.Errorf("remove pod failed")
	}
}

func TestUnionList(t *testing.T) {
	s := []string {"test", "test1"}
	s1 := []string {"test1", "test2"}
	s2 := UnionList(s, s1)
	if len(s2) != 3 {
		t.Errorf("lenght incorrect")
	}
}

func TestGetCRDNamespace(t *testing.T) {
	s := "localhost/operator/default/httpd-seccomp.json"
	ns, crd_name := getCRDandNamespace(s)
	if ns != "default" {
		t.Errorf("incorrect namespace")
	}
	if crd_name != "httpd-seccomp" {
		t.Errorf("incorrect crd name")
	}
}

func mockSysched() (*SySched, error) {
        v1beta1.AddToScheme(scheme.Scheme)

	//fake out the framework handle
	ctx := context.Background()
        fr, err := st.NewFramework(registeredPlugins, Name, ctx.Done(),
                frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset()))
	if err != nil {
                return nil, err
	}

	sys := SySched{handle: fr, clientSet: &spoclient}
        sys.HostToPods = make(map[string][]*v1.Pod)
        sys.HostSyscalls = make(map[string]map[string]bool)
        sys.ExSAvg = 0
        sys.ExSAvgCount = 1

	return &sys, err
}

func TestGetSyscalls(t *testing.T) {
	//fake out a pod
	pod := st.MakePod().
		Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()

	sys, _ := mockSysched()
	syscalls, err := sys.getSyscalls(pod)
	assert.Nil(t, err)
	assert.NotNil(t, syscalls)
	expected := spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string)
	assert.EqualValues(t, len(syscalls), len(expected))
}

func TestCalcScore(t *testing.T) {
	sys, _ := mockSysched()
	score := sys.calcScore(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
	expected := spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string)
	assert.EqualValues(t, score, len(expected))
}

func TestScore(t *testing.T) {
        v1beta1.AddToScheme(scheme.Scheme)

	//create nodes and pods
	hostIP := "192.168.0.1"
	node := st.MakeNode()
	node.Name("test")
	node.Status.Addresses = []v1.NodeAddress{v1.NodeAddress{Address: hostIP}}

	//fake out an existing pod
	pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
		"localhost/operator/default/z-seccomp.json").Obj()
	pod.Name = "pod1"
	pod.Status.HostIP = hostIP

        //fake out the framework handle
        ctx := context.Background()
        fr, err := st.NewFramework(registeredPlugins, Name, ctx.Done(),
                frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset(node.Obj())))
	if err != nil {
                t.Error(err)
	}

        sys := SySched{handle: fr, clientSet: &spoclient}
        sys.HostToPods = make(map[string][]*v1.Pod)
        sys.HostSyscalls = make(map[string]map[string]bool)
        sys.ExSAvg = 0
        sys.ExSAvgCount = 1

	sys.addPod(pod)

	//fake out a new pod to be scheduled
	pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/x-seccomp.json").Obj()

	score, _ := sys.Score(context.Background(), nil, pod1, "test")
	assert.EqualValues(t, score, 2)
}

func TestNormalizeScore(t *testing.T) {
	nodescores := framework.NodeScoreList{framework.NodeScore{Name: "test", Score: 100},
				framework.NodeScore{Name: "test1", Score: 200}}
	sys, _ := mockSysched()
	pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/httpd-seccomp.json").Obj()
	pod.Name = "pod1"
	ret := sys.NormalizeScore(context.TODO(), nil, pod, nodescores)
	assert.Nil(t, ret)
	assert.EqualValues(t, nodescores[0].Score, 50)
	assert.EqualValues(t, nodescores[1].Score, 0)
}

func  TestGetHostSyscalls(t *testing.T) {
        v1beta1.AddToScheme(scheme.Scheme)

        //create nodes and pods
        hostIP := "192.168.0.1"
        node := st.MakeNode()
        node.Name("test")
        node.Status.Addresses = []v1.NodeAddress{v1.NodeAddress{Address: hostIP}}

        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/httpd-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        //fake out the framework handle
        ctx := context.Background()
        fr, err := st.NewFramework(registeredPlugins, Name, ctx.Done(),
                frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset(node.Obj())))
        if err != nil {
                t.Error(err)
        }

        sys := SySched{handle: fr, clientSet: &spoclient}
        sys.HostToPods = make(map[string][]*v1.Pod)
        sys.HostSyscalls = make(map[string]map[string]bool)
        sys.ExSAvg = 0
        sys.ExSAvgCount = 1

        sys.addPod(pod)

	cnt, _ := sys.getHostSyscalls(hostIP)
	expected := spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string)
	assert.EqualValues(t, cnt, len(expected))
}

func TestUpdateHostSyscalls(t *testing.T) {
        v1beta1.AddToScheme(scheme.Scheme)

        //create nodes and pods
        hostIP := "192.168.0.1"
        node := st.MakeNode()
        node.Name("test")
        node.Status.Addresses = []v1.NodeAddress{v1.NodeAddress{Address: hostIP}}

        //fake out an existing pod
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        //fake out the framework handle
        ctx := context.Background()
        fr, err := st.NewFramework(registeredPlugins, Name, ctx.Done(),
                frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset(node.Obj())))
        if err != nil {
                t.Error(err)
        }

        sys := SySched{handle: fr, clientSet: &spoclient}

        sys.HostToPods = make(map[string][]*v1.Pod)
        sys.HostSyscalls = make(map[string]map[string]bool)
        sys.ExSAvg = 0
        sys.ExSAvgCount = 1

        sys.addPod(pod)

        pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/x-seccomp.json").Obj()
        pod1.Name = "pod2"
        pod1.Status.HostIP = hostIP

	sys.updateHostSyscalls(pod1)

	cnt := len(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
	sc, _ := sys.getHostSyscalls(hostIP)

	assert.EqualValues(t, sc, cnt+1)
}

func TestAddPod(t *testing.T) {
	sys, _ := mockSysched()

        //fake out a pod
        hostIP := "192.168.0.1"
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

	sys.addPod(pod)

	assert.EqualValues(t, sys.HostToPods[hostIP][0], pod)
	expected := len(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
	assert.EqualValues(t, len(sys.HostSyscalls[hostIP]), expected)
}

func TestRecomputeHostSyscalls(t *testing.T) {
        sys, _ := mockSysched()

        //fake out a pod
        hostIP := "192.168.0.1"
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/x-seccomp.json").Obj()
        pod1.Name = "pod2"
        pod1.Status.HostIP = hostIP

	syscalls := sys.recomputeHostSyscalls([]*v1.Pod{pod, pod1})
	expected := len(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
	assert.EqualValues(t, len(syscalls), expected+1)
}

func TestRemovePod(t *testing.T) {
	sys, _ := mockSysched()

        //fake out a pod
        hostIP := "192.168.0.1"
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/x-seccomp.json").Obj()
        pod1.Name = "pod2"
        pod1.Status.HostIP = hostIP

	sys.addPod(pod)
	sys.addPod(pod1)

	sys.removePod(pod)

	assert.EqualValues(t, len(sys.HostToPods[hostIP]), 1)
	expected := len(spoResponse1["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
	assert.EqualValues(t, len(sys.HostSyscalls[hostIP]), expected)
}

func TestPodUpdated (t *testing.T) {
        sys, _ := mockSysched()

        //fake out a pod
        hostIP := "192.168.0.1"
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
		"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
		"localhost/operator/default/x-seccomp.json").Obj()
        pod1.Name = "pod2"
        pod1.Status.HostIP = hostIP

        sys.addPod(pod)
        sys.addPod(pod1)

	pod1.Status.Phase = v1.PodSucceeded

	sys.podUpdated(pod1, nil)

        assert.EqualValues(t, len(sys.HostToPods[hostIP]), 1)
        expected := len(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
        assert.EqualValues(t, len(sys.HostSyscalls[hostIP]), expected)

	pod1.Status.Phase = v1.PodPending
	pod1.Status.HostIP = hostIP

	sys.podUpdated(pod1, nil)

        assert.EqualValues(t, len(sys.HostToPods[hostIP]), 2)
        expected = len(spoResponse["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
        assert.EqualValues(t, len(sys.HostSyscalls[hostIP]), expected+1)
}

func TestPodDeleted(t *testing.T) {
        sys, _ := mockSysched()

        //fake out a pod
        hostIP := "192.168.0.1"
        pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/z-seccomp.json").Obj()
        pod.Name = "pod1"
        pod.Status.HostIP = hostIP

        pod1 := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
			"localhost/operator/default/x-seccomp.json").Obj()
        pod1.Name = "pod2"
        pod1.Status.HostIP = hostIP

        sys.addPod(pod)
        sys.addPod(pod1)

        sys.podDeleted(pod)

        assert.EqualValues(t, len(sys.HostToPods[hostIP]), 1)
        expected := len(spoResponse1["spec"].(map[string]interface{})["syscalls"].
			([]map[string]interface{})[0]["names"].([]string))
        assert.EqualValues(t, len(sys.HostSyscalls[hostIP]), expected)
}

func TestNew(t *testing.T) {
        ctx := context.Background()
	fakeclient := clientsetfake.NewSimpleClientset()
        fr, err := st.NewFramework(registeredPlugins, Name, ctx.Done(),
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(fakeclient, 0)),
		frameworkruntime.WithKubeConfig(&restclient.Config{}),
                frameworkruntime.WithClientSet(fakeclient))
        if err != nil {
                t.Error(err)
        }

	sys, err := New(nil, fr)
	assert.Nil(t, err)
	assert.NotNil(t, sys)
}
