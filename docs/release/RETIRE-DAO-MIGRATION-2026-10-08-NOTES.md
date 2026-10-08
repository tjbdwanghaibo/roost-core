# DAO 自动迁移与 Remote 旧发布端清理：说明

维护者原话：DAO schema 自动迁移不需要；Remote 旧发布端分支不需要；Cube 三处旧 API 需要全部改成 Roost。

## R1 · DAO 只加载当前 schema

删除生成 Migrate、DAORegistry、MigrationRunner、自动写 WAL 后重读聚合及迁移投影冲突特赦。持久 DAO 必须声明 SchemaVersion；所有 DAO 的版本校验通过后才恢复并发布实体。旧版与未来版均返回可用 errors.Is 判断的 `dataengine.ErrSchemaMismatch`。正式生成 DAO 在解码前执行相同校验。

`NewEntityRepository(manager, store, gate)` 不再有 migration 参数。game-demo 不再生成 db/migrations 或在启动时注册步骤，改用正式 DAO 恢复测试。通用显式业务版本 Registry、系统事务提交/投影等待保留，不能把它们当成 DAO 自动迁移删掉。

## R2 · Remote 兴趣只接受当前发布端

发布端和接收端要求非零 Generation 与完整订阅身份；空 payload 的 Delete 返回失败。删除无代际旧发布端的撤销分支，保留当前协议的迟到续租/撤销水位及表满保护。消息字段和当前序列化格式未改变，接收校验收紧。

## R3 · Cube 范围

Cube HEAD c68a2f5 仍使用 cube-core v1.1.0、cube-kit v1.0.4；812 个 Go 文件导入旧框架。三处三参数 NestOptionWithWorkerNumAndMsgCap 不能直接换成 Roost 双池 API 而保留旧框架实体类型。已向维护者提出整体迁移/另轮实施的范围选择，本轮尚未修改 Cube，不能宣称三处已完成。

## 升级边界

尚未上线，不保留旧 schema 转换或旧兴趣发布端兼容。不自动清库、删除文件或改写 WAL。需要保留的数据先用负责原 WAL 的旧程序完成落库；不同 schema 的文档仍不能由新程序直接加载，需另行明确离线处理。先停旧再起新；所有关联 DAO/Entity 用当前生成器重生成并编译。此轮不改变 DAO schema 常量或 WAL codec，不把协议收紧伪称为兼容升级。

实现和验收见 [实现](RETIRE-DAO-MIGRATION-2026-10-08-IMPLEMENTATION.md)。无部署、发版、tag 或性能测试。
