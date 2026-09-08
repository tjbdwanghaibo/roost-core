# M-00：本机迁移前基线

状态：验证中，五仓门禁阻塞；未执行实现迁移。
关联：[统一方案](../CORE_KIT_REFACTOR_AND_AUDIT_PLAN.zh-CN.md)。

## 1. 环境和源码集合

本机共同父目录：`D:/whb_s`。入口 Go 为 windows/amd64 1.24.0，GOTOOLCHAIN=auto；进入 Core 后自动选用已可用的 Go 1.27.0，未修改全局 Go 配置。

| 模块 | 本地目录 | HEAD |
| --- | --- | --- |
| Core | cube-core | 57d4514eef25784753a7aee2882d5ebd0a77babe |
| Kit | cube-kit | c3426f23d02267e620fc71323f8d446c1bc14ac9 |
| Skill | cube-skill | 24ac8a9cba17291a76e5641d5dfa73730ec67397 |
| Codegen | cube-codegen | 16e0c68d848cae5cbfeba047fd7a6a1b8787c837 |
| Service | 未定位 | 未验证 |

Core 已有 examples/go.mod 的 go 指令修改保留；统一方案及其导航修改也保留。其他三仓执行前后工作树干净。没有 pull、提交、推送或发版。

创建隔离的本地工作区文件 `.roost-refactor-baseline/go.work`（位于共同父目录下，不属于任何框架仓库），仅包含四个已定位模块。通过进程级 `GOWORK=D:/whb_s/.roost-refactor-baseline/go.work` 指定，不创建/覆盖共同父目录 go.work。

Service 搜索范围：当前工作区模块，以及 D:/project、D:/dd、D:/tjbdw、C:/Users/tjbdw 的有限深度同名目录。未找到不等于整台机器不存在。cube、galaxy、planet 的 module 均不是 roost-service，不作替代。

## 2. 实际验证结果

日志位于本机 `D:/whb_s/.roost-refactor-baseline/logs/`，未作为跨机器证据制品提交。

| 操作 | Core | Kit | Skill | Codegen |
| --- | --- | --- | --- | --- |
| go list -m all | 0 | 0 | 0 | 0 |
| go build ./... && go vet ./... | 0 | 0 | 1，build 阻塞，vet 未执行 | 0 |
| go test -count=1 -timeout 90s ./... | 0 | 0 | 1 | 0 |

额外：

- Core `go test -count=1 ./entity ./nest ./redis ./mongo`：退出 0。
- Kit `go test -count=1 ./remoteentity`：退出 0。
- Kit `go vet -tags integration ./remoteentity && go test -race -count=1 ./remoteentity`：退出 0。
- 根模块 `./...` 不覆盖嵌套 go.mod（Core examples、Skill examples、Skill integration/sync-e2e）；这些尚未验收。
- 未运行真实 Mongo/NATS/Redis/etcd 集成、故障矩阵、生成工程真启动或性能测试。
- PATH 检查只找到 redis-cli 和 gcc，未找到 docker、mongosh、nats-server、etcdctl；不据此推断其他位置没有安装，真实环境待准备。

## 3. 当前阻塞，不得伪装成迁移引入的 Bug

### ENV-01：Skill 仍消费不同模块身份

Skill 根 module 名虽为 roost-skill，但 go.mod 仍 require cube-core v1.8.0；combatcomponent、skillsync 及嵌套 examples/integration 中仍有 cube-core/cube-kit import。外层 workspace 的 roost-core 不会替换另一个模块身份的 cube-core。

当前 build 直接失败：`no required module provides package github.com/tjbdwanghaibo/cube-core/dataengine`。skillsync 测试虽绿，不能证明它消费的是当前 Roost Core。这不是一处 import 的孤立问题，不能添加旧模块依赖来让它假绿。

下一处理：独立的消费方对齐子批，枚举根模块与嵌套模块的 import/API 差异，统一到当前 Roost 依赖，运行测试并验证 go list 的实际解析。属于 M 前置结构调整，不混入单包 U 修复。

### ENV-02：Service source-head 未定位

需提供或准备真实 Service 工作树；没有它，不能完成五仓基线及生成 game 模板的服务消费验收。未自动 clone 未确认的仓库。

### TOOL-01：Codegen 内部关闭 workspace

`internal/roost/dependencies.go`：`runDependencyCommand` 显式设置 GOWORK=off；依赖更新调用 go get 查询版本，随后 tidy。普通生成也可调用此 tidy 路径。外部四仓测试成功不代表生成流程已使用本地 source-head。

下一处理：在 M-01 设计明确的 dev 解析通路及生成测试，不直接删除发布隔离。还需检查生成器子进程、临时工程、Makefile、go tool 调用的完整上下文。

## 4. 首批静态依赖发现

| 能力 | 当前 Kit 内部依赖 | 迁移前处理 |
| --- | --- | --- |
| remoteentity 生产实现 | remote_entity_mod.go → kit/mods；其余本次 import 扫描主要依赖 Core | Mod 留 Kit；公共构造及生命周期接口待设计 |
| remoteentity 单测 | mongo_committer_test.go → kit/mongo/mongotest | 先解决严格测试支撑归属，禁止 Core 测试反向依赖 Kit |
| remoteentity 集成 | mongo_committer_integration_test.go → kit/mongo | 拆 Core 存储集成与 Kit 装配测试 |
| dataengine | runtime/projector/mod → kit/nestwal；mod → kit/mods | WAL 先行，Mod 留 Kit；测试也有 mongotest 依赖 |
| saga | command_consumer/jetstream/nest_start_consumer → kit/nats；nest_start_consumer → kit/nestwal | 客户端或必要能力子批前移，不能机械搬文件 |

这只是 import 级初扫，尚不是全部函数、锁或行为审计；Core 全量依赖边界（含测试标签）需在 M-01 加可执行检查。

## 5. 历史工作的使用方式

已完整阅读 ledger 与 HANDOFF。保留原 U/B/T 状态，不将本次基线编译等同于 C1–C8 审计完成，也不更新任何格子的“已审”。本机历史修复是否齐全须按对应测试与当前文件继续核对，不能仅凭交接中的提交/tag 结论。

## 6. 下一步

1. 定位 Service 工作树；准备真实依赖环境。
2. 开消费方依赖对齐子批修正 ENV-01，先审查全部旧模块引用，不引入兼容 alias。
3. 重跑五仓及嵌套模块基线。
4. 完成 Codegen dev 解析设计后再进入 M-01/M-02。

停止点：M-00 尚未通过，不移动 Remote Entity，不对外宣称五仓全绿。
