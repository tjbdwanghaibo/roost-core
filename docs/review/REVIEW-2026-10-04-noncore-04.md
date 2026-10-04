# 非三大核心第三批：N02 请求边界与正式 Webroute 消费

本轮先完成[NC-01～04 四项修复](REVIEW-2026-10-04-noncore-03.md)，再按 roost-review 检查 security/gateway/webroute 与实际生成消费者。新增[NC-05～07 三个未修 P2](../bug/REVIEW-2026-10-04-noncore-04.md)。新审查不改产品实现，反例/fixture 只放 docs；源码为 `483350ca438a690b1c80da56ce79a64d2154dcf6`，执行在提交前相同产品工作树。

## 当前范围与场景

N02 清单8个生产Go文件：本批新读 security3、gateway2、webroute1；之前HTTP client/server两文件的主链/修复证据复用，未有源码增量。现为 **8/8源文已读，场景部分验证**。另外读 Codegen Webroute parse/gen/Run/CLI 四个生产文件并实际生成、编译、注册、启动HTTP与退役验证；N08复用这条证据，不重复计为新的整个Codegen收口。

| 实际源文 / 主链 | 新执行 | 状态/未验 |
| --- | --- | --- |
| security/session_token.go、payload_signature.go | Token 到期前/整点到期、错player/secret、tamper、空secret、payload签名 10项控制 | 通过；完整业务撤销/跨传输、防重放协议未验 |
| security/ratelimit.go | 过大需求占key 1失败；持续补充/key上限/并发容量/同key tokens 4控制 | NC-05未修；满表扫描与IdleTTL预算需容量验证 |
| gateway/gateway.go、middleware.go | Recover4项含1失败；父deadline/新增deadline、无limiter匿名拒绝、取消已认证4控制 | NC-06未修；非协作endpoint/report阻塞与真实socket组合未验 |
| webroute/route.go | 3非法模式失败、JSON/raw/限长7控制、参数形状覆盖1观察 | NC-07未修；认证/错误映射业务策略、跨模块全矩阵未验 |
| codegen/internal/webroute/parse.go、gen.go、main.go、codegen/cmd/webroute/main.go | 正式CLI build，4隔离业务module；正常8、退役2、非法3，共13叶子执行 | 10控制通过/3行为失败；不是发布tag、部署或完整Codegen全域测试 |
| httpclient/client.go、httpserver/server.go | 复用已读当前不变source与原四项修复，关联回归 | 不将回归计为又读两新文件，不冒认TLS/代理/网络容量 |

**overlay 34个叶子/独立项 = 5失败 + 28正常控制 + 1仅观察**，exit=1；race/vet六包通过，111test pass事件、0fail/skip。正式消费者另13次叶子执行=3失败+10通过；五次顶层skip只是每阶段不选择另一个mode的测试，不作为验证场景计入，不是环境/编译失败。所有失败均有断言与实际数据，[原始日志/摘要/复跑](evidence/noncore-review-20261004-04/README.md)。不将不同层次同一路径反例计成多个根因。

生成消费者用当前正式CLI和local replace指向本次Core；合法模式通过真实回环HTTP请求验证。正常200、坏完整文档400/未进handler、超限413、业务error400/已进handler、raw GET通过。全部marker退役后生成文件删除，动态Module装配不再注册旧路由，真实POST/GET旧URL404；关闭此前RR-20260930-08列明的这项验证缺口，未重做整个旧Codegen修复审计。

## 图谱证据

Tier2 Verify，roost-core root/ready核对；generation仍`2026-09-30T11:58:14Z`，metadata_changed/new附件not_tracked以完整当前源码补证。security搜索40、gateway29、webroute29、codegen/internal/webroute31、CLI2，分页has_more=false。Recover/AllowN/VerifySessionToken/Registrar.Register/GenerateDir双向depth1与关键片段核对；同名heuristic边未当作真实调用。

最初猜测codegen/webroute与kit/http*结构查询0结果，后改为真实codegen/internal/webroute及CLI，前者不是不存在证明。未发现节点不推定没有Kit/模板/动态Module使用；业务consumer在隔离工程实际注册补证。[coverage](evidence/noncore-review-20261004-04/graph-coverage.json)与源blob清单覆盖证据路径，无记录gap不保证穷尽。未停止/重建共享索引。

## 设计、性能与后续

[请求准入与生成路由机制](IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md)区分token校验/业务身份、限流tokens/key容量、context预算/实际退出、parser/生成/路由注册/handler副作用。建议复用现有RateLimiter、Recover及chi校验，不引入新网关框架。

性能仅为源码分析：满表新key在全局锁内O(N)扫描；DecodeJSON先全量body再string/decoder/extra值，需受限body；schema复制、checker聚合另见N01。没有本轮benchmark/线上SLO/真实传输负载，不能写吞吐或延迟提升。参数名不同而形状相同的路由静默覆盖为契约观察，未升RR。

N01/N02源文均已读但场景尚未全部收口；新NC-05～07未修。下一批进入**N03 bus/nats/servicerpc/etcd**请求关联、取消/订阅关闭/恢复，Kit同行；N01阻塞/Group预算/完整App故障与N02满表/跨模块/真实业务鉴权余项保持台账。用户未修时跳过旧验收继续新范围。其他三大核心与Service/Codegen已声明历史证据复用，不改另一agent模块。

本轮技能镜像一致，未新增后台定时任务、发版、tag、部署、外部资源HA/长稳。修复提交与本份docs审查分开；预算更新见[剩余计划](NONCORE-REVIEW-PLAN-2026-10-03.md)，不从文件读取率或测试数计算全仓覆盖率。
