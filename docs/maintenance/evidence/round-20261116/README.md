# 2026-11-16 整理轮：阶段 7 全量与演练证据（2026-11-17）

本轮内容：单实例锁改存 etcd、`game/controllers` 合成一个包、组件包与 view 窄接口、组件钩子改为实体事件。方案与维护者决定见[本轮总览](../../FEATURE-2026-11-16-round-overview.md)。环境：本机 macOS arm64；真实依赖用长期集成环境 `~/.roost-it`（Mongo 副本集、NATS 集群、Redis、toxiproxy），etcd 由各演练自起。

## 全量（pretag）

pretag 跑的是 `scripts/pretag.sh` 的副本，只去掉了"发版清单版本号等于本次版本"这一条检查（这次不发版，清单仍是 v1.51.0），其余检查不变。

| 次 | 提交 | 结果 | 文件 |
|---|---|---|---|
| 1 | d58e81df | `go test ./...` 共 14 个用例失败，原因都是本轮引入、之前没跑全量。根因与修复见下表 | [pretag-run1-failures.txt](pretag-run1-failures.txt) |
| 2 | 25949fa9 | build、vet、tidy、`go generate` 无漂移、`go test ./...` **全部通过**；玩法与生成脚本 18 个通过，`test-modules-generated` 失败（见下表） | [pretag-run2.txt](pretag-run2.txt) |
| 2' | de9cde74 | 单独重跑 `test-modules-generated`：`modules: ok` | [test-modules-generated.txt](test-modules-generated.txt) |

合入 main 后，`scripts/release.sh` 在 main 上再完整跑一次 pretag，那一次才是发版的依据。

### 第 1、2 次失败的根因与修复

| 用例 / 脚本 | 根因 | 修复 |
|---|---|---|
| `TestFrameworkModsReadConfigOnlyThroughDeclarations` | etcd 版 `SingletonStore` 绕开配置声明，直接读 viper | c49a6bdc：按声明读，行为不变 |
| `TestFaultMatrixScriptNamesEveryFullEnvironmentSuite`、`TestRedisClusterScriptNamesEveryClusterKeyedSuite` | 故障矩阵脚本没跑新增的 `wiring/etcd` 集成测试；Redis 版锁删掉后，Redis Cluster 清单里的 `wiring/redis` 已没有用例 | 096ada68 |
| `TestModuleAddDeclaresTheFrameworkServicesItUses` 等 3 条 codegen 用例 | 期望过时：锁改要 etcd 段、不再要 redis 段（CHANGELOG 第一条）。"rank 不按键路由就不引入 etcd"这条本意保留，改为区分"锁用的配置段"和"路由用的 Mod" | 5b1c8a4e |
| notice 门禁（7/8） | 合包后 `Start` 经公共 Controller，测试缺 Nest 占位；缺 chat 时不再启动失败（与 README 不符）；notice+gm 的 4 条联动同样是红的，但没有门禁覆盖 | ea87061b；25949fa9 把 notice+gm 加进门禁（还原修复时失败：[notice-gm-red.txt](notice-gm-red.txt)） |
| friends 门禁（13/18） | 合包时漏搬 `friends_test.go`，5 条端点场景没跑 | 292bf01a：按原文恢复；其余模块核对过，没有同样的遗漏 |
| gm、profile+mailbox、task 门禁 | 期望过时：profile 场景 20 → 21（本轮新增"升级监听出错整笔回滚"） | c1100ea8 |
| team 门禁、turn-pvp 配方 | 变异锚点跟不上组件包与实体事件的写法（一条把代码改得编译不过，一条目标文件已移动） | 0957aae7：重定锚点，意图不变 |
| 入门文档用例 | 期望过时：handler 改收 `view.I<X>Entity`；文档还漏列了两个 view 文件 | 0957aae7 |
| `test-modules-generated` | 脚本里 profile 仍写 20；固定层清单没有 `game/event` | de9cde74 |

## 先失败后通过

| 项 | 修前（失败） | 修后（通过） |
|---|---|---|
| etcd 停机时进程退不出（d58e81df） | [singleton-drill-red.txt](singleton-drill-red.txt)：第 4 步 B 在 etcd 停后约 3s 判定丢锁，但卡在 etcd Mod 的撤销注册（拿到约 37s 停机预算），20s 内没有退出 | [singleton-drill-green.txt](singleton-drill-green.txt)：撤销最多等 5s，四步全部通过 |
| 监听出错时整笔回滚（c024b2fc） | [event-tx-red.txt](event-tx-red.txt)：把 `Publish` 改成吞掉监听错误，值是 1111，应为 10 | `TestEntityEventListenerFailureRollsBackTheWholeTransaction` 通过；`-race -count=3` 通过 |
| notice+gm 联动进门禁（25949fa9） | [notice-gm-red.txt](notice-gm-red.txt)：还原 ea87061b 的 with/gm 修复后，门禁报 `nest client is unavailable` | 通过 |
| rehome 演练插桩锚点（22715daa） | [alliance-e2e-anchor-drift-red.txt](alliance-e2e-anchor-drift-red.txt)：11-15 的 3a062e64 之后锚点失效，用例一条都没跑 | [alliance-e2e.txt](alliance-e2e.txt) |

## 真实进程演练

- **单实例锁 etcd 演练**（`scripts/testdata/singleton-etcd-drill/singleton-etcd-drill.sh`，第一次运行；33f1a5e4 先修了演练库）：
  1. 同 sid 两个进程，B 在启动时等待；
  2. A 被冻结后，B 在 15s 接管；
  3. A 恢复后 fail-stop；
  4. etcd 停 20s，B fail-stop；etcd 恢复 18s 后，同 sid 的进程重新拿到锁。

  四步全部通过。
- **Gate 演练**（`scripts/testdata/gate-drill/gate-drill.sh`）：account、game（Game Ingress）、gate 三个进程，经 Gate 完成登录、新建与进入：[gate-drill.txt](gate-drill.txt)。
- **alliance-e2e**（`scripts/test-alliance-e2e.sh`）：11 条全部通过，包括本轮简化后的 `TestAllianceE2ERedisOutage`（锁已不在 Redis 上）和 `TestAllianceE2ESocialRehome`：[alliance-e2e.txt](alliance-e2e.txt)。
- **生成工程 TCP 写入冒烟**（`scripts/perf/generated-tcp.sh`，按新布局改写后跑 5s、10 个连接）：通过，WAL 未确认为 0。只作功能冒烟，不是容量数据。

## 没有覆盖

- 跨机、Linux、长稳、容量。
- `scripts/test-demo-rehome.sh` 与本次 alliance-e2e 共用同一个插桩脚本，但本身这次没有跑。
