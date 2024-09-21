# Overview

This folder holds the disk IO aware scheduling plugin implementations based on [Disk IO Aware Scheduling](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/kep/624-disk-io-aware-scheduling/README.md). 

The disk IO aware scheduler plugin utilizes a [IO Driver](https://github.com/intel/cloud-resource-scheduling-and-isolation) to profile disk IO allocable capacity, and update each disk’s normalized available IO capacity to API Server for making scheduling decisions. A model server is also required to host normalization functions for various device models. Disk vendors can write their a file server to host their funcs. The model server can sit togeter with the IO driver.

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [x] 👶 Alpha (used in companies for pilot projects)
- [ ] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial

- Customize your normalization functions with your own coefficients which should implement the interface defined in [normalizer/ directory] (normalizer/interface.go). There is a sample plugin in [sampleplugin/foo/foo.go](sampleplugin/foo/foo.go). For the time being, the plugin uses golang's plugin mechanism to load normalization functions. However, it’s important to note that Golang plugins are experimental and come with certain [limitations](https://pkg.go.dev/plugin). There are potential improvements to the plugin loader through alternative solutions such as [cel-go](https://github.com/google/cel-go), [Hashicorp’s go-plugin](https://github.com/hashicorp/go-plugin) and [Wasm](https://webassembly.org/). When compiling the plugin, ensure that CGO is enabled and that the compile flags and platform are consistent with the caller.
``` shell
CGO_ENABLED=1 go build -buildmode=plugin -o sampleplugin/foo/foo.so sampleplugin/foo/foo.go
``` 
- Setup a model server. For demo purpose, we use `python3` to host normalization functions. 
``` shell
cd pkg/diskioaware/sampleplugin
python3 -m http.server 8080
``` 
- Load a disk model ConfigMap which consists of disk vendor, model and download url
``` shell
kubectl apply -f manifests/diskioaware/diskmodel-configmap.yaml
```
Or update the disk model in `manifests/diskioaware/diskVendors` and load ConfigMap from file
``` shell
kubectl create cm normalization-func --from-file manifests/diskioaware/diskVendors -n kube-system
```
- Apply RBAC rules and CRDs for disk IO aware scheduler plugin
``` shell
kubectl apply -f manifests/diskioaware/cluster-role.yaml
kubectl apply -f manifests/crds/diskio.x-k8s.io_nodediskdevices.yaml
kubectl apply -f manifests/crds/diskio.x-k8s.io_nodediskiostats.yaml
```
- Deploy the scheduler plugin as a secondary scheduler 
``` shell
kubectl create cm -n kube-system diskio-aware-scheduler-config --from-file manifests/diskioaware/scheduler-config.yaml 
kubectl apply -f manifests/diskioaware/deploy.yaml
```
Following is an example scheduler configuration. Disk IO aware plugin supports two score strategy: `LeastAllocated` and `MostAllocated`.     
* MostAllocated - favors node with the least amount of available disk IO resource
* LeastAllocated - favors node with the most amount of available disk IO resource
`NSWhiteList` stands for a namespace whitelist. The disk IO resources utilized by pods in specified whitelist are exempt from being calculated by disk IO aware plugin.
``` yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
profiles:
- schedulerName: diskio-aware-scheduler
  plugins:
    multiPoint:
      enabled:
      - name: DiskIO
  pluginConfig:
  - name: DiskIO
    args:
      # other strategies are MostAllocated
      scoreStrategy: "LeastAllocated"
      # add your cni namespace accordingly. eg. kube-flannel
      nsWhiteList: 
      - kube-system
      - ioi-system
      - kube-flannel
```
- Start the IO Driver. An reference IO Driver implementation is provided for testing purpose. The IO driver reports node device info to API server and update each device's IO status at runtime. Update the proxy and image registry accordingly in the commands below.
``` shell
git clone https://github.com/intel/cloud-resource-scheduling-and-isolation.git
cd cloud-resource-scheduling-and-isolation
HTTPS_PROXY=<Your-Corperate-Proxy> REPO_HOST=<IMAGE REGISTRY> make image 
REPO_HOST=<IMAGE REGISTRY> make push_image
kubectl create ns ioi-system
kubectl apply -f manifests/cluster_role.yaml
kubectl apply -f manifests/daemonset.yaml
```
- Scheduler pods with disk io request. The total/read/write disk IO capacity of the fake device reported by IO driver is 1000Mi/1000Mi/2000Mi and the coefficient is 1.0. When the pod below is scheduled, schedule a duplicate pod. The new pod will be in pending state due to the error "node xxx disk IO read bandwidth not enough. preemption: 0/1 nodes are available".  
``` yaml
apiVersion: v1
kind: Pod
metadata:
  name: pod1
  annotations:
    blockio.kubernetes.io/resources: "{\"rbps\": \"900Mi\", \"wbps\": \"900Mi\", \"blocksize\": \"4k\"}"
spec:
  schedulerName: diskio-aware-scheduler
  containers:
  - image: busybox
    name: busybox
```





