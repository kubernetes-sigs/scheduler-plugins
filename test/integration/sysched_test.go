/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kubernetes/pkg/scheduler"
	schedapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	imageutils "k8s.io/kubernetes/test/utils/image"
	"sigs.k8s.io/controller-runtime/pkg/client"
	schedconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/sysched"
	"sigs.k8s.io/scheduler-plugins/test/util"
	spo "sigs.k8s.io/security-profiles-operator/api/seccompprofile/v1beta1"
)

var (
	fullseccompSPOCR = spo.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "full-seccomp",
			Namespace: "default",
		},
		Spec: spo.SeccompProfileSpec{
			Architectures: []spo.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*spo.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit", "fsync", "chroot", "recvfrom", "newfstatat", "setresgid", "poll", "lstat", "listen", "getpgid", "sigreturn", "setreuid", "setgid", "signaldeliver", "recvmsg", "bind", "close", "setsockopt", "openat", "container", "getpeername", "lseek", "procexit", "uname", "statfs", "utime", "pipe", "getcwd", "chdir", "execve", "rt_sigaction", "set_tid_address", "dup", "ioctl", "munmap", "rename", "kill", "getpid", "alarm", "umask", "setresuid", "exit_group", "fstat", "geteuid", "mprotect", "read", "getppid", "fchown", "capset", "rt_sigprocmask", "accept", "setgroups", "open", "set_robust_list", "fchownat", "unlink", "getdents", "fcntl", "readlink", "getgid", "fchmod"},
			},
			},
		},
	}

	badSPOCR = spo.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad",
			Namespace: "default",
		},
		Spec: spo.SeccompProfileSpec{
			Architectures: []spo.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*spo.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit", "fsync", "chroot", "recvfrom", "newfstatat", "setresgid", "poll", "lstat", "listen", "getpgid", "sigreturn", "setreuid", "setgid", "signaldeliver", "recvmsg", "bind", "close", "setsockopt", "openat", "container", "getpeername", "lseek", "procexit", "uname", "statfs", "utime", "pipe", "getcwd", "chdir", "execve", "rt_sigaction", "set_tid_address", "dup", "ioctl", "munmap", "rename", "kill", "getpid", "alarm"},
			},
			},
		},
	}

	good1SPOCR = spo.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "good1",
			Namespace: "default",
		},
		Spec: spo.SeccompProfileSpec{
			Architectures: []spo.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*spo.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk"},
			},
			},
		},
	}

	good2SPOCR = spo.SeccompProfile{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "security-profiles-operator.x-k8s.io/v1beta1",
			Kind:       "SeccompProfile",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "good2",
			Namespace: "default",
		},
		Spec: spo.SeccompProfileSpec{
			Architectures: []spo.Arch{"SCMP_ARCH_X86_64"},
			DefaultAction: "SCMP_ACT_LOG",
			Syscalls: []*spo.Syscall{{
				Action: "SCMP_ACT_ALLOW",
				Names:  []string{"clone", "socket", "getuid", "setrlimit", "nanosleep", "sendto", "setuid", "getpgrp", "mkdir", "getegid", "getsockname", "clock_gettime", "prctl", "epoll_pwait", "futex", "link", "ftruncate", "access", "gettimeofday", "select", "getsockopt", "mmap", "write", "connect", "capget", "chmod", "arch_prctl", "wait4", "brk", "stat", "getrlimit"},
			},
			},
		},
	}
)

func TestSyschedPlugin(t *testing.T) {
	testCtx := &testContext{}
	testCtx.Ctx, testCtx.CancelFn = context.WithCancel(context.Background())

	cs := kubernetes.NewForConfigOrDie(globalKubeConfig)

	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	_ = spo.AddToScheme(scheme)

	extClient, err := client.New(globalKubeConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	testCtx.ClientSet = cs
	testCtx.KubeConfig = globalKubeConfig

	if err := wait.PollUntilContextTimeout(testCtx.Ctx, 100*time.Millisecond, 3*time.Second, false, func(ctx context.Context) (done bool, err error) {
		groupList, _, err := cs.ServerGroupsAndResources()
		if err != nil {
			return false, nil
		}
		for _, group := range groupList {
			if group.Name == "security-profiles-operator.x-k8s.io" {
				t.Log("The CRD is ready to serve")
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for CRD to be ready: %v", err)
	}

	// Create the Seccomp Profile CRs
	for _, spocr := range []*spo.SeccompProfile{&fullseccompSPOCR, &badSPOCR, &good1SPOCR, &good2SPOCR} {
		err := extClient.Create(testCtx.Ctx, spocr)
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := util.NewDefaultSchedulerComponentConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Work around https://github.com/kubernetes/kubernetes/issues/121630.
	cfg.Profiles[0].Plugins.PreScore = schedapi.PluginSet{
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].Plugins.Score = schedapi.PluginSet{
		Enabled:  []schedapi.Plugin{{Name: sysched.Name}},
		Disabled: []schedapi.Plugin{{Name: "*"}},
	}
	cfg.Profiles[0].PluginConfig = append(cfg.Profiles[0].PluginConfig, schedapi.PluginConfig{
		Name: sysched.Name,
		Args: &schedconfig.SySchedArgs{
			DefaultProfileNamespace: "default",
			DefaultProfileName:      "full-seccomp",
		},
	})

	ns := fmt.Sprintf("integration-test-%v", string(uuid.NewUUID()))
	createNamespace(t, testCtx, ns)

	testCtx = initTestSchedulerWithOptions(
		t,
		testCtx,
		scheduler.WithProfiles(cfg.Profiles...),
		scheduler.WithFrameworkOutOfTreeRegistry(fwkruntime.Registry{sysched.Name: sysched.New}),
	)
	syncInformerFactory(testCtx)
	go testCtx.Scheduler.Run(testCtx.Ctx)
	defer cleanupTest(t, testCtx)

	// Create a Node.
	for i := 0; i < 2; i++ {
		nodeName := fmt.Sprintf("fake-node-%d", i)
		node := st.MakeNode().Name(nodeName).Label("node", nodeName).Obj()
		//node.Spec.PodCIDR = "192.168.0.1/24"
		node.Status.Addresses = make([]v1.NodeAddress, 1)
		ip := fmt.Sprintf("192.168.1.%v", 1+i)
		node.Status.Addresses[0] = v1.NodeAddress{
			Type:    v1.NodeInternalIP,
			Address: ip,
		}
		node, err = cs.CoreV1().Nodes().Create(testCtx.Ctx, node, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Node %q: %v", nodeName, err)
		}
	}

	// Construct 3 Pods.
	var pods []*v1.Pod
	podNames := []string{"bad", "good1", "good2"}
	pause := imageutils.GetPauseImageName()
	for i := 0; i < len(podNames); i++ {
		pod := st.MakePod().Namespace(ns).Name(podNames[i]).Container(pause).Obj()
		pods = append(pods, pod)
	}
	// Make pods[0] bad.
	badseccomp := "security-profiles-operator.x-k8s.io/default/bad.json"
	pods[0].Spec.SecurityContext = &v1.PodSecurityContext{}
	pods[0].Spec.SecurityContext.SeccompProfile = &v1.SeccompProfile{
		Type:             "Localhost",
		LocalhostProfile: &badseccomp,
	}
	// Make pods[1] good1.
	good1seccomp := "security-profiles-operator.x-k8s.io/default/good1.json"
	pods[1].Spec.SecurityContext = &v1.PodSecurityContext{}
	pods[1].Spec.SecurityContext.SeccompProfile = &v1.SeccompProfile{
		Type:             "Localhost",
		LocalhostProfile: &good1seccomp,
	}
	// Make pods[2] good2.
	pods[2].ObjectMeta.Annotations = map[string]string{
		"seccomp.security.alpha.kubernetes.io": "security-profiles-operator.x-k8s.io/default/good2.json",
	}

	// Create 3 Pods
	t.Logf("Start to create 3 Pods.")
	for i := range pods {
		t.Logf("Creating Pod %q", pods[i].Name)
		_, err = cs.CoreV1().Pods(ns).Create(testCtx.Ctx, pods[i], metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create Pod %q: %v", pods[i].Name, err)
		}
		// Wait for all Pods to be scheduled.
		err = wait.PollUntilContextTimeout(testCtx.Ctx, time.Millisecond*20, wait.ForeverTestTimeout, false, func(ctx context.Context) (bool, error) {
			if !podScheduled(cs, pods[i].Namespace, pods[i].Name) {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Update the pod's HostIP address field
		p, err := cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pods[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get Pod %q: %v", pods[i].Name, err)
		}
		n, err := cs.CoreV1().Nodes().Get(testCtx.Ctx, p.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get Node %q: %v", n.Name, err)
		}
		p.Status.HostIP = n.Status.Addresses[0].Address
		_, err = cs.CoreV1().Pods(ns).UpdateStatus(testCtx.Ctx, p, metav1.UpdateOptions{})

		// Do a fake pod status update here to force propagation of Pod state to listener
		p, err = cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pods[i].Name, metav1.GetOptions{})
		p.Status.HostIP = "1"
		_, err = cs.CoreV1().Pods(ns).UpdateStatus(testCtx.Ctx, p, metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Failed to update Pod %q: %v", pods[i].Name, err)
		}
	}
	defer cleanupPods(t, testCtx, pods)

	// Checking correct pod to node assignment
	// We will expect them to be placed:
	// "bad" pod is on one node and "good1" and "good2" pods on the other node.
	for i := range pods {
		pods[i], err = cs.CoreV1().Pods(ns).Get(testCtx.Ctx, pods[i].Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get Pod %q: %v", pods[i].Name, err)
		}
	}
	if pods[0].Spec.NodeName == pods[1].Spec.NodeName || pods[0].Spec.NodeName == pods[2].Spec.NodeName || pods[1].Spec.NodeName != pods[2].Spec.NodeName {
		t.Fatalf("Incorrect pod placement")
	}
}
