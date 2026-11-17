# 交接：2026-11-17 大厅 / relay 与配置导出之外，还没完成的工作

给接手的 agent。所写内容截至 2026-11-17 晚，main 的 HEAD 在 187e4a24 之后。本轮的代码都已在 main 上提交，**还没有推送，也没有发版**。

## 0. 先读

- **编码规范**：`~/.claude/skills/roost-coding/SKILL.md`
- **本轮做了什么**：CHANGELOG `[Unreleased]` 段。
- **方案**：
  - `FEATURE-2026-11-16-lobby-relay.md`，§6 是维护者的 12 条决定；
  - `FEATURE-2026-11-17-client-config-export.md`。
- **现行契约**：
  - 服务端：`docs/framework/{GATEWAY,LOBBY,RELAY}.md`；
  - 客户端：`client/README.md`、`client/relay/README.md`、`client/ts/README.md`、`client/dotnet/Roost.Client.Relay/README.md`；
  - 生成器：`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`（§2.2、§9.2）。
- **限制与遗留**：`docs/maintenance/KNOWN-LIMITS.md`。一里是框架本身的限制，二里是遗留问题。
- **各组的证据**：`docs/maintenance/evidence/lobby-relay-*-20261117/`、`client-config-export*-20261117/`。

## 1. 规则（维护者的长期规则）

- **提交**：
  - 直接在 main 上提交，用 pathspec 提交（`git commit -- <paths>`），不要 `git add -A`。
  - 提交信息用中文，结尾写 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`。
- **需要维护者批准的事**：
  - 推送、打 tag、发版、删分支或 worktree、清缓存、下载新依赖（包括 npm 包和浏览器二进制）；
  - 发版必须等维护者明确说“发 vX”。
- **证据**：
  - 修 bug 先红后绿：改之前先让用例变红，改完再变绿；
  - 新行为要有回归用例，关键的一行要做变异（撤掉后用例变红）；
  - 不许为了变绿而放宽断言、删用例或跳过用例；
  - 没确认的事写成“尚未确认”，不写成“已定”。
- **测试**：每一步只跑快速检查，也就是改过的包，加 `-race`、`go vet`。慢的套件放到最后统一跑一次。
- **真实依赖**：用 `source ~/.roost-it/roost-dataengine-it/env.sh` 加载隔离环境里的 Redis、NATS、Mongo、etcd，不要用 `/tmp`。
- **开跑前**：先用 `df -h /` 看磁盘，剩余不到 60G 就停下。
- **收尾**：用 `ps` 查，清理自己起的进程。
- **兼容**：线上尚未部署，不需要兼容旧数据和旧进程。
- **并发**：同时跑的 agent 最多 5 个，不嵌套子 agent。
- **语言**：用中文报告，限制和遗留问题分开写。

## 2. 冲突与时机（重要）

- **portal 组正在改的地方**，任务做完之前不要碰：
  - `modules/login/**`、`modules/portal/**`
  - `codegen/internal/roost/` 里模块和生成器相关的部分
  - `docs/framework/LOBBY.md`
  - `docs/agent-skills/roost-genre/references/compose.md`
  - `CHANGELOG.md`、`KNOWN-LIMITS.md`

  它在做的事：让 portal 能装进不带 game 的 `-template lobby` 工程，然后用真实进程演练整条链路。
- **最后一次全量 pretag 用的是工作区**：portal 组完成后，协调者会跑一次全量 pretag，然后推送。这段时间里不要在 main 的工作区改代码。
  - A 类（只读排查）随时可以做。
  - 要改代码的任务，等协调者推送之后再在 main 上提交，或者先在自己的 git worktree 分支上做，等维护者决定怎么合。
- **CHANGELOG 和 KNOWN-LIMITS**：每个任务把建议写进去的文字放在自己证据 README 的末尾，由协调者统一写进这两个文件。

## 3. 任务清单

### A 类：现在就能做（只读排查，不改代码）

**A1. `./client/relay` 出现过一次没复现的失败**（KNOWN-LIMITS 二“客户端：遗留”）
- 背景：sdk 组开发中有一次 `./client/relay/...` 输出了 FAIL，但原因被 `tail` 截掉了，之后再也没复现。当时 relay 组正在改 `service/relay`。
- 做法：
  1. `source ~/.roost-it/roost-dataengine-it/env.sh`；
  2. 跑 `GOWORK=off go test -race -count=50 ./client/relay/`；
  3. 再在 CPU 加压的情况下跑一轮（例如同时跑 `stress` 或几个 `go test` 占满核）；
  4. 保存完整输出，不要只看 tail。
- 结论：
  - 复现了：给出根因和修法，写成 bug 报告。修复要等推送之后。
  - 跑了足够多次仍没复现：写明跑了多少次、在什么负载下，由维护者决定是否删掉这条。

**A2. 生产 compose 不发布玩家 TCP 7000 端口，尚未确认**（KNOWN-LIMITS 二“大厅 lobby 与转发 relay：遗留”）
- 背景：deploy 组发现生成的生产 compose 本来就不发布玩家 TCP 7000，所以声明了 websocket 的工程里，Gate 只发布了 7080。
- 做法：读 `codegen/internal/roost/render_deploy.go`、生成的 `deploy/` 模板，以及相关文档（docs/deploy 或生成工程的 README）。用 `git log -S` 查这个行为是什么时候、因为什么引入的。
- 结论：判断是有意为之（例如约定走前置负载均衡）还是遗漏，给出证据。是遗漏的话写出修法，修复要等推送之后。

### B 类：需要维护者本机或外部环境

**B1. Unity / IL2CPP 实跑**（KNOWN-LIMITS 二“客户端：遗留”“配置表导出：遗留”）
- 要在维护者的 Unity 客户端仓库里跑（Unity 6.7）。服务端仓库用 v1.53.0 生成，协议导出已经在 Unity 里通过。
- 验证三样：
  1. 新的配置导出：`client_export.config`，加上 `roost client sync --config-code-to <目录> --config-data-to <目录>`。在 Unity 里 `ConfigTables.Load` 读回数据，IL2CPP 构建后在 Player 里能读。
  2. 开房间 C# SDK `client/dotnet/Roost.Client.Relay`：在 Editor 和 IL2CPP Player 里连 relay（TCP），走完建房、加入、收发、换 host。
  3. 协议导出在 IL2CPP Player 下的表现（之前只在 Editor 里跑过）。
- 要等推送之后，服务端仓库才能用到这些改动。

**B2. 其他浏览器与真实后台**（KNOWN-LIMITS 二“只在内置浏览器面板里演练过”）
- 演示页：`cd client/ts && npm run build && source ~/.roost-it/roost-dataengine-it/env.sh && node demo/serve.mjs`，然后打开 `http://127.0.0.1:8787/demo/`。
- 演练步骤见 `docs/maintenance/evidence/lobby-relay-browser-20261117/README.md`。
- 要测的：
  - Safari、Firefox、手机浏览器（iOS Safari、Android Chrome）；
  - 房主页面**真的**切到后台，或者手机锁屏：Worker 里的规则会不会被限流，房主心跳会不会超时（每 2s 一次），5s 后会不会让出房主。
- 不要为这个装 Playwright，下载浏览器需要维护者批准。

**B3. 跨机器**（KNOWN-LIMITS 二“平台与外部验证”）
- 测三种情况：
  - lobby 和 relay 在不同机器上，客户端直连 relay 公布的地址（`advertise_addr`、`ws_advertise_addr`）；
  - WebSocket 经过七层负载均衡，并终结 TLS（wss）；
  - 多个 relay 实例，确认 lobby 按房间数选 relay。

### C 类：本轮留下的功能缺口（等推送之后再改代码）

先给出方案，交维护者定，定了再实施。

**C1. Unity WebGL 连不上 relay**
- C# SDK `Roost.Client.Relay` 只有 TCP，而 WebGL 只能用 WebSocket。需要给 C# SDK 加 WebSocket 传输，WebGL 下的 WebSocket 一般要靠 jslib 桥接。Go SDK 同理，也只有 TCP。
- 先确认维护者要不要出 WebGL 包。

**C2. relay 的两个小协议补丁**
- Welcome 帧带上 epoch。现在连上之后还没经历过换 host 的客户端，查询到的 epoch 是 0（RELAY.md、三份 SDK 都要改）。
- 方案 §4.7.3 的“前台成员上报”还没有对应的控制帧，Web 端让出房主时没法优先选前台的成员。

**C3. 七层负载均衡后的真实 IP**
- `max_connections_per_ip` 把负载均衡后面的所有连接算成同一个 IP，需要读 `X-Forwarded-For`，并且只信任配置里列出的代理。Gate 和 relay 共用的 `infra/network/wsconn` 都要改。

**C4. 快照压缩**
- 方案原文是 LZ4 或 zstd，现在三份 SDK 缺省都用 deflate，压缩接口可以替换。要不要提供 zstd，由维护者决定。提供的话要加依赖，需要批准。

**C5. 配置导出的类型范围**
- 不支持具名类型（如 `type Kind int32`），index 和导出都会拒绝。
- 浮点数的规范写法不一致：Go 是 `1e+21`，C# 是 `1E+21`，只影响浮点列上的 unique 和 enum。
- 横排表转换报错里的 `row=` 是去掉注释行之后的序号，不是文件里的行号。

**C6. 游戏账号 token（gt1）不能单张吊销**
- 现在只能靠封号，因为每次校验都会重读账号。要不要做单张吊销，由维护者决定。

### D 类：以前留下的（KNOWN-LIMITS 二，都没动过）

逐条处理之前，先读 KNOWN-LIMITS 二对应的条目和链接，确认还没过时。

- **CellWorld 业务适配进模块（D21 后半）**：维护者 2026-11-17 说这一批先不做，没得到维护者重新点头不要开工。
- **game-demo 机器人负载**：100 或 200 人同一秒起跑不通过（2026-10-22）。
- **登录潮时 account CreateRole 超时**：尚未确认（2026-11-15，在地形与无缝世界那一节）。
- **单个角色换服**：account 只记录期限，不会自动中止过期的换服，也不拒绝迟到的提交。
- **换服 committed 之后，源库里的旧档（Q4）**。
- **Remote 在 128 / 256 并发时约 3s 的全体超时簇**：仍未找到原因。
- **大量性能与平台待测项**（M-1～M-14、C-*、Linux 复测、云 Mongo、真实弱网、跨机 HA）：大多需要 Linux、目标硬件或多机。可以先整理出一份“本机能做 / 要外部环境”的清单，交维护者排期。

## 4. 交付

每个任务交付三样东西：
- 证据目录：`docs/maintenance/evidence/<任务名>-<日期>/README.md`，只放纯文本摘要；
- 用中文写的报告：结论、证据、要维护者定的点；
- 建议写进 CHANGELOG 和 KNOWN-LIMITS 的文字，放在证据 README 的末尾。
