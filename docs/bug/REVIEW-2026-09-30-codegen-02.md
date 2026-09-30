# Codegen 第二轮新增问题：Entity / Nest 输入退役后留旧生成物

基线 Core `f6566d4e0a9026484d66d78f53036bbd9bc5f3fc`，Core 内 `codegen/` 为当前实现。本轮先修 [RR-20260930-01/02](../bugfix/RR-20260930-01.md) 后继续审未覆盖的 entity、nest、registry 输入删除链；以下两项只记录，**未修复**。[运行](../review/REVIEW-2026-09-30-codegen-02.md) · [隔离复现](../review/evidence/codegen-review-20260930-02/README.md)。

## RR-20260930-04 · P2 · entity 最后标记删除后旧 wire 与注册仍留存

- **源码：**`codegen/internal/entity/main.go:55-78,84-132,135-174`。`findEntityDirs` 仅返回当前仍有 marker 的目录；零目录直接成功返回，既不枚举也不清理由该目录上次生成的 `<entity>_gen_wire.go`。`codegen/internal/entity/gen.go:345-370` 的 wire 含 `//roost:register phase=entity`，`codegen/internal/registry/parse.go:56-89` 会扫描普通生成 Go 文件；旧注册继续被纳入聚合是基于这两处源码的调用链推断，尚未独立运行最终聚合。
- **复现：**隔离目录用当前 CLI 生成 `player_gen_wire.go`，再把唯一 `//roost:entity` 改为普通注释并重跑。两次都 exit 0；第二次输出 `no entity markers found`，旧 wire 仍存在，且第 16 行仍有 `//roost:register phase=entity`。该输出与当前输入不一致，可能维持已删除的实体构建入口；具体消费者构建/运行后果待业务工程验证。
- **实施方向：**以目录和生成头/固定命名建立当前预期集合，零 marker 与实体改名都对账；删除 wire 时同时处理其生成的 guard test，保持同包多实体的注册函数唯一。避免误删手写同名文件，测试需覆盖 registry 聚合的真实输出。

## RR-20260930-05 · P2 · nest 最后标记删除后 wrapper/sender 仍留存

- **源码：**`codegen/internal/nest/main.go:46-175` 只遍历仍有 `//roost:nest` 的源文件并写当前产物，`len(funcs)==0` 直接跳过；未见按生成头/预期源文件集合清理旧 wrapper、sender、syncsender 与 sender guard test 的路径。
- **复现：**隔离目录生成 `handler_nest_gen.go`、`sender/handler_nest_gen.go`、`sender/handler_nest_gen_test.go`、`syncsender/handler_nest_gen.go`；移除唯一标记后重跑输出 `all files up to date`、exit 0，三个 `*_nest_gen.go` 及 guard test 仍在。对根目录名为 `game` 的正式生成链，aggregate bootstrap 会按新 marker 集合刷新；旧 per-file API 的保留与实际注册/调用结果仍需消费者验证，不能把这个反例写成 bootstrap 一定继续注册。
- **实施方向：**按扫描范围的预期输出集合处理四类自身生成文件，并核对 `-sender=false`、源文件改名、接收者 handler、game bootstrap 与手写文件边界。保持出错时不提前删除可用产物。

两项都经当前 CLI 的“先生成、删标记、重跑”观察到输出留存；是否会导致具体业务调用错误，要在正式生成工程中继续验收。与 RR-20260930-01/02 同属输入退役问题，但发生在不同生成器，未包含在本轮改码授权的原两项范围中。
