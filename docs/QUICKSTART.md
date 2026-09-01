# Roost 五分钟快速开始

本页只解决一件事：让第一次接触 Roost 的开发者得到一个可运行、可继续开发、没有本地 `replace` 的项目。

## 1. 准备环境

- Linux 或 WSL2；Go 1.25 及以上。
- Docker + Docker Compose，用于本地 Redis、MongoDB replica set、NATS JetStream 和 etcd。
- Git。生产部署再准备 systemd 或 Kubernetes，不影响本地开始。

确认：

```bash
go version
docker version
docker compose version
```

## 2. 安装并生成项目

`roost-codegen v1.7.0` 发布后执行：

```bash
go install github.com/tjbdwanghaibo/roost-codegen/cmd/roost@v1.7.0
roost project new planet \
  -services game,gate \
  -mods configdata,etcd,redis,mongo,nats,sync,remote_entity,dataengine,nest
cd planet
```

生成目录中的关键内容：

```text
main.go                         程序入口
roost.yaml                      项目、Service、Mod、版本和 ID 空间的唯一声明
internal/bootstrap/generated.go 框架装配代码
configs/service/                每个 Service 的开发/生产配置示例
deploy/                         Shell、Docker、Kubernetes 部署基线
docs/                           本项目的使用、实现和部署说明
```

## 3. 启动本地依赖和 game Service

```bash
make dev-up
make generate
make run SERVICE=game SID=1001
```

另一个终端检查：

```bash
curl --fail http://127.0.0.1:9100/healthz
curl --fail http://127.0.0.1:9100/readyz
curl --fail http://127.0.0.1:9100/metrics
```

`healthz` 表示进程活着；只有 `readyz` 成功才可以接流量。退出使用 `Ctrl+C`，框架会在统一停机期限内按 Service → Nest → Saga → Data Engine projection/outbox/WAL → 中间件的逆依赖方向收敛。

## 4. 业务代码写在哪里

正常业务路径只有四层：

1. 接入层把玩家消息解析为类型化请求。
2. codegen 生成的 Sender 把请求送进 Nest。
3. Nest 定位 Entity、排序并加实体锁，然后调用生成包装的 handler。
4. handler 调用 Component/DAO 的生成方法；方法负责 dirty、undo 和 patch，不直接写私有字段。

先用生成器创建骨架：

```bash
roost add dao hero
roost add entity hero
roost add nest add_gold
roost generate
make ci
```

不要在 handler 外自行给 Entity 加锁，不要绕过生成的 setter 修改 DAO，不要从业务层直接控制 WAL 或 Mongo transaction。

## 5. 修改项目能力

编辑 `roost.yaml` 中的 `services`、`shared_mods` 或 `features`，然后运行：

```bash
roost project diff
roost project sync
roost project doctor
```

生成器只覆盖带生成标识的文件；业务文件和本地 Secret 不归生成器所有。提交前必须运行 `make ci`，并确认根 `go.mod` 没有 `replace`。

## 6. 下一步

- 开始写游戏逻辑：[开发者完整使用说明](USER_GUIDE.md)
- 准备上线：[生产部署手册](DEPLOYMENT.md)
- 排查一致性语义：[实现原理与不变量](INTERNALS.md)
