package sysched

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pluginconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
)

var (
	spoResponse = v1beta1.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "z-seccomp",
			Namespace: "default",
		},
		Spec: v1beta1.SeccompProfileSpec{
			Architectures: []v1beta1.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*v1beta1.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit", "fsync", "chroot", "recvfrom", "newfstatat", "setresgid", "poll", "lstat", "listen", "getpgid", "sigreturn", "setreuid", "setgid", "signaldeliver", "recvmsg", "bind", "close", "setsockopt", "openat", "container", "getpeername", "lseek", "procexit", "uname", "statfs", "utime", "pipe", "getcwd", "chdir", "execve", "rt_sigaction", "set_tid_address", "dup", "ioctl", "munmap", "rename", "kill", "getpid", "alarm", "umask", "setresuid", "exit_group", "fstat", "geteuid", "mprotect", "read", "getppid", "fchown", "capset", "rt_sigprocmask", "accept", "setgroups", "open", "set_robust_list", "fchownat", "unlink", "getdents", "fcntl", "readlink", "getgid", "fchmod"},
			},
			},
		},
	}

	spoResponse1 = v1beta1.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "x-seccomp",
			Namespace: "default",
		},
		Spec: v1beta1.SeccompProfileSpec{
			Architectures: []v1beta1.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*v1beta1.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit", "fsync", "chroot", "recvfrom", "newfstatat", "setresgid", "poll", "lstat", "listen", "getpgid", "sigreturn", "setreuid", "setgid", "signaldeliver", "recvmsg", "bind", "close", "setsockopt", "openat", "container", "getpeername", "lseek", "procexit", "uname", "statfs", "utime", "pipe", "getcwd", "chdir", "execve", "rt_sigaction", "set_tid_address", "dup", "ioctl", "munmap", "rename", "kill", "getpid", "alarm", "umask", "setresuid", "exit_group", "fstat", "geteuid", "mprotect", "read", "getppid", "fchown", "capset", "rt_sigprocmask", "accept", "setgroups", "open", "set_robust_list", "fchownat", "unlink", "getdents", "fcntl", "readlink", "getgid", "dup3"},
			},
			},
		},
	}

	spoResponseFull = v1beta1.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "full-seccomp",
			Namespace: "default",
		},
		Spec: v1beta1.SeccompProfileSpec{
			Architectures: []v1beta1.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*v1beta1.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit", "fsync", "chroot", "recvfrom", "newfstatat", "setresgid", "poll", "lstat", "listen", "getpgid", "sigreturn", "setreuid", "setgid", "signaldeliver", "recvmsg", "bind", "close", "setsockopt", "openat", "container", "getpeername", "lseek", "procexit", "uname", "statfs", "utime", "pipe", "getcwd", "chdir", "execve", "rt_sigaction", "set_tid_address", "dup", "ioctl", "munmap", "rename", "kill", "getpid", "alarm", "umask", "setresuid", "exit_group", "fstat", "geteuid", "mprotect", "read", "getppid", "fchown", "capset", "rt_sigprocmask", "accept", "setgroups", "open", "set_robust_list", "fchownat", "unlink", "getdents", "fcntl", "readlink", "getgid", "dup3", "pivot_root", "_sysctl", "adjtimex", "sync", "acct", "settimeofday", "statx", "pkey_free", "pkey_alloc", "pkey_mprotect"},
			},
			},
		},
	}

	registeredPlugins = []tf.RegisterPluginFunc{
		tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
		tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
	}
)

func TestRemove(t *testing.T) {
	tests := []struct {
		name        string
		pods        []*v1.Pod
		removeIndex []int
		expectedLen int
	}{
		{
			name: "Remove one",
			pods: []*v1.Pod{
				st.MakePod().Name("Pod-1").Obj(),
				st.MakePod().Name("Pod-2").Obj(),
				st.MakePod().Name("Pod-3").Obj(),
			},
			removeIndex: []int{1},
			expectedLen: 2,
		},
		{
			name: "Remove two",
			pods: []*v1.Pod{
				st.MakePod().Name("Pod-1").Obj(),
				st.MakePod().Name("Pod-2").Obj(),
				st.MakePod().Name("Pod-3").Obj(),
			},
			removeIndex: []int{1, 0},
			expectedLen: 1,
		},
		{
			name: "Remove all",
			pods: []*v1.Pod{
				st.MakePod().Name("Pod-1").Obj(),
				st.MakePod().Name("Pod-2").Obj(),
				st.MakePod().Name("Pod-3").Obj(),
			},
			removeIndex: []int{2, 1, 0},
			expectedLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pods
			for _, i := range tt.removeIndex {
				p = remove(p, i)
			}
			if len(p) != tt.expectedLen {
				t.Errorf("remove pod failed %d", len(p))
			}
		})
	}
}

func TestParseNameNS(t *testing.T) {
	tests := []struct {
		name         string
		s            string
		expectedName string
		expectedNS   string
	}{
		{
			name:         "Get Name Space and Name",
			s:            "localhost/operator/default/httpd-seccomp.json",
			expectedName: "httpd-seccomp",
			expectedNS:   "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, name := parseNameNS(tt.s)
			if ns != tt.expectedNS {
				t.Errorf("incorrect namespace")
			}
			if name != tt.expectedName {
				t.Errorf("incorrect name")
			}
		})
	}
}

func mockSysched() (*SySched, error) {
	// fake out the framework handle
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr, err := tf.NewFramework(ctx, registeredPlugins, Name,
		frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset()))
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := clientscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}

	objs := []runtime.Object{&spoResponse, &spoResponse1, &spoResponseFull}
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	sys := SySched{handle: fr}
	sys.client = client
	sys.HostToPods = make(map[string][]*v1.Pod)
	sys.HostSyscalls = make(map[string]sets.Set[string])
	sys.ExSAvg = 0
	sys.ExSAvgCount = 1
	sys.DefaultProfileName = "full-seccomp"
	sys.DefaultProfileNamespace = "default"

	return &sys, err
}

func TestReadSPOProfileCR(t *testing.T) {
	tests := []struct {
		name        string
		ns          string
		profilename string
		expected    []string
	}{
		{
			name:        "Empty CR path",
			ns:          "",
			profilename: "",
			expected:    []string{},
		},
		{
			name:        "Valid CR path",
			ns:          "default",
			profilename: "z-seccomp",
			expected:    spoResponse.Spec.Syscalls[0].Names,
		},
	}

	sys, _ := mockSysched()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syscalls, err := sys.readSPOProfileCR(tt.profilename, tt.ns)
			assert.Nil(t, err)
			assert.NotNil(t, syscalls)
			assert.EqualValues(t, len(tt.expected), len(syscalls))
		})
	}
}

func MakePodWithSecurityContext() *v1.Pod {
	pod := st.MakePod().Obj()
	s := "localhost/operator/default/z-seccomp.json"
	pod.Spec = v1.PodSpec{
		SecurityContext: &v1.PodSecurityContext{
			SeccompProfile: &v1.SeccompProfile{
				Type:             v1.SeccompProfileTypeLocalhost,
				LocalhostProfile: &s,
			},
		},
	}
	return pod
}

func MakePodContainerWithSecurityContext() *v1.Pod {
	s := "localhost/operator/default/z-seccomp.json"
	containers := []v1.Container{
		{
			SecurityContext: &v1.SecurityContext{
				SeccompProfile: &v1.SeccompProfile{
					Type:             v1.SeccompProfileTypeLocalhost,
					LocalhostProfile: &s,
				},
			},
		},
	}
	pod := st.MakePod().Containers(containers).Obj()
	return pod
}

func MakePodWithHostIP(IP string) *st.PodWrapper {
	p := st.MakePod()
	pod := p.Obj()
	pod.Status.HostIP = IP
	return p
}

func TestGetSyscalls(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected int
	}{
		{
			name: "Pod with annotation",
			pod: st.MakePod().
				Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Obj(),
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
		{
			name:     "Pod with SecurityContext",
			pod:      MakePodWithSecurityContext(),
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
		{
			name:     "Pod with Container SecurityContext",
			pod:      MakePodContainerWithSecurityContext(),
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
		{
			name:     "Pod with empty SecurityContext",
			pod:      st.MakePod().Obj(),
			expected: len(spoResponseFull.Spec.Syscalls[0].Names),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syscalls := sys.getSyscalls(tt.pod)
			assert.NotNil(t, syscalls)
			assert.EqualValues(t, tt.expected, syscalls.Len())
		})
	}
}

func TestCalcScore(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name     string
		syscalls sets.Set[string]
		expected int
	}{
		{
			name:     "Calculate exs score",
			syscalls: sets.New[string](spoResponse.Spec.Syscalls[0].Names...),
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := sys.calcScore(tt.syscalls)
			assert.EqualValues(t, tt.expected, score)
		})
	}
}

func TestScore(t *testing.T) {
	// create nodes and pods
	node := st.MakeNode()
	node.Name("test")

	// fake out an existing pod
	pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
		"localhost/operator/default/z-seccomp.json").Name("Existing pod").Node("test").Obj()

	// fake out the framework handle
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fr, err := tf.NewFramework(ctx, registeredPlugins, Name,
		frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset(node.Obj())))
	if err != nil {
		t.Error(err)
	}

	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Error(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Error(err)
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Error(err)
	}
	if err := clientscheme.AddToScheme(scheme); err != nil {
		t.Error(err)
	}

	objs := []runtime.Object{&spoResponse, &spoResponse1, &spoResponseFull}
	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

	sys := SySched{handle: fr}
	sys.client = client
	sys.HostToPods = make(map[string][]*v1.Pod)
	sys.HostSyscalls = make(map[string]sets.Set[string])
	sys.ExSAvg = 0
	sys.ExSAvgCount = 0

	sys.addPod(pod)

	tests := []struct {
		name     string
		pod      *v1.Pod
		expected int
	}{
		{
			name: "Test score difference",
			pod: st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
				"localhost/operator/default/x-seccomp.json").Name("pod").Obj(),
			expected: 2,
		},
		{
			name: "Test score same",
			pod: st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
				"localhost/operator/default/z-seccomp.json").Name("pod").Obj(),
			expected: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, _ := sys.Score(context.Background(), nil, tt.pod, "test")
			assert.EqualValues(t, tt.expected, score)
		})
	}
}

func TestNormalizeScore(t *testing.T) {
	tests := []struct {
		name       string
		nodescores framework.NodeScoreList
		expected   []int
	}{
		{
			name: "Normalize score",
			nodescores: framework.NodeScoreList{framework.NodeScore{Name: "test", Score: 100},
				framework.NodeScore{Name: "test1", Score: 200}},
			expected: []int{50, 0},
		},
		{
			name: "Normalize score 2",
			nodescores: framework.NodeScoreList{framework.NodeScore{Name: "test", Score: 0},
				framework.NodeScore{Name: "test1", Score: 200}},
			expected: []int{100, 0},
		},
	}

	sys, _ := mockSysched()
	pod := st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
		"localhost/operator/default/httpd-seccomp.json").Name("pod1").Obj()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ret := sys.NormalizeScore(context.TODO(), nil, pod, tt.nodescores)
			assert.Nil(t, ret)
			for i := range tt.nodescores {
				assert.EqualValues(t, tt.expected[i], tt.nodescores[i].Score)
			}
		})
	}
}

func TestGetHostSyscalls(t *testing.T) {
	tests := []struct {
		name     string
		pods     []*v1.Pod
		nodeName string
		expected int
	}{
		{
			name: "single",
			pods: []*v1.Pod{st.MakePod().Name("pod1").
				Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj()},
			nodeName: "test",
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
		{
			name: "many",
			pods: []*v1.Pod{
				st.MakePod().Name("pod1").
					Annotation("seccomp.security.alpha.kubernetes.io",
						"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
				st.MakePod().Name("pod2").
					Annotation("seccomp.security.alpha.kubernetes.io",
						"localhost/operator/default/x-seccomp.json").Node("test").Obj(),
				st.MakePod().Name("pod3").
					Annotation("seccomp.security.alpha.kubernetes.io",
						"localhost/operator/default/full-seccomp.json").Node("test1").Obj(),
			},
			nodeName: "test",
			expected: sets.New[string](spoResponse.Spec.Syscalls[0].Names...).Union(sets.New[string](spoResponse1.Spec.Syscalls[0].Names...)).Len(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys, _ := mockSysched()
			sys.ExSAvgCount = 0

			for i := range tt.pods {
				sys.addPod(tt.pods[i])
			}

			cnt, _ := sys.getHostSyscalls(tt.nodeName)
			assert.EqualValues(t, tt.expected, cnt)
		})
	}
}

func TestUpdateHostSyscalls(t *testing.T) {
	utilruntime.Must(v1beta1.AddToScheme(clientscheme.Scheme))
	tests := []struct {
		name     string
		nodes    []*v1.Node
		newPods  []*v1.Pod
		basePods []*v1.Pod
		expected int
	}{
		{
			name: "On same host",
			nodes: []*v1.Node{
				st.MakeNode().Name("test").Obj(),
				st.MakeNode().Name("test1").Obj(),
			},
			basePods: []*v1.Pod{
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
			},
			newPods: []*v1.Pod{
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Node("test").Obj(),
			},
			expected: sets.New[string](spoResponse.Spec.Syscalls[0].Names...).Union(sets.New[string](spoResponse1.Spec.Syscalls[0].Names...)).Len(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// fake out the framework handle
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			nodeItems := []v1.Node{}
			for _, node := range tt.nodes {
				nodeItems = append(nodeItems, *node)
			}
			fr, err := tf.NewFramework(ctx, registeredPlugins, Name,
				frameworkruntime.WithClientSet(clientsetfake.NewSimpleClientset(&v1.NodeList{Items: nodeItems})))
			if err != nil {
				t.Error(err)
			}

			scheme := runtime.NewScheme()
			if err := v1.AddToScheme(scheme); err != nil {
				t.Error(err)
			}
			if err := v1alpha1.AddToScheme(scheme); err != nil {
				t.Error(err)
			}
			if err := v1beta1.AddToScheme(scheme); err != nil {
				t.Error(err)
			}
			if err := clientscheme.AddToScheme(scheme); err != nil {
				t.Error(err)
			}

			objs := []runtime.Object{&spoResponse, &spoResponse1, &spoResponseFull}
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

			sys := SySched{handle: fr}
			sys.client = client
			sys.HostToPods = make(map[string][]*v1.Pod)
			sys.HostSyscalls = make(map[string]sets.Set[string])
			sys.ExSAvg = 0
			sys.ExSAvgCount = 0

			for i := range tt.basePods {
				sys.addPod(tt.basePods[i])
			}

			for i := range tt.newPods {
				sys.updateHostSyscalls(tt.newPods[i])
			}
			sc, _ := sys.getHostSyscalls("test")
			assert.EqualValues(t, tt.expected, sc)
		})
	}
}

func TestAddPod(t *testing.T) {
	sys, _ := mockSysched()

	tests := []struct {
		name     string
		pods     []*v1.Pod
		expected int
	}{
		{
			name: "Add 1 pod",
			pods: []*v1.Pod{
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
			},
			expected: len(spoResponse.Spec.Syscalls[0].Names),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.pods {
				sys.addPod(tt.pods[i])
			}
			for i := range sys.HostToPods["test"] {
				assert.EqualValues(t, tt.pods[i], sys.HostToPods["test"][i])
			}
			assert.EqualValues(t, tt.expected, len(sys.HostSyscalls["test"]))
		})
	}
}

func TestRecomputeHostSyscalls(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name     string
		pods     []*v1.Pod
		expected int
	}{
		{
			name: "Recompute 2 pods",
			pods: []*v1.Pod{
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Node("test").Obj(),
			},
			expected: sets.New[string](spoResponse.Spec.Syscalls[0].Names...).Union(sets.New[string](spoResponse1.Spec.Syscalls[0].Names...)).Len(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syscalls := sys.recomputeHostSyscalls(tt.pods)
			assert.EqualValues(t, tt.expected, len(syscalls))
		})
	}
}

func TestRemovePod(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name           string
		pods           []*v1.Pod
		removePods     []*v1.Pod
		expectedPodNum int
		expected       int
	}{
		{
			name: "Remove 1 pod",
			pods: []*v1.Pod{
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Name("pod1").Node("test").Obj(),
				st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Name("pod2").Node("test").Obj(),
			},
			removePods: []*v1.Pod{st.MakePod().Annotation("seccomp.security.alpha.kubernetes.io",
				"localhost/operator/default/z-seccomp.json").Name("pod1").Node("test").Obj()},
			expectedPodNum: 1,
			expected:       len(spoResponse1.Spec.Syscalls[0].Names),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.pods {
				sys.addPod(tt.pods[i])
			}

			for i := range tt.removePods {
				sys.removePod(tt.removePods[i])
			}

			assert.EqualValues(t, tt.expectedPodNum, len(sys.HostToPods["test"]))
			assert.EqualValues(t, tt.expected, len(sys.HostSyscalls["test"]))
		})
	}
}

func TestPodAdded(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name           string
		basePods       []*v1.Pod
		updatedPods    []*v1.Pod
		expectedPodNum int
		expected       int
	}{
		{
			name: "Update 1 pod",
			basePods: []*v1.Pod{
				st.MakePod().Name("pod1").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Phase(v1.PodRunning).Node("test").Obj(),
				st.MakePod().Name("pod2").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Phase(v1.PodRunning).Node("test").Obj(),
			},
			expectedPodNum: 2,
			expected:       sets.New[string](spoResponse.Spec.Syscalls[0].Names...).Union(sets.New[string](spoResponse1.Spec.Syscalls[0].Names...)).Len(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.basePods {
				sys.podAdded(tt.basePods[i])
			}

			assert.EqualValues(t, tt.expectedPodNum, len(sys.HostToPods["test"]))
			assert.EqualValues(t, tt.expected, len(sys.HostSyscalls["test"]))
		})
	}
}

func TestPodUpdated(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name           string
		basePods       []*v1.Pod
		updatedPods    []*v1.Pod
		expectedPodNum int
		expected       int
	}{
		{
			name: "Update 1 pod",
			basePods: []*v1.Pod{
				st.MakePod().Name("pod1").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
			},
			updatedPods: []*v1.Pod{
				MakePodWithHostIP("192.168.0.2").Name("pod2").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Phase(v1.PodPending).Node("test").Obj(),
			},
			expectedPodNum: 2,
			expected:       sets.New[string](spoResponse.Spec.Syscalls[0].Names...).Union(sets.New[string](spoResponse1.Spec.Syscalls[0].Names...)).Len(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.basePods {
				sys.addPod(tt.basePods[i])
			}

			for i := range tt.updatedPods {
				sys.podUpdated(tt.updatedPods[i], nil)
			}

			assert.EqualValues(t, tt.expectedPodNum, len(sys.HostToPods["test"]))
			assert.EqualValues(t, tt.expected, len(sys.HostSyscalls["test"]))
		})
	}
}

func TestPodDeleted(t *testing.T) {
	sys, _ := mockSysched()
	tests := []struct {
		name           string
		basePods       []*v1.Pod
		deletedPods    []*v1.Pod
		expectedPodNum int
		expected       int
	}{
		{
			name: "Delete 1 pod",
			basePods: []*v1.Pod{
				st.MakePod().Name("pod1").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/z-seccomp.json").Node("test").Obj(),
				st.MakePod().Name("pod2").Annotation("seccomp.security.alpha.kubernetes.io",
					"localhost/operator/default/x-seccomp.json").Phase(v1.PodPending).Node("test").Obj(),
			},
			deletedPods: []*v1.Pod{st.MakePod().Name("pod2").Annotation("seccomp.security.alpha.kubernetes.io",
				"localhost/operator/default/x-seccomp.json").Phase(v1.PodPending).Node("test").Obj()},
			expectedPodNum: 1,
			expected:       len(spoResponse.Spec.Syscalls[0].Names),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.basePods {
				sys.addPod(tt.basePods[i])
			}

			for i := range tt.deletedPods {
				sys.podDeleted(tt.deletedPods[i])
			}

			assert.EqualValues(t, tt.expectedPodNum, len(sys.HostToPods["test"]))
			assert.EqualValues(t, tt.expected, len(sys.HostSyscalls["test"]))
		})
	}
}

func TestGetArgs(t *testing.T) {
	args := pluginconfig.SySchedArgs{
		DefaultProfileNamespace: "default",
		DefaultProfileName:      "x-seccomp.json",
	}
	retargs, err := getArgs(&args)
	assert.Nil(t, err)
	assert.EqualValues(t, &args, retargs)
}

func TestNew(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fakeclient := clientsetfake.NewSimpleClientset()
	fr, err := tf.NewFramework(ctx, registeredPlugins, Name,
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(fakeclient, 0)),
		frameworkruntime.WithKubeConfig(&restclient.Config{}),
		frameworkruntime.WithClientSet(fakeclient))
	if err != nil {
		t.Error(err)
	}

	args := pluginconfig.SySchedArgs{
		DefaultProfileNamespace: "default",
		DefaultProfileName:      "x-seccomp.json",
	}

	sys, err := New(ctx, &args, fr)
	assert.Nil(t, err)
	assert.NotNil(t, sys)
}
