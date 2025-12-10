
# Priority Optimizer Plugin

- [Priority Optimizer Plugin](#priority-optimizer-plugin)
  - [Overview](#overview)
  - [Code Structure and Extension Points](#code-structure-and-extension-points)
  - [Upstream version](#upstream-version)
  - [Integrating](#integrating)
  - [Building](#building)
    - [Binary (recommended)](#binary-recommended)
    - [Docker image](#docker-image)
  - [Running](#running)
    - [KWOK (recommended)](#kwok-recommended)
    - [Kind](#kind)
      - [Version mismatch between Scheduler-plugin and KWOK versions](#version-mismatch-between-scheduler-plugin-and-kwok-versions)
  - [Testing](#testing)
    - [Workload Once Generator](#workload-once-generator)
      - [Deterministic scheduling](#deterministic-scheduling)
      - [Workload configuration file](#workload-configuration-file)
      - [Job file](#job-file)
      - [Bootstrap script](#bootstrap-script)
        - [Using Vagrant for bootstrap script development](#using-vagrant-for-bootstrap-script-development)
      - [Generating test jobs](#generating-test-jobs)
        - [Deterministic jobs with default scheduler](#deterministic-jobs-with-default-scheduler)
        - [Default scheduler jobs](#default-scheduler-jobs)
        - [Python solver jobs](#python-solver-jobs)
      - [Result replication and running test jobs](#result-replication-and-running-test-jobs)
      - [Expected folder structure after running all jobs](#expected-folder-structure-after-running-all-jobs)
      - [Estimate time to complete all jobs in UCloud](#estimate-time-to-complete-all-jobs-in-ucloud)
      - [Analysis](#analysis)
    - [Trace Replayer](#trace-replayer)
    - [Unit and Integration Tests](#unit-and-integration-tests)
    - [GitHub Actions](#github-actions)
  - [Useful kubectl/kwokctl commands](#useful-kubectlkwokctl-commands)

## Overview

This project introduces an **optimized, priority-based** approach for placing pods optimally onto nodes via the **MyPriorityOptimizer** plugin for the Kubernetes scheduler. The project is a fork of the Kubernetes-sigs project [scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins) and extends it with a new plugin. For background see description of the [Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/).

<center><img src="./images/scheduling-framework.png" alt="Scheduling Framework" width="60%"/></center>

Our goal is to schedule as many *high-priority* pods as possible, especially when the *default scheduler* fails to do so (a possibly side effect of our approach is better resource utilization). The plugin enables integration of an *external* solver—here, a Python solver using [Google's CP-SAT](https://developers.google.com/optimization/cp/cp_solver)—to compute an optimal placement plan that maximizes high-priority pods while *minimizing disruption* by reducing the number of preemptions (*movements* and *evictions*). Given the solver's solution, the plugin applies it by evicting and moving pods, possibly across multiple nodes in the cluster (Note: default scheduler can only preempt pods within a *single* node, which may lead to more preemptions than necessary).

The solver can be triggered in different **optimization modes**:

- *PerPod* – optimize for every new pod that arrives (not recommended for large clusters).
- *Periodic* – optimize all pods (running and pending) at fixed intervals.
- *Interlude* – optimize all pods during interlude windows (i.e. when no pods are arriving).
- *Manual* – runs normal scheduling like the ones above, but optimization is only triggered manually (via HTTP). Used for testing and evaluation.
- *ManualBlocking* – same as *Manual*, but all pods are blocked from entering the cluster until the solver is triggered via HTTP and completes. Used for testing and evaluation.

Moreover, the optimization is running in either **sync** or **async mode**:

- *Sync* – the scheduling of new pods is blocked while the solver is running and while the plan is being applied.
- *Async* – the scheduling of new pods is not blocked while the solver is running, only while the plan is being applied. When the solver completes, the cluster state is re-checked to ensure the plan is still valid before applying it; otherwise, the plan is discarded.

The following sections describe how to **build, run, and test** the scheduler with the plugin.
For [result replication](#result-replication-and-running-test-jobs) from the paper, read the provided instructions.

## Code Structure and Extension Points

The code for the **MyPriorityOptimizer** plugin is located under `pkg/mypriorityoptimizer/`.

Some of the main files and their purpose are described below:

- `plugin.go`: Main entry point for the plugin that sets up the plugin.
- `args.go`: Contains the configuration arguments for the plugin (e.g. optimization mode, solver timeout, etc.).
- `optimization_flow.go`: Contains the main logic for running the optimization flow, including triggering the solver, applying the plan, etc.
- `hook_preenqueue.go`: Implements the PreEnqueue scheduling extension point. This is mainly used for blocking new pods while an optimization is running or a plan is being applied.
- `hook_prefilter.go`: Implements the PreFilter scheduling extension point. Its main purpose is targeting the pod onto the node assigned by the solver in the plan (if any).
- `hook_postfilter.go`: Implements the PostFilter scheduling extension point. This is mainly used to mark a pod as unschedulable as the default scheduler failed to place it. If in mode *PerPod*, it also triggers the optimization for every new pod that arrives.
- `hook_reserve_unreserve.go`: Implements the Reserve/Unreserve scheduling extension points. It is used as we cannot rely on specific pod names (e.g. pods from a ReplicaSets), instead we count how many of each that should be placed on each node.
- `plan_completion_watch.go`: A background watcher for plan completion, checking that all pods in the plan are assigned to the correct nodes.

## Upstream version

Latest upstream version are listed in the [Releases](https://github.com/kubernetes-sigs/scheduler-plugins/releases), it should follow the [Kubernetes versioning](https://kubernetes.io/releases).
After merging with upstream, always verify that all works as intended, e.g. by running a small smoke test with the `test_runner.py` script (see below).

## Integrating

To enable and use plugins in the Kubernetes scheduler, you must apply a **scheduler configuration manifest** that selects the plugins and their settings; the manifest for this plugin is `manifests/mypriorityoptimizer/plugin-scheduler-config.yaml` (see also [Scheduler Configuration on Kubernetes webpage](https://kubernetes.io/docs/reference/scheduling/config/) for background).

This file enables the **MyPriorityOptimizer** plugin and the extension points, described above, that it implements.

The plugin is referenced and registered in `cmd/scheduler/main.go` such that the scheduler will include it at build time.

## Building

The scheduler+plugin can be built either as a **binary** (recommended) or as a **docker image** and can then be run in a cluster (e.g. using KWOK or Kind).

The following tools are required (if Windows host, use WSL2 w/ e.g. Ubuntu) to build the scheduler+plugin:

- `git` (tested with 2.43.0)
- `make` (tested with 4.3)
- `python3` (tested with 3.10.12)
- `pip` (tested with 24.0)
- `Go` (tested with 1.24.3)

When building as a docker image:

- `docker` (tested with v28.3.2)
- `docker-buildx-plugin` (tested with v0.25.0)

Currently, it is only tested on **amd64** architecture and some code may need to be modified to run on other architectures (should not be a problem).

### Binary (recommended)

To build the binary, run the following command in the root of the repo:

```bash
make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
```

The built binary will be located in `bin/kube-scheduler`.

### Docker image

We have **modified** the default Dockerfile used to build the scheduler image to include the plugin.

To build the docker image, ensure Docker is running, then run the following command in the root of the repo:

```bash
docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .
```

## Running

To run the scheduler with the plugin, you can e.g. run it in a **KWOK** (recommended) or in a **Kind** cluster.

The following tools are required (tools already mentioned in [Building](#building) are omitted):

- `kubectl` (tested with client v.1.32.7)
- When running in a Kind cluster:
  - `kind` (tested with v0.20.0)
- When running in a KWOK cluster:
  - `kwok`+`kwokctl` (tested with v0.7.0)

### KWOK (recommended)

To set up a KWOK cluster with the scheduler+plugin one needs to provide a configuration file to KWOK. An example of a configuration file is `data/configs-kwokctl/plugin-scheduler.yaml`.

```yaml
kind: KwokctlConfiguration
apiVersion: config.kwok.x-k8s.io/v1alpha1
options:
  kubeSchedulerConfig: manifests/mypriorityoptimizer/plugin-scheduler-config.yaml
  kubeSchedulerBinary: bin/kube-scheduler
  kubeSchedulerImage: localhost:5000/scheduler-plugins/kube-scheduler:dev
componentsPatches:
  - name: kube-scheduler
    # uncomment for more verbose logging
    # extraArgs:
    #   - key: v
    #     value: "10"
    extraEnvs:
      - name: OPTIMIZE_MODE
        value: "periodic" # choices: per_pod, periodic, interlude, manual, manual_blocking
      - name: OPTIMIZE_SOLVE_SYNCH
        value: "true" # choices: true, false
      - name: OPTIMIZE_PERIODIC_INTERVAL
        value: 30s # e.g. 10s, 30s, 60s
      - name: SOLVER_PYTHON_ENABLED
        value: "true"
      - name: SOLVER_PYTHON_TIMEOUT
        value: 10s # e.g. 1s, 10s, 20s
```

As can be seen this file can be used to set environment variables to configure the plugin.

Having set up the KWOK cluster configuration file, ensure you have built the latest scheduler binary or Docker image (see [Building](#building)). Also if using the binary setup, ensure the the latest Python solver is available at `/opt/solver/main.py` and that the Python environment is set up, by following these steps:

- From the root of the repo, run the following commands to set up the Python environment with the required dependencies:

   ```bash
   sudo install -d -m 0755 /opt/venv/
   sudo python3 -m venv /opt/venv/
   sudo /opt/venv/bin/python -m pip install --upgrade pip
   sudo /opt/venv/bin/pip install --no-cache-dir -r scripts/python_solver/requirements.txt
   ```

- Copy the Python solver code to the location expected used by the plugin:

   ```bash
   sudo install -d -m 0755 /opt/solver/
   sudo cp -a scripts/python_solver/main.py /opt/solver/main.py
   ```

   NOTE: If you change the code of the Python solver, you *must* copy it again.

Finally, to create and run the KWOK cluster with the scheduler+plugin, run the following command:

```bash
kwokctl create cluster --name <cluster_name> --runtime <docker/binary> --config <path/to/cluster-config.yaml>
```

If you later want to delete the cluster, run:

```bash
kwokctl delete cluster --name <cluster_name>
```
  
### Kind

We have also made bash scripts for setting up the scheduler with the plugin in a Kind cluster.

To create a Kind cluster with some specified number of nodes, run:

```bash
./kind/kind-create-cluster.sh <cluster_name> <num_nodes>
```

Then load the scheduler docker image into the Kind cluster (it will also build the docker image):

```bash
./kind/kind-load-plugins.sh <cluster_name>
```

#### Version mismatch between Scheduler-plugin and KWOK versions

Sometimes when running on a specific scheduler-plugin version, it may be needed to force the version when building the scheduler binary.
For example, KWOK may report

```bash
# Running the command
kwokctl logs kube-scheduler --name kwok1
# may report
E1210 13:54:11.815001   22220 run.go:72] "command failed" err="[emulation version 0.33 is not between [1.31, 0.33.0], minCompatibilityVersion version 0.32 is not between [1.31, 0.33]]"
```

That can be fixed by building the scheduler with the `VERSION` field set to the desired version (note that we use v1 instead of v0), e.g.:

```bash
make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64' VERSION=v1.33.0
```

## Testing

For testing the plugin, a script has been made to create a bootstrap folder containing all content needed to run the tests, including the built binary, the Python solver code, and all test jobs and configuration files. From the root of the repo, run:

```bash
./make_bootstrap_folder.sh
```

Then, two different approaches for testing have been made:

1) Workload Once Generator (used in the **paper**): A script that can generate an initial workload on a KWOK cluster running the scheduler with the plugin.
2) Trace Replayer: A script that can simulate a live cluster with fluctuating workloads using the scheduler with the plugin.

The first approach is used for evaluating the optimization capabilities of the plugin under different workloads and cluster sizes, while the second approach is used for testing the plugin under changing conditions and compare the different optimization modes.

### Workload Once Generator

For evaluating the plugin, we first run the default scheduler (deterministically) to find 100 seeds where not all pods are running.
Hereafter, we run the default scheduler (as-is) and the scheduler with the `MyPriorityOptimizer` plugin on these seeds.

The script for generating the initial workload and running the tests is `scripts/kwok_workload_once/test_runner.py`. The script can be setup by reading settings from three types of sources, with later ones overriding earlier ones:

1) a workload configuration file
2) a test job file
3) command-line arguments to the script (highest priority)

After choosing one of more of these source, the script can be run to generate the workload and run the tests. An example of how to run the script from the root of the repo or the `bootstrap` folder is:

```bash
python -m scripts.kwok_workload_once.test_runner \
--cluster-name my-cluster \
--kwok-runtime binary \
--job-file data/jobs/kwok_workload_once/<job_file>.yaml \
--workload-config-file data/configs-workload/<workload_config_file>.yaml \
--kwokctl-config-file data/configs-kwokctl/<kwokctl_config_file>.yaml
```

The idea is that `<job_file>.yaml` contains the specific configuration for a job, `<workload_config_file>.yaml` contains the workload configuration to use, and `<kwokctl_config_file>.yaml` contains the KWOK cluster configuration to use (see above [KWOK (recommended)](#kwok-recommended) for more details on this file). In the following sections the [job file](#job-file) and the [workload configuration file](#workload-configuration-file) are described, however, first we shortly describe the deterministic scheduling setup.

#### Deterministic scheduling

To be able to reproduce seeds where not all pods are running using the default scheduler, another plugin called **MyDeterministicScore** is created, located under `pkg/mydeterministicscore/`. This plugin breaks scoring ties by name, disables `DefaultPreemption` plugin and sets `parallelism=1`, helping making scheduling deterministic.

The scheduler configuration file for using this plugin is `data/configs-kwokctl/deterministic-scheduler-config.yaml`.

#### Workload configuration file

As mentioned, the `test_runner.py` script can read a workload configuration file to set up the workload to generate. The file can be used as a template reducing the need to specify all settings in every job file.

An example of a workload configuration file is `data/configs-workload/base.yaml`:

```yaml
kind: WorkloadConfiguration
namespace: test
cpu_per_pod: ["100m", "1000m"]
mem_per_pod: ["100MB","1000MB"]
num_replicas_per_rs: [1, 4]
wait_pod_mode: running
wait_pod_timeout: 2s
settle_timeout_min: 3s
settle_timeout_max: 10s
```

Here the settings, for example, specify that pods should request between 100m and 1000m CPU and between 100MB and 1000MB memory, that ReplicaSets should have between 1 and 4 replicas, the script should wait for pods to be in running state with a timeout of 2s, and that the script should wait between 3s and 10s for the cluster to settle before checking the scheduling results.

#### Job file

To specify the specific configuration for a test job, a job file can be used. The job file can also specify which workload configuration file to use as a template, which kwokctl configuration file to use, which seed file to use, and other settings specific to the job--actually all settings that is possible in the test script.

All the test jobs used previously to evaluate the plugin can be found under `data/jobs/kwok_workload_once`. An example of a job file is `data/jobs/kwok_workload_once/nodes4_pods16_prio4_util095_timeout10.yaml`:

```yaml
workload-config-file: data/configs-workload/base.yaml
kwokctl-config-file: data/configs-kwokctl/plugin-scheduler.yaml
seed-file: data/seeds/kwok_workload_once/nodes4_pods16_prio4_util095.txt
output-dir: results/plugin-scheduler/nodes4_pods16_prio4_util095_timeout10
save-scheduler-logs: true
save-solver-stats: true
solver-trigger: true
override-workload-config:
  num_nodes: 4
  num_pods: 16
  num_priorities: 4
  util: 0.95
override-kwokctl-envs:
- name: SOLVER_PYTHON_TIMEOUT
  value: 10s
```

Here the job file specifies which workload configuration file, kwokctl configuration file, and seed file to use. It also specifies the output directory to save the results to, that scheduler logs and solver stats should be saved, and that the solver should be triggered manually over HTTP. Finally, it overrides/adds some settings in the workload configuration file (number of nodes, pods, priorities, and target utilization) and in the kwokctl configuration file (the Python solver timeout).

#### Bootstrap script

To run the test jobs faster by parallelizing the evaluation using HPC resources (we used [UCloud](https://docs.cloud.sdu.dk/)), a `bootstrap.sh` script is provided under `scripts/bootstrap/` that can be used to set up a job runner (HPC or VM) and run the tests. The script will ensure all prerequisites are installed and the tests are run.

The bootstrap script accepts parameters that the `test_runner.py` script accepts, but the two main parameters to provide are:

- `--content-dir`: path to the `bootstrap` folder created using the `make_bootstrap_folder.sh` script.
- `--job-file`: path to the job file to run (e.g. see jobs under `data/jobs/`).

Note, that if the binary or docker image is not provided this script can also pull the repo and build the latest version before running the tests.

##### Using Vagrant for bootstrap script development

To develop and test the bootstrap script it can be beneficial to run it in a VM on a local machine. For that reason, a `Vagrantfile` is provided in the root of the repo. Ensure the `bootstrap` folder is created first by running the `make_bootstrap_folder.sh` script.
Using Vagrant, it will create an Ubuntu 22.04 VM with all prerequisites installed. To use it, install `Vagrant` (tested with v2.4.7) and `VirtualBox` (tested with v7.1.10), then run:

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

#### Generating test jobs

Jobs can be generated using the provided job generator script `scripts/helpers/job_generator.py`.
It generates one job file per combination of `--num-nodes`, `--avg-pods-per-node`, `--num-priorities`, `--utils`, and `--timeouts`.

To regenerate the jobs, follow the steps in the below sections.

Note that the [deterministic jobs](#deterministic-jobs-with-default-scheduler) should be run first to discover 'not-all-running' seeds under the default scheduler. For each combination, take the produced `seeds_not_all_running.txt` and place it in `data/seeds/` as one file per combination (name it `nodes<N>_pods<P>_prio<R>_util<XYZ>.txt`). Then run both the [default scheduler](#default-scheduler-jobs) and [Python solver jobs](#python-solver-jobs) sets, passing `--seed-file data/seeds/` so they automatically pick the per-combo seed lists.

##### Deterministic jobs with default scheduler

```bash
python -m scripts.helpers.job_generator.py \
--out-dir data/jobs/kwok_workload_once/default-deterministic \
--output-dir results/kwok_workload_once/default-deterministic \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default-deterministic.yaml \
--seed-file data/seeds/kwok_workload_once/seeds_all.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 0.90 0.95 1.00 1.05 \
--seeds-not-all-running 100 \
--default-scheduler
```

##### Default scheduler jobs

0.90-0.95 utils runs on the seeds found using the deterministic job generation above.

```bash
python -m scripts.helpers.job_generator \
--out-dir data/jobs/kwok_workload_once/default \
--output-dir results/kwok_workload_once/default \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default.yaml \
--seed-file data/seeds/kwok_workload_once/ \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 0.90 0.95 \
--default-scheduler
```

1.00-1.05 utils runs on a fixed seed file with 100 seeds.

```bash
python -m scripts.helpers.job_generator \
--out-dir data/jobs/kwok_workload_once/default \
--output-dir results/kwok_workload_once/default \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/default.yaml \
--seed-file data/seeds/kwok_workload_once/seeds_100.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 1.00 1.05 \
--default-scheduler
```

##### Python solver jobs

0.90-0.95 utils runs on the seeds found using the deterministic job generation above.

```bash
python -m scripts.helpers.job_generator \
--out-dir data/jobs/kwok_workload_once/plugin-scheduler \
--output-dir results/kwok_workload_once/plugin-scheduler \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/plugin-scheduler.yaml \
--seed-file data/seeds/kwok_workload_once/ \
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
python -m scripts.helpers.job_generator \
--out-dir data/jobs/kwok_workload_once/plugin-scheduler \
--output-dir results/kwok_workload_once/plugin-scheduler \
--workload-config-file data/configs-workload/base.yaml \
--kwokctl-config-file data/configs-kwokctl/plugin-scheduler.yaml \
--seed-file data/seeds/kwok_workload_once/seeds_100.txt \
--num-nodes 4 8 16 32 \
--avg-pods-per-node 4 8 \
--num-priorities 1 2 4 \
--utils 1.00 1.05 \
--timeouts 1 10 20 \
--save-scheduler-logs \
--save-solver-stats \
--solver-trigger
```

#### Result replication and running test jobs

After generating the jobs, they are ready to run. For faster evaluation, run them in parallel via the bootstrap script on HPC or VM resources.

To run a test jobs, follow these steps:

  1) Run the provided `make_bootstrap_folder.sh` script from the root of the repo to create the `bootstrap` folder containing the bootstrap script, the built binary, the Python solver code, and all content needed to run the tests (incl. the job files and configuration files, etc.):
  
      ```bash
      ./make_bootstrap_folder.sh
      ```
  
  2) Upload the `bootstrap` folder to HPC/VM provider where it can be found (can be renamed if needed). This folder contains the bootstrap script, the built binary, the Python solver code, and all content needed to run the tests (incl. the job files and configuration files, etc.).
  3) (Optional) Enable SSH access, by adding your public SSH key.
  4) Create an Ubuntu 22.04 instance (no GUI needed) for every job
  5) Once all jobs are done, download the folder containing the results to your local machine.

An example of a instance setup using [UCloud](https://docs.cloud.sdu.dk/) is shown below - any comparable HPC or cloud platform should look/works similarly.

<center><img src="./images/ucloud_job_example.png" alt="UCloud Terminal Example" width="70%"/></center>

As shown in the illustration, only two parameters are needed to run the bootstrap script:

- `--content-dir`: path to the `bootstrap` folder uploaded in step 1.
- `--job-file`: path to the job file to run (e.g. see already made jobs under `data/jobs/`).

#### Expected folder structure after running all jobs

Having downloaded the results folder containing results from all jobs - the job files ensures that the results is organized by job type, as follows:

```results/
results/
├── default-deterministic/
│   ├── nodes4_pods16_prio1_util090/
│   │   ├── results.csv
│   │   ├── info.yaml
│   │   ├── seeds-all-running.txt        (if applicable)
│   │   └── seeds-not-all-running.txt    (if applicable)
│   ├── nodes4_pods16_prio1_util095/
│   ├── nodes4_pods16_prio1_util100/
│   ├── nodes4_pods16_prio1_util105/
│   ├── ...
│   └── nodes32_pods256_prio4_util105/
├── default/
│   ├── nodes4_pods16_prio1_util090/
│   │   ├── results.csv
│   │   ├── info.yaml
│   │   ├── seeds-all-running.txt        (if applicable)
│   │   └── seeds-not-all-running.txt    (if applicable)
│   ├── ...
│   └── nodes32_pods256_prio4_util105/
└── plugin-scheduler/
    ├── nodes4_pods16_prio1_util090_timeout01/
    │   ├── results.csv
    │   ├── info.yaml
    │   ├── seeds-all-running.txt        (if applicable)
    │   ├── seeds-not-all-running.txt    (if applicable)
    │   ├── scheduler-logs/
    │   └── solver-stats/
    ├── nodes4_pods16_prio1_util090_timeout10/
    ├── nodes4_pods16_prio1_util090_timeout20/
    ├── nodes4_pods16_prio1_util095_timeout01/
    ├── nodes4_pods16_prio1_util095_timeout10/
    ├── nodes4_pods16_prio1_util095_timeout20/
    ├── nodes4_pods16_prio1_util100_timeout01/
    ├── nodes4_pods16_prio1_util100_timeout10/
    ├── nodes4_pods16_prio1_util100_timeout20/
    ├── nodes4_pods16_prio1_util105_timeout01/
    ├── nodes4_pods16_prio1_util105_timeout10/
    ├── nodes4_pods16_prio1_util105_timeout20/
    ├── ...
    └── nodes32_pods256_prio4_util105_timeout20/

```

The `results.csv` file contains the scheduling results for the job, while the `info.yaml` file contains the job configuration used. If applicable, the `seeds-all-running.txt` and `seeds-not-all-running.txt` files contain the seeds where all pods were running and where not all pods were running, respectively. For the plugin jobs, the `scheduler-logs/` folder contains the saved kube-scheduler logs for each seed, while the `solver-stats/` folder contains the saved solver statistics for each seed.

#### Estimate time to complete all jobs in UCloud

Use `scripts/helpers/jobs_eta.py` to list ETAs for running jobs. It recursively scans for `eta_*` files, extracts recorded ETA and seed progress and prints a sorted summary.

```bash
python -m scripts.helpers.jobs_eta
```

#### Analysis

Analyzed results are placed under `analysis/kwok_workload_once/`. They assume the layout shown in [Expected folder structure after running all jobs](#expected-folder-structure-after-running-all-jobs); if yours differs, code changes may be needed.

1. First, merge all job outputs into a single CSV by running `python scripts/kwok_workload_once/combine_results.py` (it writes `analysis/kwok_workload_once/per_combo_results.csv`; adjust input/output paths in the script if needed).
2. Then run `python scripts/kwok_workload_once/plots_and_tables.py` to produce every figure and table used in the report.
   Outputs are saved under `analysis/kwok_workload_once/figures/` and `analysis/kwok_workload_once/tables/`.

### Trace Replayer

TODO: Implementation in progress.

### Unit and Integration Tests

To make running tests easier, a `run_tests.sh` script has been provided in the root of the repo that can be used to run all tests (both Python and Go tests), simply run:

```bash
./run_tests.sh
# or to run only unit tests, run:
./run_tests.sh unit
# or to run only integration tests, run:
./run_tests.sh int
```

### GitHub Actions

A GitHub Actions workflow is provided in `.github/workflows/opt-prio-ci.yml` that runs on every push and pull request to the default branch `opt-prio-main` branch. The workflow runs the unit and integration tests.

## Useful kubectl/kwokctl commands

- Get pods

  ```bash
  # get all pods
  kubectl get pods -A -n <namespace>
  # get pending pods
  kubectl get pods -A -n <namespace> --field-selector=status.phase=Pending
  # get full description of a pod
  kubectl describe pod <pod_name> -n <namespace>
  ```

- Get nodes

  ```bash
  # get all nodes
  kubectl get nodes
  # get capacity and allocatable for a node
  kubectl get node <node> -o jsonpath='{.status.capacity}{"\n"}{.status.allocatable}{"\n"}'
  ```

- Delete namespace

  ```bash
  kubectl delete ns <namespace>
  ```

- Show recent cluster events

  ```bash
  kubectl get events -A --sort-by=.lastTimestamp
  ```

- Get kube-scheduler logs from KWOK cluster

  ```bash
  kwokctl logs kube-scheduler --name <cluster_name>
  ```

- Getting plugin configuration from kube-scheduler

  ```bash
  kubectl -n kube-system get cm kube-scheduler -o jsonpath='{.data.config\.yaml}' | yq e .
  ```

- Getting saved solver plans from kube-scheduler

  ```bash
  kubectl -n kube-system get cm -l plan
  kubectl -n kube-system get cm <CM> -o jsonpath='{.data.plan\.json}' | jq .
  ```

- KWOK: list clusters / create / delete

  ```bash
  # get all clusters
  kwokctl get clusters
  # create / delete cluster
  kwokctl create cluster --name <cluster_name>
  kwokctl delete cluster --name <cluster_name>
  ```

- KWOK: Get reasons for pod(s) not scheduling

  ```bash
  kubectl --context <ctx> -n <namespace> get events --field-selector involvedObject.kind=Pod -o json | jq '.items[] | {name: .involvedObject.name, reason: .reason, message: .message}'
  ```
