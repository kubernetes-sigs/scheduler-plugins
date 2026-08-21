# ARCSync Liqo/Volcano 调度压测用例

> **集群配置**：
> - 主集群 gy006：本地节点，8 张 NPU 卡
> - 虚拟节点 cn12-001：远端集群，20 张 NPU 卡（当前唯一可用虚拟节点）
> 
> 资源类型：`huawei.com/ascend-1980`

## 一、基础功能验证

### TC-01: 无虚拟节点时行为不变
- **前置条件**：集群中无 Liqo 虚拟节点（无 `liqo.io/remote-cluster-id` 标签的 Node）
- **Pod 配置**：runner pod 带 `required-npu-count=1`，无 NamespaceOffloading
- **预期**：所有本地非 cordoned 节点参与调度，行为与原始 ARCSync 完全一致
- **验证点**：pod 正常调度到空闲最多的本地节点

### TC-02: 有虚拟节点但 namespace 无 NamespaceOffloading
- **前置条件**：集群有虚拟节点（带 `liqo.io/remote-cluster-id`，Allocatable 20 张），pod 所在 namespace 无 NamespaceOffloading CR
- **Pod 配置**：runner pod 带 `required-npu-count=1`
- **预期**：虚拟节点从 `nodeFreeNPU` 中排除，只本地节点参与调度
- **验证点**：pod 只调度到本地节点，不会卡 Pending

### TC-03: 本地剩余 > 虚拟节点剩余 → 本地胜
- **前置条件**：
  - namespace1 有 NamespaceOffloading（clusterSelector 匹配虚拟节点）
  - 本地节点：8 张卡，0 占用 → 本地剩余 8
  - 虚拟节点：20 张卡，15 张占用（runner pod）→ 虚拟剩余 5
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：本地胜出，虚拟节点被 Filter 拒绝，pod 调度到本地节点
- **验证点**：pod 绑定到本地节点，不绑定到虚拟节点

### TC-04: 虚拟节点剩余 > 本地剩余 → 虚拟节点胜
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - 本地节点：8 张卡，6 张占用 → 本地剩余 2
  - 虚拟节点：20 张卡，0 占用 → 虚拟剩余 20
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：虚拟节点胜出，本地节点被 Filter 拒绝，pod 调度到虚拟节点
- **验证点**：pod 绑定到虚拟节点，runner pod 在虚拟节点上运行

### TC-05: 平局 → 本地优先
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - 本地节点：8 张卡，4 张占用 → 本地剩余 4
  - 虚拟节点：20 张卡，16 张占用 → 虚拟剩余 4
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：本地胜出（`>=` 比较），pod 调度到本地节点
- **验证点**：pod 绑定到本地节点

### TC-06: 胜者侧无足够 NPU → pod 保持 Pending
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - 本地剩余 = 0（8 张全满），虚拟节点剩余 = 0（20 张全满）
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：PreFilter 返回 Unschedulable，pod 保持 Pending
- **验证点**：pod 状态为 Pending，不调度

---

## 二、Volcano Queue 集成

### TC-07: Queue 限额 < 实际总卡数 → 本地总量被 cap
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - namespace1 annotation `scheduling.volcano.sh/queue-name: q1`
  - Queue q1 `spec.capability[huawei.com/ascend-1980] = 4`
  - 本地节点总卡数 8，已占用 2 → 物理剩余 6，但 Queue 限额 4 → 本地剩余 = 4-2 = 2
  - 虚拟节点剩余 15
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：本地剩余 2 < 虚拟剩余 15 → 虚拟节点胜出
- **验证点**：pod 调度到虚拟节点（如果没有 Queue cap，本地剩余 6 < 15，虚拟也会胜出；但如果虚拟剩余为 3，则 6 > 3 本地会胜出，而 2 < 3 虚拟胜出——Queue cap 改变了结果）

### TC-07b: Queue 限额改变调度决策
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - Queue q1 限额 = 6
  - 本地总卡数 8，已占用 3 → 物理剩余 5，Queue cap 6 → 本地剩余 = 6-3 = 3
  - 虚拟节点剩余 4
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：本地剩余 3 < 虚拟剩余 4 → 虚拟节点胜出
- **验证点**：如果没有 Queue cap，本地剩余 5 >= 4 → 本地会胜出。Queue 限额将本地剩余从 5 降到 3，改变了胜者

### TC-08: Queue 限额 > 实际总卡数 → 不影响
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - Queue q1 限额 = 20
  - 本地总卡数 8，已占用 2 → 本地剩余 6
  - 虚拟节点剩余 3
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：min(20, 8) = 8，本地剩余 6 >= 3 → 本地胜出
- **验证点**：pod 调度到本地节点

### TC-09: Queue CRD 不存在 → 退化为实际总卡数
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - namespace1 有 annotation `scheduling.volcano.sh/queue-name: q1`
  - Volcano Queue CRD 未安装（informer 为 nil）
  - 本地总卡数 8，已占用 2 → 本地剩余 6
  - 虚拟节点剩余 3
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：getQueueNpuLimit 返回 false，本地总量 = 实际总卡数 8，本地剩余 6 >= 3 → 本地胜出
- **验证点**：pod 调度到本地节点，无错误日志

### TC-10: Queue 中无对应 NPU 资源键 → 退化为实际总卡数
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - Queue q1 `spec.capability` 只有 `cpu` 和 `memory`，无 NPU 资源键
- **Pod 配置**：runner pod `required-npu-count=1`，namespace1
- **预期**：getQueueNpuLimit 返回 false，本地总量 = 实际总卡数
- **验证点**：行为与无 Queue 时一致

---

## 三、nodeSelector 交互

### TC-11: nodeSelector 限定虚拟节点 → 直接调度到该虚拟节点
- **前置条件**：
  - namespace1 有 NamespaceOffloading（clusterSelector 匹配 cn12-001 和其他虚拟节点）
  - 本地节点 8 张全空闲
  - 虚拟节点 cn12-001 剩余 15 张
  - 其他虚拟节点剩余 8 张
- **Pod 配置**：runner pod `required-npu-count=1`，`nodeSelector: {liqo.io/remote-cluster-id: cn12-001}`
- **预期**：
  - nodeFreeNPU 只包含 cn12-001（nodeSelector 过滤掉本地和其他虚拟节点）
  - liqo 比对：localRemaining=0（无本地节点在 nodeFreeNPU 中），bestVirtRemaining=15
  - 虚拟节点自动胜出
- **验证点**：pod 绑定到 cn12-001 虚拟节点

### TC-12: nodeSelector 限定虚拟节点，该虚拟节点满 → pod Pending
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - 虚拟节点 cn12-001 满（20 张全占用，剩余 0）
  - 本地节点 8 张全空闲
- **Pod 配置**：runner pod `required-npu-count=1`，`nodeSelector: {liqo.io/remote-cluster-id: cn12-001}`
- **预期**：
  - nodeFreeNPU 只包含 cn12-001（free=0）
  - 本地节点不匹配 nodeSelector → 不在 nodeFreeNPU 中
  - hasCandidate = false → Unschedulable
- **验证点**：pod 保持 Pending，不会调度到本地节点

---

## 四、跨 namespace 隔离

### TC-13: namespace1 的 Queue 限额不影响 namespace2
- **前置条件**：
  - namespace1 有 NamespaceOffloading + Queue 限额 4
  - namespace2 无 NamespaceOffloading
  - 本地节点 8 张卡，namespace1 的 workflow pod 占用 4 张
  - 虚拟节点满（20 张全占用）
- **Pod 配置**：
  - pod A：namespace1 的 runner pod（本地 Queue 剩余 = min(4,8)-4 = 0，虚拟剩余 0 → Pending）
  - pod B：namespace2 的 runner pod `required-npu-count=1`
- **预期**：
  - pod A：本地剩余 = 0，虚拟剩余 0 → Pending
  - pod B：nodeFreeNPU[local] = 8-4 = 4（物理剩余，不受 Queue 影响）→ 正常调度
- **验证点**：pod B 正常调度到本地节点，不受 namespace1 Queue 限额影响

### TC-14: namespace1 物理占满本地 → namespace2 也排队
- **前置条件**：
  - namespace1 有 workflow pod 占满本地 8 张卡
  - namespace2 无 NamespaceOffloading
- **Pod 配置**：namespace2 的 runner pod `required-npu-count=1`
- **预期**：nodeFreeNPU[local] = 8-8 = 0 → hasCandidate = false → Pending
- **验证点**：pod B 保持 Pending（物理资源确实耗尽）

### TC-15: FIFO 不跨 namespace 阻塞
- **前置条件**：
  - namespace1 和 namespace2 用相同 NPU 类型（`huawei.com/ascend-1980`）
  - namespace1 的 runner pod（T1 创建）Pending（本地和虚拟都满了）
  - namespace2 的 runner pod（T2 > T1 创建）有充足本地资源
- **Pod 配置**：namespace2 的 runner pod
- **预期**：FIFO 检查跳过 namespace1 的 pod（不同 namespace）→ 通过
- **验证点**：namespace2 的 pod 正常调度，不被 namespace1 的 Pending pod 阻塞

---

## 五、FIFO + nodeSelector 交互

### TC-16: 同 namespace，runner1 限定虚拟节点（满），runner2 可用本地 → 不阻塞
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - runner1（T1）：`nodeSelector: {liqo.io/remote-cluster-id: cn12-001}`，cn12-001 虚拟节点满 → Pending
  - runner2（T2 > T1）：无 nodeSelector，本地有 4 张空闲
- **Pod 配置**：runner2
- **预期**：
  - FIFO 检查发现 runner1，但 runner1 有 nodeSelector `{liqo.io/remote-cluster-id: cn12-001}`
  - runner2 的 nodeSelector 无此 key → `hasUnsharedConstraint = true` → 跳过 runner1
  - FIFO 通过 → runner2 调度到本地
- **验证点**：runner2 正常调度，不被 runner1 的 head-of-line blocking 影响

### TC-17: 同 namespace，两个 runner 无 nodeSelector → 正常 FIFO
- **前置条件**：
  - namespace1，runner1（T1）和 runner2（T2），都无 nodeSelector
  - 本地资源只够一个（如本地剩余 1，各需 1 张）
- **Pod 配置**：runner2
- **预期**：FIFO 发现 runner1 更老且无 unshared constraint → 阻塞 runner2
- **验证点**：runner2 保持 Pending，等 runner1 先调度

### TC-18: 同 namespace，两个 runner nodeSelector 相同 → 正常 FIFO
- **前置条件**：
  - namespace1，runner1（T1）和 runner2（T2）
  - 两者都有 `nodeSelector: {liqo.io/remote-cluster-id: cn12-001}`
  - cn12-001 虚拟节点只够一个（剩余 1，各需 1 张）
- **Pod 配置**：runner2
- **预期**：nodeSelector 相同 → 无 unshared constraint → FIFO 阻塞 runner2
- **验证点**：runner2 等 runner1 先调度

### TC-19: 同 namespace，runner1 限定 cn12-001，runner2 限定另一虚拟节点 → 不互相阻塞
- **前置条件**：
  - namespace1，runner1（T1）`nodeSelector: {liqo.io/remote-cluster-id: cn12-001}`
  - runner2（T2）`nodeSelector: {liqo.io/remote-cluster-id: <其他虚拟节点>}`
  - cn12-001 满（20 张全占用），另一个虚拟节点有资源
- **Pod 配置**：runner2
- **预期**：runner1 的 nodeSelector 值 `cn12-001` ≠ runner2 的值 → unshared constraint → 跳过 runner1
- **验证点**：runner2 正常调度到另一个虚拟节点
- **注意**：当前环境只有一个虚拟节点 cn12-001，此用例需要新增第二个虚拟节点后才能测试

---

## 六、虚拟节点 NPU 占用计算

### TC-20: 虚拟节点占用通过 runner pod 标签累加
- **前置条件**：
  - 虚拟节点 20 张卡
  - 虚拟节点上有 3 个 runner pod：required-npu-count 分别为 4、8、2
- **预期**：calcVirtualNodeOccupied = 14，虚拟节点剩余 = 20-14 = 6
- **验证点**：新 runner pod `required-npu-count=8` 时，虚拟节点剩余 6 < 8，不会调度到虚拟节点

### TC-21: 虚拟节点占用只算匹配型号的 runner pod
- **前置条件**：
  - 虚拟节点 20 张卡
  - runner pod A：`ResourceDomain=huawei.com, ResourceModel=ascend-310, required-npu-count=4`
  - runner pod B：`ResourceDomain=huawei.com, ResourceModel=ascend-910, required-npu-count=8`
- **Pod 配置**：runner pod `ResourceModel=ascend-310`
- **预期**：只算 A（ascend-310），占用 = 4，不算 B（ascend-910）
- **验证点**：虚拟节点剩余 = 20-4 = 16

### TC-22: in-flight 预留不叠加到虚拟节点（避免双重计数）
- **前置条件**：
  - namespace1 有 NamespaceOffloading
  - 虚拟节点 20 张卡
  - namespace1 的 runner pod（required-npu-count=8）已绑定到虚拟节点
  - runner pod 的 in-flight 预留仍存在（PostBind 不清除 runner pod 预留）
- **Pod 配置**：namespace1 的新 runner pod
- **预期**：
  - calcVirtualNodeOccupied = 8（runner pod 在 snapshot 中）
  - in-flight 预留：virtualNodes[virtNodeName] = true → 跳过（不加）
  - 虚拟节点总占用 = 8（不是 16），剩余 = 12
- **验证点**：虚拟节点剩余 12，不是 4（如果没有跳过预留，剩余会是 20-8-8=4）

---

## 七、降级与容错

### TC-23: Liqo CRD 未安装 → 静默降级
- **前置条件**：Liqo CRD（`offloading.liqo.io`）未安装，集群有虚拟节点（手动创建的带 liqo 标签的 Node，20 张卡）
- **Pod 配置**：runner pod
- **预期**：
  - crdExists 返回 false → nsOffloadingLister 为 nil
  - PreFilter 中 `pl.nsOffloadingLister == nil` → else 分支 → 删除虚拟节点
  - 无错误日志，无 informer 重试日志
- **验证点**：pod 正常调度到本地节点，日志无噪声

### TC-24: Volcano CRD 未安装 → 静默降级
- **前置条件**：Volcano CRD（`scheduling.volcano.sh`）未安装
- **Pod 配置**：runner pod，namespace 有 NamespaceOffloading
- **预期**：
  - queueLister 为 nil
  - applyLiqoComparison 中 `pl.nsLister != nil` 但 getQueueNpuLimit 中 `qLister == nil` → 返回 false
  - 本地总量 = 实际总卡数 8（不受 Queue 限额影响）
- **验证点**：liqo 比对正常执行，只是没有 Queue cap

### TC-25: NamespaceOffloading 的 clusterSelector 未匹配任何虚拟节点
- **前置条件**：
  - namespace1 有 NamespaceOffloading，clusterSelector 匹配 `liqo.io/remote-cluster-id: nonexistent`
  - 集群中无匹配的虚拟节点（只有 cn12-001、gy006）
- **Pod 配置**：namespace1 的 runner pod
- **预期**：
  - getEligibleVirtualNodes 返回空集合
  - applyLiqoComparison 直接 return（不修改 nodeFreeNPU）
  - 本地和虚拟节点都在 nodeFreeNPU 中
  - hasCandidate 检查所有节点
- **验证点**：pod 可调度到本地或虚拟节点（未执行 liqo 比对）

### TC-26: 动态 informer 缓存未同步时调度
- **前置条件**：
  - 调度器刚启动，informer 缓存尚未同步
  - 集群有虚拟节点（20 张卡）和 NamespaceOffloading CR
- **Pod 配置**：runner pod
- **预期**：
  - nsOffloadingLister.Get 返回 false（缓存未同步）
  - 走 else 分支：删除虚拟节点
  - pod 只在本地节点调度
  - 缓存同步后，后续 pod 正常执行 liqo 比对
- **验证点**：启动初期 pod 调度到本地节点，无崩溃

---

## 八、多虚拟节点场景

> **注意**：当前环境只有一个虚拟节点 cn12-001，以下用例需要新增第二个虚拟节点后才能测试。

### TC-27: 多个虚拟节点，选剩余最多的
- **前置条件**：
  - namespace1 有 NamespaceOffloading（clusterSelector 匹配所有虚拟节点）
  - 虚拟节点 cn12-001：20 张卡，占用 16 → 剩余 4
  - 虚拟节点 cn12-002：20 张卡，占用 6 → 剩余 14
  - 本地剩余 6
- **Pod 配置**：runner pod `required-npu-count=1`
- **预期**：
  - bestVirtRemaining = 14（cn12-002）
  - localRemaining 6 < 14 → cn12-002 胜出
  - nodeFreeNPU 只保留 cn12-002
- **验证点**：pod 绑定到 cn12-002

### TC-28: clusterSelector 过滤部分虚拟节点
- **前置条件**：
  - namespace1 有 NamespaceOffloading，clusterSelector: `{matchLabels: {liqo.io/remote-cluster-id: cn12-001}}`
  - 虚拟节点 cn12-001：剩余 4
  - 虚拟节点 cn12-002：剩余 14（但不匹配 clusterSelector）
  - 本地剩余 6
- **Pod 配置**：runner pod
- **预期**：
  - eligibleVirtuals = {cn12-001}（cn12-002 不匹配 clusterSelector）
  - bestVirtRemaining = 4（cn12-001）
  - localRemaining 6 >= 4 → 本地胜出
  - cn12-001 和 cn12-002 都从 nodeFreeNPU 删除
- **验证点**：pod 绑定到本地节点，不绑定到 cn12-001 或 cn12-002

---

## 九、并发与预留

### TC-29: 多个 runner pod 并发调度，in-flight 预留防止超卖
- **前置条件**：
  - 本地节点 8 张卡，0 占用
  - 5 个 runner pod 并发提交，各 `required-npu-count=2`
- **预期**：
  - 第 1 个 pod：Reserve 预留 2 张 → 本地剩余 6
  - 第 2 个 pod：Reserve 预留 2 张 → 本地剩余 4
  - 第 3 个 pod：Reserve 预留 2 张 → 本地剩余 2
  - 第 4 个 pod：Reserve 预留 2 张 → 本地剩余 0
  - 第 5 个 pod：本地剩余 0 < 2 → Unschedulable
- **验证点**：只有 4 个 pod 调度成功，第 5 个 Pending

### TC-30: runner pod 绑定后预留保留，workflow pod 启动后预留清除
- **前置条件**：
  - namespace1，runner pod `required-npu-count=2` 调度到本地节点
  - runner pod 绑定后 PostBind 保留预留（runner pod 不请求 NPU）
  - 随后 workflow pod 启动（容器 Requests[NPU]=2）
- **预期**：
  - runner pod 绑定后：预留仍占用 2 张卡
  - workflow pod 在 snapshot 中出现后：activeWorkflows[baseName] = true → 预留被跳过
  - workflow pod 绑定后：PostBind 清除 workflow pod 预留
  - 物理占用接管（workflow pod 的 AllocatedNPUCount 或容器 Requests 计入 physUsage）
- **验证点**：NPU 占用从预留平滑过渡到物理占用，无双重计数

---

## 十、边界值

### TC-31: 本地总剩余为负（Queue 限额 < 实际占用）→ clamp 到 0
- **前置条件**：
  - namespace1 Queue 限额 4
  - 本地实际占用 6（超过 Queue 限额，可能是 Queue 配置前已有占用）
  - 虚拟节点剩余 5
- **Pod 配置**：runner pod
- **预期**：
  - localTotalCapacity = min(4, 8) = 4
  - localRemaining = 4 - 6 = -2 → clamp 到 0
  - 0 < 5 → 虚拟节点胜出
- **验证点**：pod 调度到虚拟节点

### TC-32: required-npu-count 超过任何单节点容量 → Pending
- **前置条件**：
  - 本地节点单节点最大 8 张卡
  - 虚拟节点最大 20 张卡
  - namespace1 有 NamespaceOffloading
- **Pod 配置**：runner pod `required-npu-count=21`
- **预期**：虚拟节点最大 20 < 21，本地最大 8 < 21 → hasCandidate = false → Pending
- **验证点**：pod 保持 Pending

### TC-33: 虚拟节点 Allocatable 中无 NPU 资源
- **前置条件**：
  - 虚拟节点通过 ResourceSlice 协商了 CPU/memory 但未协商 NPU
  - `node.Status.Allocatable[huawei.com/ascend-1980]` 不存在 → 返回 0
- **Pod 配置**：runner pod `required-npu-count=1`
- **预期**：虚拟节点 free = 0 - occupied → 负数，不会胜出
- **验证点**：pod 调度到本地节点（如果本地有资源）
