# Bugfix 记录

`docs/bug/` 是只审查不改码的发现（RR 编号）；本目录记录这些发现是怎样被修掉的：
改了哪几行、为什么这样改、用什么测试证明修前红修后绿、留下了什么没做。
每条一个文件，编号沿用 RR，账本单元编号（U-）指向 [history/ledger.md](../history/ledger.md)。

| 编号 | 仓库 | 问题 | 修复单元 | 记录 |
| --- | --- | --- | --- | --- |
| RR-20260908-01 | kit | Session `Enter` 丢掉幂等账本 Create 的落败结果，两个 owner 都成功 | U-0154（方案 B） | [RR-20260908-01.md](RR-20260908-01.md) |
| RR-20260908-02 | core | ReadThrough 跟随者取消后不归还等待名额 | U-0155 | [RR-20260908-02.md](RR-20260908-02.md) |
| RR-20260908-03 | codegen | `--consolidate` 对单行 import 的混合分流产出非法 Go | U-0156 | [RR-20260908-03.md](RR-20260908-03.md) |
| RR-20260909-01 | core/docs | Quickstart 固定过时 codegen 版本且缺 `-module` | U-0157 | [RR-20260909-01.md](RR-20260909-01.md) |
| RR-20260909-02 | kit | Session 撞 RequestID 的撤回误删同 owner 重新取得的 claim（ABA） | U-0158 | [RR-20260909-02.md](RR-20260909-02.md) |
| RR-20260909-03 | core | Assembly 停机未完成就忘掉 Runtime，重试虚报成功 | U-0159 | [RR-20260909-03.md](RR-20260909-03.md) |
| RR-20260909-04 | codegen | 同包多个 Entity 生成重名注册符号 | U-0160 | [RR-20260909-04.md](RR-20260909-04.md) |
| RR-20260909-05 | kit | Enqueue 幂等重放返回其他 Subject 的 Ticket | U-0164 | [RR-20260909-05.md](RR-20260909-05.md) |
| RR-20260909-06 | codegen | 多 Entity 配合显式 `-output` 静默覆盖生成结果 | U-0162 | [RR-20260909-06.md](RR-20260909-06.md) |
| RR-20260910-01 | core | 极短 IdleTTL 推导出零扫描周期 | U-0163 | [RR-20260910-01.md](RR-20260910-01.md) |
| RR-20260910-02 | kit | 已领取邮件淘汰后重投生成新发奖 token | U-0165 | [RR-20260910-02.md](RR-20260910-02.md) |
| RR-20260910-03 | core | 完成事务的等待者因缓存淘汰读到已删记录而 panic | U-0168 | [RR-20260910-03.md](RR-20260910-03.md) |
| RR-20260910-04 | core | Checkpoint 恢复按 ID 重排完成历史,淘汰顺序分叉 | U-0169 | [RR-20260910-04.md](RR-20260910-04.md) |
| RR-20260910-05 | codegen | category 指向业务包常量时生成物遗漏 import(M-05 引入) | U-0166 | [RR-20260910-05.md](RR-20260910-05.md) |
| RR-20260910-06 | codegen | 聚合注册的固定 entity 导入与业务包别名冲突(M-05 引入) | U-0167 | [RR-20260910-06.md](RR-20260910-06.md) |

重构类改动(不对应任何 RR,不关闭任何 RR)另记,编号 M-:

| 编号 | 仓库 | 改了什么 | 记录 |
| --- | --- | --- | --- |
| M-01 | core | entity kind 注册表改为按 kind 的无锁定长表;含 category 重设计四步方案,仅第一步已实施 | [M-01-entity-kind-registry.md](M-01-entity-kind-registry.md) |
| M-02 | core | category 离开 EntityID,注册表成为唯一权威;ID 那两位降为历史填充,零数据迁移 | [M-02-category-leaves-the-id.md](M-02-category-leaves-the-id.md) |
| M-03 | core | 声明 category 后值即锁序,remote 档强制最先,锁序注册期派生;新增 `ValidateEntityRegistry` | [M-03-category-lock-order.md](M-03-category-lock-order.md) |
| M-04 | core + codegen | **破坏性**:删除 `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*`;推荐分类常量;codegen 拒绝 `remote=capable`、脚手架默认 Other | [M-04-drop-capable-and-the-group-hook.md](M-04-drop-capable-and-the-group-hook.md) |
| M-05 | codegen | `category=` 进实体标记并直接进生成物,拆掉运行期查表的隐式前置;生成的聚合注册末尾调 `ValidateEntityRegistry` | [M-05-marker-owns-the-category.md](M-05-marker-owns-the-category.md) |

写法约定：**问题**（一句话）→ **根因**（指向具体行）→ **方案选择**（列出考虑过的方案与取舍）→
**改动**（文件与要点）→ **证明**（红测试名、修前失败文本、修后结果）→ **未做 / 边界**。
前四项已随 core v1.15.2 / kit v1.14.3 / codegen v1.15.4（2026-09-09）发版；RR-20260909-02/03/04 已修复待下次发版（core v1.15.3 / kit v1.14.4 / codegen v1.15.5）。
