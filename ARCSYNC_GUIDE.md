# ARCSync 调度器使用指南

本调度器插件旨在解决 GitHub ARC (Actions Runner Controller) 在 NPU 资源不足时，Runner Pod 提前启动导致 GitHub Insight 统计排队时间不准确的问题。

## 1. 核心逻辑
- **全局预检**：在 Runner Pod 调度前，`ARCSync` 插件会扫描集群内所有节点。
- **资源核算**：对每个节点，通过以下优先级计算 NPU 剩余量：
    1. 检查 Pod 标签 `ascend-ci.com/npu-count`。
    2. 若无标签，则检查容器的 `Resources.Requests`。
- **调度决策**：只要集群中**至少有一个节点**能满足后续 Workflow Pod 的需求，Runner Pod 即可正常调度（允许调度到任何节点，不局限于有卡的节点）；否则，Runner Pod 保持 `Pending`。

## 2. 标签 (Label) 使用说明

### Runner Pod (声明需求)
在 Runner Pod 模板中添加以下标签，以便调度器进行预检：

| 标签 Key | 示例值 | 说明 |
| :--- | :--- | :--- |
| `ascend-ci.com/required-npu-count` | `"1"` | **必填**。触发拦截逻辑并声明所需 NPU 数量。 |
| `ascend-ci.com/npu-resource-domain` | `"huawei.com"` | **必填**。资源域名。 |
| `ascend-ci.com/npu-resource-model` | `"ascend-310"` | **必填**。NPU 型号。 |

### Workflow Pod (状态标识)
为了使调度器核算更精准，建议 Workflow Pod 包含以下标签：

| 标签 Key | 示例值 | 说明 |
| :--- | :--- | :--- |
| `ascend-ci.com/npu-count` | `"1"` | 声明当前 Pod 实际占用的 NPU 数量。 |
| `ascend-ci.com/npu-resource-domain` | `"huawei.com"` | **必填**。资源域名。 |
| `ascend-ci.com/npu-resource-model` | `"ascend-310"` | **必填**。NPU 型号。 |

## 3. 调度器配置
在 `KubeSchedulerConfiguration` 中启用插件：

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: ascend-scheduler
    plugins:
      preFilter:
        enabled:
          - name: "ARCSync"
```

## 4. 预期效果
- 当 NPU 资源耗尽时，Runner Pod 会停留在 `Pending` 状态。
- GitHub Actions 会显示 "Waiting for a runner..."。
- **GitHub Insight 将正确统计这段时间为 Queue Time**。

## 5. Liqo 虚拟节点集成

当集群中存在 Liqo 虚拟节点（带 `liqo.io/remote-cluster-id` 标签的 Node）且 Pod 所在 namespace 配置了 `NamespaceOffloading` CR 时，ARCSync 会执行本地与虚拟节点之间的 NPU 资源比对：

1. **本地总剩余** = 所有本地非 cordoned 节点的空闲 NPU 之和
2. **虚拟节点剩余** = `Allocatable[NPU]` - 该虚拟节点上 runner pod 的 `required-npu-count` 标签累加值（远程 workflow pod 不可见，用 runner pod 声明量估算）
3. 取两者中剩余更多的一方进行调度，另一方被 Filter 拒绝
4. 平局时本地优先

### NamespaceOffloading 配置

在需要使用虚拟节点的 namespace 中创建 `NamespaceOffloading` CR（`offloading.liqo.io/v1beta1`），通过 `spec.clusterSelector` 指定可调度的虚拟节点：

```yaml
apiVersion: offloading.liqo.io/v1beta1
kind: NamespaceOffloading
metadata:
  name: default
  namespace: your-namespace
spec:
  clusterSelector:
    matchLabels:
      liqo.io/remote-cluster-id: "target-cluster"
```

## 6. Volcano Queue 集成

当 Pod 所在 namespace 通过 annotation `scheduling.volcano.sh/queue-name` 关联了 Volcano Queue 时，本地资源总量取 Queue 限额与实际总卡数的最小值：

- `本地资源总量 = min(Queue.spec.capability[NPU], Σ 本地节点 Allocatable[NPU])`
- `本地剩余 = 本地资源总量 - 本地已占用`

```yaml
apiVersion: scheduling.volcano.sh/v1beta1
kind: Queue
metadata:
  name: my-queue
spec:
  capability:
    huawei.com/ascend-310: "8"
```

Namespace annotation：

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: your-namespace
  annotations:
    scheduling.volcano.sh/queue-name: "my-queue"
```
