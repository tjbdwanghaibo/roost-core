# roost-core main 续审 · 2026-11-21

远端 main 为 `108ff00b8b19737d880c023ddd9435b4a92ccdb3`，自上轮审核以来新增 0 个提交。上轮待修的独立 Gate 断线问题仍可复现。本轮继续检查分区 AOI 的跨区移动，新确认一项 P2，在 v1.59.0（`605bdba165b10d4fe591a2b3e55266e4d867e84f`）也可复现。[上轮报告](MAIN-REVIEW-20261120-02.md) · [复跑说明](evidence/main-review-20261121/README.md)

## RV-20261121-01 · P2 · 已可见目标跨区落在两个半径之间，会提前离开视野

`policy.AOICluster` 有两个距离门槛。目标进入半径 8 以内后可见，随后只要距离不超过离开半径 12，就应继续可见。本例只放一个观察者和一个目标，关闭可见数量上限。观察者固定在 x = −7，双方 y = 0。在相同坐标上分别运行单区和以 x = 0 为界的双区地图，每次移动后读取 `Visible` 和 `Flush`。[半径契约](https://github.com/tjbdwanghaibo/roost-core/blob/108ff00b8b19737d880c023ddd9435b4a92ccdb3/framework/sync/entitysync/policy/aoi.go#L56) · [探针](evidence/main-review-20261121/seam.go.txt)

| 目标动作 | 距离 | 单区 | 双区 |
| --- | ---: | --- | --- |
| 首次出现在 x = 2 | 9 | 不可见，无事件 | 相同 |
| 移到 x = −1 | 6 | 可见，Enter | 相同 |
| 跨到 x = 1，再回 x = −1 | 8 → 6 | 保持可见，无事件 | 相同 |
| 从 x = −1 跨到 x = 2 | 9 | 保持可见，无事件 | **不可见，Leave** |
| 回到 x = 1 | 8 | 保持可见，无事件 | **重新可见，Enter** |
| 移到 x = 5，再到 x = 6 | 12 → 13 | 距离 12 仍可见，13 才 Leave | 相同 |

跨区时，`moveSubjectLocked` 在目标区域调用 `AddSubject`，随后从原区域移除目标。目标区域按本区的历史判断这对观察者和目标是否已可见，首次相遇时使用进入半径。距离 9 超过 8，因此目标区域没有产生 Enter。原区域移除目标产生 Leave，集群合并两个区域的事件后，从公开可见集合移除了目标。订阅方此时收到一次提前离开，目标回到距离 8 时又收到一次进入。[跨区移动](https://github.com/tjbdwanghaibo/roost-core/blob/108ff00b8b19737d880c023ddd9435b4a92ccdb3/framework/sync/entitysync/policy/aoi_cluster.go#L199) · [区域内的距离判定](https://github.com/tjbdwanghaibo/roost-core/blob/108ff00b8b19737d880c023ddd9435b4a92ccdb3/framework/sync/entitysync/policy/aoi.go#L889) · [集群合并事件](https://github.com/tjbdwanghaibo/roost-core/blob/108ff00b8b19737d880c023ddd9435b4a92ccdb3/framework/sync/entitysync/policy/aoi_cluster.go#L386)

main 的 block、crosslist、kdtree 三种索引，以及 v1.59.0 的默认 block 索引，各跑三轮，结果一致。main 现有 policy 包测试也通过三轮。已有跨区测试覆盖进入半径内的往返，上轮探针比较同一布局中各索引的输出，两者都无法区分这次轨迹上的单区与双区结果。修复验收可直接要求上表两种布局的可见集合和事件逐步一致，同时保留首次出现在距离 9 时不可见的对照。[main 输出](evidence/main-review-20261121/main.log) · [v1.59.0 输出](evidence/main-review-20261121/tag.log) · [包测试](evidence/main-review-20261121/upstream.log) · [现有跨区测试](https://github.com/tjbdwanghaibo/roost-core/blob/108ff00b8b19737d880c023ddd9435b4a92ccdb3/framework/sync/entitysync/policy/aoi_test.go#L219)

## RV-20261120-03 · P2 · 独立 Gate 的旧视野仍未释放

本轮复跑上轮生成工程的原场景。连接 A 是当前进图并 Ready 的连接，B 属于同一玩家、已认证但未进图。独立 Gate 下关闭 A 后，补查循环运行 2.2 秒，旧成员仍有 1 个，Manager 会话仍为 Ready，随后 Flush 产生 1 帧按玩家 ID 发送的同步数据。独立 Gate 关闭最后一条连接，以及嵌入 TCP 下保留 B 的对照，均释放成员并产生 0 帧。[本轮输出](evidence/main-review-20261121/gate.log) · [原场景、源码与验收条件](MAIN-REVIEW-20261120-02.md)

## 验证边界

AOI 证据覆盖单进程公开策略 API 的可见集合和增量事件。Gate 使用同版生成工程的场景清理和 Manager，连接表与帧传输使用测试替身。本轮未测真实多进程 Gate、客户端实际收包或画面表现，也未运行全仓回归和容量压测。
