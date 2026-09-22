# U-0278：场景 lane 逐个推、遇错整批放弃——一个推不到的会话让同批其后的观察者少收帧

**仓库 / 位置**：roost-core `demo/internal/service/game/scene.go.tmpl`（生成物 `internal/service/game/scene.go`）的 `sceneLane.AdmitBatch`、`Scene.Leave`；
`codegen/internal/roost/render_player_tcp.go` 生成的 `pushPlayer` / `pushSession`。
**来源**：W-2026-09-22-03（U-0277 修 RR-20260922-01 时登记的另一半），维护者 2026-09-22 拍板直接修。**没有 RR 编号**。
**修复单元**：U-0278（C8）。**定位文档**：TROUBLESHOOTING T-173。

## 问题

房间把一次 flush 的全部帧按观察者 id 升序排成**一批**交给应用的 `AtomicBatchTransport`。生成工程的 `sceneLane.AdmitBatch` 逐个 `PushPlayer`，
第一个失败就 `dropAsync` + `return err`。core 侧对这个返回值的理解是"原子：整批没进"，于是 `AbortWithError` 保留脏位、下一 tick 重试；
实际发生的是"前缀进、后缀不进"：

- 排在失败会话**之前**的观察者已经收到这一帧，下一 tick 再收一遍（RR-20260922-01 实跑：幸存者同一 `(subject, version)` 各 21 次）；
- 排在它**之后**的观察者这一 tick 一帧都没收到；只要那个会话每 tick 都失败，它们就每 tick 都收不到。

U-0277 之前失败会话永远撤不掉，所以是"永远"；U-0277 之后会话关闭钩子那一 tick 能撤掉它，窗口缩成一个 tick——但**任何一个活着而推送失败的会话**
（写超时、缓冲满）仍会每 tick 饿死其后所有人，而且推送失败的任何一种都会让一个 `ErrTransportUnavailable`（接入层整体不在）把玩家踢出场景。
另外 `pushPlayer` 对"无会话"返回 `ErrSessionNotFound` **不计任何指标**，`/metrics` 上看不到有人还在向一个走了的玩家推送。

## 根因

一条 lane 把三种不同的失败混成一个返回值：

1. 接入层不在（`ErrTransportUnavailable`）——谁都推不到，是**批次**的失败，该让房间保留脏位重试；
2. 某个玩家推不到（无会话 / 写失败）——是**那个玩家**的失败，不该影响同批其他人，该让它离开场景；
3. 编码错误等程序性错误——极少见，归入 1 一起报错。

旧代码对 1、2 都做同一件事（踢玩家 + 整批报错），两边都错：1 不该踢人，2 不该整批。

## 方案选择

- **（选中）按失败种类分流**：`ErrTransportUnavailable` → 整批返回错误、不踢人；其他 → `dropUnreachable(playerID, err)`（Info 一条 + 现有 `dropAsync → Leave`）后 **continue**，
  批次返回 nil。给走了的玩家的那一帧"算送达"——送到一个已不存在的会话，本来也是去那里的。不需要新的 core API，在已发布的 core v1.16.1 上就生效。
- **（未采用）把错误包成 `nettransport.AdmissionError{Session}` + `ErrReliableBackpressure` 并切到 `SlowConsumerEvict`**：让 core 的 sink 剔除并跳过。
  语义更"core"，但 sink 剔除后会**重试剩余部分**，lane 已推出去的前缀会再收一次；且把"无会话"说成"背压"是借用。
- **（未采用）core 加 `EvictSubscriber` 不投递入口**：另一个单元（core API + 模板两边），且 U-0277 之后 Leave 已经能撤掉死会话，收益小。

`Leave` / `HideSubject` 里 `UnregisterSubject` 对 `ErrRoomSubjectNotRegistered` 不再记日志：U-0277 之后退役完成即已注销，这条 Debug 每个离开者一条、全是噪音。

## 改动

- `demo/internal/service/game/scene.go.tmpl`：`AdmitBatch` 拆成"按失败分流"的循环 + `pushFrame`（原逐片推送逻辑）；新增 `Scene.dropUnreachable`
  （成员存在时 `slog.Info("scene: player unreachable, leaving the scene")` 一次，再 `dropAsync`）；两处 `UnregisterSubject` 忽略 `ErrRoomSubjectNotRegistered`。
- `codegen/internal/roost/render_player_tcp.go`：`pushPlayer` / `pushSession` 的"无会话"分支各加 `player_tcp_push_no_session_total`（与 `player_tcp_push_error_total` 分开计：
  同名计数器换标签集在指标注册处会撞）。
- `demo/internal/service/game/scene_test.go.tmpl`：`sceneRecorder` 加 `disconnect(playerID)`（无会话 → `ErrSessionNotFound`）与 `setUnavailable`；新增两条用例。

## 证明

红（模板改前，渲染工程 rvRed，core v1.16.1）：

```text
--- FAIL: TestAnUnreachablePlayerDoesNotStarveTheOthers
    scene_test.go:492: the observer sorted after the unreachable one received nothing (flush error: global frame (1 subjects): entitysync: envelope admission failed
        room: room frame admission failed
        scene: push to player 9402: player tcp: session not found: player 9402); got map[]
--- FAIL: TestAnUnavailableTransportKeepsTheBatchAndThePlayers
    scene_test.go:560: a transport outage threw players out of the scene: members=0, want 2
```

绿（模板改后重新渲染 rvGreen，**不带 replace、钉 core v1.16.1**）：`go build ./... && go vet` 通过；`go test ./internal/service/game/ ./internal/access/...` 绿；
渲染工程全量 `go test ./... -count=1` 15 包 ok；core `go test ./codegen/... -count=1` 15 包 ok。

端到端（rvGreen，core v1.16.1 即**不含 U-0277**，冷进程 16 机器人，`scene_expect` 超时 40s，debug 日志，观察者 × subject 探针；
证据目录 `<scratch>/expF/`）：

```text
run done state=finished started=16 success=16 failure=0 elapsed=11.0s
首个断线 17:07:56.107（100001）；之后仍在线的 14 个观察者（100002–100015）每个都收到 7 帧（last=17:07:56.611–56.623）；
收到 0 帧的两个（100001、100016）正是已断线的两个。
scene: 日志：1 × "scene: player unreachable, leaving the scene"（player_id=100001），1 × "scene: unsubscribe"（修前 196 条），
        0 × "subject not retired" / "subject not unregistered"（修前各 16 / 16）；WARN 仍只有 slow dispatch。
```

也就是说：即使没有 U-0277，只靠模板这半也能让其他人继续收帧——因为 lane 不再把死会话的失败报给房间，Leave 信封"算送达"，`Unsubscribe` / `RetireSubject` 随之完成。
两半各自独立有效，合起来是：死会话在会话关闭那一 tick 被撤掉（U-0277），撤掉之前的那几个 tick 里也不影响别人（U-0278）。

## 未做 / 边界

- 单个玩家推送失败的那一帧对它本人是**丢的**（它随即离开场景）。对"活着但一次写超时"的玩家这意味着被踢出场景——这是 demo 一贯的选择
  （原注释"The player is gone"），接入层写超时通常也会关掉该会话。一个不想踢人的部署应在 lane 里换成按会话重试。
- 失败会话的帧在 sink 里"算送达"：它的 `roomObjectRefs` / 序号照常前进。它正在离开，无后果；若 Leave 失败而它没走，下一 tick 会再来一次 `dropUnreachable`。
- `dropUnreachable` 的 Info 在 Leave 的 goroutine 跑起来之前的连续几个 tick 可能重复 1–2 次。
- 这是 codegen 模板：已生成的工程要 `roost project upgrade` 或手工同步 `scene.go`；未发版。
