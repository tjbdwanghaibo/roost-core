# 非三大核心模块审查第一轮：App 与 HTTP

2026-10-03，`roost-core/main`，源码基线 `746567ff37cf69878b74331cb9bcc69b159dfb63`。四项 P2 均已复现，**未修复**。编号使用 NC 命名空间，避免与并行核心工作线碰撞。本轮只产出文档。

[运行记录](../review/REVIEW-2026-10-03-noncore-01.md) · [原始反例与复跑](../review/evidence/noncore-review-20261003-01/README.md) · [后续计划](../review/NONCORE-REVIEW-PLAN-2026-10-03.md)。Tier 2 图谱定位、双向 trace、片段与 coverage；图谱 metadata_changed，全部关键结论回读当前源码并实跑。

## RR-20261003-NC-01：P2 单 Mod 绕过依赖和输入校验

位置：`app/app.go:621-624`，调用入口 `:176`、`:217`；契约 `app/mod.go:37-51`。

`sortMods` 在 `len(mods) <= 1` 时直接返回，跳过 nil/空名字校验、硬依赖检查与环检测。只有一个共享 Mod，或一个服务专属 Mod，即会触发。多个 Mod 才进入完整校验。

期望：所有非空集合都校验；未装配硬依赖、自依赖、nil/空名应返回错误，不能进入该 Mod 的 Init。实际：五个 helper 反例（nil、空名、缺失硬依赖、硬自循环、可选自循环）均返回 nil error；两个真实 `App.Execute` 反例分别在共享和服务专属分组中，缺失硬依赖仍进入 Init/Start/Serve，最终得到无关的 `serve failed`。单 nil 后续会在 `mod.Name()` 处 panic，源码可见，本轮没有把这个推断另算实跑案例。

影响：单 Mod 的精简接入、测试工程和按需启动不能获得与多 Mod 一致的启动前校验；实际基础设施可在配置不合法时被启动。已有 `TestSortModsRefusesNilUnnamedAndDuplicateMods` 使用双元素数组，未覆盖长度为 1 的分支。

建议：保留空集合行为，单元素也走现有校验/拓扑遍历；复用 `ModDependencyProvider`、`ModOptionalDependencyProvider` 和 `external`，无需新的依赖框架。保证缺失 optional 继续合法、合法外部共享依赖继续合法、有效单 Mod 正常启动。证据：`app_review_test.go.txt`，7 个失败反例、5 个正常/拒绝对照。

## RR-20261003-NC-02：P2 Clone 的 WithTimeout 不生效

位置：`httpclient/client.go:62-83`、`:91-97`；实际请求 `:164-168`。

`New(WithTimeout(300ms))` 创建的内部 `http.Client` 具有 300ms 超时。`Clone(WithTimeout(20ms))` 只改变包装层 `timeout`，仍继承父对象的 `client` 指针；非 nil client 使后续重建分支不执行，DoJSON 继续使用原 client。

期望：未显式注入自定义 HTTP client 时，Clone 的新 timeout 应约束请求。实际：通过只替换内部 Transport、保留库创建 client 的探针，观察到标准库实际传给 RoundTrip 的 context deadline 仍为 300ms；独立 `New(WithTimeout(20ms))` 的对照为 20ms。这里观察实际请求 deadline，不读取 timeout 字段作断言，也不依赖 sleep。父级/无 timeout 修改的 clone 均为 300ms，header clone 隔离正常。

影响：希望为某条 RPC/HTTP 接入设置更短截止时间时，会继续等待父级的更长超时；设置更长超时时反向提前失败。没有声称已发生真实生产超时事故。

建议：复用现有 Option 和标准库 HTTP client，明确库创建 client 与显式 `WithHTTPClient` 的优先级；避免直接改共享父 client.Timeout 引入父实例行为变化和并发竞态。可对库拥有的 client 作安全副本，保留 Transport/连接池等能力；自定义 client 的兼容行为应另测。证据：`TestNoncoreClientDeadlines/clone_short_timeout` 失败，其余三个 deadline 对照及 header 隔离通过。

## RR-20261003-NC-03：P2 错误响应解码失败掩盖 HTTP 状态和原文

位置：`httpclient/client.go:178-186`。

DoJSON 在检查非 2xx 状态之前，先把响应体解码到成功响应 `out`。上游返回 503 文本/HTML，或 401 JSON 与成功结构不兼容时，提前返回 SyntaxError/UnmarshalTypeError，无法再通过 `errors.As(err, *StatusError)` 取得 HTTP 状态与错误体。

期望：已收到的非 2xx 响应能保留 StatusError 的状态和原文；兼容的错误 JSON 仍可按既有行为填入 out。实际：503 文本得到 `*json.SyntaxError`，401 `{"message":123}` 配 string 成功字段得到 `*json.UnmarshalTypeError`；两者均不能提取 StatusError。相同 503 在 `out=nil` 时正常得到 StatusError，兼容 JSON 502 对照也正常；200 非法 JSON 仍应属于成功响应解码错误。

影响：依据 401 刷新认证、429 限流或 5xx 重试/熔断的消费者失去必要分类信息，排障也丢失框架错误对象中的原文。已有错误响应测试只使用兼容成功结构的 JSON。

建议：复用现有 StatusError，先确立非 2xx 的错误分类，再尽力解码错误体；需要暴露解码错误时考虑包装/Join 且保持 `errors.As` 有效。不要破坏既有兼容错误体填入 out 的能力。证据：`TestNoncoreStatusErrorClassification` 六个分支，2 fail/4 pass。

## RR-20261003-NC-04：P2 BindJSON 接受首个 JSON 后的垃圾或第二个值

位置：`httpserver/server.go:233-245`，业务入口 `:218-231`。

ReadBody 已取得整段限长 body，但 BindJSON 只调用一次 Decoder.Decode，成功后不检查 EOF。`{"amount":1}garbage`、`{"amount":1}{"amount":999}` 都被当成完整有效请求，HandleJSON 调用业务函数并返回 200。

期望：JSON 请求体是一个完整 JSON 值，允许尾随空白；额外内容返回 400 且不进入业务函数。实际：两个反例均 status=200、business_calls=1，业务获得 amount=1。正常对象/尾随空白通过；非法首部 400、超限 413 且业务调用数为 0。空 body 按当前代码仍接受零值，本轮作为现有行为记录，没有强行认定其必须改为错误。

影响：无效 JSON 仍可触发写操作，并使完整 body 校验方与框架绑定结果不一致。本轮没有证明认证绕过、请求走私或具体业务资产损失。

建议：在现有 Decoder 上对第二次 Decode 检查 `io.EOF`，或针对当前先读全量 body 的实现采用 `json.Unmarshal` 的单值完整校验；保持限长、合法尾随空白和已声明空 body 兼容行为。无需引入第二套 HTTP 组件。证据：`TestNoncoreJSONRequestBoundary` 七个分支，2 fail/5 pass。

## 观察项与验证限制

- Client 响应 `io.ReadAll` 未限制字节数。这是容量/接入策略缺口，尚无既定上限与失败 SLO，未单独分配功能 bug 编号；后续 N02 要明确允许的响应体规模和错误体截断策略。
- App 的 Service/Mod 停机超时会保留尚可能被使用的依赖。这是当前源码的保护策略，不按资源未立即释放重复登记 bug。
- 本轮不覆盖真实代理/TLS/重定向、完整 gateway/auth/Webroute 启动、生产集群、HA 和长期容量；这些不由本机 7 包通过替代。
