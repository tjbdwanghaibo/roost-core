# 地形近期提交审查（2026-11-10）

本报告登记上一轮已确认问题，供实施方修复。被审查代码为 main `8f241247`，范围是 `23247c38..8f241247` 的地形副本、移动、编辑账本、类型同步与定时器投递相关改动；并非对所有新增功能和所有部署组合的完整验收。报告保存在固定审查分支，运行验证在独立 main 检出进行，审查分支自己的旧运行时代码不代表被测代码。

图谱工具不可用，使用精确源码、定向探针和相关包回归。未修改生产代码，未新建 issue / PR，未运行压测。用户确认之前的问题尚未修复；下列条目保持打开。

## RV-20261110-01：超长地形订阅无法被自己的接收端解码（P2，已复现，未修复）

位置：[replica.go 的 subscribe](https://github.com/tjbdwanghaibo/roost-core/blob/8f241247/gameplay/terrain/replica.go#L385)、[接收端长度检查](https://github.com/tjbdwanghaibo/roost-core/blob/8f241247/gameplay/terrain/replica.go#L758)。引入提交：`e2981073`。

`Want` 接受地图内的块列表，并按拥有者分组。`subscribe` 将一个拥有者的整组 keys 编成单条消息，不校验条数、不分片；但 `decodeReplicaMsg` 拒绝超过 `replicaMaxKeys = 32768` 的订阅。地图允许每边至多 1024 块，合法配置可以超过这个限制。

触发：一个 channel 对同一个 owner 想要 32769 块。整份订阅被接收端拒绝；后续续订每次仍是同一份超长消息，无法自行补齐。初次订阅不能建立，已有订阅扩张超过上限时旧订阅最终到期。默认 48×48 块单格配置不会触发，不能把这个问题描述成当前默认部署必现。

复现使用有效的 256×256 块地图，构造发送方 Replicator，经 `Want` / `takeInput` 捕获真实编码消息，再交给同包真实解码器。32768 块对照通过；32769 块连续三轮报：

```text
terrain: malformed replica message: 32769 keys
```

证据：[探针](evidence/terrain-review-20261110/subscription_probe_test.go.txt)、[三轮失败原文](evidence/terrain-review-20261110/subscription-red.log)。这是协议编码/解码回归，不是跨机或容量测试。

复跑：在 main `8f241247` 的独立检出中，将探针复制为 `gameplay/terrain/review_1110_test.go`，执行：

```sh
go test ./gameplay/terrain -run TestReviewLargeSubscription -count=3
```

建议：在发送侧明确校验可支持的订阅上限并给调用方可处理的错误，或设计带批次身份、完整性与乱序处理的分片订阅。不能只把 keys 切成多份现有消息：`answer` 每次以该包覆盖 `(sid, channel)` 的整份订阅，简单拆包只会留下最后一片。

关闭判据：阈值前后均有明确行为；合法订阅不能静默进入永远解码失败的重试；若采用分片，覆盖缺片、乱序、续期、撤销和 owner 重启；原来的小范围订阅恢复行为保持。

## RV-20261110-02：合入后的进度文档仍有互相冲突的状态（P3，文档问题，未修复）

- [TERRAIN-STATUS](https://github.com/tjbdwanghaibo/roost-core/blob/8f241247/docs/maintenance/TERRAIN-STATUS.md)：T01/T02 明确写已修复，但“P2 单格编辑”行仍写存在两项缺陷；页首与演练行仍写“分支未合入”，实际上演练已包含在被审查 main。
- [KNOWN-LIMITS](https://github.com/tjbdwanghaibo/roost-core/blob/8f241247/docs/maintenance/KNOWN-LIMITS.md)：部分顶部条目仍称原两项去重与 P3 未完成，未与当前状态表同步。

影响：接入方无法据文档区分已完成、当前缺陷和历史验证边界。修复应更新当前汇总与链接；历史证据保留当时基线，不重写历史测试结果。此项不代表新发现运行时缺陷，也不否认 T01/T02 的已登记修复。

## 上一轮实际验证

Linux、Go 1.27.2，main `8f241247`：

```sh
go test -race ./gameplay/terrain ./gameplay/movement ./framework/nest/entitytimer ./framework/sync/entitysync -count=1
go test -race ./gameplay/cellworld -count=1
```

五包通过，分别见 [四包日志](evidence/terrain-review-20261110/packages-race.log) 与 [cellworld 日志](evidence/terrain-review-20261110/cellworld-race.log)。正常回归未包含后来新增、预期失败的审查探针；不能把探针失败隐去并称“全部测试通过”。

未执行：最新生成 demo 的真实 Mongo/WAL 验收、Unity、跨机与容量验收。随后 main 更新到 `3d1ec2b5` 的建筑/区域水位等逻辑属于继续审查范围，本报告不把尚未汇总的新结论混入上一轮结果。
