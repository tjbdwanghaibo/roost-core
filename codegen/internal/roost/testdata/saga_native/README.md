# saga_native：新手工程的“填业务”覆盖

`TestGeneratedSagaNativeStepsAndEffectConsumer`（`codegen/internal/roost/add_saga_test.go`）在新生成的工程里依次运行
`roost add access / entity / component / dao / handler / saga -native / effect`，再把本目录按原路径覆盖进去（DAO
字段、钱包逻辑、四个原生步骤 handler、发起赠礼与升级的 handler、服务 Init / Shutdown 与真实依赖用例），
`steps.go`、effect 载荷与消费者骨架只做小处修改（见测试里的 `fillSagaNativeBusiness`），然后 build / vet，并在真实
Mongo / NATS / DataEngine / Nest 上跑 `internal/bootstrap` 的 `TestSagaNativeStepsAndEffectRealDependencies`。

`SAGAGEN_DB` 是占位符，测试换成本次唯一的 DAO 库名（DAO 库名是编译期常量）。
