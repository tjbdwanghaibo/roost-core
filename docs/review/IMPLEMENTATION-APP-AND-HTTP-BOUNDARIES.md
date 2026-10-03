# App 生命周期与 HTTP 接入机制

源码基线：`746567ff37cf69878b74331cb9bcc69b159dfb63`；2026-10-03。事实来自当前源码；已执行场景见[本轮记录](REVIEW-2026-10-03-noncore-01.md)。下面的建议尚未实现。

## App 的启动、能力发布与停机

`App.Execute → run` 先加载/校验配置、初始化日志和 Registry，再对共享 Mods 作依赖排序、逐阶段 Init → Provide → Start；随后用共享 Mod 名集合排序服务专属 Mods，同样分阶段执行。业务 Service.Init 消费 Registry，之后 Service.Serve 在独立 goroutine 中运行。

Registry 初始提供 health、metrics、admin、lifecycle 和 RuntimeFailure。`RegisterBatch` 在锁内先检查所有名字，再统一写入，拒绝重复时不会发布部分能力；`Lookup[T]` 将缺失/错误类型转为 false。Registry 是能力注册机制，不替每个能力定义初始化事务或销毁协议。

`sortMods` 将硬依赖当作必须存在的边，将 optional 依赖当作已安装时的排序边，允许服务 Mod 依赖此前初始化的共享 Mod；正常排序保持注册顺序。**当前单元素快速返回绕过了全部规则**，这是 [NC-01](../bug/REVIEW-2026-10-03-noncore-01.md#rr-20261003-nc-01p2-单-mod-绕过依赖和输入校验) 的根因。

信号监听先于 Serve 启动。App 等待终止信号、RuntimeFailure 首个故障或 Serve 退出；随后取消 Serve context，发 stopping 生命周期事件，并并发等待 Serve 和 Shutdown 完成。RuntimeFailure 的首次报告唤醒停机，后续错误 Join 供诊断，不是持续消费的事件总线。

Service 两条执行链均完成后，先反序停止服务 Mods，再反序停止共享 Mods，使用同一个 shutdown.total_timeout。`ModStopBudgetProvider` 可声明预算；未声明者有 3 秒规划保底，预算不足时按既有规则缩放/均分。`stopModSafely` 隔离 Stop panic，并受 context 截止约束。Stop 超时不能杀掉 goroutine；如果 Service 或 Mod 仍可能访问依赖，App 返回错误并保留后续依赖，等进程退出。这个行为用于避免释放依赖造成并发 use-after-close，不能承诺不配合 context 的用户代码自动结束。

边界：Init 失败后的资源由谁回收、生命周期回调阻塞预算、manager/lifecycle 自身实现及完整 Kit 的装配顺序仍需 N01 后续审查；本轮没有把它们写成确认缺陷。Seven-package race 通过只表示现有场景的结果。

## HTTP Client 的请求与配置所有权

`PostJSON → DoJSON → json.Marshal → NewRequestWithContext → 配置 headers/request ID/签名 → http.Client.Do → ReadAll → 解码/状态错误`。签名覆盖实际 marshaled body；连接与 Transport 由标准库 client 管理。WithRequestID 使用自己的 context key，默认 Header 在 clone 时深拷贝 values。

New 在未注入 HTTP client 时创建内部 client，Timeout 来自包装层选项。Clone 深拷贝 header，却共享内部 HTTP client 指针；修改 wrapper 的 timeout 没有改变标准库实际 deadline（NC-02）。修复应保护父实例和连接池共享关系，并把自定义 client 的优先级说明清楚。

收到响应后，当前实现先按成功类型解码，再检查状态（NC-03）。HTTP 状态属于已收到响应的传输结果，out 的类型属于消费者的解码选择；错误分类不能被后者吞掉。已有兼容错误体能填入 out 的行为可继续保留，成功状态的坏 JSON 仍应报解码错误。

## HTTP Server 的绑定和业务调用门槛

`Engine → requestContextMiddleware → chi 路由 → HandleJSON → BindJSON → ReadBody → fn → JSON`。context 保存 request ID 和引擎 body 限额；ReadBody 使用 MaxBytesReader，超限返回 413，读取/首个解码失败返回 400。恢复中间件为尚未写出的 panic 返回 JSON 500；本轮仅复用既有测试，不称所有半写响应行为已验。

BindJSON 当前先读完整 body，再用 Decoder 只解码首个值，因此“Decode 成功”不等于“整个 body 是合法单个 JSON”（NC-04）。写业务 fn 是明确的副作用边界，应在调用之前完成单值校验。合法尾随空白继续接受；空 body 返回零值是现有行为，应由契约/业务必填校验决定是否另行限制。

设计评价：App、HTTP 都已有可复用的能力与边界，本轮四项修复方向可基于现有 Mod/Option/StatusError/JSON reader 完成。没有依据建议替换框架或新增接入层。性能评价限于源码机制：请求/响应全量 materialize、JSON 编解码与 clone header 都有分配成本；没有本轮 benchmark、生产负载或 SLO，故不量化吞吐与延迟。

## 10-04 修复后的实际机制

上文“当前”描述的是 10-03 审查基线，原失败证据保留。[NC-01～04](../bugfix/README.md) 本轮已经修复、具名场景验证，尚未发版。

`sortMods` 仅对空集合快速返回；单元素复用原有校验和排序，不复制一套规则。Client 记录内部 client 的所有权；Clone 仅在内部 client 的 timeout 改变时复制 `http.Client` 值，保留共享 Transport，避免改变父实例。外部 client 仍由调用者配置，选项顺序不改变这一优先级。

`DoJSON` 先建立非 2xx 的 `StatusError`，读取/解码出错再 Join 原因；可兼容解码的错误响应仍填入 out，200 的读/解码错误保持直接返回。BindJSON 对已经受限读入的完整 body 执行 Unmarshal，业务副作用前完成整段校验。没有增加额外无界 reader。

原 30 场景全部转绿，补充 15 项父子配置、外部 client、context、并发 Clone、partial body/Close 和错误原因对照。最终 11 个关联包 race / vet 通过，194 个 test pass 事件包含父/子/Example，不等于 194 条独立业务场景。原 review overlay 也全部通过。[执行证据](../bugfix/evidence/noncore-bugfix-20261004-01/README.md)。
