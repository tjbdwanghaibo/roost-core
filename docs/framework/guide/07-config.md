# 配置、数据表与热更：设计与使用

适用版本：v1.28.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/07-config.md) · [模块总目录](../README.md)

## 先理解这一块

服务器地址、连接超时是进程配置；怪物血量、掉落表是游戏数据。两者的加载和更新规则不同。

先选对配置入口，再看热更。不要为了方便，让每个模块私自读一份同名配置。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 两类配置

进程配置定义地址、连接、容量、超时和开关；业务表描述玩法数据。前者用 Mod/Service 的 ConfigSchema 结构体与 tag 声明，后者用 configdata 的表定义、规则、快照及生成器接入。不要把表热更当成运行中修改网络基础设施配置。

## 2. 单一声明

键路径、类型、默认值、必填、敏感值及示例声明一次，运行期读取、启动检查、生成默认配置与 doctor 共用。未声明键、非法值、缺失必填不能静默降级成零值。Validator 表达开启功能后的条件必填或跨字段约束。读另一个 Mod 的键、要求两边一致的规则（例如 saga 的效果流必须就是同进程 DataEngine 的那条）由 Mod 实现 `app.ModConfigCrossChecker`：App 在本服务全部声明检查通过之后调用一次，启动前、`--check-config` 与 doctor 的 `config-check:<service>` 走同一处，读别的 Mod 的键仍只经它的声明。

跨**服务**的规则单个进程查不到，同样写在声明上：`shared:"<资源名>"` 标记同一工程各服务必须取同一个值的共享资源名（总线前缀、效果流、saga 库等），`follows:"<键>"` 说明没写时取哪个键的值，`listen:"<开关键>"` 标记监听地址。`roost config check --all` 与 doctor 的 `config-across-services` 按这些 tag 比较各服务的配置（共享资源名一致、开发配置集里监听端口不冲突），新增共享资源或监听地址时只在声明上加 tag。

secret:optional 表示敏感值允许为空，非空生产值仍不能使用 dev- 凭据；secret:true 的生产非空要求保持。不要把可选敏感声明解释成启用鉴权仍可空密码。

## 3. 业务表发布

读取/解码候选数据 → 执行字段和跨表规则 → 建立完整候选快照 → 发布及通知 → 按事务式协议处理失败/回滚。所有读者以一个快照版本为视图；不能分表先后更新导致同一请求读到混合代际。键大小写保持定义的语义，不由配置框架擅自折叠。

手写 TableDef、cfg tag 自动表、外部生成表均是入口；选一条对应业务源数据，不重复注册同一表。tablegen、cfggen 是不同源格式与生成契约，生成后仍需运行规则校验。

## 4. 请求内一致性

异步消息在每次准入时捕获当前 RuntimeConfig，排队后保持一代；同步调用继承当前请求代际。热更之后新消息见新版本，不能要求旧排队消息突然切换。featureflag 回滚恢复发布前实际视图，包括 GM 覆盖，而非从一个过时默认表重建。

## 5. 运维重载

接了 configdata Mod 的进程自带三条运维命令，经 ops 的 `/admin/execute` 调用（`ops.admin_enabled` 与 admin token 照常生效），与 `hotcode.*`、`bus.dlq.*` 同一处注册，业务不用手写：

| 命令 | 参数 | 回答 |
| --- | --- | --- |
| `configdata.reload` | `reason?`、`dry_run?` | `version`（新版本）、`previous`（发布前的版本）、`hash`、`changed`、`changes`（`added` / `removed` / `changed` 的表与 `rows_before` / `rows_after`，`unchanged` 个数）；`dry_run` 只构建校验、报告变化、不发布 |
| `configdata.rollback` | `reason?` | 回到上一代（只保留一级），字段同上 |
| `configdata.status` | 无 | 当前 `version`、`hash`、`loaded_at`、`dir` 与各表行数 |

重载只读 `config_data.dir` 下的 JSON。改了 `configs/table` 下的 CSV 要先 `roost generate` 生成 JSON，再重载；忘了这一步时重载照样成功，但回答 `changed=false` 并在 `message` 里写明“no table changed”，日志是一条 Warn，指标记 `configdata.reload.total{result="unchanged"}`。内容没变的重载仍然发布新版本号（维护者 2026-10-18 决定，方案 B）：版本号不是内容版本（失败的构建也会占用一个号），而监听者依赖每次重载都执行——例如 game-demo 的开关表用重载把 `gm.flag.set` 的进程内改动恢复成表里的值。判断数据是否真的变了，看 `changed` / `changes` 或 `hash`。违反表规则（必填、唯一、范围、枚举、引用）的数据整次拒绝，错误写明表 / 行 / 字段 / 规则，旧快照继续服务。

```bash
curl -s -H 'X-Admin-Token: <token>' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"configdata.reload","payload":{"reason":"shop price 25->99"}}'
```

## 6. 维护

发布前验证 hash、规则、监听者失败和回滚行为；有副作用的监听者必须定义补偿。table manifest 的 generated_at 未填充不能用于判断更新时间。日志不输出 secret；敏感配置和环境文件不进入 Git。hotcode 真实插件受目标平台/工具链约束，不能把编译通过写成 Linux 插件实机验收。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
