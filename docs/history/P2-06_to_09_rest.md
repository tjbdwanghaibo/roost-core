# P2-⑥～⑨：网络同步、actionflow / ai、基础包与 skill 并入 core

批次：P2-⑥ nettransport → room / lockstep / spatial；P2-⑦ actionflow / ai；P2-⑧ gateway / versionstore / servicerpc / robot / syncstream；P2-⑨ roost-skill 整仓
状态：全部完成（core 侧）；kit 侧旧路径保留到 P3
负责人、工作树：本机，`roost-core` 分支 `consolidation`（HEAD `1d8a872`）

## ⑥ nettransport / spatial / lockstep / room

- nettransport、spatial：无依赖、无冲突，整包搬入（`merge_kit_pkg.py <pkg>`）。
- lockstep：kit 侧只有 `room.go`（房间层）；它以**无别名**方式 import core/lockstep，脚本增加"无别名自身包 import"处理后合入 `core/lockstep`；`kit/nettransport` → `core/nettransport`。
- room：广播 / JetStream 与 NATS 同步总线 / 房间管理 / 传输汇合入 `core/room`；`room_mod.go`、`room_mod_stop_test.go` 留 kit。

## ⑦ actionflow / ai

别名 `coreflow` / `coreai` 去掉后合入同名包，无文件名或标识符冲突。U-0077 / U-0083 / U-0100 随文件通过。

## ⑧ gateway / versionstore / servicerpc / robot / syncstream / statslog / configdata

- gateway：中间件合入 `core/gateway`（别名 `coregateway`）。
- versionstore、servicerpc：整包搬入（service 的直接依赖，先于 P3 就位）。
- robot：kit 的拨号器与 lockstep 机器人合入 `core/robot`；外部测试包 `robot_test` 的 `roost-kit/robot` import 改 core；`robot_test.go` 改名 `robot_impl_test.go`。
- syncstream：kit 的发布器 / 缓冲发布器与 core 的历史 / 观测**有语义不同的同名导出**：`HealthOptions`、`HealthStatus`（kit 描述发布器与缓冲区，core 描述历史）——kit 侧改名 `PublisherHealthOptions` / `PublisherHealthStatus`；`MetricSink` 接口完全相同，删 kit 定义；`ErrPayloadTooLarge` 两处同义（文案 "syncstream adapter:" vs "syncstream:"），删 kit 定义用 core 的。这些改名写进映射表 `renames`。**教训**：P0 的冲突检查只看 `^(func|type|var|const) Name`，漏掉 `var (` 块内的声明；编译器兜住了。
- statslog、configdata：各只有一个文件，Mod 与实现（stats providers / 项目配置→Core 配置转换）写在一起，按定义就是接入层——**改为 keep，留 kit**，映射表已改。

## ⑨ roost-skill 整仓并入 core/skill

- `git subtree add --prefix=skill <roost-skill> main`（整仓历史保留），然后：`skill/skill/*` 上提到 `skill/`（包 `skill` 在 `core/skill`），`combat` / `combatcomponent` / `skillcompose` / `skillsync` 成为 `core/skill/<名>`；删除 `.github`、`scripts`、`go.mod`、`go.sum`、`.gitignore`、`LICENSE`；`docs/*` → `core/docs/skill/`，`CHANGELOG.md` → `core/docs/history/SKILL_CHANGELOG.md`；`README.md` 留在 `skill/`。
- import 改写 28 个文件（`roost-skill/<pkg>` → `roost-core/skill[/<pkg>]`，sync-e2e 的 `roost-kit/syncstream` → `roost-core/syncstream`）；文档与注释里的旧路径同步改写（10 个文件）。
- 嵌套模块：`skill/examples`（模块路径 `roost-core/skill/examples`，`replace roost-core => ../..`）、`skill/integration/sync-e2e`（`roost-core/skill/integration/sync-e2e`，`replace => ../../..`，不再依赖 kit）。两者 `GOWORK=off go mod tidy && go build/test` 通过。
- `prompt_test.go` 读提示词文档的相对路径改为 `../docs/skill/`。
- mongo-driver 依赖（combatcomponent）core 已有。

## 公共 API 变化（本组）

- 路径：`roost-kit/{nettransport,spatial,room,lockstep,actionflow,ai,gateway,versionstore,servicerpc,robot,syncstream}` → `roost-core/<同名>`；`roost-skill/*` → `roost-core/skill/*`。
- 符号：`kit/syncstream.HealthOptions/HealthStatus` → `core/syncstream.PublisherHealthOptions/PublisherHealthStatus`；`kit/syncstream.MetricSink/ErrPayloadTooLarge` → core 同名。
- 留 kit：`NewRoomMod/RoomMod`、`statslog.*`、`configdata.*`。

## 命令与结果

每包：build / vet / vet -tags integration / 包测试 + 根模块边界测试全绿。全仓 `go vet ./... && glsvet && go test -count=1 ./...` 在 ⑥ 后与 ⑦⑧ 后各跑一次，无 FAIL；⑨ 后再跑一次（后台）。

## P2 汇总

kit 的 27 个包里：18 个实现已下沉 core（其中 dataengine 落 `dataengine/engine`），7 个保留 kit（mods、manager、ops、nest、lock、configdata、statslog），mongotest 随 mongo。留在 kit 的还有各包的 Mod 胶水与 7 个 Mod 级测试（nats 的 JetStream RPC 故障测试、remoteentity 的 sid 测试与真实 Mongo 提交器集成、dataengine 的 fatal_fence / real_fixture / real / failover / toxic）。core go.mod 新增 nats.go、go-redis、etcd、quic-go、kcp-go。

## 未验证项 / 风险

- 真实环境集成（Mongo / NATS / Redis）在 core 侧没有覆盖，全部留在 kit 的 Mod 级测试里；P3 决定夹具去向。
- 迁移后性能对比待 P5（迁移前基线还在跑）。
- `_impl` 后缀文件名共 20 余个，P3 后可择机改名。

## 下一步唯一动作

P3：kit 瘦身 + roost-service 并入。
