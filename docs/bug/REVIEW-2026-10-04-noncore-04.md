# N02 接入审查：限流、Recover 与生成路由

2026-10-04，源码 `483350ca438a690b1c80da56ce79a64d2154dcf6`。执行时为 `7e0d6ee2` 加本轮四项 Manager/Admin/Ops 修复的工作树，与此提交产品源码一致；本报告涉及 security/gateway/webroute/codegen 文件没有修改。三个新 P2 **已复现、未修**；RR-20261004-NC-01～04 是已经关闭的前一批。

| 编号 | 问题 | 失败证据 |
| --- | --- | --- |
| RR-20261004-NC-05 | 超 burst 的 AllowN 请求被拒绝，却占用 key 名额并挡住有效新 key | overlay 1 fail，Keys=1 / MaxKeys=1 / valid_new_key_allowed=false |
| RR-20261004-NC-06 | Recover 的 report callback 再 panic，突破 endpoint 错误边界 | overlay 1 fail，error=nil / panic=reporter failed |
| RR-20261004-NC-07 | 非法 chi 路径通过生成/编译，注册时 panic 而非返回启动 error | runtime 3 fail + 正式 CLI/独立 module 消费者 3 fail |

[实际执行与范围](../review/REVIEW-2026-10-04-noncore-04.md) · [复跑附件](../review/evidence/noncore-review-20261004-04/README.md) · [机制与实施建议](../review/IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md)。runtime overlay 34 项 = 5 失败 + 28 正常控制 + 1 观察；正式生成消费者 13 次叶子执行 = 3 失败 + 10 通过；不要把同一非法路径在两个层次的实证算成六个独立根因。

## RR-20261004-NC-05

**P2：不可满足的 AllowN 请求消耗 key 容量。** `security/ratelimit.go:105–141` 在校验 n>burst 之前创建 bucket、占用有界 bkt 并更新 lastSeen。配置 Capacity=1、Refill=1、Interval=1h、MaxKeys=1，key1 的 n=2 请求正确被拒绝；随即 key2 的正常 n=1 也被拒绝，Stats 为 `{Keys:1 MaxKeys:1 CapacityRejected:1 Evicted:0}`。原有测试只检查同一 key 的 tokens 未扣，没有检查新 key 容量。

这是被拒绝的非法需求阻断合法 admission，满容量前可以用多组过大请求填满 key 表；影响持续到相应闲置回收。Gateway 当前调用 Allow(n=1)，不声称现行网关客户端可直接控制 n；触发者是使用 AllowN 的调用方。不是凭图谱 degree 或包测试数推断 bug，也不是鉴权绕过。

建议在桶查找/创建与 lastSeen 副作用前拒绝超过实际 burst 的 n，保留 nil limiter/非正数需求的现行规则。复用既有 token bucket 与有界 key 表，补“非法新 key 不占名额”“满表时非法请求不掩盖正常拒绝/指标”以及原同 key tokens 控制；拒绝需求是否刷新已有 key 的 idle 活性写清楚。

## RR-20261004-NC-06

**P2：上报 callback panic 破坏 Recover 的错误转换。** `gateway/middleware.go:67–82` 在 defer 已经 recover endpoint panic 后，先调用 report，再设置 ret=nil/ErrEndpointPanic。report 再 panic 时后两行没执行，外层调用者实际得到 `error=<nil> panic=reporter failed reports=1`，没有取得约定的 ErrEndpointPanic。

nil reporter、正常 reporter 的 endpoint panic 均返回固定 sentinel，健康 endpoint 不报告的控制通过。仅确认显式安装此 middleware、report callback 会 panic 的请求边界；没有把 graph 只查到测试 caller 推定为全业务使用，也没有证明标准库 HTTP 外层不存在其他 recover。

建议先建立固定、不泄漏 endpoint 私有详情的返回结果，再在独立保护内执行 best-effort report；保留观察失败的诊断，不递归调用失败 reporter，不加每请求 goroutine/自动业务重试。验收四个原控制和失败 reporter 的 string/error/nil panic，明确 reporter 阻塞预算仍由调用方负责。

## RR-20261004-NC-07

**P2：生成链未校验路由模式，运行期 error API 也不能阻止 panic。** `codegen/internal/webroute/parse.go:103–141` 只检查路径非空和 `/` 前缀；gen.go:121–131 只按字面 method/path 去重。`webroute/route.go:53–84` 同样只做基础校验，在调用 chi Get/Post 前已写 seen，router 模式解析 panic 会逃逸 Register 的 error 返回链。

三个路径 `/broken/{id`、`/broken/{id:[}`、`/broken/*/tail` 分别因缺括号、无效正则和 wildcard 尾部规则失败。runtime 原文为 `registration_error=<nil> panic=chi: ...`。正式构建 codegen/cmd/webroute CLI，在独立 Go module 用真实 handler 生成：三次 generation_exit=0，生成物成功编译并进入 test，Service → RegisterModules → Registrar 启动实际 panic，business_calls=0；consumer_exit=1 是预期断言失败而非编译失败。

正常生成 JSON/raw 请求、完整单值/限长/handler error 门槛，以及删除全部 marker 后新消费者旧 POST/GET URL 均404，10次控制通过。这补齐旧 RR-20260930-08 的具名动态退役缺口；新 NC-07 不是旧退役 bug 未修，而是独立路径校验问题。

建议复用当前 chi 解析能力，在已有 webroute 包形成可复用模式校验入口，Codegen parse 与 runtime Registrar 同一规则，不另写不完整的大型 path parser。非法模式应在生成前拒绝并给出 handler/path 上下文；Registrar 也返回 error，seen 只在成功安装后提交。若捕获 router 自身 panic，还需说明半安装状态，不把 recover 当完整 rollback。验收三非法/正常参数与正则/通配符、错误后可继续正常注册、不同模块的重复路径；不能为让生成通过放宽到自动改写路径。

## 观察与设计边界

同 method 的 `/items/{id}` 与 `/items/{name}` 都注册成功，`GET /items/42` 返回后者：当前 duplicate 契约按字面路径，底层 chi 按匹配形状，存在静默覆盖风险。本批保留为契约观察，需先决定是否禁止语义等价路径再升级，未登记第四个 RR。

满 key 表每个未知 key 在锁内进行一次全 idle sweep，存在 O(MaxKeys) 拒绝成本；本轮仅源码机制分析，没有 benchmark/SLO。Timeout 只传 context，不杀不配合的 endpoint。Stateless session token 的撤销/nonce 单次使用与 webhook 的防重放属于业务协议，不能由 HMAC 校验成功推出已解决。

## 2026-10-04 修复接续

NC-05～07 **3/3 已修、声明场景验证，未发版**：[限流](../bugfix/RR-20261004-NC-05.md)、[Recover](../bugfix/RR-20261004-NC-06.md)、[生成路由](../bugfix/RR-20261004-NC-07.md)，[正式红绿和消费者](../bugfix/evidence/noncore-bugfix-20261004-03/README.md)。上文未修与失败保留原审查时点；原34项已转绿，形状覆盖仍是观察，不把它当作已禁止。新通信链问题不属本批修复。
