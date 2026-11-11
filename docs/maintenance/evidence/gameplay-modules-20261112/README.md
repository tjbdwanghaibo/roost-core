# 场景线与补充业务模块验收（2026-11-12）

范围为 `feat/business-modules` 的场景线八个模块和 naming、cdkey、notice、gacha、friends、idle 六个模块。基线为 `ec7661037e577d88604ea3e42191e6eb6510a9bd`，结果包含本批未发布代码；使用 macOS、本地 source-head replace、`GOWORK=off`。场景采用正式生成 DAO / lifecycle / Nest / Sync 接线，持久提交与部分外部客户端使用替身；不是生产存储或跨机验收。

## 生成工程与仓库检查

- **最终完整组合脚本退出 0**：从空工程正序、反序安装全部 29 个模块、15 组联动（其中 14 组有独立验收项），两套工程各 43 项报告、598 PASS、0 SKIP；build / vet / ID / 生成物漂移、全包测试、重复安装与工作树不变断言全部通过。之前的九个增量/旧模块阶段也完整通过。见 [foundry](foundry-verify.json)、[citadel](citadel-verify.json)、[执行摘要](validation-summary.json)、[完整输出](module-combinations.log.gz)。最终只读脚本 SHA256 为 `8b09d5a29095caca7f70f9c187287b93c84bbb0a2640de32f58beb19188a19a8`，运行期间源码保持冻结。
- 首次全仓 `go test -json -count=1 -timeout 30m ./...`：9,770 PASS、99 条条件 SKIP、2 FAIL。两个失败分别是生成 Mod 文档遗漏 Directory，以及旧测试错误要求 self 视图包含 `nosync` 的 AttributeGear。没有放宽同步字段边界；补文档并修正测试，原失败入口分别复验通过：[文档/安装](documentation-and-install-green.log)、[C# SDK 与生成 TCP 工程](client-generated-green.log)。原失败名称与计数保留在 [摘要](go-test-summary.json) 和 [失败摘录](first-repo-failures.log)，不把补跑描述成第二次全仓通过。
- 全新自定义 module 的 game-demo：774 PASS、14 条预期 SKIP、0 FAIL；49 个有测试的包通过。随后发现 Robot YAML 仍使用旧属性字段名，补了从 YAML 检查真实 DAO WireName 的回归并修正配置，整个 `cmd/loadtest` 包 race 通过：[修前](robot-fields-red.log)、[修后](robot-fields-green.log)。新增回归不计入此前的 774 条。
- game-demo 的 `module verify`：16 项报告、229 PASS、0 SKIP，build / vet / ID / 生成物漂移四项全部通过，见 [JSON](demo-verify.json)。全仓 build / vet 与根包门禁分别通过。
- Nest 生成器导入作用域修复后再次通过全仓 build / vet、根包门禁，并用新 CLI 检查上述 game-demo 无生成物漂移；没有为这处局部修复重复整轮全仓测试。
- Actor 单一同步工厂的安装顺序、自身视图、显式业务 packer 冲突回滚见 [日志](actor-sync-orders.log)。Nest `RequestMultiSettled` 的实际完成/取消/排空契约 race 验证见 [日志](nest-settled-race.log)。
- CBM 在最终源码冻结后刷新，generation `2026-11-11T18:36:27Z`，增量覆盖记录更新于 `2026-11-11T18:48:29Z`。已检查本批 337 个新增/修改证据路径：240 个未记录解析缺口、64 个局部解析、33 个按规则排除；代码路径均与当前文件元数据一致，全项目无 skipped。局部缺口多为模板编号占位和复合字面量，已逐段直接读源码补证；文档/脚本按排除规则直接检查。图谱没有记录缺口不等于语义完整，也不能替代生成工程编译与行为验证。

## 本轮发现并修正的生命周期缺陷

Monster 的退出事务可能已经离场，但成功回复丢失；Scene 若只对仍有成员的 kind 调用退出者，会漏掉等待 lifecycle 销毁的记录。Scene 现在在管理锁外调用全部已登记退出者，各退出者按 Scene 实例过滤；Monster 只在明确收到 Remove 成功后记退休状态，失败保留重试责任。[修前](monster-drain-red.log)、[修后](monster-drain-green.log)。

仅在投递前比较 EntityManager 当前指针仍有时间窗口：检查后旧实例被正式销毁并以同 ID 重建，后续按 ID 排队的 Sender 会取得新实例。旧实现的精确复现见 [修前](monster-toctou-red.log)。现在在创建记录时固定不可变的本地 `monster.Target`，Spawn / 队列 respawn / Damage / Remove 的 handler 取得实际 Monster 和 Scene 锁后先比较实例身份。旧 Scene 被清理后的 ID 不再重读；Target 不持久化、不跨进程，也不替代同实例内的 LifeSeq。

这两个问题共同暴露了“ID 或成员列表足以证明资源归属”的错误前提。本轮将退出责任保留在管理记录，将实例授权检查放在真正持锁执行的位置；没有用延时或扩大重试预算掩盖窗口。8 个确定交错分别覆盖四个入口遇到 Monster / Scene 同 ID 重建，使用真实 Nest 和生命周期；Monster 共 25 个场景 race 通过，11 个可编译变异全部检出：[生成验收与变异](monster-target-generated-green.log)、[race](monster-target-race.log)。

Combat 另修复既有适配器成功结果遗漏 `ExpectedFailureNone`，说明和红绿证据见 [RV-20261112-01](../../CORE-REVIEW.md#2026-11-12战斗适配器结果契约rv-20261112-01p1已修复)。

完整组合随后抓到另一处生成器缺陷：不同 handler 源文件的合法同名导入别名被合并，导致 Dungeon + Terrain 同装时生成代码编译失败。原失败报告为 43 项中仅 3 项通过，build/vet 失败，ID/漂移检查通过；保留[失败摘要](first-combination-failure.json)。修复删除包级 imports 合并，保留文件级类型解析与跨文件请求字段元数据；回归与整包测试见 [RV-20261112-02](../../CORE-REVIEW.md#2026-11-12nest-生成器文件级导入rv-20261112-02p2已修复)。业务模块别名保持原样。

修复导入后的全组合预验得到 592/598 PASS，四项工程检查通过；剩余六条是 Dungeon/Terrain 夹具错误：四条假定每个 DAO 都有 `Marshal()`，遇到附加的 CombatDao 失败；两条把首次事务的文档数写死，漏算了版本 0 附加 DAO 必须随主 DAO 一起创建的文档。没有改生产提交逻辑；夹具统一通过 `nest.MutationParticipant.PrepareMutation(AllFields)` 冻结完整 BSON，核对 schema、文档身份和版本，既不 Accept 也不虚增持久版本。地形两条仍固定四个 Chunk 的精确坐标，再与 Player 主 DAO、首次附加 DAO 和建筑键逐个核对，不放宽为数量下限。[六条修前](aggregate-fixtures-red.log)、[六条 race 修后](aggregate-fixtures-green.log)。修后全组合预验 43 项 / 598 PASS / 0 SKIP、四项工程检查全部通过；两个模块的 fresh 安装、联动与可编译变异专项也通过，见[日志](dungeon-terrain-generated-green.log)。

## 复跑与边界

组合验收入口为 `bash scripts/test-modules-generated.sh`，需要允许本机 TCP 监听；脚本创建独立临时消费者，固定模块通过数，检查增量、反序、重复安装与四项工程检查。模块定向回归入口是 `GOWORK=off go test -count=1 -timeout 30m ./codegen/internal/roost -run 'TestModule|TestDemo'`。按模块 README 运行真实依赖验证时需另外准备隔离资源。

未覆盖：本批模块的真实 Mongo/WAL/Redis/NATS/Gate 联合验收、Linux、跨机、性能与长稳；demo 的 cellworld、LOD/队伍视图与场景/怪物/地形专有适配也没有全部迁为模块。Combat 当前是本地固定双人战斗，Monster 不自动攻击；模块 README 和 KNOWN-LIMITS 中的功能边界继续有效。没有合入 main 或发版。
