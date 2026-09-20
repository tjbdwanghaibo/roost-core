# U-0270：发布清单里的 codegen 版本停在 v1.15.19，受保护的发布闸连红十次

- 来源：进度盘点时对照 CI 发现（无 RR 编号）
- 仓库 / 位置：roost-codegen `ci/framework-release.yaml`、`scripts/pretag.sh`
- 缺陷类：C4（跨包字面量耦合：同一个版本号写在 tag 和清单两处，漂移不报错）

## 问题

`.github/workflows/release.yml` 的受保护 gate：

```bash
go run ./cmd/roost framework verify --manifest ci/framework-release.yaml \
  --expected-codegen "$RELEASE_TAG" --lock framework-lock.json ...
```

`VerifyFrameworkRelease` 第一件事就是比对清单里的 `codegen:` 与本次 tag（`framework_release.go:115`）。
清单停在 `v1.15.19`，于是：

```text
framework release codegen v1.15.19 does not match release tag v1.15.29
```

`gh run list --workflow framework-release` 显示 v1.15.22 起（能查到的最近 8 次）**每一次都红在这一步**。

## 为什么没人发现

这个失败的形状最难察觉：**tag 本身完全可用**。`go get github.com/tjbdwanghaibo/roost-codegen@v1.15.29`
正常解析，生成工程正常编译，本地 `pretag.sh` 全绿——因为 pretag 检查的是"这个 tag 能不能被消费"，
而漂移的是"这次发布声明了什么"。唯一的受害者是 gate 的产物：`framework-lock.json`
（core / kit 的 module path、checksum、replace 与内部伪版本校验）**十个版本没有产出过**。
而 release 记忆里那条"发 tag 之后不用等 CI"（pretag 已经跑过测试）正好让它安静了十次。

## 方案选择

| 方案 | 取舍 | 结论 |
| --- | --- | --- |
| 让 workflow 从 tag 推导 codegen 版本，删掉清单字段 | 漂移不可能发生，但清单不再是"这次发布声明了什么"的完整记录，lock 里也就没有它 | 未采用 |
| 只把字段改对 | 这次好了，下一次接着漂 | 不够 |
| **改对 + 在 tag 之前校验** | 判据挪到唯一能阻止坏 tag 的地方 | **采用** |

`pretag.sh` 的存在理由本来就是这个（脚本头注释：workflow 只能报告 tag 不可用，无法阻止它）。
这条检查因此正属于它：清单的 `codegen:` 必须等于要打的版本，不等就在打 tag 之前失败。

## 改动

- `ci/framework-release.yaml`：`codegen: v1.15.19` → `v1.15.30`（本次发布的版本）。
- `scripts/pretag.sh` 新增第 4 步（在构建/测试之前，快速失败），文件不存在时跳过——core 与 kit 没有这份清单。

## 证明

```text
$ ./scripts/pretag.sh v1.15.31
pretag: ci/framework-release.yaml says codegen: v1.15.30 but this release is v1.15.31;
        the release workflow compares the two and fails the framework gate once the tag is pushed

$ ./scripts/pretag.sh v1.15.30
pretag: framework release manifest names v1.15.30
...
pretag: github.com/tjbdwanghaibo/roost-codegen@v1.15.30 is ready to tag
```

发布后核对 `framework-release` 工作流本身是否转绿——这一次值得等 CI，因为这条闸就是本单元的交付物。

## 未做 / 边界

- **前十个版本的 lock 不会补**：工作流按 tag 检出，旧 tag 上的清单仍是 v1.15.19，重跑也还是红。
  要补只能为那些版本重打 tag，代价大于收益；v1.15.30 起恢复正常产出。
- **只防住了 codegen 这一个字段**：`framework.core` / `framework.kit` 与实际 pin 的漂移仍然只有 CI 会发现
  （U-0264 就是那一类）。把"清单 == go.mod 实际 pin"也纳入 pretag 是下一步，本单元没做。
