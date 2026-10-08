# 观测、安全与运维：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/11-observability.md) · [模块总目录](../README.md)


## 1. 观测分层

metrics 记录 counter/gauge/duration/histogram；health 评估组件状态，ops 提供探针与管理面。指标、健康和审计不能互相替代：错误计数增加不一定需要退出，fence 也不能只靠仪表盘人工发现。

## 2. readiness

服务 started/stopping 生命周期控制就绪位；Fail 阻止 readyz，Degraded 表示仍可接流量但需关注。Nest 队列、持久积压、entitysync 容量和驱动状态由正式装配注册。依赖缺失、恢复未完成应按业务准入契约处理，不把 HTTP 活着解释成服务可写。

## 3. 指标

名称与 dashboard/OBSERVABILITY.md 由根包门禁对齐。counter 是累计数，gauge 是当前值，时延分位数来自 histogram，不能从一个 duration 累加值声称 P99。统计窗口、拒绝数、全量最大值和成功样本吞吐分别报告。

ExportMetrics 的存在不代表项目已向指定监控平台接线。业务新增指标应控制标签基数，禁止 playerID/requestID 作为无限标签。

## 4. 日志与失败记录

结构化日志包含请求/实体上下文；跨 goroutine 必须明确传递，不假定本地上下文自动复制。系统时间用于审计排序。failurelog 是有界记录存储，裁剪与写失败要可观测，不能承诺无限保留；外部平台的长期归档由部署负责。

## 5. 管理与安全

admin 元数据注册不是完整的人工审批系统。管理入口认证、参数限长、审计、调用超时和危险操作授权需要一起配置。令牌/HMAC 只证明持有相应凭据，业务对象权限仍由服务检查。默认开发凭据不能用于生产；密钥不进日志、文档或 Git。

httpclient/httpserver 是外部 HTTP 集成基础，保留公共 API 不表示框架所有链路都默认使用它们。接入方负责 TLS、代理、网络策略、限流及取消传播。


## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
