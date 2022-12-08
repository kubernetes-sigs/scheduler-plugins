## TopologicalSort Plugin

#### Extension point: QueueSort 

Pods belonging to an AppGroup should be sorted based on their topology information. 
The `TopologicalSort` plugin compares the pods' index available in the AppGroup CRD for the preferred sorting algorithm. 

If pods do not belong to an AppGroup or belong to different AppGroups, we follow the 
strategy of the **less function** provided by the [QoS plugin](https://github.com/kubernetes-sigs/scheduler-plugins/tree/master/pkg/qos).

```go
// Less is the function used by the activeQ heap algorithm to sort pods.
// Sort Pods based on their App Group and corresponding service topology.
// Otherwise, follow the strategy of the QueueSort Plugin
func (ts *TopologicalSort) Less(pInfo1, pInfo2 *framework.QueuedPodInfo) bool {
    // 1) Check if both pods belong to an AppGroup
    (...)
    // 2) If one of them does not belong -> Return: Follow the Less function of the QoS Sort plugin. 
    (...)
    // 3) Check if both pods belong to the same AppGroup
    (...)
    // 4) If Pods belong to the same App Group -> Get AppGroup from AppGroup lister
    (...)
        // 4.1) Binary search to find both order indexes since topology list is ordered by Workload Name
        (...)
        // 4.2) Return: a lower index is better, thus invert result!
        return !(order(pInfo1) > order(pInfo2))
    // 5) Pods do not belong to the same App Group: return and follow the strategy from the QoS plugin
    (...)
}
```

#### `TopologicalSort` Example

Let's consider the Online Boutique application shown previously. 
The AppGroup consists of 11 workloads (11 groups of pods) and the topology order based on the KahnSort algorithm is **P1, P10, P9, P8, P7, P6, P5, P4, P3, P2, P11.**

The plugin favors low indexes. Thus, depending on the two pods evaluated in the Less function, the result (bool) is the following: 

<p align="center"><img src="../../../kep/260-network-aware-scheduling/figs/queueSortExample.png" title="queueSortExample" width="528" class="center"/></p>

For instance, consider the following scheduler config as an example to enable the `TopologicalSort` plugin:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1beta2
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "REPLACE_ME_WITH_KUBE_CONFIG_PATH"
profiles:
- schedulerName: network-aware-scheduler
  plugins:
    queueSort:
      enabled:
      - name: TopologicalSort
      disabled:
      - name: "*"
  pluginConfig:
  - name: TopologicalSort
    args:
      namespaces:
      - "default"
```