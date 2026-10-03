# 非三大核心模块 Review 第一批：App 与 HTTP

2026-10-03。确认四个未修 P2：[单 Mod 校验、Clone 超时、错误状态丢失、JSON 尾随输入](../bug/REVIEW-2026-10-03-noncore-01.md)。新内容 30 个具名场景/控制，12 个失败反例与 18 个通过控制；既有七包 race/vet 通过。全部改动属于 docs。

## 同步、范围与历史接续

仓库 `D:/whb_s/cube-core`，module `github.com/tjbdwanghaibo/roost-core`，remote `https://github.com/tjbdwanghaibo/roost-core.git`，main 跟踪 origin/main。会话初始干净，更新前 HEAD 为 `db4b70099bc2bfbd4d94a927cc12becec0b73c32`，fetch 后 fast-forward 到 `746567ff37cf69878b74331cb9bcc69b159dfb63`；进入本批再次 fetch/ff 确认 already up to date，源码审查与所有本轮执行都基于此 SHA。收尾 fetch 也确认 origin/main 未前进；未来有新提交仍保留这个实际基线，未运行的新代码不纳入已验声明。

用户说明另一 agent 已基本跑过 Nest、Sync、DataEngine。本批不重新审三个域，建立[其余 15 单元的计划](NONCORE-REVIEW-PLAN-2026-10-03.md)，并从 App/HTTP 开始。历史 Service 10/10 主链阶段记录与独立复审复用；Codegen 接续尚未完成的消费者与配置缺口。Wanted 10-01 四条已有 RR/bugfix 去向，本批只是关联，不重复登记，不声称重新验收了修复。

同步技能工作也已完成：仓库维护的 roost-bugfix/roost-coding/roost-optimize 完整资源包与本机同名 skill 字节一致；仓库无 roost-review 正本，保留并更新本机入口以衔接单仓/技能同步规范。本轮 docs 不复制个人 skill 或改产品代码。

## 图谱与源码证据

采用 codebase-memory **Tier 2 Verify**。项目 roost-core 根为当前 repo，index_status ready、32285 nodes / 209828 edges，Git context HEAD 为当前 SHA；coverage generation 仍 `2026-09-30T11:58:14Z`，不是最新源码索引证明。

search_graph 对 app/app.go、app/* 生命周期、httpclient/*、httpserver/* 定位：run/stop/sort 与 Clone/DoJSON/BindJSON/HandleJSON。httpserver 42 条分两页 30+12，相关查询分页已读完；run/DoJSON/Clone/BindJSON/sortMods 的双向 depth=1 trace 未截断。关键 get_code_snippet 与当前源码对读。trace 的标准库同名 heuristic 误连（例如 Decode 连到 versionstore）未作为调用事实；BindJSON 零 caller 不代表没有泛型/动态回调消费者。

check_index_coverage 包含下表及相关 app/helper/HTTP 测试、app/httpclient/httpserver scopes，无记录缺口/剩余页，但关键路径 freshness=metadata_changed，均直接读当前源文件。最初错误猜测两个测试文件名不存在，纠正为真实 httpclient_test.go/httpserver_test.go，并完成 coverage/读取。这是工具准备纠正，非产品缺陷。get_architecture 的包视图只返回 15 个外部依赖，不能支持内部全模块盘点；范围清单按 Git 跟踪文件建立。

| 当前源码实际读取 | 本批覆盖机制 | 状态 |
| --- | --- | --- |
| app/app.go | run、shared/service 排序、提供/启动/停机、预算与错误传播 | 全文件读取；单 Mod 具名反例；其余回归复用 |
| app/mod.go、service.go | 生命周期、硬/可选依赖、停止预算、Service 关闭契约 | 当前源码读取 |
| app/registry.go、runtime_failure.go | 能力批量发布、Typed Lookup、首故障唤醒与后续 Join | 当前源码读取，包回归通过；未新增全域并发审计 |
| httpclient/client.go | New/Clone/Option、请求 ID/签名、DoJSON、错误/响应生命周期 | 全文件读取；本轮 11 个场景/控制 |
| httpserver/server.go | Engine/Group、context、限长绑定、HandleJSON、panic/production timeout | 全文件读取；本轮 7 个场景/控制 |
| app/app_test.go（主链与辅助段）、guards_promises_test.go、mod_order_test.go；HTTP 现有测试 | 检查原有场景为何遗漏单元素/Clone timeout/坏错误体/尾随 JSON | 指定段或文件读取；不把测试文本数量算实现覆盖 |

共七个生产主文件实际读取；本批不将 app 剩余实现文件、manager/lifecycle/security 的包回归升级为已读或模块完成。

## 实际验证

[复现源与脚本](evidence/noncore-review-20261003-01/README.md)，临时输出根 `D:/whb_s/.tmp/noncore-review-20261003-01`，Go 1.27.0 Windows/amd64。复用既有缓存与本机 file GOPROXY，没有修改依赖声明或下载新工具。

| 命令/场景 | 结果 | 解释 |
| --- | --- | --- |
| `go test -overlay ... -count=1 -v -run '^TestNoncore' ./app ./httpclient ./httpserver` | exit=1；30 场景中 12 fail/18 pass | 原实现的预期红；四根因，不是 12 个独立 bug |
| App 单 Mod/helper +真实 Execute | 7 fail/5 pass | 两类分组确实越过依赖检查进入 Init；合法单 Mod/外部共享/缺失 optional/多 Mod 拒绝对照 |
| Client timeout/header/error | 3 fail/8 pass | Clone deadline 300ms 而非 20ms；两类错误体丢状态；父/正常 clone/header、兼容错误体/成功解码对照 |
| Server 单值/限长/副作用边界 | 2 fail/5 pass | 尾随 garbage/第二个 JSON 200且调用业务；有效/空白、首部坏输入、超限、当前空 body 行为 |
| 七包 `go test -race -count=1 -timeout=120s -json ./app ./httpclient ./httpserver ./lifecycle ./manager ./health ./security` | exit=0，7 package pass、123 test pass 事件；0 fail/skip | 包含父/子/Example，不叫 123 叶子或新增场景 |
| 同七包 `go vet` | exit=0、无诊断 | 静态检查通过不抵消实跑反例 |

新增 App 探针后重跑完整证据以记录最终数量；没有扩成整个核心域压力测试，也未启动真实资源/修改共享索引服务。产品 Go 文件与既有测试保持原样；`.go.txt` 只是审查证据，随文档提交。

## 设计/性能评价与停点

现有生命周期、能力注册、标准库 client/chi 和 JSON reader 足够支撑四项修复方向，优先复用。根因都来自快路径或边界判断顺序：小集合省掉输入校验、wrapper 配置与共享实际 client 脱节、成功模型先于错误分类、首值解码代替整段校验。既有 happy-path 与坏输入测试没有涵盖这些分支。

机制与所有权解释见[App/HTTP 学习文档](IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md)。响应体无容量上限列为设计观察；没有本批 benchmark/SLO，不能给吞吐或成熟度分数。未验证真实代理/TLS/重定向、gateway/auth/Webroute 进程、多机 HA、Linux/长稳。

**N01/N02 均仍部分完成。** 下轮从 lifecycle/manager 自身状态机、注册/启动/停止竞争及 admin/health 开始；随后 security/gateway 与正式 Webroute。剩余 15 单元预算 54～92 有效小时，按计划逐批重新估算。源码阶段、四项未修 RR、外部验证缺口分别管理。
