# Roost CI/CD P0/P1/P2 实施方案

本文同时约束 roost-codegen 仓库的框架验收流水线，以及由 codegen 生成的业务项目流水线。目标不是简单增加若干 GitHub Actions，而是保证一次提交使用固定依赖图、一次发布只构建一组不可变制品，并能够通过 Shell、Docker 和 Kubernetes 三种方式安全交付。

## 职责边界

- core、kit、skill 各自负责单元测试、race、fuzz、静态检查和模块发布。
- codegen 负责生成器质量、已发布框架兼容、四仓 main 源码兼容、生成场景和历史项目升级验收。
- 业务项目负责自己的业务测试、配置验证、制品发布和环境部署。
- 普通 CI 只使用仓库已经提交的 `go.mod/go.sum`。追踪 `latest` 只能发生在独立依赖升级流水线，并通过 PR 合入。

## P0：可复现质量门禁和安全发布

### codegen

1. PR/main 执行 Linux、Windows 单测，Linux 执行 vet 和关键包 race。
2. 增加并发取消、统一超时、最小权限和模块整洁检查。
3. 生成项目 smoke 使用已发布框架版本，检查生成幂等、Shell、Docker、Compose 和 Kustomize。
4. tag 发布前检查模块路径、major、replace 和伪版本。
5. core、kit 的 workflow 必须监听 `v*` tag，使发布门禁真实生效。

### 生成项目

1. `ci.yml` 不更新依赖，只校验提交的依赖锁、生成代码、配置和部署描述。
2. `dependency-update.yml` 定时或手动运行 `project upgrade`、`deps-update`、`roost-up`，测试成功后输出变更；生产接入时使用机器人创建 PR，禁止直接推 main。
3. `release.yml` 在 `v*` tag 上构建 Linux amd64/arm64 二进制包、校验和和多架构 OCI 镜像；发布 job 才拥有写权限。
4. 所有部署 workflow 使用 GitHub Environment，production 要求仓库侧审批保护。

## P1：兼容矩阵、升级矩阵和三种部署

### 框架兼容矩阵

- minimum：codegen 声明的最低 core/kit/skill 版本。
- released：生成时解析并锁定的一组最新正式版本。
- source-head：在临时生成项目中使用不提交的 `go.work` 联调四仓 main；不再以写入 module 的临时 replace 作为标准研发流程。
- version-skew：当前 codegen 对上一框架集合，明确支持边界。

每个场景必须生成两次且第二次零差异，并至少覆盖 minimal、entity/nest、persistence、remote/saga、replication、skill 和多服务项目。

source-head 与 release 是两道独立门禁：前者证明当前五仓代码同代，后者必须显式
`GOWORK=off` 并只解析正式 tag。尚未发布新 API 时允许 source-head 继续研发，但 release
矩阵必须保持失败，直到按 core → kit → skill → service → codegen 完成版本闭包。

### 历史项目升级

保存最近两到三个 codegen 版本生成的项目 fixture，验证：

- `project upgrade` 不覆盖业务文件。
- 升级失败不留下半提交状态。
- 第二次升级是 no-op。
- 升级后能使用当前框架编译、测试和构建镜像。

### Shell/systemd

- Release 提供版本化压缩包和 SHA256SUMS。
- 安装到不可变 release 目录，原子切换 `current`。
- 每个 Service/SID 使用独立用户、配置、状态和 WAL 目录。
- readiness 失败自动切回上一 release；首次发布失败停服。
- 多主机按 canary/批次部署，任一批失败停止后续批次。

### Docker/Compose

- CI 构建镜像；Release 构建并推送不可变 tag 和 digest。
- Compose 使用同一镜像运行不同 Service，分别配置 SID、配置文件和 WAL volume。
- 默认 non-root、只读根文件系统、丢弃 capability、资源限制和健康检查。
- 部署记录前一个 digest，失败恢复原 digest。

### Kubernetes/Kustomize

- `base` 保存稳定资源，`overlays/staging` 和 `overlays/production` 保存环境差异。
- PR 先 render 和 schema 校验；部署前执行 server-side dry-run。
- 发布使用镜像 digest，不允许 production 使用 `latest`。
- 有持久 WAL 的实例使用 StatefulSet 和独占 RWO PVC，不能直接扩 replicas 共享 writer 身份。
- rollout 失败回滚 workload；涉及持久化格式、WAL、wire 或 schema 不兼容时只能前滚或先执行显式迁移。

## 生成项目流水线

```text
pull request
  -> ci: format/vet/test/race/generated/config/security/deploy validate

schedule/manual
  -> dependency update -> full ci -> dependency upgrade PR

tag vX.Y.Z
  -> release-check
  -> build binary packages once
  -> build image once and record digest
  -> staging shell/docker/k8s deploy
  -> smoke and observation
  -> protected production promotion
```

## 验收标准

- PR CI 不执行 `go get -u`、`deps-update` 或任何隐式 latest 解析。
- `go mod tidy`、生成器第二次运行和 Kustomize render 都没有未提交差异。
- Shell 脚本通过语法与 ShellCheck；Compose 能完成 `docker compose config`；K8s 通过严格 schema 校验。
- Release 的二进制和镜像带版本、commit、构建时间、checksum、SBOM 和 provenance。
- staging 与 production 晋级同一制品，不重新构建。
- 任一部署方式均有 readiness、超时、审计和明确回滚入口。
- `project upgrade` 能把旧生成项目安全升级到当前 CI/CD 模板。

## P2：框架发布列车

P2 优先实施一个统一发布准入点，而不是让 codegen 在后台替其他仓库打 tag。`ci/framework-release.yaml`
声明本次发布唯一允许的 codegen、core、kit、skill 精确版本和消费端 Go 版本。执行：

```bash
roost framework verify \
  --manifest ci/framework-release.yaml \
  --expected-codegen v1.10.0 \
  --lock framework-lock.json
```

verify 会从 Go module proxy 下载三个正式模块，检查 module path、checksum、replace，以及框架模块间的
伪版本依赖；任意一项不满足都拒绝发布。`consumer_go` 同样以发布清单为唯一数据源，verify 将其转换为
GitHub Actions JSON matrix，避免清单和工作流分别维护版本。`.github/workflows/release.yml` 随后使用同一
版本集合生成完整双 Service 消费项目，在清单声明的 Go 版本上编译，再在 Linux、Windows、macOS 执行
原生 CLI smoke。
最后一个 publish job 进入受保护的 `framework-release` Environment，生成 Linux/Windows/macOS、
amd64/arm64 压缩包、SHA256SUMS、SBOM、provenance 和 `framework-lock.json`，并创建不可变 GitHub Release。

已发布的 kit v1.9.1 的 go.mod 仍引用 core 伪版本，因此该组合会被新门禁正确拒绝。当前开发分支已经
改为依赖正式 core v1.9.1，`ci/framework-release.yaml` 也已声明目标 kit v1.9.2。必须先发布 kit
v1.9.2，等待 module proxy 可获取后再发布 codegen v1.10.0；不能移动 v1.9.1 标签或绕过门禁。
