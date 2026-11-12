# 配方登录链路验收（2026-11-12）

基于 `feat/business-modules-recipes-login` 的 `6ee057c2` 完成[交接任务](../../HANDOFF-2026-11-12-recipes-login.md)。这是 casual / turn-pvp 的定向验收，不是全量发版报告。生成、构建、测试均使用本 worktree 的 core，`GOWORK=off`，macOS arm64。

## 改动与证据

- 保留既有 login 认证器、进服票据先新建、承载服/在途迁移的进入前检查、casual 首次发放及旧 session 令牌入口；未重写这些逻辑。
- 新建配方从业务服务配置读取 SID / TCP 端口，为 account **开发配置**填入 `login.dev_token` 和 `portal.servers`。不修改生产示例、Secret 或渠道实现。缺配置的修前失败见 [config-before.txt](config-before.txt)。
- turn-pvp 新增 TCP 场景：空名字拒绝且零提交，正常新建后通过正式 Nest 查询读到档案名，重复新建不改名。
- modules 手册、能力清单、已知限制与两份生成说明已同步；`compose.md`、demo、login/portal/enter/servertime 模块及框架执行链路未改。

| 验证 | 结果及范围 |
| --- | --- |
| `go test ./codegen/internal/roost -run '^TestRecipe' -count=1 -v -timeout 20m` | 通过，173.849s；见 [recipe-tests.txt](recipe-tests.txt) |
| 两个自定义 module / service 工程 | casual 7、turn-pvp 6 条配方场景，零 SKIP；各自 `go test ./...`、定向 race、build、vet、module verify 与生成漂移检查 |
| 变异自检 | 7 条行为变异全部被断言捕获，无存活；不是编译失败。名字两条的失败输出另存 [name-mutations.txt](name-mutations.txt) |
| 重复安装 | casual 11 个模块 / 426 个文件、turn-pvp 8 个模块 / 406 个文件保持不变；排除 `.git`、`.dev`、`.acceptance` 和 `bin`，见 [repeat-install.txt](repeat-install.txt) |
| 真实 casual | [casual-real.txt](casual-real.txt)：curl 三步、TCP 握手、拒绝未新建进入、新建、首次进入、重连后非首次进入 |
| 真实 turn-pvp | [turn-pvp-real.txt](turn-pvp-real.txt)：同上，另验空名字拒绝 |
| 根包与文档 | 见 [repository-checks.txt](repository-checks.txt) |

7 条变异分别是：跳过首次发放、对已初始化玩家再次初始化、取消进服票据的先新建标志、放行换服在途、允许重复新建、把角色名写成固定错误值、放行空名字。逐条仅修改临时生成工程，检查指定用例的行为错误，然后恢复文件；没有修改被其他任务维护的模块源码。

## 真实进程范围

两个工程分别名为 `recipecasual`、`recipeturn`，顺序执行 `make dev-run`，每个工程启动 account、activity、chat、global、mail、match、platform、rank、session、game 共 10 个进程。实际确认 `dev-run` 自动登记 SID 1000，HTTP 清单推荐该服并返回 `127.0.0.1:7000`。

- 依赖为本机 etcd `2379`、Redis `6379`，本轮独立 NATS JetStream `24222` 与单节点 Mongo 副本集 `recipe-rs` / `27028`。所有项目使用各自生成的项目名前缀与数据库；未修改共享服务配置、未做故障注入。
- 首次启动使用共享 NATS 时失败于 `mod syncbus provide: jetstream sync: ensure stream ...: nats: no responders`，确认该实例未启用 JetStream；共享 Mongo 也是 standalone。因此改用隔离的 JetStream 与 Mongo 副本集，满足生成工程已声明的依赖要求，未放宽框架配置。
- 执行器退出会回收后台子进程，首次 ready 后 curl 曾报连接拒绝；最终在保持前台会话的情况下运行 `dev-run`，另一个会话执行探针。该执行环境问题不计为产品测试通过或产品缺陷。
- 两份工程完成后均 `make dev-stop`；隔离 Mongo 与 NATS 已正常退出。原有共享服务保留。
- HTTP 请求与响应票据仅存临时工程的 `.acceptance/`，不提交、不打印；仓内仅保留无票据的结果和复跑探针。

真实客户端沿用 core 的 `robot/transport` 与生成 PB，不另造网络协议。它验证登录、新建和重连；**没有解析 self_sync 内容，没有运行真实购买/完整对局、独立 Gate、跨机、多实例故障、重启恢复或性能测试**。正式 Nest / account / Redis / Mongo / NATS 参与不等于这些额外边界已验收。全仓测试、完整 `TestDemo|TestModule`、`test-modules-generated.sh` 与玩法矩阵留给登录线统一收口。

## 复跑

先用目标 core 构建 CLI，再为两个配方各建一个独立工程。尚未发布的 core 用 `--skip-deps` 和本地 replace：

```sh
export ROOST_CORE=/absolute/path/to/roost-core
export RECIPE_WORK=/absolute/path/to/isolated-acceptance
mkdir -p "$RECIPE_WORK"
cd "$ROOST_CORE"
GOWORK=off go build -o "$RECIPE_WORK/roost" ./codegen/cmd/roost
"$RECIPE_WORK/roost" project new recipeturn -module example.com/acceptance/recipeturn \
  -recipe turn-pvp -out "$RECIPE_WORK/turn-pvp" --skip-deps
cd "$RECIPE_WORK/turn-pvp"
GOWORK=off go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$ROOST_CORE"
GOWORK=off go mod tidy
```

按生成 README 准备 Mongo **副本集**、Redis、NATS **JetStream**、etcd；端口不同时只修改临时工程开发配置的连接地址，保持同工程各服务一致。默认开发监听要求本机 7000、8081、9100–9109 空闲。不要用正在服务业务的数据库与前缀做这项验证。

在生成工程安装探针（不是业务源码）：

```sh
python3 - <<'PY'
import os, re
from pathlib import Path
source = Path(os.environ['ROOST_CORE']) / 'docs/maintenance/evidence/recipes-login-20261112'
module = re.search(r'^module\s+(\S+)', Path('go.mod').read_text(), re.M).group(1)
Path('.acceptance').mkdir(exist_ok=True)
Path('.acceptance/main.go').write_text((source / 'login_probe.go.tmpl').read_text().replace('{{MODULE}}', module))
PY
GOWORK=off make dev-run
bash "$ROOST_CORE/docs/maintenance/evidence/recipes-login-20261112/portal-smoke.sh" -check-name
GOWORK=off make dev-stop
```

casual 对应 `-recipe casual`、另一个工程名/module/目录，探针不带 `-check-name`（未装 profile）。脚本每次使用新 `open_id`；账号票据不放入命令行。自动化执行环境若在命令结束时回收子进程，需在持有 `dev-run` 的会话继续等待，另一个会话执行探针与 `dev-stop`。

## 图谱核验

使用 CBM 项目 `Users-whb-roost-worktrees-recipes-login`（Tier 2）。`scaffoldRecipe` → `writeRecipeLoginDevConfig`、配置追加与正式生成入口均核对源代码。图谱把标准库 `Join` / YAML `Unmarshal` 误连到同名业务符号，未据此判断依赖；以实际 import 和编译为准。涉及的 Go 文件没有记录的解析缺口；配方公共 TCP、casual TCP、turn-pvp TCP 与 profile 组件模板存在占位符解析缺口，已读取全部相关行补证。文档和验收 artifacts 属索引排除范围，直接读取核对；无缺口记录不代表全仓完整性。
