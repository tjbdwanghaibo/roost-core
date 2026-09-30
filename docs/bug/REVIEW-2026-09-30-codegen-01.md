# Codegen 第一轮问题：删除输入后旧产物仍有效

审查基线：Core `1b7a2fc5aa4bfd9ae921ae8c80efd2392d08a165`；独立 Codegen 仓库 `1e028fa4b2927ffb5d7772b90440e7447dc665cf` 已冻结，当前实际生成器位于 Core `codegen/`。两项均在本机隔离目录以当前 CLI 确认；**未修复，未修改生产源码**。[运行和复现](../review/REVIEW-2026-09-30-codegen-01.md) · [原始命令与输出](../review/evidence/codegen-review-20260930-01/README.md)。

## RR-20260930-01 · P2 · servicerpc 删除标记后 `-check` 假通过

- **位置：**`codegen/internal/servicerpc/run.go:76-79,82-131`，`codegen/internal/servicerpc/parse.go:88-152`。`ParseDir` 返回零服务时，`Run` 在检查输出目录之前返回；有服务时只比较本次预期文件，不枚举退役产物。
- **触发：**先为 `//roost:rpc` 接口生成 `sample_rpc_gen.go` 与 `sample_rpc_assembly_gen.go`，随后删除接口上的标记，保留原接口和旧文件，再运行相同目录的 `servicerpc -check`。
- **期望/实际：**期望 CI 漂移门报告旧传输/装配已无对应标记，或明确退役它们；实际打印 `no //roost:rpc interfaces`，退出码 **0**，两个文件仍在。即使再次运行非 check 模式也不会清理。旧装配仍可能保留注册及调用入口，具体业务影响取决于消费者是否引用它。
- **证据：**隔离目录中实际先生成 2 个文件、删标记再运行，`CHECK_EXIT=0 BEFORE=2 AFTER=2`。现有 `TestCheckDetectsDrift` 覆盖“标记仍在、文件内容被修改”，未覆盖“标记已删除”。
- **实施方向：**建立此生成器的预期文件集合，并仅识别自身生成头/固定命名的旧文件；`-check` 报孤儿，普通生成在安全边界内清理孤儿。`-emit transport/assembly` 与 `-out` 跨包模式分别只处理自己的半边，不能误删手写文件或另一半。补零服务、接口重命名、跨包半边、手写同名文件保留的回归。

## RR-20260930-02 · P2 · protocol 定义清空后旧协议产物不退役

- **位置：**`codegen/internal/protocol/run.go:66-90,93-190`。`defs.Structs` 为空且无 bootstrap 时直接成功返回；有 bootstrap 时只重写空 bootstrap，旧 proto/PB/msgid/manifest 等仍不处理。非空路径也只写当次预期文件，没有清理已删除消息对应的旧 handler 文件。
- **触发：**按现有定义先生成协议，再删除最后一份 `//roost:proto` 定义（保留空的 `protocol/def` 包），以相同参数重新运行。
- **期望/实际：**期望输出反映空定义或报告必须显式退役；实际退出码 **0**，`protocol.proto`、`protocol.pb.go`、`msgid_gen.go`、`protocol_manifest.json` 四份旧产物逐字保留。生成项目因此仍可携带已删除的消息 ID/类型；是否仍对外注册取决于具体 bootstrap 与接入代码。
- **证据：**隔离临时 Go module 中现有 CLI 首次生成 4 份文件；删定义后第二次输出 `no protocol structs found`，`SECOND_EXIT=0 BEFORE=4 AFTER=4`。DAO 的 `Run` 已有删除最后一条定义时清理孤儿的对照实现（`codegen/internal/dao/main.go:76-88,146-159`）。
- **实施方向：**先定义生成器拥有的固定文件和按消息派生文件，再做期望集合差集。空定义、删除单条消息、改 ID/handler、配置禁用输出、手写文件均需验证；对旧协议 ID 的兼容策略须先由业务决定，不能无条件删除可能仍有线上客户端使用的消息。

两项都是本轮新发现，不等于对整个 Codegen 或生成项目做了穷尽审计；行号与复现基于上述 Core HEAD。
