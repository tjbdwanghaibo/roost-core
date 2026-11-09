# cellworld 整包 -race 偶发失败（2026-11-10）

基线 main `58b9d9f9`；对照取改为按 `Beat.Elapsed` 积分（`dcd0a0c4`）之前的第一父提交 `34bbabbe`（两者在 `gameplay/cellworld` 内只差 `dcd0a0c4`；heartbeat / nest 等其他包另有 main 上的后续提交）。macOS（Apple M5，10 核），Go 1.27，`GOWORK=off`；同机长期有别的负载（平均负载 7～22）。图谱工具不可用，以当前源码为准。只改了测试文件，实现没有改动。

## 结论

| 观察到的失败 | 判定 | 与 T05（按经过时间积分）的关系 |
| --- | --- | --- |
| `TestHandoffHysteresisAbsorbsJitter`：`1 handoffs inside the hysteresis band` | **用例余量问题（已证实机制）**，不是实现缺陷 | T05 让它变脆：按拍数积分时位置落在 20 的格点上，换向余量是整整一拍；按经过时间积分后只剩 3～5 个单位，心跳间隔被拉长 ~10ms 就越过交接线 |
| `TestIdleTierNoFlappingAtWakeEdge`：`2 promotions, 2 demotions; want 1 and 0` | **用例余量问题（已证实机制）** | T05 让它变脆：开环 sleep 换向，按经过时间积分后每段时长的抖动直接变成位置漂移；名义上在 W 外只比 IdleAfter 少 50ms |
| `TestHandoffMovesRealAndLeavesGhost`：A 仍是 `frozen` | 用例时序问题（已证实，确定性复现） | 无关，T05 之前同样失败 |
| `TestWorldStraightCrossing` / `TestGroupPanelFollowsAHandoff`：`frozen in (…) (want Real only in …)` | 同上：`assertSingleReal` 不等交接收尾 | 无关，T05 前后都出现 |
| `TestWritebackIsIdempotentAndOrdered`：`base snapshot version {2 45}, want the last accepted {2 42}` | 用例读取竞争（按源码推断，未构造确定性复现） | 无关 |

实现侧核对：`advance` 每个子步不超过 `Period`、每步之后判交接；`ShouldHandoff` 用半开矩形（`x < 10000 + H` 才算在带内）。用例失败时交接都发生在玩家真的走出迟滞带之后（见下文注入实验里的峰值），没有发现“大 elapsed 越过迟滞带”的实现缺陷。

T05 开发时那次没有测试名的 race 失败：本轮 36 次整包 -race（含加压）没有出现 `DATA RACE`，**尚未确认**。

## 失败率对照

命令：先各自 `go test -race -c` 出测试二进制，在包目录运行 `<bin> -test.count=1 -test.timeout=20m`；每轮 main 与对照各 2 份同时跑（4 份并行）。加压 = 另开 8 个 `yes > /dev/null`。

| 批次 | main `58b9d9f9` | 对照 `34bbabbe` |
| --- | --- | --- |
| 5 轮不加压 | 0 / 10 失败 | 1 / 10（`TestHandoffMovesRealAndLeavesGhost`） |
| 4 轮加压 | 3 / 8（`TestHandoffMovesRealAndLeavesGhost`、`TestGroupPanelFollowsAHandoff`、`TestWritebackIsIdempotentAndOrdered`） | 2 / 8（`TestHandoffMovesRealAndLeavesGhost`、`TestWorldStraightCrossing`） |

修后（加压，每轮修后与 main 原样各 2 份同时跑，4 轮）：

| 批次 | 修后 | main `58b9d9f9` 原样 |
| --- | --- | --- |
| 4 轮加压 | 1 / 8（`TestWorldStraightCrossing`：等 3s 后 A 仍 `frozen`，见下文“尚未确认”） | 1 / 8（同一次运行里 `TestHandoffHysteresisAbsorbsJitter` `1 handoffs inside the hysteresis band` 与 `TestHandoffMovesRealAndLeavesGhost` 同时失败） |

前两批的两条原始失败在 36 次里没有复现（样本太少，不作频率结论）；它们的机制改用下面的探针与可控注入证明。整包里才出现、单跑不出现：单跑时心跳准点（2s 内 40 拍，见探针），整包前面的用例留下的负载 / GC 才会把某一拍的间隔拉长。

## 机制证据（临时测试，未提交）

**1. 迟滞用例的实际余量**（原用例照抄，记录每个向右段 witness 里看到的换向位置与峰值；`-race -count=6` 单跑）：

```
main: turnSeen=10078 peak=10097 turnSeen=10077 peak=10097 turnSeen=10076 peak=10095 handoffs=0   （6 次都在 10095～10097）
pre : turnSeen=10060 peak=10080 turnSeen=10060 peak=10080 turnSeen=10060 peak=10080 handoffs=0   （6 次全同）
```

交接线是 10100。对照里每拍正好 20，从 9960 出发第 5 拍恰好到 10060，峰值 10080，再多一整拍才到 10100；main 里每拍按实际经过时间积分并截断（实测有效速度 389～391/s，设定 400/s；对照 400/s），第 5 拍只到 ~10057，第 6 拍才看到 ~10077，峰值 ~10097——之后任何一拍的间隔比 50ms 长 ~8ms 以上就越过 10100。按拍数积分时心跳间隔变长不增加位移，所以对照对这种抖动不敏感。

**2. W 边缘用例的漂移**（原用例照抄，记录每个来回结束时的位置与 witness 估计的最长在 W 外时长；`-count=5`）：

```
main: x after each cycle [5790 5788 5789 5790 5790 5788 5787 5787 5786 5786]; longest outside W 250.6ms
pre : x after each cycle [5790 5790 5790 5790 5790 5790 5790 5790 5790 5790]; longest outside W 251.4ms
```

名义上每次在 W 外约 250ms，IdleAfter 是 300ms，余量 50ms。对照的位移锁在拍数上，main 的每段位移跟着实际时长走、在格点外随机游走；某一段的输入晚到几十毫秒，玩家就在 W 外多待 IdleAfter 以上，降档是正确行为。

**3. 可控注入的红 / 绿**（loopback 给 `kindInput` 加延迟，两边都是确定性红）：

| 用例 | 注入 | 修前（main / 对照） | 修后 |
| --- | --- | --- | --- |
| 迟滞 | 每个输入延迟 150ms | 3/3 红 `5 handoffs inside the hysteresis band` / 2/2 红 `1 handoffs …` | 2/2 绿，峰值 x=10155（交接线 10900）；延迟 1s 仍 2/2 绿，峰值 x≈10490 |
| W 边缘 | 向左的输入延迟 100ms | 3/3 红 `1 promotions, 1 demotions; want 1 and 0` / 2/2 红 | 2/2 绿，最长在 W 外 ~651ms（IdleAfter 2s） |
| 交接收尾 | `kindOfferReply` 与 `kindGhost` 延迟 500ms | 原写法 3/3 红 `A holds {… Role:frozen Owner:(1,0)} …, want a ghost owned by B`（与整包失败文本一致） | 改为等收尾后 3/3 绿 |

## 改动（只改测试）

- `TestHandoffHysteresisAbsorbsJitter`：这一条用 G=2000、H=900 的网格（新增 `worldGridBand`，满足 R + H ≤ G），换向点、速度不变；换向点离交接线 840（400/s 约 2.1s），盖过一拍上限 max(1s, Period) 的 400 加控制延迟。承诺不变：越界不超过 H 就不交接；忽略迟滞（越过边界即交接）或迟滞取小于 60 的值仍会被抓住。交接发生时先看 witness 里的峰值，区分“用例失去控制”（峰值已越过交接线，交接是对的）与“带内交接”。
- `TestIdleTierNoFlappingAtWakeEdge`：换向改为看 witness 里玩家自己的位置（闭环，不再 sleep 定时），IdleAfter 取生产默认 2s；W 外时长约 250ms 加两倍控制延迟，与 2s 相称。降档滞后若被忽略（一出 W 就降），每个来回都会降档，仍会被抓住。失败时报告 witness 估计的最长在 W 外时长，超过 IdleAfter 时明确说是用例失去控制。
- `TestIdleTierWithoutPlayersAndWakeOnApproach`（同类，未观察到失败）：原来 1000/s 走到 10200 才停，离 B 交回 A 的线（9900）只有 300ms；改为 NPC 在 9800、玩家 400/s 走到 10500 停（余量 600 ≈ 1.5s），走远的判定点相应改到 11800（离 NPC 仍 2000）。
- `assertSingleReal`：等到“只有 want 格是 Real、别处没有 Real / Frozen”成立（期限 3s），超时按最后一次检查报错；任一时刻至多一个 Real 仍由 `realChecker` 每次提交后检查。修好 `TestWorldStraightCrossing`、`TestGroupPanelFollowsAHandoff` 一类。
- `TestHandoffMovesRealAndLeavesGhost`：B 成为 Real 后等 A 离开 frozen，再断言 A 留下 owner = B 的 Ghost。
- `TestWritebackIsIdempotentAndOrdered`：角色停下后仍按 `WritebackEvery` 定时回写，`Hooks.Writeback` 在 Base 提交之后才调用，原来“先拷 hook 记录、再读 Base 状态”之间可能又接受了新回写。改为断言 Base 的快照版本不旧于此前最后一份，并等 hook 记下同一版本。

同包其余按速度控制位置的用例逐条核过：`scenarioStraightCrossing`、`scenarioJitter`、`scenarioDiagonal`、`TestWorldLossyNetworkCrossingAndBack`、`TestHandoffConcurrentCrossingBack`、`TestHandoffChainAcrossCorner`、`TestCellsInSeparateProcesses`、lifecycle 里三处过界都是“看到已越过某点就停 / 换向”，多走只会离边界更远，不依赖余量，未改。仍依赖真实时钟、本轮未改的：`TestIdleTierWithoutPlayersAndWakeOnApproach` 的“升档后 1s 内至少 15 拍”（心跳不补跑，负载下拍数会少，与 T05 无关）；`TestHandoffChainAcrossCorner` 在 `sleep(3*worldPeriod)` 后读 Base 的 owner。

## 附带观察：子步截断让角色略慢（已证实，未修，见 KNOWN-LIMITS）

`integrate` 每个子步 `VX*dt/1s` 向零截断。T05 之前 dt 恒为 `Period`，400/s 每拍正好 20、没有损失；T05 之后 elapsed 不是 `Period` 的整数倍（49.7ms → 19，余下的 0.3ms 子步 → 0），平均每拍少约 0.5 个单位，实测 400/s 只走 389～391/s（约 2.5%）。不是失败的根因，但它把迟滞用例的换向推迟了一拍。修法需要维护者选（见 KNOWN-LIMITS 对应条目）。

## 尚未确认：旧格 frozen 超过 HandoffAbort 仍未消失

修后加压批次里 `TestWorldStraightCrossing` 一次失败：`entity 1873 is frozen in (0,0) (want Real only in (1,0))`，当时 `assertSingleReal` 已等 3s。玩家越过交接线到停下约 1.2s，再加 `checkClient` 的 200ms 与 3s 等待，A 的区域表里这条 frozen 记录至少存在了约 4.4s，长于缺省 `HandoffAbort`（3s）——正常应已收到结论（OfferReply 或新格 Ghost 的隐式确认）转为 Ghost，或按超时放弃删除。当时平均负载 33（8 个 `yes` + 4 份整包 -race + 同机其他负载），可能只是心跳 / 区域事务被饿住，也可能是区域表没收到角色的报告；样本只有这一次，没有各格计数。

- 已做：`assertSingleReal` 期限改为 5s（长于 HandoffAbort），超时时打出各格区域表记录（role / epoch / seq / x / owner）与交接计数、心跳次数、出站队列（与 `waitSettled` 共用 `cellDiag`）。
- 已试未复现：修后二进制只跑 `TestWorldStraightCrossing|TestGroupPanelFollowsAHandoff|TestHandoffMovesRealAndLeavesGhost|TestWorldJitter`，4 份并行 × 15 次、加 8 个 `yes`，60 组全部通过。
- 下一步：整包加压复跑，出现时按诊断看 A 的 `aborted` 与 `actorBeats` 是否在涨（不涨 = 角色心跳停了；涨了而记录仍 frozen = 区域表没收到报告）。

## 修后验证

| 检查 | 结果 |
| --- | --- |
| `gofmt -l gameplay/cellworld` | 空 |
| `GOWORK=off go vet ./gameplay/cellworld` | 通过 |
| `GOWORK=off go test -race -count=1 ./gameplay/cellworld` 连续 5 遍 | 5 / 5 通过，每遍 156～158s（合并 main 前；平均负载 3～5） |
| 合并 main（`b28859c3`，跨格移动地面钩子）后：`gofmt -l`、`go build ./gameplay/...`、`go vet ./gameplay/cellworld` | 通过 |
| 合并后 `GOWORK=off go test -race -count=1 ./gameplay/cellworld` 连续 5 遍 | 5 / 5 通过，每遍 161～162s（平均负载 2～4） |
| 合并后 `GOWORK=off go test -count=1 .` | 通过 |
| `python3 scripts/check-doc-anchors.py` | 0 broken |

加压下的修后批次只有 8 次，不作“已消除偶发”的频率结论；子步截断与旧格 frozen 超时两项未关闭。

