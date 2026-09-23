# M-14：ARCH-10 收尾——held/ready 会话、syncNamespace、`entitysync/policy`，demo 兴趣系统搬进 core

- 单元：M-14（架构重构，不占 U 编号，不计缺陷）· 承接 [M-13](M-13-entitysync-manager.md)，关闭 ARCH-10 剩余三点
- 仓库：roost-core `entitysync`、`entitysync/policy`（新）、`entity`、`codegen/internal/entity`、`demo` 模板
- 分支：`feature/arch-10-sync-policy`（维护者 review 后合入；M-13 已在 main）

## 一句话

分层收成四层，每层只知道下一层：

| 层 | 包 | 知道什么 | 不知道什么 |
| --- | --- | --- | --- |
| 内容 | `entity`（`SubjectSyncState`、`PrepareTick`） | 版本、脏位、packer、CommitLSN、Namespace | session、传输、谁在看 |
| 机制 | `entitysync`（`Manager`、subject、session、wire、`Transport`） | subject 的订阅者、会话的帧时钟、门槛、两种失败 | 为什么有人订阅（距离？队伍？房间？） |
| 组织 | `entitysync/policy`（`Interest`、`Group`、`Direct`、`RelationSource`） | 谁该订谁；把决定说给 Manager | 帧、线、传输、会话生命周期 |
| 应用 | demo `internal/service/game/scene.go` | 传输适配（一帧一 push）、会话生命周期（进场 held / ready / 离场）、地图尺寸、半径、队伍 | 聚合规则、重试规则、编码 |

## 三件收尾

**1. "会话 ready 再开始"。** `Manager.OpenHeldSession / HoldSession / ReadySession`：held 的会话可以订阅但不出帧，Ready 之后第一帧是新 epoch 的 FrameFull；
Hold 一个在收帧的会话 = 让它重新开始（重连、客户端重置）。demo：`enter_game` 以 held 开会话，新增客户端消息 **`scene_ready`（id 10024）**，
handler `HandleSceneReady → Scene.Ready(playerID)`；机器人在 `scene_watch` 之后发 `scene_ready`。首帧竞态由此**消失**而不是变窄。

**2. `syncTopic` → `syncNamespace`。** `EntitySyncBuilderParam.Namespace` / `EntitySyncCreateParam.Namespace`；codegen 标记 `syncNamespace=`，
旧键 `syncTopic=` 直接报"已改名"；`validateSyncNamespaceParam` 沿用 RR-20260918-07 的裸标识符拒绝。Namespace 随每个组件下发，是客户端唯一的分流键
（帧头 RoomID 已是常量）。fixture `player_gen_wire.go` 重生成；demo 的 `SyncNamespacePlayer / SyncNamespaceMonster`。

**3. `entitysync/policy`。** demo 模板里的 `InterestSystem` + `RelationSource`（AOI + 关系源聚合、first-source-subscribes / last-source-unsubscribes、
band → profile、被拒重试）**整体搬进 core** 成为 `policy.Interest`，且直接驱动 Manager（`Apply() []Refusal`，调用方只决定日志级别）；
新增 `policy.Group`（成员全互见，subject / member 两个集合，预算，关闭时退役全部 subject；09-23 由 Room 改名，避免与 `lockstep.Room` 混淆）与 `policy.Direct`（显式绑定）。
demo 删除 `game/scene/runtime/{interest,relations,interest_test}.go.tmpl`，`scene` 契约去掉 Source / Relations / SubscriptionChange / Interest，
Scene 实体不再暴露 `Interest()`；bridge 用 `policy.NewInterest` 装配（地图尺寸来自 `WorldSceneConfig`，半径与 team 关系是 bridge 的常量）。

## 改动清单

- `entity/subject_sync.go`、`entity_base.go`：Topic → Namespace
- `entitysync/manager.go`、`session.go`：held 状态、`OpenHeldSession/HoldSession/ReadySession`，Flush 跳过 held，`Unsubscribe` 对 held 直接删，`Stats.HeldSessions`
- `entitysync/policy/{source,interest,room,direct}.go` + `policy_promises_test.go`（8 条）
- `codegen/internal/entity/{parse,gen}.go` + 测试 + testdata + fixture；`codegen/internal/roost/{demo.go,help.go}`、`docs/CODEGEN_REFERENCE.zh-CN.md`
- demo：`scene.go.tmpl` 重写为四层中的应用层；`scene_test.go.tmpl`（`joinReady`、去 `SetArea`）；`game/scene/scene.go.tmpl`、`runtime/runtime.go.tmpl`、`entities/scene/entity.go.tmpl` 收窄；
  `protocol/def/scene_ready.go.tmpl`、`controllers/player/scene_ready.go.tmpl`、`controller.go.tmpl`（`sceneJoiner.Ready`）、`service.go.tmpl`（去 `SetArea`）、
  `cmd/loadtest/main.go.tmpl`（`scene_ready` 动作）、`loadtest/scenarios/demo.yaml.tmpl`、`entities/{player,monster}/entity.go.tmpl`（`syncNamespace`）

## 证明

- `go test -race ./entitysync/...`：Manager 10 条 + policy 8 条（含从 demo 移植的 6 条：self 经关系源、距离订阅/释放且边界抖动零帧、关系比距离活得久、离场双向释放、被拒重发直到被接受、已释放 pair 不重试）。
- `go test ./entity ./codegen/internal/entity`：绿（fixture 重生成后）。
- 渲染工程（rvArch2，`replace` 本地 core）：`go vet ./...` 干净、`go test ./...` 15 包 ok（含 scene 8 条）。
- core 全仓 `go test ./... -count=1`：116 包 ok；`go test ./codegen/... -count=1`：15 包 ok（渲染工程与机器人 codec 补 `SceneReady` 之后重跑仍绿）。

## 端到端

rvArch3（`replace` 本地 core，冷进程，16 机器人，`scene_expect` 超时 40s，debug 日志，观察者 × subject 探针；证据目录 `<scratch>/expI/`）：

```text
run done state=finished started=16 success=16 failure=0 elapsed=10.8s
首帧：16 个观察者的**第一帧自身 subject 全部带 pos_x**（M-13 之前这个数字在 6–10/16 之间随机——快照抢在 scene_watch 之前）
    6 个首帧 ver=1（进场后没改动就 ready），10 个首帧 ver=2（ready 前已有一次改动，快照带新版本）
FRAME 2699 行，重复 (obs, subj, ver) = 0；首个断线 19:45:01.023 之后无新内容，无人受影响
scene: 日志：1 × "player unreachable, leaving the scene"；WARN 仍只有 slow dispatch ×14
```

机器人脚本一开始漏了给 `pbCodec` 加 `SceneReady` 的 marshal / unmarshal 分支（`pb codec: no marshal for *pb.SceneReadyRequest`，16/16 红），补上后如上。

## 边界

- `policy.Interest` 的 `Session` 映射由应用给（demo：实体 id → player id）；不给则 session id == observer id。
- `Group` 不管会话开关：会话是传输的事，`Join` 之前应用先 `OpenSession`。
- `scene_ready` 是 demo 协议；用 nettransport 直连的部署在会话握手完成处调 `ReadySession` 即可，Manager 不关心信号来源。
- 未发版；破坏性（标记改名、协议加消息、删 demo 运行时文件）。
