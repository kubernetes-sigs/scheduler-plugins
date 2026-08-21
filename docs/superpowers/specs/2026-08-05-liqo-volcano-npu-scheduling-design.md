# Liqo 虚拟节点 + Volcano Queue NPU 调度设计

## 概述

在现有 ARCSync 插件（`pkg/arcsync/arcsync.go`）基础上扩展，实现本地节点与 Liqo 虚拟节点之间的 NPU 资源比对调度。当 namespace 配置了 Liqo 虚拟节点时，调度器比较本地总剩余 NPU 卡数与各虚拟节点剩余卡数，选择剩余更多的一侧进行调度。若 namespace 同时配置了 Volcano Queue，本地资源总量取 Queue 限额与实际总卡数的最小值。

## 触发条件

新逻辑仅在**同时满足以下两个条件**时激活：

1. 集群中存在 Liqo 虚拟节点（带 `liqo.io/remote-cluster-id` 标签的 Node）
2. 待调度 pod 的 namespace 中存在 `NamespaceOffloading` CR（`offloading.liqo.io/v1beta1`），且其 `spec.clusterSelector` 能匹配到至少一个虚拟节点

任一条件不满足时，ARCSync 保持当前行为不变（所有非 cordoned 节点统一参与调度）。

## 外部系统

### Liqo

- **虚拟节点识别**：Node 对象带 `liqo.io/remote-cluster-id` 标签。
- **NamespaceOffloading CRD**（`offloading.liqo.io/v1beta1`）：存在于目标 namespace 中，`spec.clusterSelector`（标准 label selector）决定该 namespace 可调度到哪些虚拟节点。`spec.podOffloadingStrategy` 可选值 `Local`/`Remote`/`LocalAndRemote`，但不影响比对逻辑本身。
- **虚拟节点资源**：虚拟节点的 `node.Status.Allocatable` 反映通过 `ResourceSlice` 协商的资源量。

### Volcano

- **Namespace 关联**：namespace 通过 annotation `scheduling.volcano.sh/queue-name` 指定关联的 Queue 名称。
- **Queue CRD**（`scheduling.volcano.sh/v1beta1`）：`spec.capability` 是一个 `ResourceList`（资源名到数量的映射），定义 Queue 的资源上限（hard constraint）。

### 虚拟节点 NPU 占用计算的约束

用户使用 GitHub ARC（Actions Runner Controller）。Runner pod 触发实际占用 NPU 资源的 workflow pod。若 runner pod 调度到虚拟节点，其触发的 workflow pod 运行在远程集群，本地集群无法感知。因此虚拟节点的 NPU 已占用数只能通过 runner pod 的 `ascend-ci.com/required-npu-count` 标签和运行中的 runner pod 数量间接计算。

## 实现方案

采用 **Dynamic Informers** 方案：在 `New()` 中创建 `dynamic.Interface` 客户端，为 `NamespaceOffloading` 和 `Queue` CRD 各注册一个 informer，使用 unstructured 对象提取所需字段。不引入 Volcano 或 Liqo 的 Go 模块依赖。

## 架构

### ARCSync 结构体扩展

```
ARCSync struct {
    handle               framework.Handle
    podLister            corev1listers.PodLister
    inFlightReservations map[string]reservation
    mu                   sync.Mutex
    // 新增
    virtNodeLabelKey    string             // "liqo.io/remote-cluster-id"
    nsOffloadingLister  cache.GenericLister // NamespaceOffloading 缓存
    queueLister         cache.GenericLister // Volcano Queue 缓存
}
```

### New() 中的 Informer 注册

1. 从 `handle.KubeConfig()` 创建 `dynamic.NewForConfig(kubeConfig)` 客户端。
2. 创建 `dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 30*time.Second)`。
3. 注册 NamespaceOffloading informer：
   - GVR: `offloading.liqo.io/v1beta1`，resource `namespaceoffloadings`
4. 注册 Queue informer：
   - GVR: `scheduling.volcano.sh/v1beta1`，resource `queues`
5. 启动 informer（`Start(ctx.Done())` + `WaitForCacheSync(ctx.Done())`）。
6. 若 CRD 不存在或 informer 创建失败，catch error 并记录日志，插件以 nil lister 运行，PreFilter 中判断 lister == nil 时跳过比对（降级为当前行为）。

### preFilterState

结构不变，语义变化：`nodeFreeNPU` 只包含决策后胜出侧的节点。

```
preFilterState struct {
    requiredCount int64
    resourceName  v1.ResourceName
    nodeFreeNPU   map[string]int64   // 仅包含胜者侧节点
}
```

## 数据流

### PreFilter（核心决策点）

在现有 PreFilter 逻辑基础上，增加以下步骤：

#### 步骤 1 — 虚拟节点识别与分类

遍历 `nodeInfos`，将节点分为两类：
- **本地节点**：无 `liqo.io/remote-cluster-id` 标签
- **虚拟节点**：有该标签

#### 步骤 2 — 计算 NPU 占用（两类节点不同算法）

- **本地节点**：维持现有逻辑——遍历节点上所有 pod，取容器 `Requests[NPU]` 和 `AllocatedNPUCount` 标签的较大值，求和为物理占用。
- **虚拟节点**：遍历节点上所有 pod，凡带 `RequiredNPUCount` 标签且资源域（`ResourceDomain`）/型号（`ResourceModel`）匹配的 pod，取其 `required-npu-count` 值累加为占用。因为远程 workflow pod 不可见，用 runner pod 声明的需求量作为已占用估算。

两类节点的 in-flight 预留均纳入占用（现有逻辑不变）。

#### 步骤 3 — 计算空闲 NPU

- 每个虚拟节点空闲 = `Allocatable[NPU]` - 占用 - in-flight 预留
- 每个本地节点空闲 = `Allocatable[NPU]` - 占用 - in-flight 预留（现有逻辑）
- 本地总空闲 = Σ 各本地节点空闲

#### 步骤 4 — Volcano Queue 调整本地资源总量（如适用）

若 namespace annotation `scheduling.volcano.sh/queue-name` 存在：
- 查 Queue CRD `spec.capability[fullResourceName]` 得到 `queueLimit`
- `localTotalCapacity = min(queueLimit, Σ 本地节点 Allocatable[NPU])`
- `localRemaining = localTotalCapacity - Σ 本地节点物理占用 - in-flight 预留`

否则：
- `localTotalCapacity = Σ 本地节点 Allocatable[NPU]`（即实际总卡数）
- `localRemaining = localTotalCapacity - Σ 本地节点物理占用 - in-flight 预留`（即 Σ 各本地节点空闲）

#### 步骤 5 — 比对决策

从 NamespaceOffloading 的 `clusterSelector` 匹配出 namespace 有权调度的虚拟节点集合。

取 `localRemaining` 与每个有权虚拟节点空闲量中的**最大值**：

- **本地胜**（含平局） → `nodeFreeNPU` 仅包含本地节点（虚拟节点不写入，被 Filter 拒绝）
- **某虚拟节点胜** → `nodeFreeNPU` 仅包含该虚拟节点（本地节点和其他虚拟节点不写入）

若胜者侧最大空闲 < `requiredCount` → 返回 Unschedulable（pod 保持 Pending）。

负数 `localRemaining` 统一 clamp 到 0 参与比对。

#### 步骤 6 — FIFO 检查

现有逻辑不变。

### Filter

不变。节点不在 `nodeFreeNPU` 中则返回 Unschedulable，在其中则检查空闲量是否足够。

### Score

不变。按 `nodeFreeNPU[nodeName]` 打分。当虚拟节点胜出时，只有该虚拟节点进入评分，runner pod 自然调度到它上面。

### Reserve / Unreserve / PostBind

不变。Reserve 记录的 `nodeName` 对虚拟节点同样有效，in-flight 预留按节点名追踪。

## 关键函数

### 虚拟节点 NPU 占用计算

```
func calcVirtualNodeOccupied(nodeInfo, resDomain, resModel) int64:
    occupied := 0
    for pod in nodeInfo.Pods:
        skip if completed/failed
        if pod.Labels[RequiredNPUCount] != ""
           && pod.Labels[ResourceDomain] == resDomain
           && pod.Labels[ResourceModel] == resModel:
            count := parseInt(pod.Labels[RequiredNPUCount])
            occupied += count
    return occupied
```

### Volcano Queue 限额查询

```
func getQueueNpuLimit(namespace, fullResourceName) (int64, found bool):
    queueName := namespace.Annotations["scheduling.volcano.sh/queue-name"]
    if queueName == "": return 0, false

    obj := queueLister.Get(queueName)  // unstructured
    capability := obj.GetNested("spec", "capability")
    val := capability[fullResourceName]
    return val, true
```

## 降级策略

| 场景 | 降级行为 |
|------|---------|
| 集群中无虚拟节点 | 完全走现有逻辑，不触发比对 |
| namespace 无 NamespaceOffloading CR | 走现有逻辑 |
| NamespaceOffloading 的 `clusterSelector` 未匹配到任何虚拟节点 | 走现有逻辑（等价于"无 liqo 配置"） |
| Queue CRD 不存在或 informer 未就绪 | `getQueueNpuLimit` 返回 `found=false`，本地总量退化为实际总卡数 |
| Queue `spec.capability` 中无对应 NPU 资源键 | 同上，退化为实际总卡数 |
| Dynamic informer 创建失败 | New() 中 catch error，日志告警，插件以 nil lister 运行，PreFilter 中判断 lister == nil 时跳过比对 |

## 边界条件

- **平局**：`localRemaining == virtualNodeRemaining` → 本地胜出（本地优先，减少跨集群开销）。
- **多个虚拟节点剩余相同且最大** → 取第一个（确定性：后续可扩展为按 remote-cluster-id 排序保证确定性）。
- **in-flight 预留跨节点**：现有逻辑已按 `res.nodeName` 追踪，虚拟节点的预留同样按虚拟节点名追踪。
- **本地总剩余为负**：clamp 到 0，虚拟节点必然胜出（若虚拟节点剩余也全为 0，则 `hasCandidate = false`，pod 保持 Pending）。

## 测试策略

在 `pkg/arcsync/arcsync_test.go`（新建文件）中编写单元测试，使用 `testutil.NewTestFramework` 模式（参考 `capacityscheduling_test.go`）：

| 测试用例 | 验证点 |
|---------|--------|
| 无虚拟节点 | 行为与现有逻辑一致 |
| 有虚拟节点但 namespace 无 offloading | 行为与现有逻辑一致 |
| 本地剩余 > 虚拟节点剩余 | `nodeFreeNPU` 仅含本地节点 |
| 虚拟节点剩余 > 本地剩余 | `nodeFreeNPU` 仅含获胜虚拟节点 |
| Volcano queue 限额 < 实际总卡数 | 本地总量被 cap，可能改变胜者 |
| Volcano queue 限额 > 实际总卡数 | 本地总量不受影响 |
| Queue CRD 不存在 | 退化为实际总卡数 |
| 平局 | 本地胜出 |
| 胜者侧空闲 < requiredCount | pod Unschedulable |
| 虚拟节点上 runner pod 占用计算 | `RequiredNPUCount` 标签正确累加 |
| NamespaceOffloading clusterSelector 过滤 | 仅匹配的虚拟节点参与比对 |

## 文件变更清单

| 文件 | 变更类型 |
|------|---------|
| `pkg/arcsync/arcsync.go` | 修改：扩展结构体、New()、PreFilter |
| `pkg/arcsync/arcsync_test.go` | 新建：单元测试 |
| `ARCSYNC_GUIDE.md` | 修改：补充 liqo/volcano 集成说明 |
