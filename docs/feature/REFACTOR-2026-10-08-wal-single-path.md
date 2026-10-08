# WAL 单格式与单提交路径收敛

基线 `6907150e`；维护者已授权实施。只保证 macOS/Linux，性能验证继续暂停，不部署、不自动发版。

## 原因与目标

历史 PlayerOwners 动态租约已被静态绑定取代，不重开迁移机制。当前可移除的是 WAL V1/V2 灰度兼容、Mutation 旧字段转换和无仓内生产入口的 nestwal.Committer/Runtime。CBM 代际 2026-10-08T05:39:49Z 定位，目标源码 coverage 无记录缺口；图谱不证明仓外无调用。

目录保持 nestwal / dataengine / dataengine/engine / kit/dataengine，不新建包。nestwal 负责文件、校验、刷盘、checkpoint；engine.Projector 为正式提交和投影路径。JetStreamEffectPublisher 仍供正式 outbox 使用，不能随旧 Committer 一并删除；旧 Committer 专用的 EffectPublisher/EffectPublishFunc 抽象一并移除。

## 实施顺序及接口变化

1. 更新 roost-coding 规则源及 roost-optimize 入口，镜像本地 Codex skill。
2. WAL codec 固定为 7（替代 5/6），移除 WriterVersion 与 writer_version 配置、旧编码/解码；旧文件明确拒绝且保留原件。Mutation 只保留 Key/Kind/ExpectedVersion/NextVersion，移除旧 EntityID/Database/DatabaseScope/Resource/Version 字段及转换逻辑；调用方、模板和测试使用完整规范字段。
3. 删除独立 Committer/Runtime，保留正式 JetStream effect 发布器。唯一 Nest 外部回归调用改用 engine.Projector；对旧 Committer 专属测试逐项核对 engine 的 held、取消、排空、重放守卫，补必要行为覆盖，不用测试替身重建旧框架。
4. 按影响执行普通/race、全仓 build/vet/test、go generate、正式生成 game-demo 验证。记录实际执行结果，不重跑负载。

历史 NEST_TRANSACTION_WAL.md 将独立 Committer/Runtime 归为 C8 保留 API；本次维护者“按建议实施”授权明确收敛这组重复路径，取代该组旧决定，不改变其他 C8 API 的去留。

## 不变量与升级

保留 writer.lock、fsync/group commit、连续成功前缀 checkpoint、版本 CAS、冷加载投影屏障、Saga lease fence/驱逐与 Remote 持久发布契约。重命名或去兼容不能移除这些正确性保证。

破坏性 API/格式变化：旧生成工程须按当前模板重新生成并编译；旧 codec 5/6 WAL 不能直接供新程序消费。升级先停旧进程并以旧版本完成需要保留数据的投影；旧数据处理由维护者决定，新程序使用新 WAL 目录。不得自动删旧 WAL 或跳坏记录。回退代码须配套旧格式目录，不能让旧程序消费 codec 7。

## 验证记录

已完成本轮实施与非压测验收：macOS 全仓 build/vet/test（131 包）、七个受影响包 race、正式生成 game-demo（19 包）及两套资源夹具生成编译；Linux/arm64 交叉构建通过。详细命令/日志/失败保留与未验边界见[实现记录](../release/WAL-SINGLE-PATH-2026-10-08-IMPLEMENTATION.md)。没有性能收益结论或新的部署验收。
