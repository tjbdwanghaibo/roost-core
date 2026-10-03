# planet

本项目由 roost-codegen 生成，项目声明位于 roost.yaml。

安装 roost 后先运行 roost env doctor；用 roost help 查看能力目录，用 roost version 确认当前版本。

## 第一级：完全新手，五分钟运行

第一次只阅读 [小白逐步操作手册](docs/BEGINNER_WORKBOOK.zh-CN.md)，然后反复执行 make next。工具会根据
真实文件只给出当前一个动作，不需要先在多份文档间选择。

    make deps-update
    make next
    # 执行 next 输出的命令或文件修改，然后再次 make next
    make test
    make run SERVICE=account

更新 codegen 管理的工程模板使用 make project-upgrade；更新框架依赖使用 make deps-update；更新全部项目依赖使用 make roost-up；更新本机安装的 codegen 使用 make codegen-up。

确认 http://127.0.0.1:9100/readyz 返回成功后开始编写 Entity、DAO 和 Nest handler。

## 第二级：有经验开发者

阅读 [完整使用说明](docs/USAGE.zh-CN.md)，了解 Service/Mod 装配、生成命令、配置、测试与常用业务路径；修改项目声明前查阅 [roost.yaml 字段参考](docs/ROOST_YAML.zh-CN.md)。

## 第三级：框架实现与生产运维

- [实现说明](docs/IMPLEMENTATION.zh-CN.md)：Entity/Nest、Data Engine/WAL、Remote Entity、Saga、同步与技能边界。
- [部署说明](docs/DEPLOYMENT.zh-CN.md)：Shell/systemd、Docker、Kubernetes、升级回滚与故障演练。

提交和发布前执行：

    make ci
