# 浏览器演练：Web 端房主（2026-11-17，协调者）

## 环境
- 浏览器：Claude 桌面应用内置浏览器面板（Chromium 内核），开两个标签页。
- 服务：`client/ts/demo/serve.mjs`。它起官方测试对端（`go test -c ./client/relay`：lobby + relay 同进程，房间目录用隔离环境的 Redis），同源代理 `/lobby`，relay 走 WebSocket。
- 演示页规则跑在 Web Worker 里（`WorkerRunner`），由 `WebHost` 接线。

## 第一轮（web 组 c397fdbd）：发现 bug
| 步骤 | 结果 |
|---|---|
| A 建房，B 加入 | 通过：A 是房主（槽位 1），B 是成员（槽位 2），A 每 3s 推一次快照 |
| 成员点“+1” | 通过：操作经 relay 到房主，房主广播的计数两边一致 |
| A 主动转交给槽位 2 | **失败**：B 显示 host、epoch 1，交接数据来源、快照 seq 10 都对，但规则循环没启动，tick 停住，不推快照，sessionStorage 没有存档 |
| B 刷新 | 因为没有存档，B 没回来；宽限期过后 A 从快照接手（seq 10，年龄约 18s），这条路正常 |
| 开局前掉线的成员重新加入 | 通过：回到原槽位 2 |

根因（web 组 f2532dba）：WebHost 和业务共用 RelayClient 的单槽位回调，演示页注册的日志回调把 WebHost 的处理顶掉了。修法是让 WebHost 走 SDK 内部监听；修复时补了真实 relay 用例，修前红、修后绿。

## 第二轮（f2532dba 之后）：全部通过
| 步骤 | 结果 |
|---|---|
| A 建房，B 加入，B 点“+1” | 通过 |
| A 主动转交给槽位 2 | 通过：B 的规则循环在跑，状态来源是交接数据，tick 持续增长；快照 seq 从 relay 存的 13 接到 `1<<32 + 1`，每 3s 推一次 |
| 刷新 B（房主）页面 | 通过：约 1s 内回到房间，仍是房主（槽位 2），规则从 sessionStorage 存档恢复；A 收到 `HostResumed epoch 1` |
| B 点“模拟隐藏”（覆盖 visibilityState 并派发 visibilitychange） | 通过：约 5s 后自动转交给槽位 1；A 从交接数据接手，B 收到 `YouAreMember epoch 2` |

## 已知
- 内置浏览器面板里，后台标签页的 `document.visibilityState` 也是 `visible`，所以“切到后台”只能用演示页的“模拟隐藏”按钮触发，没法靠真实切换标签页来测。
- 刷新后回来的那一页显示 `epoch 0`：Welcome 帧不带 epoch，只影响查询（已记入 KNOWN-LIMITS）。
- 没在 Safari、Firefox、手机浏览器上测；房主页面真正处于后台时，计时器限流对 Worker 的影响没有实测。
