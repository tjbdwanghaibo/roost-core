# M-16：发布屏障期间的真实 Prepare 重试

**决定：保留 A，不采用延后构造修订哈希表的 B。** 在冻结的固定输入上，B 的密集拒绝重试明显变快，但正常成功路径每 Flush 都新增 2 次堆分配、192 B。按事先门槛停止；未运行 B 的原尺度无探针 c4，未修改或提交产品目录。以下全规模数据属于观测 A，不是产品验收。工作树基点 `fix/main-review-fixes@24b704bf`。

## 原程序及逐交付核账

1000 玩家、10000 实体、预热 30 秒＋正式 30 秒、收尾上限 60 秒。私有 localhost Gate/NATS，独占测量锁。完整窗口内业务、心跳及 Gate RTT 通过。原程序 **FAIL / exit 1**：正式提交的 55,087 条增量全部在截止后收到，截止前样本为空（不是 p99=0）。全部交付 p50/p95/p99 宽桶上界分别为 **10,360 / 11,140 / 11,160 ms**，无负值或 60 秒溢出；p99 要求不超过 100 ms。收尾 25.127 秒，含 self 的全员最终视图通过。小规模 Gate 原程序 PASS/0：472 条增量全部窗内、All p99 桶上界 10 ms（原 50 ms 门限）。

## 实际拒绝点和阶段时钟

以正式窗内完整、成功返回的普通轮为分母（157 轮，排除跨边界及强制轮）。在原状态锁判定点读结果，不预检屏障，不推测 commit gate。659,932 次 Prepare 命中 `policyHolds` 分支、0 次命中 `!commitGate.ready()` 分支；原短路保留，未推断 hold 与 gate 同时受阻的次数。另有 528 次 dirty 成功，25,741 次无 Prepare。每次 hold 拒绝都实际追加 commit_pending；settleDeferrals 新插入 422,846 次、237,086 次对象已在 pending。两者之和对齐拒绝总数。

| hold 拒绝对象访问：观察到的累计墙钟 | 秒 |
|---|---:|
| 对象访问合计 | 23.967 |
| 订阅预处理（含首次对象锁等待） | 17.245 |
| profile 与捕获判定 | 2.464 |
| 修订表采样 | 2.762 |
| Prepare API（包括锁/回调） | 0.313 |
| 重锁及其余尾部等 | 1.184 |

657,245 次在相邻轮再次被拒。最慢 1% 为 551 条交付、3 个事实；它们 Eligible→Selected 的去重等待为 10.512 秒，和实际 hold 拒绝对象访问相交 8.239 秒。重复拒绝访问内原修订表采样的严格界为 2.757–2.762 秒；在上述尾部等待并集内的相交量为 0.9425–0.9437 秒（`revision-bounds.json`）。没有将组均值分配给单对象；这些数字是原工作及时间重叠，**不是可省时间，也不是对生产延迟的预测**。以前的混合轮/严重积压轮只能按扇出及整访问比较（`meter-source.zip` 中 `prior/`），不能追认旧轮的锁内返回原因。

## 唯一候选与固定门

上次同一 subject/state 的真实 Prepare 走 commit_pending 分支，只是临时表示提示。本次仍在原对象锁下采样全部指针和修订，照常解锁、调用原 Prepare、重锁、重试/结算；只在成功走分发时回填哈希。无提示继续原表示。不改变发布时机、异步默认、预算、容量或收尾条件。B 源码只在隔离实验和证据包内，不是产品改动。

A 的真实 hold 拒绝中，65+ 扇出对象占 23.416 / 23.967 秒，因此事先选 `dense_held` 为主场。64 个对象，稠密场每对象 128 订阅；另有稀疏和正常成功对照。A1/B1/B2/A2，各场每组 5 个样本 × 32 个 Flush。原始字节/释放时序合同在 A/B 各重复三次；抑制指定 release 的负对照在预期断言处失败。A 观测接线另过定向 race、vet 与五次相同 17 帧摘要。未以测试代替产品验收，未对拒绝后的 B 做进一步认证。

| 固定输入，合并中位数 | 完整 Flush：A → B（µs） | 分配次数：A → B / Flush | 字节：A → B / Flush |
|---|---:|---:|---:|
| 稠密，被拒 | 376.101 → 241.972 | 221 → 210 | 41,800 → 32,448 |
| 稀疏，被拒 | 30.240 → 17.798 | 161 → 161 | 30,272 → 30,272 |
| 稠密，成功 | 1,860.759 → 1,607.232 | 1,250 → 1,252 | 335,936 → 336,128 |
| 稀疏，成功 | 35.530 → 35.131 | 288 → 290 | 38,004 → 38,196 |

主场完整 Flush 两对均改善，合计快 35.7%。但在两种成功对照上，`allocs/op` 都多 2，独立 `heap-allocs/sample` 都多 64（32 次调用），正反序均复现。**冻结门 STOP**；不调门、不换样本补跑。未测 B 能否达到千人 100 ms 门槛。

## 离线复算（不跑 Go 或服务）

在本目录运行 `python3 -B verify.py small`（Python 3.9 即可）或 `python3 -B verify.py c4`（`analyze.py` 用到 `bisect` 的 `key=`，需 Python 3.10+）。各自读取 `*-raw.zip`，与同名 `*-checked.json.gz` 解压后的 JSON 比较；父目录的 audit / analyze / validate100 与 `fix/main-review-fixes@bddcf653` 中的同名文件相同，已一并放在 `tail100/`。2026-11-27 在 main 上复跑 small，输出与 `small-checked.json.gz` 完全一致。

**公开仓库里的取舍**（2026-11-27）：
- 千人原始包 `c4-raw.zip`（17.6 MB，SHA-256 `704f1e540e0f39c22c28dbb31103ac5fa59cf0e53ba919da0706c3065d8af609`）不进仓库，只在维护者本机 `artifacts/perf/m16-prepare-retry-20261127/original/` 保存。复算 c4 时把它放回本目录。`c4-checked.json.gz` 仍在仓库里。
- `fixed-rejected.zip`、`meter-source.zip`、`small-raw.zip` 里的本机路径已替换为 `$MESHY_WORK`、`$MESHY_PROJECT`、`$HOME`，涉及 overlay、pins、`*.exit.json`、`selected-product.json`、`product-b.json`、`build-and-contracts.log`、`runner.log`。
- 因此这些文件里记录的内部哈希（`pins.json`、`inputs.json` 的 `pins_sha256`、`selected-product.json` 的 `overlay_sha256`、`exit.json` 的 `runner`）对应的是脱敏前的原件。原件连同 `SHA256SUMS` 也在上述本机目录里。
- `verify.py` 校验的 `sample.json`、`run.log`、`trace.json.gz`、`nats-*.log` 不含本机路径，没有改动。脱敏后的 `fixed-rejected.zip` 跑下面的 gate 仍得到 `False`。`schema.json` 列出时钟范围；`observe.patch.gz`、`build.json` 和 `meter-source.zip` 保存观测来源、测试和旧轮分层。`fixed-rejected.zip` 保存冻结门、四组日志、已执行的负对照、结果和被否决的小范围产品差异。解压到临时目录，在该目录可独立运行 `PYTHONPATH=fixed python3 -B -c "from gate import score; print(score('fixed-pair','dense_held')['pass'])"`，预期为 `False`。`review/bounds.py` 对原始 `trace.json.gz` 复算严格界。仅文档包在隔离工作树中，未暂存、未提交。
