
# Cross-Node Preemption Plugin

- [Cross-Node Preemption Plugin](#cross-node-preemption-plugin)
  - [Overview](#overview)
  - [Building the scheduler+plugin](#building-the-schedulerplugin)
    - [Prerequisites for building the scheduler+plugin](#prerequisites-for-building-the-schedulerplugin)
    - [Build as a binary (recommended)](#build-as-a-binary-recommended)
      - [Create a Python environment for the Python solver](#create-a-python-environment-for-the-python-solver)
      - [Copy the Python solver to solver location](#copy-the-python-solver-to-solver-location)
    - [Build as a docker image](#build-as-a-docker-image)
  - [Running the scheduler+plugin](#running-the-schedulerplugin)
    - [Prerequisites for running the scheduler+plugin](#prerequisites-for-running-the-schedulerplugin)
    - [Run scheduler+plugin in a KWOK cluster (recommended)](#run-schedulerplugin-in-a-kwok-cluster-recommended)
      - [KWOK (Manual)](#kwok-manual)
      - [KWOK (Automated)](#kwok-automated)
    - [Run in a Kind cluster](#run-in-a-kind-cluster)
  - [Testing the scheduler+plugin](#testing-the-schedulerplugin)
    - [Test scripts](#test-scripts)
      - [(Initial) workload generator (`test_generator.py`)](#initial-workload-generator-test_generatorpy)
    - [Test jobs](#test-jobs)
    - [Running test jobs using HPC resources and init script (recommended)](#running-test-jobs-using-hpc-resources-and-init-script-recommended)
      - [Running test jobs using `test_generator.py` directly](#running-test-jobs-using-test_generatorpy-directly)
      - [Using Vagrant for development and test of init script](#using-vagrant-for-development-and-test-of-init-script)
    - [Generating test jobs](#generating-test-jobs)
    - [Estimate time to complete all jobs in UCloud](#estimate-time-to-complete-all-jobs-in-ucloud)
      - [Live-simulator](#live-simulator)
  - [Results Analysis: Plots, Tables, etc](#results-analysis-plots-tables-etc)
    - [Scripts](#scripts)
  - [Useful kubectl/kwokctl commands](#useful-kubectlkwokctl-commands)
  - [Live-workload-simulator](#live-workload-simulator)
  - [TODOs](#todos)
    - [Later TODOs](#later-todos)
    - [Test](#test)
      - [Later Tests](#later-tests)
    - [Questions](#questions)
      - [Open Questions](#open-questions)
      - [Closed Questions](#closed-questions)

## Overview

This project introduces an *optimized priority-based* approach for placing pods on to nodes by introducing a **Cross-Node Preemption Plugin** for the Kubernetes scheduler.
The goal is to schedule as many *high-priority* pods as possible, especially when the *default scheduler* fails to do so.
The plugin makes it possible to integrate an *external* solver in this case a Python solver using **Google's CP-SAT** solver to find the optimal plan for placing pods on nodes that maximizes the number of scheduled high-priority pods while *minimizing disruption* by minimizing the number of preemptions (*movements* and *evictions*).
Having the plan from the solver, the plugin then applies it by evicting and moving pods possibly across multiple nodes in the cluster. Note that the default Kubernetes scheduler is only able to preempt pods within a *single* node.

The plugin support different **optimization modes**:

- *For every pod* – optimize for every new pod that arrives.
- *All synch* – optimize all pods (both running and pending) at fixed intervals (or on demand via HTTP). It stop scheduling while waiting for the optimization to be completed and for the plan to be applied before proceeding.
- *All asynch* – same as all synch, but do not wait for the optimization to complete before proceeding. That means, it will only apply the plan if the cluster state is the same as when the plan was created. Scheduling is however still stopped while the plan is being applied to avoid conflicts.

The plugin can be is integrated in different **scheduling phases**. Either before enqueuing the pod (*pre-enqueue*) or after having tried to schedule it normally using the default scheduler (*post-filter*).

For a more **detailed description**, please refer to the paper ([Priority Matters: Optimising Kubernetes Clusters Usage with Constraint-Based Pod Packing](link-to-paper)) or the thesis report ([Optimizing Kubernetes Scheduler](link-to-thesis-report)).

The code of the plugin can be found in `pkg/mycrossnodepreemption/` and the manifests to deploy the scheduler with the plugin can be found in `bootstrap/content/manifests/`.

To make the plugin available to the Kubernetes scheduler, it is referenced in `cmd/scheduler/main.go`.

## Building the scheduler+plugin

The scheduler+plugin can be built either as a **binary (recommended)** or as a **docker image**.

### Prerequisites for building the scheduler+plugin

The following tools are required (if Windows host, use WSL2 w/ e.g. Ubuntu) to build and run the scheduler with the plugin:

- `git` (tested with 2.43.0)
- `make` (tested with 4.3)
- `python3` (tested with 3.10.12)
- `pip` (tested with 24.0)
- `Go` (tested with 1.24.3)
- When building as a docker image:
  - `docker` (tested with v28.3.2)
  - `docker-buildx-plugin` (tested with v0.25.0)

Currently, it is only tested on amd64 architecture and some code may need to be modified to run on other architectures.

### Build as a binary (recommended)

To build the binary, run the following command in the root of the repo:

```bash
make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
```

The built binary will be located in `bin/kube-scheduler`.

#### Create a Python environment for the Python solver

As the plugin uses a Python solver, the Python environment and the solver code also need to be set up.
This step is not needed if you are building it as a docker image (as the docker image will contain everything needed).

```bash
sudo install -d -m 0755 /opt/venv/
sudo python3 -m venv /opt/venv/
sudo /opt/venv/bin/python -m pip install --upgrade pip
sudo /opt/venv/bin/pip install --no-cache-dir -r bootstrap/content/scripts/python_solver/requirements.txt
```

#### Copy the Python solver to solver location

NOTE: If you modify the Python solver code, you need to copy it again.

```bash
sudo install -d -m 0755 /opt/solver/
sudo cp -a bootstrap/content/scripts/python_solver/main.py /opt/solver/main.py
```

### Build as a docker image

```bash
docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .
```

## Running the scheduler+plugin

To run the scheduler with the plugin, you can either run it on a **KWOK** (recommended) or a **Kind** cluster.

### Prerequisites for running the scheduler+plugin

The following tools are required to run the scheduler with the plugin (tools already mentioned in the build prerequisites are omitted):

- `kubectl` (tested with client v.1.32.7)
- When running in a Kind cluster:
  - `kind` (tested with v0.20.0)
- When running in a KWOK cluster:
  - `kwok`+`kwokctl` (tested with v0.7.0)

### Run scheduler+plugin in a KWOK cluster (recommended)

#### KWOK (Manual)

If you just want to test it manually on a KWOK cluster, first create a scheduler config (see `bootstrap/content/manifests/plugin-kube-scheduler-config.yaml`) and a cluster config file (see `bootstrap/content/data/configs-kwokctl/all_synch_python.yaml`).

NOTE: Make sure you have the latest binary or docker image of the scheduler with the plugin built (see above). Also make sure the latest Python solver is copied to `/opt/solver/main.py` (see above).

Then create the cluster with:

```bash
kwokctl create cluster --name <cluster_name> --runtime <docker/binary> --config <path/to/cluster-config.yaml>
```

To delete the cluster, run:

```bash
kwokctl delete cluster --name <cluster_name>
```

#### KWOK (Automated)

To run the scheduler with the plugin on a KWOK cluster, the easiest way is to use the provided test generator script (`bootstrap/content/scripts/kwok/test_generator.py`, described below). It will create a KWOK cluster, fill it with random pods, and run the scheduler with the plugin.
  
### Run in a Kind cluster

To run the plugin in a Kind cluster, run the provided script `kind/kind-create-cluster.sh` to create a Kind cluster with some specified number of nodes:

```bash
./kind/kind-create-cluster.sh <cluster_name> <num_nodes>
```

Then load the scheduler image into the Kind cluster (it will also build the docker image):

```bash
./kind/kind-load-plugins.sh <cluster_name>
```

## Testing the scheduler+plugin

### Test scripts

To test the plugin using KWOK, some test scripts have been made under `bootstrap/content/scripts/kwok/`:

- `bootstrap/content/scripts/kwok/test_generator.py`: Generates a KWOK cluster with random pods and nodes and runs the scheduler with the plugin and generates statistics.
- `bootstrap/content/scripts/kwok/stats.py`: Manually statistics from a KWOK cluster e.g. number of scheduled pods, current utilization, etc.

#### (Initial) workload generator (`test_generator.py`)

The script `bootstrap/content/scripts/kwok/test_generator.py`, generates an initial workload on a KWOK cluster and runs the scheduler with the plugin.

It has several parameters to configure the plugin, workloads, etc. To see all available parameters, run:

```bash
python3 test_generator.py --help
```


### Test jobs

All the test jobs used to evaluate the plugin can be found under `bootstrap/content/data/jobs/`.

### Running test jobs using HPC resources and init script (recommended)

To make it faster to evaluate the plugin by parallizing evaluation using HPC resources (we used [UCloud](https://docs.cloud.sdu.dk/)), the content to bootstrap a job runner (HPC or VM) is provided under `bootstrap/`.
It contains everything needed to run the tests including the init script `bootstrap.sh` that will ensure everything is set up and the tests are run.

To run the already generated test jobs, follow the steps (using UCloud as an example):

1) Upload the `bootstrap` folder to UCloud and place it under `Files` (can be renamed if needed).
2) If you want to be able to SSH into the instance, add your public SSH key to `SSH Keys` under `Resources`.
3) Create a Terminal instance with Ubuntu 22.04. A illustration is shown below:

   ![UCloud Terminal Instance](./ucloud_job_example.png)

  As shown in the illustration, the important options to the init script are:
     - `--content-dir`: Path to the `bootstrap` folder uploaded in step 1.
     - `--job-file`: Path to the job file to run (e.g. see already made jobs under `bootstrap/content/data/jobs/`).
4) Submit the instance and wait until it is running.
5) To save the results use the App `Archive` and select the folder uploaded in step 4 which should now hold the results. Note if you also want to save the stdout from the instance save the file located under `Files -> Jobs -> <job_id> -> stdout-0.log`.

#### Running test jobs using `test_generator.py` directly

If you prefer or need to run the test jobs directly using the `test_generator.py` script, then first `cd` into the `bootstrap/content/` folder, then run:

```bash
python3 scripts/kwok/test_generator.py \
--job-file data/jobs/<job_file>.yaml
```

More parameters can be provided to customize the test run. To see all available parameters, run:

```bash
python3 scripts/kwok/test_generator.py --help
```

#### Using Vagrant for development and test of init script

To develop and test the init script it can be beneficial to run it in a VM on a local machine. To make it easy, a `Vagrantfile` is provided in the root of the repo. It will create an Ubuntu 22.04 VM with all prerequisites installed and the repo cloned. To use it, install `Vagrant` (tested with v2.4.7) and `VirtualBox` (tested with v7.1.10), then run:

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

### Generating test jobs

Jobs can be generated using the provided job generator script `bootstrap/content/job_generator.py`.

The already generated test jobs can be reproduced using the following commands:

**Deterministic jobs with default scheduler**:

Runs until 100 seeds are found where not all pods are running using the default scheduler.

NOTE: This make use of another plugin called `MyScoreBreaker` to break ties in scoring by name and disables preemption and sets number of parallelism to 1 to make the job generation deterministic. This plugin can be found under `pkg/myscorebreaker/`.

```bash
python3 job_generator.py \
--out-dir data/jobs/default-deterministic \
--output-dir results/default-deterministic \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default-deterministic.yaml \
--seed-file data/seeds/seeds_all.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 0.90 0.95 1.00 1.05 \
--seeds-not-all-running 100 \
--default-scheduler
```

**Default scheduler jobs**:

0.90-0.95 utils runs on the seeds found using the deterministic job generation above.

```bash
python3 job_generator.py \
--out-dir data/jobs/default \
--output-dir results/default \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default.yaml \
--seed-file data/seeds/ \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 0.90 0.95 \
--default-scheduler
```

1.00-1.05 utils runs on a fixed seed file with 100 seeds.

```bash
python3 job_generator.py \
--out-dir data/jobs/default \
--output-dir results/default \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default.yaml \
--seed-file data/seeds/seeds_100.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 1.00 1.05 \
--default-scheduler
```

**Python solver jobs**:

0.90-0.95 utils runs on the seeds found using the deterministic job generation above.

```bash
python3 job_generator.py \
--out-dir data/jobs/all_synch_python \
--output-dir results/all_synch_python \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/all_synch_python.yaml \
--seed-file data/seeds/ \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 0.90 0.95 \
--timeouts 1 10 20 \
--save-scheduler-logs \
--save-solver-stats \
--solver-trigger
```

1.00-1.05 utils runs on a fixed seed file with 100 seeds.

```bash
python3 job_generator.py \
--out-dir data/jobs/all_synch_python \
--output-dir results/all_synch_python \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/all_synch_python.yaml \
--seed-file data/seeds/seeds_100.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 1.00 1.05 \
--timeouts 1 10 20 \
--save-scheduler-logs \
--save-solver-stats \
--solver-trigger
```


### Estimate time to complete all jobs in UCloud

The script `bootstrap/content/jobs_eta.py`, estimates the time to complete each running job in UCloud based on average time per seed.

#### Live-simulator


## Results Analysis: Plots, Tables, etc

### Scripts

The scripts used to generate plots and tables from the results can be found under `analysis/`.

These scripts expects the format of the results to be as generated by the provided test generator script (`bootstrap/content/scripts/kwok/test_generator.py`) and how they are saved according to the job files. If something else is used, the scripts may need to be modified.

After having run all the jobs the results should be organized as follows on disk:

```results/
  |── default-deterministic/
  │     ├── results.csv
  │     ├── logs/
  │     └── solver-stats/
  ├── default/
  │     ├── results.csv
  │     ├── logs/
  │     └── solver-stats/
  └── all_synch_python/
        ├── results.csv
        ├── logs/
        └── solver-stats/
```

To make the necessary combined results, a script `analysis/combine_results.py` is provided that combines the results from different runs into a single CSV file (`per_combo_results.csv`) for easier analysis.

```bash
python3 analysis/combine_results.py
```

Then, the combined results (`per_combo_results.csv`) can be used to generate plots and tables using the provided scripts under `analysis/plots_and_tables.py`.

```bash
python3 analysis/plots_and_tables.py
```

This will generate all the plots and tables used in the report and save them under `analysis/figures/` and `analysis/tables/`.

## Useful kubectl/kwokctl commands

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
  kubectl -n kube-system get cm -l plan
  kubectl -n kube-system get cm <CM> -o jsonpath='{.data.plan\.json}' | jq .
  ```

- Get reasons for pod(s) not scheduling

  ```bash
  kubectl --context <ctx> -n <namespace> get events --field-selector involvedObject.kind=Pod -o json | jq '.items[] | {name: .involvedObject.name, reason: .reason, message: .message}'
  ```

## Live-workload-simulator

Settings
- num_nodes
- initial_avg_pods_per_node (ppn): initial pod count = num_nodes * ppn (only initial sizing, after that we allow the pod-count to fluctuate)
- num_priorities
- target_util (applies to CPU and MEM; we stop when both >= target)
- solver_timeout: in seconds
- Δt: seconds between event-tick -- should be at least solver_timeout
- N: number of event-ticks (e.g. if  Δt = 10s, N = 100, --> run in 1,000s)
      (each event-tick indexed by k)
- low_util_range = [low_min, low_max] with 0 < low_min ≤ low_max < target_util

Generate deterministic events ("trace") to a file.
1) Draw (deterministic) low utilization target: low_util_k = uniform(low_min, low_max)
2) Measure current util: from all alive pods (running + pending): curr_cpu_util, curr_mem_util.
3) Delete pods until we are under low_util_k:
    while (curr_cpu_util >  low_util_k) or (curr_mem_util >  low_util_k):
        - pick a random (deterministic) victim:
            if RS-mode: if RS-replicas > 1  --> scale_rs -1; else --> delete_rs
            if standalone-mode --> delete_pod
        - apply delete; recompute curr_cpu_util, curr_mem_util.
4) Add-to-target_util:
    while (curr_cpu_util < target_util) or (curr_mem_util < target_util):
        - if RS-mode: select a random (deterministic) RS-set (scale +1) or a new (deterministic) one (create and scale to 1).
        - if standalone-mode: create_pod
        - recompute curr_cpu_util, curr_mem_util after each add; stop as soon as both >= target (overshoot allowed, will be adjusted in next tick).
5) Save state after event-tick: state-{seed}-{tick}.csv

events.csv ("trace") file (one row per event-tick, k)
- event_id: event-{seed}-{k}
- k: tick index
- Δt
- t_s: k * Δt 
- low_util_k : the drawn threshold we deleted to
- target_util: the set utilization we add back up to
- cpu_util_after: cpu utilization after applying this event-tick
- mem_util_after: mem utilization after applying this event-tick
- deletions: total number of pods removed by this tick
- additions: total number of pods added by this tick
- alive_after: total number of alive pods after this tick (running + pending)
- actions (json list): possible actions: delete_pod | delete_rs | scale_rs | create_rs | create_pod
      - delete_pod: {name}
      - delete_rs: {name}
      - scale_rs: {name, replicas_num} (new count)
      - create_rs: {name, priority, cpu_m, mem_b, replicas_num=1}
      - create_pod: {name, priority, cpu_m, mem_b}

Notes
- Produces a deterministic load-simulator that deletes pods down to low_util_k, then add up to target_util.
- Pod count will fluctuate; ppn is only for initial sizing.
- Simulator can be in sync (wait for solver completes, and delay next event-tick) or async (no waiting just applying the next event-tick)

Questions
Is it acceptable that we do not meet a fixed pod count and may slightly overshoot utilization after ticks.
Hitting both an exact pod total and an exact utilization simultaneously is not an easy task while still changing cluster state.
Instead, we guarantee a minimum utilization after each tick so the scheduler/solver is exercised under meaningful load,
and any small overshoot self-corrects on subsequent ticks.

## TODOs

- Right now in the python solver, the disruption metric is defined as:
    +1 if the pod stays in the original position
    -2 if the pod is evicted
    -1 if the pod is moved (-2 + 1)
  Jacopo: Thinks my idea reduces the search space, however, he is also fine with a simpler metric, where we just sum the x_{i,p_i.orig} to treat every pod that is not in the same place as 1.
- Also, warm-start the python solver before the loop.
- Test the PolySCIP solver (<https://polyscip.zib.de/>). Seems to support multiple objectives (<https://www.scipopt.org/doc-3.2.0/applications/MultiObjective/>).
- Make a runner script to run the scheduler more realistically for testing the modalities better. We could use a lambda value (Gaussian distribution) to determine how often new pods arrive. Also, we could have a certain percentage of pods that are long-running and some that are short-lived.
- Make the state-of-the-art section in the report for next meeting.
- Write to Jacopo if I have any interesting Related work for the paper.
- Use pluginConfig with args instead of hardcoding values. See [scheduler-config.yaml](https://github.com/AlleNeri/scheduler-plugins/blob/dev-optimizedpreemption/manifests/optimizedpreemption/scheduler-config.yaml)
- Jacopo: Fine to just talk about the missing configuration options possible within k8s.
- Jacopo: Dont add gang scheduling.
- Jacopo: Introduction and Background sections are fine -- missing contribution section in Introduction.
- Write about the non-determism when we activate blocked pods. The order in which they are activated matters. We can consider to sort them based on priority and creation time.
- Write about that gang-scheduling (all-or-nothing strategy) doesn't really make sense in our case as we also preempt pods meaning we therefore cannot revert to the old state. However, we can consider to add a permit plugin to make all planned pods schedule together (but still due it does not implement an all-or-nothing strategy, as we have preemptions).
- QoS classes are currently ignored: BestEffort, Burstable, Guaranteed. We currently assume all pods are Guaranteed Pods. See also <https://medium.com/@muppedaanvesh/a-hands-on-guide-to-kubernetes-qos-classes-%EF%B8%8F-571b5f8f7e58>
- Quotas and LimitsRanges are currently ignored. See also <https://medium.com/codex/what-you-need-to-know-to-debug-a-preempted-pod-on-kubernetes-1c956eec3f35>
- Remember to add: --trigger-optimizer --save-solver-stats --save-scheduler-logs
- Clean up the code:
  - Reduce code
  - Switch case
  - Early return
  - Default values
  - Remove magic numbers
  - Follow DRY, SOLID principles and Design Patterns where possible.
  - Sort functions based on usage from outside to inside
  - Proper logging
  - Comments
  - Proper variable names
  - Replace not understandable code
  - Use common functions
- Write report
- Remove TODOs.
- Make the test plan.
- Write about that we need both the Blocked and Batched sets otherwise we can
- Write about that we always set a lower bound based on priorities in optimal solver to prevent it providing a worse plan. If no hints provided then set it based on current state.
- Write about that we solve at the end and therefore get the status of the overall model. However, to fail fast we also solve after each stage in the lexicographic order.

### Later TODOs

- Create unit and integration tests.
- Find a better way to set verbose level.
- Consider to add a permit plugin for proper gang-scheduling to prevent not fully implemented plans.
- Somehow ensure that the cluster state is the same throughout execution. If not, consider to evict those non-planned pods during execution. We can use the snapshot to see how many there is of each RS-workloads and standalone pods and compare with the actual state. We should never have more than planned, but we can have less if something got deleted externally or if we move a pod or evict it.
- We will get a plan timeout if a pod is removed during plan execution (if a standalone pod is deleted or a workload is scaled down).
- Fix TODOs

### Test

- Test at which utilization the default scheduler stops to work properly.
- Large scale test on UCloud where i could set up multiple ubuntu servers each making on test.
- Test if python solver timing depends heavily on the node it is executed on (CPU type, etc.)
- Test the plugin works across workload type.
- Test CP-SAT vs. other solvers.

#### Later Tests

- Compare with other solvers.
- Compare with other modes.
- Compare with other hook stages.
- Compare with hints.

### Questions

#### Open Questions

#### Closed Questions

- What to do with evicted and blocked pods - put them to queue or try again immediately?
  - Jacopo: Fine, what i am doing now, by just letting them try again immediately
- What to do with node-selectors, PDBs, and other rules.
  - Jacopo: Fine, to ignore these just write about it. May, the extra constaints actually will make the solver faster (smaller search space).
