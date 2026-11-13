---
name: roost-genre
description: 用 roost-core 按玩法生成可运行的最小 demo 工程（正式 CLI 骨架 + 仓库 scripts/testdata 业务夹具 + go test 验收），并说明该玩法在 roost 上框架能做什么、不能做什么、容量/延迟/一致性边界与业务要自己补的部分。支持回合制 PvP、合作 Roguelike、异步社交经营，以及：SLG、MOBA 帧同步、MMORPG、卡牌/回合制 RPG、房间合作 PvE（塔防 / 生存撤离 / 自走棋）、放置 / 合成休闲。也用于按一段需求描述用 roost-core 定制服务逻辑、在已有 roost 工程上加功能（references/compose.md：查能力清单、`roost module` 装现成模块、其余手写、验收）。触发词：用 roost 生成 SLG/MOBA/MMO/卡牌/塔防/自走棋/放置/合成 demo、roost 做某类游戏、按需求用 roost 做一个服务、给 roost 工程加功能、某类玩法在 roost 上的限制、roost 能不能做 FPS/开放世界/RTS、roost-genre、$roost-genre。
---

# roost-genre：按玩法生成 demo 与说明边界

## 0. 先读这几条

- **任务是“按一段需求定制服务逻辑”或“在已有工程上加功能”时，直接读 [references/compose.md](references/compose.md)**，本文其余部分是按玩法出 demo 的流程与版本细节，不必读完；需要查某项能力时看[能力清单](../../framework/CAPABILITIES.md)。
- **先判断需不需要这套服务器框架**（按玩法出 demo 时同样适用）：只是平台内小人数联机开房间、局外没有要服务器保存与防作弊的状态时，不建议用 roost。例如只在 Steam 上发行、人数少的联机房间，建议 Steam Lobby + Netcode for GameObjects + Steam 传输层（通用方案见 [references/steam-lobby-ngo.md](references/steam-lobby-ngo.md)）。判断为不需要或只需要一部分时，先告诉用户理由与替代方案，由用户决定（[compose.md §0C](references/compose.md#0c-先判断这个需求真的需要这套服务器框架吗)）。
- **组件写在 `game/components/<功能>/`，包之间靠 `game/view` 的接口相连**（[compose.md §4D](references/compose.md#4d-组件的写法组件包--gameviewv1470-起)）：实体包只留薄接线；跨组件调用写 `host.Wallet()` 这类 view 访问器；多个实体共用时照 activity / bag 的宿主接口写法，状态各存在各自的 DAO，不复制代码。
- **进程内的全局逻辑先分清要不要进事务**（[compose.md §4E](references/compose.md#4e-进程内的全局逻辑manager还是挂在-world-上)）：能重建的缓存、索引、注册表用 manager；要和玩家修改一起提交回滚的全服状态挂在 World 上，不另建单例实体；要起服就载入的本地实体写 `preload=startup`（可加 `preload_order=`），DAO 用 `dbscope=sid`。
- **有战斗先分档**（[compose.md §0D](references/compose.md#0d-实时战斗先分档再判断)）：回合、MMO 级服务器权威实时战斗、帧同步对战都支持；动作 / 射击级与大规模同屏状态同步不支持或暂缓，作为冲突提出。模块里的 `combat` 只是双人 PvP，多人战斗与打怪要按 MMORPG 参考手写。
- **两种模式，开始时先问一次**：“要用专业模式吗？”默认（不选、没回答、无人可问）是业余模式，skill 自己拆业务、用样例值做完，只在需求与限制冲突时提问；专业模式把业务拆分、数值、复用结论、状态归属一次列出来请开发者确认，服务器 / 客户端的边界由开发者逐个把控，确认后才写代码；两种模式最后都生成工程说明文档（[compose.md §0B](references/compose.md#0b-两种模式业余默认与专业)）。
- **skill 对使用它的 agent 只读**：不修改本目录下任何文件，也不改 roost-core 的模块模板与框架；发现 skill 有问题告诉用户，由维护者改。**记忆放在工程里**：`docs/DEMO.zh-CN.md`（当前状态）与 `docs/DEMO-HISTORY.zh-CN.md`（逐次变更），后续增改需求先读它们（[compose.md §2A](references/compose.md#2a-后续增加与修改需求)）。
- **交付物包括一份工程说明文档**：每个生成的工程带 `docs/DEMO.zh-CN.md`，写明需求怎么分到服务器与客户端、用了哪些模块、生成了哪些实体、用了哪些服务、协议与错误码、客户端怎么对接（[compose.md §1“再分端”、§5A](references/compose.md#5a-工程说明文档)）。本 skill 只生成服务器逻辑，客户端部分交付对接说明。
- **写业务代码一律受四条规则约束**（按玩法出 demo 时同样适用，全文见 [compose.md §0](references/compose.md)）：遵循框架的核心编程思路；模块里有的逻辑复用（每个模块的能力、能力边界与复用方式写在它的 `modules/<name>/README.md`）；不能复用就借鉴最接近的模块的做法；需求与限制冲突必须向用户提出，不能悄悄跳过或降级。玩家相关的操作要有业务层日志，高频逻辑可以不打，日志只用框架的日志（compose.md §4C）。

- 本 skill 随 roost-core 版本发布，权威源在 `docs/agent-skills/roost-genre/`，提供流程、限制摘要和指针。**权威内容在 roost-core 仓库**：各篇 `docs/framework/<GENRE>-INTEGRATION.md`、`scripts/test-<genre>-generated.sh`、`scripts/testdata/<genre>/`、`docs/maintenance/KNOWN-LIMITS.md`。下文的 Markdown 链接相对本文件所在目录，反引号里写的路径相对 roost-core 根目录；读手册时以本机检出或 module cache 里对应版本的 `docs/` 为准，不以本文转述为准。
- **`game` 模板只出框架骨架，各类型玩法来自夹具或业务代码**。已有 `game-demo` 是内置的完整参考应用；不要虚构 `project new -template slg` 一类不存在的模板。现成的业务单元另有业务模块（`roost module add`，清单以 `roost module list` 为准，见 [references/compose.md](references/compose.md)）；13 类玩法还不是模块，不能用它安装。
- 夹具里的数值（金币、时长、上限）都是样例；玩法规则缺少默认实现不算框架缺陷（KNOWN-LIMITS“游戏接入验收”首段）。
- 生成 demo 前先确认目标 roost 版本。本 skill 随 tag 发布，命令按同一检出的脚本写：**v1.40.0 起** `dao_add` 写必填的 `store` 键（更早的 tag 写 `bson`）；**v1.35.0 起**每篇在最终 `roost generate` 前多一步公共 Robot 安装（`scripts/testdata/robot-common/install.py`），九类状态玩法的场景数因此各 +1（MOBA 改为扩展 KCP 用例，仍 28 条）。目标是更早的 tag 时，读该 tag 检出里的本 skill 与脚本，不把这里的命令套上去。各篇最早可用版本：card-rpg、mmorpg、merge-casual 三个夹具 **v1.33.1 起可用**（2026-10-31 在 v1.33.1 检出实测 16 / 13 / 15 条 PASS），**最低版本 v1.33.1**：v1.33.0 上仍编译失败（见 §7）。实时派对房间（`party-room`）**v1.33.2 起有纵切（12 条）；弹性成局与组同步正式入口 v1.34.0 起（13 条）；业务层补位 v1.34.1 起（18 条，2026-10-31 在对应代码实测通过）**。三档夹具与命令不同，references/party-room.md 的命令序列是当前检出的 19 条版（18 条业务 + 1 条 Robot）；v1.33.1 及更早没有这条纵切。
- 业务时间（v1.33.2 起全部十个夹具一致，guide/10-time.md §1A）：Nest handler 的最后一个参数是约定名 `nowUnixMilli`，由生成的分发包装从 fctx 填本条消息冻结的业务时间，**Sender 签名里没有时间、调用方不传 `now`**；测试拨时钟用 `clock.SetSource`（与驱动 `SetClock` 接同一个原子时钟，`t.Cleanup` 恢复）。v1.33.1 上 auto-chess / card-rpg / idle-sim / merge-casual / mmorpg / survival-extraction / tower-defense 七个夹具的 handler 仍收调用方传入的 `now int64`（Sender 带时间参数），按旧版手册写的调用代码升级后要去掉这个参数（CHANGELOG v1.33.2“变更”）。

## 1. 适用场景

| 用户要的 | 做法 |
| --- | --- |
| “按这段需求用 roost 做一个服务 / 加这几个功能”（不是照某类玩法） | [references/compose.md](references/compose.md)：查能力清单 → `roost module` 装现成模块 → 其余手写 → 验收 → 分栏报告 |
| “在已有的 roost 工程上再加功能” | 同上；先读工程的 `roost.yaml`（`modules` 段是已装的模块） |
| “用 roost 做一个 X 类游戏的 demo / 起步工程” | §2 选型 → §3 公共骨架 → 对应 reference 的命令序列 → §3.4 验证 |
| “X 类玩法在 roost 上有什么限制 / 能不能做” | §5 公共限制 + reference 的“业务限制清单”；不在已成篇七类（首批六类 + 派对房间）的看 §6 |
| “只想先看一个跑通的纵切” | §3.5 一键脚本（需 roost-core 检出） |
| 改框架 / 修 bug / 发版 | 不用本 skill，用 roost-coding / roost-bugfix |

**先查能力清单**：用户问“roost 有没有某项能力、怎么接入”时查 [docs/framework/CAPABILITIES.md](../../framework/CAPABILITIES.md)（按需求条目索引全部能力、接入方式、状态与现成参考），不要凭本文的摘要回答。

## 2. 玩法选型

| 玩法 | reference | 支撑度 | 纵切场景数（全部 PASS 才算过） |
| --- | --- | --- | --- |
| SLG / 大地图策略（建造、行军、抢地、联盟） | [references/slg.md](references/slg.md) | 高 | 7（`TestSLG`，含 Robot）；联盟另 2 条，合计 9 |
| MOBA / 竞技对战（帧同步） | [references/moba-lockstep.md](references/moba-lockstep.md) | 高 | 28（`TestMOBA`） |
| MMORPG（组队、交易、战斗、Buff、切图） | [references/mmorpg.md](references/mmorpg.md) | 高；纵切 v1.33.1 起可用（v1.33.0 失败），game-demo 参考形态 v1.33.0 起可用 | 14（`TestMMORPG…`）+ 多地图 TCP 1 |
| 卡牌 / 回合制 RPG（抽卡、养成、回合 PvE） | [references/card-turn-based.md](references/card-turn-based.md) | 高，v1.33.1 起可用（v1.33.0 失败） | 17（`TestCardRPG`） |
| 房间合作 PvE：塔防 / 生存撤离 / 自走棋 | [references/room-coop-pve.md](references/room-coop-pve.md) | 高 | 11 / 13 / 13 |
| 放置模拟 / 合成休闲 | [references/idle-merge-casual.md](references/idle-merge-casual.md) | 放置高；合成 v1.33.1 起可用（v1.33.0 失败） | 13（`TestIdleSim`）/ 16（`TestMergeCasual`） |
| 实时派对房间（2～8 人短局：抢答、计时、回合、轻反应；第二批） | [references/party-room.md](references/party-room.md) | 中高：弹性成局 / 进房 / 心跳回合 / 组同步（`sync_groups`，与 self_sync 共用连接）/ 重连 / 补位 / 结算都有纵切；PARTY §8 余下的是这类玩法够用的硬边界（心跳 ≤20Hz、只有 TCP）、房间无跨机接管、补位不超员只靠进房 handler（存储原生补位未提供），另缺容量数据、组同步的真实接入层用例与改用组同步后的真实依赖重跑，所以不到“高”。**最低：弹性成局与组同步 v1.34.0，补位 v1.34.1** | 19（`TestPartyGame`，含 Robot；v1.34.0 为 13、v1.33.2 为 12） |

新增三类见 [references/session-genres.md](references/session-genres.md)：回合制 PvP 9、合作 Roguelike 10、异步社交经营 9 个正式生成工程场景。自 v1.35.0 提供，v1.34.2 尚无对应脚本；先检查目标版本，不能把旧版运行依赖配上新夹具。

场景数出自各 `scripts/test-<genre>-generated.sh` 的计数断言与 KNOWN-LIMITS“游戏接入验收”表；v1.33.1 起这些脚本的缺省模式由 `scripts/pretag.sh` 每次发版跑一遍，v1.33.2 起是十个玩法纵切 + 三个离线生成器专项（§5 #15）。用户的玩法跨两类时（如 MMO 里的竞技副本），分别按两篇 reference 取模块，不要为“用全所有模块”硬拼（SLG-INTEGRATION §8“聊天、匹配与 session”行）。

## 3. 公共生成骨架

### 3.1 前置与依赖方式

- Go 版本按 roost-core `go.mod`（`go` / `toolchain` 两行）；全程 `GOWORK=off`。生成物声明的最低 core 看目标检出 `codegen/internal/roost/manifest.go` 的 `minimumVersions.Core`（随功能版提高），联网方式 `project new` 解析不到比它更早的版本。
- `CORE` = roost-core 源码目录，二选一：
  - **A. 本机检出**（推荐，已实测）：`CORE=<roost-core 检出路径>`，切到目标版本的 tag / main。
  - **B. 从 GitHub 取发布版**（仓库公开，2026-11-13 在 v1.45.0 实测）：`CORE=$(go mod download -json github.com/tjbdwanghaibo/roost-core@vX.Y.Z | python3 -c 'import json,sys;print(json.load(sys.stdin)["Dir"])')`，得到模块缓存里的只读源码（含 `docs/`、`modules/`、`scripts/testdata/`）。CLI 用 `go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@vX.Y.Z`。放进别的项目用的入口 skill 见仓库根目录的 [claude-skill/](../../../claude-skill/README.md)。
- 依赖两种写法：
  - **离线 / 跟检出**：`project new ... --skip-deps`，然后 `go mod edit -replace github.com/tjbdwanghaibo/roost-core=$CORE`（与仓库脚本相同；下面的命令都用这种）。
  - **联网 / 跟发布版**：去掉 `--skip-deps`、不加 replace，`project new … -roost-core-version vX.Y.Z` 解析已发布版本；CLI 用同一版本（`go install …/codegen/cmd/roost@vX.Y.Z`）。2026-11-13 在 v1.45.0 实测：`-template game` 新工程装 wallet、checkin 后 `module verify` 全部通过。

### 3.2 公共前置命令（每篇 reference 的命令都接在它后面，用 bash 执行）

```bash
set -euo pipefail
export GOWORK=off
CORE=/path/to/roost-core                  # roost-core 检出，或 module cache 目录，见 3.1
WORK=/path/to/work                        # 放 CLI 与工程的目录
NAME=mygame; MOD=example.com/mygame       # 工程名与自定 Go module
mkdir -p "$WORK"
(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost)   # CLI 与 CORE 同版本
ROOST="$WORK/roost"; DIR="$WORK/$NAME"
# dao_add <实体类型名> <文件> "字段 类型 store键 [dao标签]" ...：在 `type <X>Dao struct {` 后插入字段，标签缺省 persist
dao_add() {
  local t=$1 f=$2 ins="" n ty k tag s; shift 2
  for s in "$@"; do read -r n ty k tag <<<"$s"; ins+=$'\t'"$n $ty \`store:\"$k\" dao:\"${tag:-persist}\"\`"$'\n'; done
  T="$t" INS="$ins" perl -0pi -e 's/(type $ENV{T}Dao struct \{\n)/$1$ENV{INS}/ or die "no $ENV{T}Dao in $ARGV\n"' "$f"
}
# world <entity.go...>：多人争用的对象放进 World 锁档（锁序低于 Player）
world() { perl -pi -e 's/category=entity\.EntityCategoryOther/category=entity.EntityCategoryWorld/' "$@"; }
# fix_mod <夹具工程名> <文件...>：夹具 import 写死 example.com/<genre>-practice，换成自己的 module
fix_mod() { local from=$1; shift; FROM="example.com/$from" TO="$MOD" perl -pi -e 's#\Q$ENV{FROM}\E(?=[/"])#$ENV{TO}#g' "$@"; }

"$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game --skip-deps
cd "$DIR"
go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
```

要点：
- `-template game` 只有 game 一个业务服务，带 Player 与 World 两个实体，缺省不托管任何框架服务；`roost module add` 装的模块用到哪个托管服务（account / chat / mail / match / rank……）就在那次安装里声明哪个。要一开始就托管，加 `-framework rank,chat` 或 `-framework all`（九个全部：account / activity / chat / global / mail / match / platform / rank / session）（`roost help project`）。各纵切夹具不经生成的访问器调用托管服务（用到的服务包在测试里直接构造），所以不需要 `-framework`。
- 三个 helper 代替仓库脚本里的 python 片段：DAO 字段与实体注解是**业务文件**，只改它们（`dao_add` 每项第四个词是 dao 标签，缺省 `persist`；派对房间的 `Board` 写 `nopersist,sync`、`Coins` 写 `persist,sync`）；组件、访问器、Sender 由 `roost generate` 生成，不手改生成文件（SLG-INTEGRATION §3.1 第 3～4 步）。
- 用 `-practice` 原名（如 `MOD=example.com/slg-practice`）时 `fix_mod` 是空操作；自定 module 名必须做。只替换 import 路径，夹具里 `"moba-practice"` 这类普通字符串不动。

### 3.3 按玩法 add / 填字段 / 拷夹具

见各 reference 的“生成最小 demo”一节。公共顺序：`add dao Player --entity Player` → `add entity <X>` + `add dao <X> --entity <X>` → `add timer --entity <X>`（需要持久期限时）→ `dao_add` 填字段 → `world` 改锁档 → `cp $CORE/scripts/testdata/<genre>/*.go.txt` 到工程 → `fix_mod` → `gofmt -w` → 安装公共 Robot（v1.35.0 起，`python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" <genre>`）→ `roost generate`。

### 3.4 验证（每篇都以此收尾）

```bash
go test -mod=mod -race -count=1 -timeout 3m -v -run '^Test<前缀>' <包...> 2>&1 | tee "$WORK/test.log"
grep -c '^--- PASS: Test<前缀>' "$WORK/test.log" || true   # 必须等于 reference 给的场景数
grep -c '^--- SKIP' "$WORK/test.log" || true              # 必须为 0（无匹配时 grep 退出 1，set -e 下要 || true）
go build -mod=mod ./...    # 分两行：`a && b` 里 a 失败时 set -e 不退出
go vet -mod=mod ./...
"$ROOST" id check            # 期望输出 IDs are valid
"$ROOST" generate --check    # 期望输出 generated files are up to date
"$ROOST" project doctor || true   # 看输出里的 FAIL 行，见下
```

- 计数和 skip 都要核对：`-run` 写错或用例被跳过时 `go test` 照样 `ok`（各纵切脚本的注释）。
- `project doctor` 在新的 `-template game` 工程上退出 0、没有 FAIL（2026-11-13 起缺省不托管框架服务，就没有协作者拒绝桩）。用 `-framework` 带上 account / chat / platform / session 而没有模块接管时，各报一项 `collaborators:<服务>` FAIL：生成的拒绝桩（“is not configured”），上线前必须实现（`services/<name>/collaborators.go`，v1.47.0 之前的工程在 `internal/service/<name>/`、生成工程 `docs/SERVICES.zh-CN.md`）；那时验收只要求除这几项外没有 FAIL。`-template game-demo` 工程（MMORPG §3.1）collaborators 已实现，doctor 退出 0。（2026-11-13 之前的版本 `-template game` 托管全部九个，doctor 恰为 `collaborators:account / chat / platform / session` 4 项 FAIL，2026-10-31 在 v1.33.0～v1.33.2 与 main `24a9dbc3` 实测。）
- 以上整段 §3.2 + reference 命令 + 本节（从本文 bash 块原样抽出拼脚本）2026-10-31 在 v1.33.2 检出（`e4ec6260`）上对全部篇目连跑，自定 module：SLG 6、SLG 进阶 6 + 2、MOBA 28、MMORPG game-demo 1、MMORPG 纵切 13、卡牌 16、塔防 10、生存撤离 12、自走棋 12、放置 12、合成 15、派对房间 12 条 PASS，全部 0 SKIP，build / vet / `id check` / `generate --check` 通过；`-template game` 工程 doctor 恰为 4 项 collaborators FAIL（当时的模板托管全部九个服务），`-template game-demo` 工程 doctor 退出 0、无 FAIL。v1.33.1 上的较早一轮：SLG / 卡牌 / MMORPG / 合成四篇通过。2026-10-31 在 main `24a9dbc3`（v1.34.0 之后、未发版）上复测：派对房间（18 条版命令，自定 module `example.com/myparty`）18 条 PASS、0 SKIP，SLG（`example.com/myslg`）6 条 PASS、0 SKIP，两者 build / vet / `id check` / `generate --check` 通过、doctor 恰为 4 项 collaborators FAIL（同上，当时的模板）；其余篇目在这个提交上未复测。

### 3.5 一键方式（有检出时最快）

```bash
cd "$CORE" && ROOST_<GENRE>_KEEP=1 bash scripts/test-<genre>-generated.sh   # 例：ROOST_SLG_KEEP=1 bash scripts/test-slg-generated.sh
```

脚本打印临时目录，工程在其中的 `project/`，module 固定为 `example.com/<genre>-practice`；成功即删，`KEEP=1` 保留。想换 module 名或只要最小子集，用 reference 里的分步命令。

### 3.6 Robot 与独立工程验收

所有玩法批量验收用 `python3 scripts/test-genres-generated.py --out <新目录>`，保留独立工程、原始日志与 JSON 结果；脚本会验证自定义 module，不能只复用仓库内的已生成工程。具体口径见 `docs/framework/ROBOT.md`。

用户要压测时沿用 `robot` 的 transport/session/scenario/runner/loadtest。业务 PB 存在时 `roost generate` 同步生成 `loadtest/protocol/registry_gen.go`。生成新玩法时，自己的 PB、端点、Robot 步骤、数据断言、运行参数与说明必须一起交付；仅编译/直接调用 Nest 不算网络验收。不得自动给所有玩法塞一个 ping 然后声明完成。

v1.35.0 起（v1.36.0 补齐闭环与 SyncProbe）13 类网络验收已覆盖既有玩法的对局或经营闭环：回合 PvP 至终态、社交种植/帮助/收获、合作三关领奖；九类状态玩法覆盖对应服务器推进与结算重试；MOBA 通过公共 LockstepBot 完成 KCP 整局和可信结算。准确步骤见 `docs/framework/ROBOT.md`。手工生成须安装同检出的 Robot 文件再 generate；公共快照测试 helper 也由安装器复制，不能漏掉。读取 DAO 要在 Nest 本地执行并持实体锁，拨业务钟用原子值。真实 Gate/账号/存储的通用链路已有独立验证，不自动覆盖每种玩法；TCP 重连不等同于完整业务重登或进程崩溃恢复。功能、容量、长稳、存储和客户端平台分别出结论。

## 4. “可运行”的两档

| 档 | 含义 | 需要什么 | 证明到哪 |
| --- | --- | --- | --- |
| 一档：`go test` 可运行（缺省） | 纵切测试用替身 committer + `nest.NewEngine` 驱动生成的 Sender / Nest / DAO / 定时器 | 只要 Go | 事务、undo 回滚、锁序、幂等、期限；**不含**文件 WAL fsync、Mongo、真实重载与驱动投递循环（各手册“业务纵切证明什么”的“不证明”） |
| 二档：真起进程 / 真实依赖 | 进程连 Mongo 副本集、NATS（JetStream）、Redis | 生成工程 `make dev-up`（docker compose，`deploy/dev/docker-compose.yaml`）+ `make dev-run`；或仓库 `wiring/scripts/integration/dataengine-env.sh up`（本机 brew 二进制、不用 docker，`ROOST_IT_HOME` / `ROOST_IT_PORT_OFFSET` 隔离；本机惯例见用户记忆：`~/.roost-it`、偏移 1000） | 各纵切脚本的 `ROOST_<GENRE>_REAL=1` 追加 SIGKILL 崩溃恢复等用例，地址经 `ROOST_DATAENGINE_IT_MONGO_URI` / `_REDIS_ADDR` / `_NATS_URL`（`wiring/scripts/integration/README.md`） |

- `make dev-smoke` 只在 `-template game-demo` 工程里有 `cmd/loadtest`；`-template game` 工程会直接提示没有（生成的 Makefile）。
- 没有被要求时不起外部服务；不要指向生产库或共享业务库（MOBA-INTEGRATION §11）。

## 5. 公共限制（所有玩法都适用）

| # | 限制 | 出处 |
| --- | --- | --- |
| 1 | Nest 短 / 长业务 worker 内禁止同步等网络、磁盘、数据库、另一个 Nest Request，禁止自开 goroutine 访问 Entity/DAO；handler 内同步 Nest 请求直接被拒（`ErrSyncInHandler`），已知参与者一次传给多实体 Sender | guide/02-nest-entity.md §1；SLG-INTEGRATION §5（`framework/nest/client.go` `validateClientDispatch`） |
| 2 | `//roost:nest` 未声明时缺省 `rollback=state durability=async`；`async` 写 WAL 可先回复、不等 fsync；`strict` 等 WAL fsync，**仍不等 Mongo 投影**；`memory` 不得写持久字段 | codegen/docs/CODEGEN_REFERENCE.zh-CN.md §6；guide/02-nest-entity.md §4 |
| 3 | strict 的代价：生成工程嵌入 TCP + strict 加金币，50 连接约 **1253 TPS**（P99 75ms）、**同一实体约 108 TPS**；上限约为 short 池 worker 数 / 每笔持锁时间（strict 在锁内等 fsync，本机约 8ms）。2026-10-22 复跑 1241～1314 / 113 TPS。单样本、非独占宿主、macOS | KNOWN-LIMITS“能力与验证边界”表“生成工程完整链路持久写入”行；docs/performance/README.md 的历史快照（GENERATED-TCP-STRICT-20261020 / NIGHT-20261022）；仅为旧版本 strict 配置样本，不是当前容量结论 |
| 4 | 回复错误按表判别：`ErrCommitIndeterminate` 等结果未知不得盲目重试，按业务幂等键核对 | guide/02-nest-entity.md §6 |
| 5 | DataEngine 对 MongoDB：≥ 5.0、副本集（单节点即可）、`transactionLifetimeLimitSeconds=20`；实测过 7.0 / 8.0，5.0 / 6.0 未实测；兼容 Mongo API 的托管服务不在验证范围 | guide/03-dataengine.md §6“DataEngine 对 MongoDB 的最低要求” |
| 6 | 实体持久定时器：离线实体不准点，加载后只触发一次，补算由业务按 `Fire` 做；Remote 托管与 noPersist 实体不支持；回调只改本实体 | KNOWN-LIMITS“按实体的持久定时器（D11）”；guide/10-time.md §4；SLG-INTEGRATION §6 |
| 7 | 实体心跳只有 10 档（50ms～60s），回调不阻塞不做 I/O；`strict` 心跳每拍持锁等落盘，快池高频心跳不要用 | guide/10-time.md §4A |
| 8 | 长期幂等靠实体上的持久业务事实（序号账本、领奖事实），服务去重环只防短期重投；夹具账本满了拒绝新写、不淘汰旧回执 | SLG-INTEGRATION §9；各手册“幂等身份”节 |
| 9 | Player 静态绑定 game sid，无动态跨 sid 迁移；跨服协作走 Remote / Saga / 邮件 | MMORPG-INTEGRATION §1、§6；KNOWN-LIMITS“平台与外部验证”Player跨机 |
| 10 | 玩家接入层（player TCP、独立 Gate）只提供 TCP；KCP/UDP/QUIC 只生成装配函数，监听、握手鉴权、Session 绑定宿主自写 | MOBA-INTEGRATION §6；docs/framework/GATEWAY.md §4 |
| 11 | SyncBus 是默认可丢广播；要最终一致须业务自建对账 | KNOWN-LIMITS“广播与 AOI”SyncBus投递；guide/04-sync.md §5 |
| 12 | AOI：缺省 ApplySync 每玩家 ≤ 9 个视野（超出用 ApplyAsync）；一个会话持有目标受 `MaxObjects`（缺省 100）约束，超出时不断开、**超额降级**（暂缓多出的对象，Interest 从最远处收紧视野，2026-11-08 D17） | guide/04-sync.md §2“视野”上限与两种应用模式；KNOWN-LIMITS“广播与 AOI”AOI 视野③ |
| 13 | Saga 正式实现只在 macOS 单机测过：原生正向 c128 约 1050 saga/s；Linux、跨机未测；Saga 不是跨服瞬时原子交易。业务键唯一只在运行期间 + 终态保留期（`saga.terminal_retention`，缺省 1h）内成立，**保留期后可复用**，前后两轮靠协调器生成的生命周期身份区分（维护者 2026-10-31 定案 A）；要“同一业务键永远只有一笔”的业务在自己数据里记已用过的键 | KNOWN-LIMITS“Saga”；MMORPG-INTEGRATION §3；SAGA.md“定义”业务键与查询；guide/06-saga.md 幂等键 |
| 14 | match 单分区“每秒成局人数 × `match.retention`”持续上限约 4500 人（本机单样本）。v1.34.0 起有弹性人数队列（`Queue.MinSize` / `FillAfterSeconds`，键 `2-8@20s`，过早的不满员局 `ErrGroupNotReady`；**先升级匹配服务再用**，旧服务端当固定队列）；没有存储原生补位 | guide/09-services.md §4；KNOWN-LIMITS“匹配的弹性人数成局与补位” |
| 15 | 纵切脚本的**缺省模式**（离线、替身提交器）在 `scripts/pretag.sh` 里每次发版并行跑（`ROOST_PRETAG_GENRE_JOBS`，缺省 4），并把真实模式恢复夹具复制进生成工程 `go vet`（只编译不运行）：v1.33.1 起九个玩法纵切；**v1.33.2 起十个玩法纵切 + 三个离线生成器专项**（`test-heartbeat-generated.sh`、`test-level-generated.sh`、`test-sync-modes-generated.sh`）；v1.35.0 起再加三类离散玩法（turn-pvp / coop-roguelike / social-sim）。根包 `TestGeneratedScriptsAreInPretag` 守卫每个 `scripts/test-*-generated.sh` 都在 pretag 里或登记豁免（豁免只剩 dataengine / remote / demo 三个需真实依赖的脚本）。**仍不在门禁里**：真实模式（`ROOST_<GENRE>_REAL=1`，需隔离 Mongo 等）要改模板 / 生成器时手动跑；守卫只查 pretag 调用了脚本，场景是否真执行靠各脚本的通过数断言。v1.33.0 及更早没有这道门禁（三个夹具因此在 v1.33.0 编译失败无人发现） | KNOWN-LIMITS“游戏接入验收”首段；docs/maintenance/README.md §5；CHANGELOG v1.33.1、v1.33.2 |
| 16 | 真实 Unity / Godot / Unreal 引擎、跨机 HA、断电未验收 | KNOWN-LIMITS“平台与外部验证” |
| 17 | 业务时间一律取自 ctx：handler 用约定参数 `nowUnixMilli`（Sender 不带时间），心跳 `Beat.Now`、定时器 `Fire.Now`；测试用 `clock.SetSource` 拨，不让调用方传 `now`（v1.33.2 起十个夹具一致，§0） | guide/10-time.md §1A；CHANGELOG v1.33.2“变更” |
| 18 | 一个 Nest 只有一个 `EntitySync` 观察者：打开 `access.player.self_sync` 后它归自身视图的 Manager。房间 / 队伍要推给成员用 `access.player.sync_groups`（v1.34.0 起，与自身视图同一条连接、同一个推送编号，客户端按 Namespace + `Profile.Key` 分流；要求 self_sync、成员关系不持久）；不要自建 Manager 再交给 Nest——v1.34.0 前被静默覆盖，v1.34.0 起 `ErrEntitySyncObserverConflict`；政策要装到 Mod 的 Manager 上（`EntitySyncSetup.Attach`、`selfsync.Groups`） | ENTITY_SYNC.md“按组同步与 self_sync 共用连接（Groups）”；CHANGELOG v1.34.0；references/party-room.md §2、§5 |
| 19 | 提交后动作用 `if !nest.AfterCommit(fn) { fn() }`（`SyncGroup.Join / Leave / Disband` 内部就是这样）。**v1.34.0 及更早**包级 `AfterCommit` 对已提交 / 回滚的事务仍返回 true、回调丢失；能走到的真实路径是在 `RunDetachedTransaction` 里发同步 Nest `Request`（被调方的提交后回调挂到调用方事务上）。修复在 `c37e40dd`，随 v1.34.1 发布：已结束的事务返回 false、事务不再随上下文快照跨 goroutine。用 v1.34.0 及更早时不要在 detached 事务里发同步请求去触发组同步成员变更 | docs/framework/impl/02-nest-entity.md §2 第 7 条；CHANGELOG [v1.34.1]“修复” |

## 6. 不适合 / 暂缓的类型

| 类型 | 结论 | 原因（出处） |
| --- | --- | --- |
| FPS / 动作射击 | 不建议 | 现有心跳调度最短 50ms（guide/10-time.md §4A），不提供更高频的该心跳档位；这不是 Sync 发送上限，on_change 在 handler 提交、确认与解锁后汇总唤醒（guide/04-sync.md §3）。高频动作玩法还缺专门验收；无延迟补偿 / 命中回溯、无通用客户端预测（physicsroom 的 Unity 预测只针对物理闯关，KNOWN-LIMITS“UGC 物理闯关”）；接入层只有 TCP（§5 #10） |
| 无缝开放世界（多进程分区） | 需框架先补 | Player 静态 sid、无动态跨进程迁移与无缝边界，Interest 按进程（MMORPG-INTEGRATION §6）；现有只能“同进程多地图 + 按 sid 分服” |
| MOBA 状态同步 / RTS | 暂缓 | 阵营视野挂接几十个、必须 ApplyAsync 并按自身密度压测（guide/04-sync.md §2 选型表）；无纵切、无容量数据。帧同步 MOBA 走 moba-lockstep |

用户坚持做这些类型时：如实说明缺口与出处，提议先做框架设计讨论，不要用现有零件拼一个“能跑”的假 demo。

## 7. 已知卡点

| 卡点 | 处理 |
| --- | --- |
| card-rpg / mmorpg / merge-casual 夹具在 v1.33.0 编译失败：仍以 3 参数调用生成的 `Sync_Tick<X>Timer(ctx, id, nowMs)`，D22-1 后签名是 `(ctx, id)` | **v1.33.1 起可用**（2026-10-31 实测 16 / 13 / 15 条 PASS；夹具改为 `clock.SetSource` 固定业务时钟，CHANGELOG v1.33.1）。**最低版本 v1.33.1**：目标是 v1.33.0 时告诉用户升级并停在这里，不自行改夹具绕过 |
| 二档 REAL 恢复用例在 v1.33.1 上失败：七个 REAL 恢复夹具（auto-chess、card-rpg、idle-sim、merge-casual、mmorpg、survival-extraction、tower-defense）的未投影记录数没跟上 R15“新建实体本身就落库”，其中六个在屏障处失败（如 mmorpg `unacked=16 want 0 / 12`） | v1.33.2 已修（CHANGELOG v1.33.2“变更”，七个 REAL 与 MOBA 三条 Mongo 用例在共享隔离 Mongo 上通过）。要跑 `ROOST_<GENRE>_REAL=1` 用 ≥ v1.33.2；缺省模式不受影响 |
| 派对房间场景用 `^TestPartyRoom` 过滤 | 会多跑生成的 `TestPartyRoomTimerTickHandlerMatchesTheDriver`，计数对不上；用 `^TestPartyGame`（references/party-room.md §3） |
| 派对房间 `project sync` 报 `framework resolution failed ... go mod tidy: exit status 1`（`cannot find module providing package <module>/game/partymatch`） | `game/partymatch/backfill.go` 与 handler 测试要在 `project sync` 之前就位并 `fix_mod`（references/party-room.md §3 顺序）；修好后按提示 `roost project deps` 或重跑 |
| 派对房间命令套到 v1.33.2 / v1.34.0 检出 | 夹具文件不同（v1.33.2 有 `sync.go.txt`、无 sync_groups；v1.34.0 无 `backfill*.go.txt`），场景数 12 / 13 对不上；按目标版本检出里的 `scripts/test-party-room-generated.sh` 走 |
| `roost add handler <name> --entity X` 必须带 `--component`（否则 `handler requires --component <name>`） | 纯 DAO 的多实体 handler 手写：文件放 `game/handler/`，加 `//roost:nest rollback=undo durability=strict`，再 `roost generate` |
| 锁接口类型名不含 `Entity` 又没写 `target=` / `targets=` | `generate` 报 `//roost:nest found no entity parameter` 并点名 handler；接口名改成 `IXxxEntity`（`roost help nest`；CARD-RPG-INTEGRATION §9） |
| 多实体事务锁序 | 争用对象（Tile、Room、Match、Raid…）改 `EntityCategoryWorld`，Player 保持 Other（取值即锁序，`framework/entity/category_order.go`） |
| 夹具 import 写死 `example.com/<genre>-practice` | §3.2 `fix_mod`；或直接用 `-practice` 原名 |
| `--skip-deps` 后不加 replace | `generate` 里的 `go mod tidy` 会去解析可能未发布的版本；先 `go mod edit -replace` 再 add / generate |
| `project doctor` 退出 1 | §3.4：只有 `-framework` 带上、没有模块接管的托管服务的 collaborators 拒绝桩是预期；新的 `-template game` 工程应退出 0 |
| 只在 v1.33.0 及更早：`roost help project` 说 game-demo 只生成“一条写入链路”、`roost add saga` 提示“约 1200 sagas/s” | v1.33.1 已修正文案（help project 如实列出完整 demo，最小写入链路作为其中一项；help saga 改为正式实现约 1050 sagas/s，macOS 单机、Linux / 跨机未测）。旧版本以 `demo/README.md` 与 KNOWN-LIMITS“Saga”为准 |
| 去掉 `timers` 条目后生成部分不自动删除 | 手工删，见 KNOWN-LIMITS“按实体的持久定时器（D11）” |

## 8. 维护规则

- 单玩家 PB → Nest 的薄边界优先复用 `roost add endpoint`：handler 可直接接收本工程当前协议的 PB 请求指针、返回唯一 PB 响应指针（v1.38.0 起），避免手写字段搬运。普通领域参数仍可用；不能把多实体、身份来源、授权和领域 DTO 转换自动猜出来。目标取认证会话，PB 内容仍需校验；请求不能缓存到 Entity，响应不能引用 DAO 的可变容器。已有手写 endpoint 不覆盖；新建标准 endpoint 分为手写 `Handle<Protocol>`（`//roost:endpoint nest=<Handler>`）和持续生成的 `*_endpoint_gen.go`。授权写在手写入口，依赖 Entity 状态的校验写在 Nest 锁内；禁止编辑生成文件。命令与验证见 `codegen/docs/CODEGEN_REFERENCE.zh-CN.md`“PB 请求与响应直接接入 Nest”；真实 TCP + Sync 夹具为 `TestSelfSyncGeneratedProjectOverRealTCP`，存储为替身。


- SLG / MMORPG 的玩家读取联盟、公会快照，按各 reference 的“普通 Player + Remote View”段及 [Remote 指南 §6](../../framework/guide/05-remote-mirror.md#6-普通-entity-写入--remote-view-读取) 生成共用请求 DTO；I/O 池预取，handler 仅取已准备视图。权威写入与快照读取分开，不能依据陈旧视图授予写权限。


- 多引擎客户端接入可选 [native C++ 网络核心](../../../client/cpp/README.md)：Unity 使用 C ABI 与现有 C# EntityViews，UE 使用 C ABI/C++。先核对平台验收表，不把“提供构建入口”说成 Windows/Android/iOS 或 UE 已实跑；Lockstep 状态机仍以现有 C# 能力为准。

- 自 v1.35.0 起默认 DAO 同步为 PB 字段补丁，持久化仍用 BSON；v1.34.2 尚未包含此变更。生成客户端时一起交付 `dao_sync.proto` 与 `dao_sync_apply.cs`，按字段存在性替换，不能使用普通 PB MergeFrom 合并集合。完整契约见 [DAO 同步](../../framework/DAO-SYNC-PROTOBUF.md)。

- 数字与命令以 roost-core 当前版本为准。发现本文与仓库不符（场景数、脚本步骤、夹具文件名、CLI 参数、限制数字），先读对应 `scripts/test-<genre>-generated.sh`、`docs/framework/<GENRE>-INTEGRATION.md`、KNOWN-LIMITS 与源码核对，再按仓库执行，并告诉主会话本 skill 哪处过时。
- reference 的命令序列是**仓库脚本的手工版**：脚本改了步骤（新增 add、夹具文件、DAO 字段）时 reference 要跟着改；拿不准时退回 §3.5 一键脚本。
- 本 skill 必须与对应版本的夹具一起维护并提交；本机安装仅作入口，不维护第二份正文。不在这里登记缺陷；疑点按维护流程写 KNOWN-LIMITS“尚未确认”。
- 不写没有出处的数字；性能数字都带日期、宿主与样本口径，不当作承诺。
- 验证与生成用的临时目录用完即删，并检查没有遗留本轮测试进程。
