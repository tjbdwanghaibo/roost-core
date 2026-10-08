# WAL 单路径收敛说明（未发版）

维护者要求“把不需要兼容旧逻辑或者格式写进skill，然后开始按照建议实施”。[方案](../feature/REFACTOR-2026-10-08-wal-single-path.md) · [实现与验收](WAL-SINGLE-PATH-2026-10-08-IMPLEMENTATION.md)。

| 编号 | 变化与原因 | 接入影响 |
| --- | --- | --- |
| WS1 | roost-coding 明确当前未上线不兼容旧逻辑/API/配置/格式；roost-optimize 引用同一规则 | 静态 Player 跨机迁移只以已落库数据为准；WAL 不承担跨机热迁移 |
| WS2 | 删除双版本编解码，固定 codec 7 | 移除 WriterVersion/writer_version，默认路径支持 Put/Patch/Delete/Remote/effect/receipt；旧 codec 5/6 明确拒绝 |
| WS3 | Mutation 只保留正式文档身份、种类与版本；删除旧字段和 CanonicalizeMutation | 手写调用方必须提供 Key/Kind/ExpectedVersion/NextVersion；Remote 删除明确使用 MutationDelete，不能依赖旧转换推断 |
| WS4 | 删除 nestwal.Committer/Runtime 及专属接口和清理队列，统一正式 Projector/Assembly | 取代本组 API 原 C8 保留决定，不扩展到其他 C8；JetStreamEffectPublisher 和物理 WAL 原语保留 |

本次 API、WAL 格式和 CommitRecord JSON 形状不兼容。升级先停旧进程；需保留的写入先由旧版本落库并排空 outbox，再使用新 WAL 目录及重新生成的工程。程序不自动清空数据库/WAL，不跳过错误记录或伪造 checkpoint。保留旧原件，回退须配套旧格式目录。

持久策略、group commit、锁释放通知、投影前缀确认、冷加载等待、Saga/Remote fencing 与幂等恢复不降低。未部署、未发版，不运行压测，不给出新 TPS 结论。
