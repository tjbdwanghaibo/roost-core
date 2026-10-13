# Roost CI/CD 实施方案

本文约束两条流水线：roost-core 仓库自身的框架验收与发布（工作流在仓库根 `.github/workflows/`），以及由生成器生成的业务项目流水线。目标是一次提交使用固定依赖图、一次发布只构建一组不可变制品，并能通过 Shell、Docker 和 Kubernetes 三种方式安全交付。

框架是单模块：core、wiring、skill、框架服务与生成器随同一个 tag 发布，不再有多仓发布顺序。本地发版步骤（`scripts/pretag.sh`、真实资源矩阵、tag 后消费验证）见[维护手册 §5](../../docs/maintenance/README.md#5-发版)；维护者约定不等待 GitHub CI，本地验收线见同一手册 §3。

## 职责边界

- roost-core 仓库负责单元测试、race、fuzz、静态检查、生成器质量、已发布框架兼容、生成场景与历史项目升级验收，以及模块发布。
- 业务项目负责自己的业务测试、配置验证、制品发布和环境部署。
- 普通 CI 只使用仓库已经提交的 `go.mod/go.sum`。追踪 `latest` 只能发生在独立依赖升级流水线，并通过 PR 合入。

## roost-core 仓库的工作流

| 工作流 | 做什么 |
| --- | --- |
| `ci.yml` | Linux 质量检查（vet、关键包 race、模块整洁）、已提交生成代码的重生成比对、Linux 测试、Redis 服务套件、Windows 编译兼容、发布卫生检查 |
| `framework-compat.yml` | 生成物兼容矩阵：依赖集合 minimum / released / source-head × 场景 minimal / full / demo，每个场景生成两次且第二次零差异，再编译测试；另有需要网络的 codegen 测试 |
| `upgrade-compat.yml` | 用已发布的上一版生成器生成项目，再用当前生成器 `project upgrade`，验证不覆盖业务文件、失败不留半提交、第二次升级为 no-op、升级后能编译测试 |
| `release.yml`（name `framework-release`） | 手动触发的发布列车：`roost framework verify`、按清单的 `consumer_go` 生成真实消费项目、Linux / Windows / macOS CLI smoke，最后在受保护 Environment 中发布 |
| `nightly.yml`、`nightly-gapmap.yml` | 深度质量与故障矩阵；每日承诺回退采样（不阻塞） |
| `security.yml` | Dependency Review 与 CodeQL |
| `demo-publish.yml` | main 每次推送后用源码 HEAD 生成 game-demo 工程，推到 `demo-generated` 分支供浏览 |

兼容矩阵的依赖集合：

- minimum：生成器声明的最低 core 版本（`codegen/internal/roost/manifest.go` 的 `minimumVersions.Core`，当前 v1.25.0；工作流里 `-roost-core-version` 那一行是它的唯一副本，由测试逐字比对）。
- released：生成时解析并锁定的最新正式版本。
- source-head：工作区指向本 checkout，验证当前源码与生成器同代；不以写进 module 的临时 replace 作为研发流程。

source-head 与 released 是两道独立门禁：前者证明当前代码同代，后者必须 `GOWORK=off` 并只解析正式 tag。生成代码用了未发布的新 API 时，released / minimum 保持失败，直到新版本发布并把 minimum 升上去。

## 发布列车

`codegen/ci/framework-release.yaml` 声明本次发布的版本与消费端 Go 版本：

```yaml
schema: 3
release: v1.25.0     # 必须等于要发的 tag
consumer_go:
  - 1.27.x
```

```bash
roost framework verify \
  --manifest codegen/ci/framework-release.yaml \
  --expected-release v1.25.0 \
  --lock framework-lock.json
```

verify 从 Go module proxy 下载正式模块，检查 module path、checksum、replace 与伪版本依赖，任一项不满足即拒绝；`consumer_go` 以清单为唯一数据源转换为 GitHub Actions 矩阵。`scripts/pretag.sh` 在打 tag 前比对 `release:` 与 tag，release 工作流在 tag 之后再比对一次。publish job 进入受保护的 `framework-release` Environment，生成 Linux / Windows / macOS、amd64 / arm64 压缩包、SHA256SUMS、SBOM、provenance 和 `framework-lock.json`，并创建不可变 GitHub Release。已有 tag 不移动；发布失败按新版本号修复。

## 生成项目的流水线

1. `ci.yml` 不更新依赖，只校验提交的依赖锁、生成代码、配置和部署描述（Linux / Windows、race、生成幂等、全 Service 配置、ShellCheck、Compose、Kustomize 和镜像构建）。
2. `dependency-update.yml` 定时或手动运行 `project upgrade`、`deps-update`、`roost-up`，测试成功后由机器人创建 PR，禁止直接推 main。
3. `release.yml` 在 `v*` tag 上构建 Linux amd64/arm64 二进制包、校验和、SBOM、provenance 和多架构 OCI 镜像；只有发布 job 拥有写权限。
4. `security.yml`：govulncheck、Dependency Review 和 CodeQL。
5. `deploy-shell.yml`、`deploy-docker.yml`、`deploy-k8s.yml` 使用 GitHub Environment 晋级同一不可变制品，production 要求仓库侧审批。

所有第三方 GitHub Action 固定完整 commit SHA，由 Dependabot 提交升级 PR。`make cicd-check` 可在提交前检查业务代码和三套部署模板。

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

### Shell/systemd

- Release 提供版本化压缩包和 SHA256SUMS。
- 安装到不可变 release 目录，原子切换 `current`；每个 release 的 unit 一起记录，回滚连同 unit 回退。
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

各部署生成物的具体默认值（停机宽限期公式、单实例锁、启动等待）见[项目生成器 §17](PROJECT_GENERATOR.zh-CN.md#17-生产部署生成物)。

## 验收标准

- PR CI 不执行 `go get -u`、`deps-update` 或任何隐式 latest 解析。
- `go mod tidy`、生成器第二次运行和 Kustomize render 都没有未提交差异。
- Shell 脚本通过语法与 ShellCheck；Compose 能完成 `docker compose config`；K8s 通过严格 schema 校验。
- Release 的二进制和镜像带版本、commit、构建时间、checksum、SBOM 和 provenance。
- staging 与 production 晋级同一制品，不重新构建。
- 任一部署方式均有 readiness、超时、审计和明确回滚入口。
- `project upgrade` 能把上一版生成项目安全升级到当前 CI/CD 模板。
