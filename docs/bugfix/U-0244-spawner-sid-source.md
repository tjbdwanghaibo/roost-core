# U-0244：spawner 从错误的位置读 sid，启用刷怪的工程根本起不来

- 单元：U-0244 · 缺陷类 C4（跨层契约不一致）· 无 RR（**发版验证时自查发现**，已随 codegen v1.15.11 发出）
- 仓库：roost-codegen `demo/internal/service/game`
- 排障行：T-138 · U-0242 引入

## 问题

U-0242 让运行期实体 id 带上进程 sid，并且**sid 不可编码时在启动阶段拒绝**——这条拒绝是对的。
但 `processSid()` 从 `fctx.RuntimeConfig()` 取配置，而那个槽位**不是"进程的配置"**：
它保存的是最后一个写入者，`configdata` 在发布快照时会把它覆盖成 `*configdata.Snapshot`。

于是类型断言失败、sid 得到 0、allocator 拒绝、game 服务 `Init` 失败——**整个 game 进程起不来**：

```
Error: service game init: game: spawner: runtimeid: sid 0 is outside 1..65535
```

而三仓全套单测、生成工程全包测试、doctor 33 项**全部是绿的**。

## 根因

挑了一个"看起来像那件东西"的入口。`fctx.RuntimeConfig()` 返回 `any` 且没有所有权约定，
谁都可以写；`app.Registry` 从一开始就带着进程的 `*viper.Viper`，并且早就有 `Config()` 访问器——
我没找就先用了前者。

更一般的教训：**一个 `any` 类型的全局槽位不是契约**。要读"进程的什么"，应该从持有它的那个对象取。

## 改动

`demo/internal/service/game/spawner.go`：`processSid(registry *app.Registry)` 改用 `registry.Config().GetInt("sid")`，
并在注释里写明 `fctx.RuntimeConfig()` 为什么不是同一件事。core 无需改动——访问器本来就在。

## 证明

`demo/internal/service/game/spawner_test.go`（随工程生成）两条：

- `TestTheSpawnerReadsTheSidFromTheProcessConfiguration`：registry 带 `sid=1000` 时必须读到 1000，
  **并且在 `fctx.SetRuntimeConfig` 被别的东西占用时仍然读到**——这正是线上发生的情形；
- `TestAMissingSidIsRefusedRatherThanDefaulted`：没有配置 sid 时返回 0（让 allocator 拒绝），拒绝是特性。

修前：单测层面无法复现（旧实现在"恰好只有 viper 写过那个槽位"的测试里是绿的），
实跑即 `service game init ... sid 0 is outside 1..65535`，probe 打出 `type=*configdata.Snapshot`。
修后：发布版本的实跑全链路通过。

## 未做 / 边界

- **没有给这个坑加通用防护**：`fctx.RuntimeConfig()` 仍然是一个 `any` 槽位，下一个人仍可能把它当进程配置读。
  是否该给它加类型或所有权约定，写进了 `docs/bug/WANTED.md` 交 review。
- 这条说明"全套单测 + doctor 全绿"不能替代**一次真正的启动**：此前几轮的实跑一直做，这轮我在发版后才跑，
  所以它是被 v1.15.11 带出去的。发版流程里"先实跑再打 tag"应当是硬性的（见 T-138 的处置）。
