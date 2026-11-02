# v1.38～v1.40 新功能复核（2026-11-02）

基线：[v1.40.0 / main](https://github.com/tjbdwanghaibo/roost-core/commit/2be1f1e5)，比较起点为上一轮 [v1.37.1](https://github.com/tjbdwanghaibo/roost-core/commit/773ca443)。新增 9 个提交、213 个变更文件，多数行数来自生成物、金样及性能证据。

**本轮选定范围内未确认新增缺陷，设计方向与已执行回归相符。** 没有据此宣布全仓无问题或生产上线验收完成。上一轮 [Linux 验收报告](LINUX-FUNCTIONAL-20261102.md) 的失败与环境缺口不因本次功能回归通过自动关闭。

## 核对结果

| 功能 | 复核重点与结论 |
| --- | --- |
| v1.38 递归 DAO Sync | setter 记录 Field／Key／Index 路径，完整替换覆盖旧子路径；捕获版本与成功确认分离，旧确认不应清掉后续修改。Go Apply 先构造结果再发布，列表补洞／索引检查和嵌套基线验证有对应回归。默认 self_sync 接入追踪，自定义 packer 仍须遵守 Entity 锁与确认契约。相关 race 及连续事务对照通过。 |
| v1.38 Remote View／请求 DTO | 视图只含显式 remoteview 顶层字段，嵌套投影深拷贝；scope 和 schema 区分投影与形状。共享 DTO 生成预取声明与访问器，删除声明有生成物退出测试；没有把声明预取解释为业务授权。跨包生成消费者及对应生成器 race 通过，不等于真实跨机授权集成验收。 |
| v1.38 Controller endpoint | 只更新专属生成文件，保留手写入口；按 import 路径核对 PB 请求／响应类型，nil 响应转换为错误，业务鉴权仍在手写层。Endpoint／SelfSync 等选定 roost 回归通过。 |
| v1.39 递归 BSON 持久补丁 | 持久路径与 Sync 确认独立；父替换、空容器、不安全 map key、密集变化及预算回退完整字段。提交前冻结 BSON，重复／父子重叠路径在 WAL 准入前拒绝。生成 DAO 的真实 Mongo、WAL 重开／重复投影、事务回滚、冻结值不变验证通过。 |
| v1.40 store／FullMask | 缺失、重复、非法键、旧 bson 定义与多字段共声明有生成期拒绝；与上次生成物比较键及选项变化。标量完整替换改用 FullMask，递归路径保留独立记录；真实数字键／嵌套／冷加载回归及相关 race 通过。没有重跑性能，不能独立背书 TPS 提升。 |

## 本机执行证据

Linux x86_64，Go 1.27.2，GOMAXPROCS=4；源码在独立已有检出中更新，保留原审查探针，未修改 main 运行时。代码图谱工具不可用，按实际 diff 与精确源码核对。

1. `go test -race ./framework/dataengine/... ./framework/entity ./framework/nest ./codegen/internal/dao ./codegen/internal/entity ./codegen/internal/nest ./codegen/internal/marker -count=1`：8 包全部通过，见 [日志](evidence/features-20261102/targeted.log)。
2. `go test -race ./codegen/internal/roost -run 'Endpoint|SelfSync|GeneratedRemote' -count=1`：通过，见 [接线日志](evidence/features-20261102/wiring.log)。该选定表达式的成功不是整个 roost 包全部用例验收。
3. `ROOST_CORE_REPLACE=<当前检出> bash codegen/scripts/dao-golden-runtime.sh`：在独立 Mongo 7.0.45 三节点副本集 URI 下运行，vet 和生成 DAO runtime 通过，见 [日志](evidence/features-20261102/golden.log)。没有设置性能开关；套件内 helper／性能及契约排除项仍可能跳过，不声明整个套件零跳过。
4. 对 `TestPersistPathsRealMongo`、`TestPersistPathsNullContainersRealMongo`、`TestNumericStorageKeysRealMongo` 再作明确的 `-race -count=1 -v` 验证：三个顶层用例及 15 个路径子场景通过、无 SKIP，见 [原始日志](evidence/features-20261102/real-paths.log)。同机功能测试，不代替主机隔离与存储断电。
5. 新增独立审查探针：固定 30 个种子 × 20 批事务 × 每批 5 次修改，混合 map 更新／删除、列表追加／索引替换／整体替换、嵌套对象更新；每批应用增量后与权威全量编码逐字节对比，再确认并清理变化。600 批均通过 race，见 [日志](evidence/features-20261102/sequences.log)。这是有界功能属性检查，不是压测，也未覆盖所有随机操作组合。

探针不安装到运行时，可在相同版本工具链下重跑：

```bash
python3 docs/maintenance/evidence/features-20261102/run-sequences.py /path/to/reviewed-core
```

[探针源码](evidence/features-20261102/review_sequences_test.go.txt)随报告保存；目标检出必须包含该版生成 goldens/runtime，不能对本固定审核分支的旧运行时代码直接套用新 API。

## 升级条件及未验证部分

- v1.38 将 DAO Sync 改为格式 3；新服务端、生成 proto、C# apply／C++ SDK 必须配套更新，旧连接关闭后取 Full。不能把外层 EntitySync 编码不变理解为客户端兼容。
- v1.40 补 `store` 声明时，按旧生成物保留 BSON 物理键和选项。主动换为数字短键仍需业务 schema 与离线迁移；生成期 WARN 不是自动迁移。新 API 也要求生成器与 core 同版。
- 本轮对 C#/C++ 递归 Apply 做了源码检查，没有在 Linux 重新构建完整客户端：当前没有可用的 protoc／protobuf C++ 开发配置，完整 .NET SDK 环境亦存在上一轮记录的首次迁移阻塞。没有运行 Unity Editor、Mono/IL2CPP 或移动设备，不能引用 main 的其他平台结果作为本机实测。
- 没有重跑全仓、13 类游戏、完整故障矩阵、生产 WAN 或性能长稳。旧 Linux 报告的 Lockstep／etcd 不稳定失败、NATS／SLG 夹具缺口，需要在各自修复基线单独关闭。

本轮固定分支只提交审查记录与可复跑探针；不修改生产实现，不提交 issue/PR。
