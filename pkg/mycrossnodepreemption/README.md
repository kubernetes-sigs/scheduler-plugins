# MyCrossNodePreemption Plugin

## Overview

An improved cross-node preemption plugin that addresses the limitations of the default scheduler's preemption. This plugin implements efficient algorithms for cross-node preemption with optimization strategies.

### Scheduling flow

TODO_HC

## Build and Run

### Prerequisites

The following tools are required (if Windows host, use WSL2 w/ e.g. Ubuntu):

- git (tested with 2.43.0)
- make (tested with 4.3)
- python3 (tested with 3.10.12)
- pip (tested with 24.0)
- kubectl (tested with client v.1.32.7)
- kwok+kwokctl (tested with v0.7.0)
- Go (tested with 1.24.3)

Currently, it is only tested on amd64 architecture.

### Build the scheduler with the plugin

The scheduler with the plugin can be built either as a binary or as a docker image.

Common steps to set up the python solver environment:

```bash
sudo install -d -m 0755 /opt/venv/
sudo install -d -m 0755 /opt/solver/
sudo cp -a bootstrap/content/scripts/python_solver/main.py /opt/solver/main.py
sudo python3 -m venv /opt/venv/
sudo /opt/venv/bin/python -m pip install --upgrade pip
sudo /opt/venv/bin/pip install --no-cache-dir -r bootstrap/content/scripts/python_solver/requirements.txt
```

#### Building the binary

```bash
make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
```

#### Building the docker image (recommended for faster builds)

To build the docker image, docker (tested with v28.3.2) and docker-buildx-plugin (tested with v0.25.0) must be installed.

```bash
docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .
```

### Run the scheduler with the plugin on a KWOK cluster

The easist way is to use the provided test generator script which can be found in `bootstrap/content/scripts/kwok/kwok_test_generator.py`.
It will create a KWOK cluster, fill it with random pods, and run the scheduler with the plugin. It has a number of parameters, see the help:

```bash
python3 bootstrap/content/scripts/kwok/kwok_test_generator.py --help
```

If you just want to test it manually on a KWOK cluster, first create a scheduler config (see `manifests/mycrossnodepreemption/scheduler-config.yaml`) and a cluster config file (see `bootstrap/content/data/configs/a/01.yaml`). Then create the cluster with kwokctl:

```bash
kwokctl create cluster --name <cluster_name> --runtime <docker/binary> --config <path/to/cluster-config.yaml>
```

To delete the cluster, run:

```bash
kwokctl delete cluster --name <cluster_name>
```

### Run the scheduler with the plugin on a Kind cluster

To run the scheduler with the plugin on a Kind cluster, install docker (tested with v28.3.2) + docker-buildx-plugin (tested with v0.25.0) + Kind (tested with v0.20.0), and run the provided script `kind/kind-create-cluster.sh` to create a Kind cluster with some specified number of nodes:

```bash
./kind-create-cluster.sh <cluster_name> <num_nodes>
```

Then load the scheduler image into the Kind cluster (it will also build the image). The script uses a fixed number of environment variables, see `kind/kind-load-plugins.sh` for details. You can modify the script to change them.

```bash
./kind-load-plugins.sh <cluster_name>
```

## Bootstrap a VM

To bootstrap a VM with all prerequisites installed and running tests with KWOK, use the provided init script `bootstrap/bootstrap.sh`.

To develop and test the init script it can be beneficial to run it in a VM. To make it easy, a Vagrantfile is provided in the root of the repo. It will create an Ubuntu 22.04 VM with all prerequisites installed and the repo cloned. To use it, install Vagrant and VirtualBox, then run:

```bash
vagrant up
```

This will create a VM named `scheduler-plugins` that you can SSH into using:

```bash
vagrant ssh
```

To delete the VM, run:

```bash
vagrant destroy -f
```

## Useful commands

- Get all pods

  ```bash
  kubectl get pods -A
  ```

- Delete namespace

  ```bash
  kubectl delete ns <namespace>
  ```

- Get kube-scheduler logs from KWOK cluster

  ```bash
  kwokctl logs kube-scheduler --name <cluster_name>
  ```

- Getting saved solver plans from kube-scheduler

  ```bash
  kubectl -n kube-system get cm -l crossnode-plan
  kubectl -n kube-system get cm <CM> -o jsonpath='{.data.plan\.json}' | jq .
  ```

- Get reasons for pod(s) not scheduling

  ```bash
  kubectl --context <ctx> -n <namespace> get events --field-selector involvedObject.kind=Pod -o json | jq '.items[] | {name: .involvedObject.name, reason: .reason, message: .message}'
  ```

## Install Metrics API in kind cluster

```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl patch -n kube-system deployment metrics-server --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

## Test

```bash
# Create cluster
./kind-create-cluster.sh mycluster 3

# Load plugins
./kind-load-plugins.sh mycluster "MyCrossNodePreemption"
```


## TODOs

- Make the test plan.
- Write a proper README.md

## TODOs Test

- Test at which utilization the default scheduler stops to work properly.
- Large scale test on UCloud where i could set up multiple ubuntu servers each making on test.
- Test if python solver timing depends heavily on the node it is executed on (CPU type, etc.)
- Test the plugin works across workload type.
- Test CP-SAT vs. other solvers.

## Later TODOs

- Make use of design patterns where possible.
- Create unit and integration tests.
- Find a better way to set verbose level.
- Add more comments to the code.
- Somehow ensure that the cluster state is the same throughout execution. If not, consider to evict those non-planned pods during execution. We can use the snapshot to see how many there is of each RS-workloads and standalone pods and compare with the actual state. We should never have more than planned, but we can have less if something got deleted externally or if we move a pod or evict it.
- We will get a plan timeout if a pod is removed during plan execution (if a standalone pod is deleted or a workload is scaled down).
- Fix TODOs

## Write

- Write about that we always recreate the cluster also when just running a new seed, as we do not know if any state in the cluster is preventing us from scheduling.
- Write about that we did not use globally installed python packages due to PEP 668 in UCLOUD.
- Write about dockerfile can't run distroless with nonroot
- Write about the difference ways of running KWOK - binary, docker, etc.
- Mention that I focus on plain CPU clusters as there can be other consideration when it comes to GPUs
- Write about that we need hashlib for making sure we get the same seeds even when we make changes to the code.
- Write about that when simulating standalone pods, then the scheduler might preempt it and we can no longer see it.
- Write about Iterative deepening depth-first search
- Write about that it were not possible to place a preemptor by workload cnts, needs to be done by name as we otherwise can let other pods of the same workload through the scheduling phase before the preemptor that hit the postfilter.
- Write about that recreation always create a new uid therefore we place by name
- Write about that the OptimizeForEvery@PreEnqueue cannot be deterministic as we do not determine which order the pods are taken.
- Write something about watchdogTTL
- Write something about the snapshotlister that it lags one scheduling cycle.
- Write about cache calls instead of client calls. Faster and better. [See documentation](https://pkg.go.dev/k8s.io/client-go/tools/cache)
- Write about deletion-cost and that it is hard to evict the right workload-owned pods, therefore I found the new eviction API.
- Write about QueuingHints and that I end up using Pod Activator for reschedule queued pods.
- Write about atomics and we only use configmap for debugging.
- Write about Reserve/Unreserve and we use it for making sure pods gets scheduled to the node otherwise we can try again. We need this to ensure race conditions not happens. We cannot rely on snapshot alone.
- Write about that Optimizer is not deterministic, when having multiple workers. However, we need multiple workers, otherwise it is too slow.

## Open Questions

## Closed Questions

- What to do with evicted and blocked pods - put them to queue or try again immediately?
  - Jacopo: Fine, what i am doing now, by just letting them try again immediately
- What to do with node-selectors, PDBs, and other rules.
  - Jacopo: Fine, to ignore these just write about it. May, the extra constaints actually will make the solver faster (smaller search space).

## Analogy

Lad os forestille os, at vi har fem legokasser fyldt med klodser i forskellige størrelser. Vi har netop købt nogle nye klodser, som vi gerne vil lægge ned i kasserne. Problemet er, at ingen af kasserne umiddelbart har plads nok, fordi de allerede næsten er fyldt. For at få plads kan vi derfor vælge at flytte nogle af de eksisterende klodser over i andre kasser, hvor der stadig er lidt ledig plads.

Men her opstår en kaskadeeffekt: når vi flytter én klods fra kasse A til kasse B, kan vi være nødt til at flytte en anden klods fra kasse B videre til kasse C – og så fremdeles – før vi til sidst får frigjort nok plads til de nye klodser. Denne kædereaktion kan blive lang og kompliceret. Derfor er målet ikke kun at få plads til de nye klodser, men også at gøre det med så få flytninger som muligt.

## Developed

TODO_HC

