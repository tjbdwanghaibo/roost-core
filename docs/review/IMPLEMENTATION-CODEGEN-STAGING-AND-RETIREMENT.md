# Codegen 现有生成链：暂存、提交与退役语义

基于 Core `1b7a2fc5aa4bfd9ae921ae8c80efd2392d08a165`；这是当前实现学习记录，拟议改进见 [Codegen 两项问题](../bug/REVIEW-2026-09-30-codegen-01.md)，尚未实施。

`roost generate` 的写入模式先由 `GenerateTransactional` 在项目同级建临时树，复制项目并对应用拥有的输入取 SHA-256 快照。它按 manifest 选择 DAO、event、errcode、protocol、entity、nest、attribute、config/table、webroute、RPC，最后无条件运行 registry；各生成器在暂存树内写文件。全部成功后运行 `go mod tidy`，规划生成物和 `go.mod/go.sum` 的差异，再核对原项目输入是否被并发改动。提交前每个目标还核对规划时的旧内容；出错则逆序回滚已提交项。这是“失败时尽量恢复”的逐文件协议，进程被强杀或回滚自身失败仍需额外验收。源码入口：`codegen/internal/roost/generate.go:61-136,163-236,241-306` 和 `codegen/internal/roost/project.go:256-366,502-572,600-685`。

生成器的**写入幂等**与**删除幂等**是两个不同契约。暂存复制了原有生成文件；当某个生成器遇到空输入直接返回时，旧文件在暂存树中不发生变化。上层 `planStagedProjectCommit` 只能比较暂存树与原树，看不出“这个输出已不再期望存在”。要完成退役，具体生成器需根据其拥有范围显式删除暂存树中的孤儿；然后上层才会在原树规划删除并保留旧字节以便回滚。DAO 的 `removeOrphanGenerated` 正是这一模式。当前 servicerpc 零服务返回和 protocol 空定义返回缺少对应动作，故最终提交层无法自行推断应删除什么。

`servicerpc` 从 `//roost:rpc` 接口直接解析方法、生成传输和装配两半；`-check` 在仍有接口时逐文件比较，CRLF 被规范化。`-emit transport/assembly` 与 `-out` 允许跨模块装配，因此所有权不能仅凭目录判断，须结合生成头、文件命名与本次半边选择。当前 `len(services)==0` 时无输出目录对账，旧代码依旧存在；[隔离复现](evidence/codegen-review-20260930-01/README.md)证实这一点。源码：`codegen/internal/servicerpc/parse.go:88-152`、`run.go:15-131`。

`protocol.Run` 从定义写 `protocol.proto`、`protocol.pb.go`、`msgid_gen.go`、manifest，并可写 player bind、robot registry、handler/bootstrap。空定义且无 bootstrap 时提前返回，有 bootstrap 时只写空 bootstrap，均没有处理其余旧产物；非空路径写 handler 也是遍历“新文件”，未比较旧文件集合。这意味着删除输入可能保留旧消息类型/ID；当前隔离复现确认最后定义删除后的四个固定文件仍在。是否应删除旧 ID 需结合上线兼容窗口判断，生成器的默认退役策略应显式约定，不能仅凭文件系统差异决定网络协议兼容性。源码：`codegen/internal/protocol/run.go:26-190`。

接入方应把“当前定义集合 → 当前生成物集合”的关系作为升级前检查：确认旧 RPC/协议是否被调用、跨包装配是否仍引用、线上客户端是否仍发旧 ID。删除定义之后，仅看到 CLI exit 0 或 `-check` 通过不足以证明旧输出已退役。下一轮的重点是部分删除、重命名和下游生成消费者的真实构建/注册结果。

## 2026-09-30 第二轮实施后的补充

上文是 `1b7a2fc5` 修前机制快照；RR-20260930-01/02 在 Core `f6566d4e` 之后已[修复并验证](REVIEW-2026-09-30-codegen-02.md)。新的 `servicerpc` 先把本次应有的传输/装配文件列出，再只把同半边、同生成头、同 regenerate 命令而不在预期集合内的文件当孤儿；检查模式失败，普通模式删除。具体实现和跨包保留边界见 [RPC 修复](../bugfix/RR-20260930-01.md)。

Protocol 空定义会主动移走可识别旧输出；有 bootstrap 时保留重新生成的空 bootstrap。没有 Go 生成头的 proto/JSON manifest 由 `protocol.IsGeneratedArtifact` 以固定路径与内容结构识别，`roost` 暂存提交规划及 `--check` 使用相同规则。这一层补齐后，生成器在暂存树的删除才能落到真实项目。其他生成器不能自动继承该行为：第二轮新复现表明 Entity/Nest 仍留旧文件，[RR-04/05](../bug/REVIEW-2026-09-30-codegen-02.md) 待实施。业务协议兼容窗口与旧调用方清理仍是接入方责任。
