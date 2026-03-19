# Coscheduling

This folder holds the coscheduling plugin implementations based on [Coscheduling based on PodGroup CRD](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/42-podgroup-coscheduling). The old version coscheduling is based on [Lightweight coscheduling based on back-to-back queue
 sorting](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/kep/2-lightweight-coscheduling).

## Maturity Level

<!-- Check one of the values: Sample, Alpha, Beta, GA -->

- [ ] 💡 Sample (for demonstrating and inspiring purpose)
- [ ] 👶 Alpha (used in companies for pilot projects)
- [x] 👦 Beta (used in companies and developed actively)
- [ ] 👨 Stable (used in companies for production workloads)

## Tutorial

### PodGroup

We use a special label named `scheduling.x-k8s.io/pod-group` to define a PodGroup. Pods that set this label and use the same value belong to the same PodGroup.

```
# PodGroup CRD spec
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: nginx
spec:
  scheduleTimeoutSeconds: 10
  minMember: 3
---
# Add a label `scheduling.x-k8s.io/pod-group` to mark the pod belongs to a group
labels:
  scheduling.x-k8s.io/pod-group: nginx
```

We will calculate the sum of the Running pods and the Waiting pods (assumed but not bind) in scheduler, if the sum is greater than or equal to the minMember, the Waiting pods
will be created.

Pods in the same PodGroup with different priorities might lead to unintended behavior, so need to ensure Pods in the same PodGroup with the same priority.

### Expectation

1. If 2 PodGroups with different priorities come in, the PodGroup with high priority has higher precedence.
2. If 2 PodGroups with same priority come in when there are limited resources, the PodGroup created first one has higher precedence.

### Config

1. queueSort, permit and unreserve must be enabled in coscheduling.
2. preFilter is enhanced feature to reduce the overall scheduling time for the whole group. It will check the total number of pods belonging to the same `PodGroup`. If the total number is less than minMember, the pod will reject in preFilter, then the scheduling cycle will interrupt. And the preFilter is user selectable according to the actual situation of users. If the minMember of PodGroup is relatively small, for example less than 5, you can disable this plugin. But if the minMember of PodGroup is relatively large, please enable this plugin to reduce the overall scheduling time.

```
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: default-scheduler
  plugins:
    multiPoint:
      enabled:
      - name: Coscheduling
    queueSort:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
```

### Demo

Suppose we have a cluster which can only afford 3 nginx pods. We create a ReplicaSet with replicas=6, and set the value of minMember to 3.
```yaml
apiVersion: scheduling.x-k8s.io/v1alpha1
kind: PodGroup
metadata:
  name: nginx
spec:
  scheduleTimeoutSeconds: 10
  minMember: 3
---
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: nginx
  labels:
    app: nginx
spec:
  replicas: 6
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      name: nginx
      labels:
        app: nginx
        scheduling.x-k8s.io/pod-group: nginx
    spec:
      containers:
      - name: nginx
        image: nginx
        resources:
          limits:
            cpu: 3000m
            memory: 500Mi
          requests:
            cpu: 3000m
            memory: 500Mi
```

```script
$ kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE
nginx-4jw2m   0/1     Pending   0          55s
nginx-4mn52   1/1     Running   0          55s
nginx-c9gv8   1/1     Running   0          55s
nginx-frm24   0/1     Pending   0          55s
nginx-hsflk   0/1     Pending   0          55s
nginx-qtj5f   1/1     Running   0          55s
```

If minMember is set to 4 at this time, all nginx pods are in pending state because the resource does not meet the requirements of minMember
```script
$ kubectl get pods
NAME          READY   STATUS    RESTARTS   AGE
nginx-4vqrk   0/1     Pending   0          3s
nginx-bw9nn   0/1     Pending   0          3s
nginx-gnjsv   0/1     Pending   0          3s
nginx-hqhhz   0/1     Pending   0          3s
nginx-n47r7   0/1     Pending   0          3s
nginx-n7vtq   0/1     Pending   0          3s
```

### Advanced Configuration

The Coscheduling plugin supports additional parameters to control backoff and rejection behavior when PodGroups fail scheduling.

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
- schedulerName: default-scheduler
  pluginConfig:
  - name: Coscheduling
    args:
      permitWaitingTimeSeconds: 60     # How long pods wait in Permit for quorum (default: 60)
      podGroupBackoffSeconds: 10       # Backoff time after PodGroup rejection (default: 0, disabled)
      podGroupRejectPercentage: 10      # Percentage of unassigned pods below which PostFilter skips rejection (default: 10)
  plugins:
    multiPoint:
      enabled:
      - name: Coscheduling
    queueSort:
      enabled:
      - name: Coscheduling
      disabled:
      - name: "*"
```

#### `podGroupBackoffSeconds`

When a PodGroup fails scheduling in PostFilter, setting `podGroupBackoffSeconds` to a positive value causes all pods in the group to be blocked from scheduling for the specified duration. This prevents wasteful scheduling cycles when the cluster clearly cannot accommodate the group.

**How it works:**
1. A pod fails the Filter phase and enters PostFilter.
2. If the PodGroup has enough pods to meet its `minMember` quorum but scheduling still fails (e.g., insufficient resources), the PodGroup is placed in a backoff cache with a TTL equal to `podGroupBackoffSeconds`.
3. During the backoff window, PreFilter immediately rejects all pods from the backed-off PodGroup with `UnschedulableAndUnresolvable`, preventing any scheduling attempts.
4. After the TTL expires, pods can be scheduled again.

**Important:** Backoff is only triggered when:
- `podGroupBackoffSeconds > 0` (feature is enabled)
- The percentage of unassigned pods exceeds `podGroupRejectPercentage` (the group is far from meeting quorum)
- The total number of pods with the PodGroup label is at least `minMember` (enough pods exist to form the group)

**Recommended values:**
- `0` (default): Disabled. Pods retry immediately after failure.
- `5-10`: For clusters with moderate resource contention.
- `30-60`: For heavily oversubscribed clusters where resources change slowly.

#### `podGroupRejectPercentage`

Controls the percentage of unassigned pods (relative to `minMember`) below which PostFilter will **not** reject the PodGroup. This implements an optimistic scheduling strategy: when a PodGroup is close to meeting its quorum, the remaining pods are allowed to retry rather than rejecting the entire group.

**How it works:**
- PostFilter calculates the percentage of unassigned pods: `(minMember - assignedPods) / minMember * 100`.
- If the unassigned percentage is at or below `podGroupRejectPercentage`, PostFilter returns `Unschedulable` without rejecting waiting pods or triggering backoff. The remaining pods get another chance to schedule.
- If the unassigned percentage exceeds `podGroupRejectPercentage`, PostFilter rejects all waiting pods and (if `podGroupBackoffSeconds > 0`) triggers backoff.

**Example:** With `minMember=100` and `podGroupRejectPercentage=10` (default):
- 92 pods assigned (8% gap): No rejection — almost there, keep trying.
- 80 pods assigned (20% gap): Rejection — the group clearly doesn't fit right now.

**Values:**
- `10` (default): Skip rejection when ≤10% of pods remain unassigned.
- `0`: Always reject on any failure (disable optimistic behavior).
- `100`: Never reject (fully optimistic — disable PostFilter group rejection entirely).
