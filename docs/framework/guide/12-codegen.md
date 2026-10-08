# 次核心：Codegen 与工程工具：设计与使用

适用运行时：v1.24.0；文档维护版：v1.24.1。[实现与维护入口](../impl/12-codegen.md) · [模块总目录](../README.md)

## 先理解这一块

Codegen根据业务定义生成重复的连接代码，让你少写DAO、消息调用和协议注册等样板。

先修改手写定义再生成；不要只改生成文件，否则下一次生成会覆盖修改。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 工具边界

codegen/cmd/roost 是统一 CLI；内部生成器负责 DAO、Entity、Nest、Protocol、ServiceRPC、配置表、事件、属性、WebRoute、错误码与项目模板。它们生成适配代码，不取代运行时的锁、持久化和身份校验。demo 是嵌入的 game-demo 模板，不是另一个维护版本。

## 2. 使用流程

用固定版本 roost 创建工程 → 在 roost.yaml 声明模块、版本策略、能力和生成目标 → 添加业务模型/handler → 执行生成 → 编译测试 → doctor/配置检查 → 启动。变更生成器或框架 API 后重新生成业务工程，禁止只手改生成物让当前编译通过。

当前生成物最低 core 是 v1.24.0；文档补丁版不引入新运行 API，因此不需要无理由抬高最低版本。release manifest 的 release 必须等于实际发布 tag。工具输出 roost-codegen 名称不代表仍是独立模块。

## 3. 生成契约

DAO setter 把修改登记到当前事务，RestorePersisted 严格校验当前 schema；Entity 生成 subjectPacker/PrepareViews 所需能力及注册；Nest marker 支持明确持久策略；ProtocolRegistry 区分 PB/Sync/Lockstep 并验证端点载荷类别；ServiceRPC 的传输和 Mod 装配分开生成。

生成标记用于识别产物，不等于授权覆盖任意用户文件。项目生成的全阶段成功后才发布目标；失败保留可诊断错误且不能留下看似成功的半工程。重复生成应无漂移，根模板与 goldens、正式消费者需要同时验证。

## 4. 版本与发布

go generate ./... 检查已提交生成物；pretag 检查 tag 尚不存在、模块 major、无 replace、干净树、release manifest、生成漂移、build/vet/tidy/test。不能先打不可用 tag 再等待 CI 来发现错误。真实 tag 消费测试不用本地 replace，和 source-head 测试分别记录。

minimum/released lane 和部分 runtime 脚本需要已发布模块；离线本地测试不能替代代理解析或真实下载。CLI 项目接线和服务容量是不同验收层。

## 5. robot 与检查器

robot 使用正式协议做会话、动作、场景和负载；错误数、拒绝数、重连与成功吞吐分开统计。glsvet 检查快池违规等待、上下文和部分时钟使用，是静态守卫，不能替代并发运行测试。

详细 CLI 参数、注解语法、manifest 字段与项目目录见 codegen/docs，维护本页只描述跨生成器共同契约。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
