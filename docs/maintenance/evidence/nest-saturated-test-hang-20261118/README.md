# nest：完成泵饱和用例在负载下挂住整个包（2026-11-18）

v1.55.0 第一次全量 pretag 时，`go test ./...` 在 `framework/nest` 上停了 47 分钟，CPU 为 0。向这个测试进程发了 SIGQUIT 取得栈，摘录见 [pretag-hang-excerpt.txt](pretag-hang-excerpt.txt)。只改了测试，产品代码没动。

## 现象

- `TestAsyncCompletionKeepsOrderWhenPumpIsSaturated` 在 `only %d of 3 transactions were enqueued` 处 `Fatalf`。
- 失败后，`defer StopNest()` 里的 `Shutdown` 一直等在途事务结束；而那些事务在等永远不会落定的票据，于是整个包一直挂到 `-timeout`。
- 不带并行负载、单独跑旧用例 `-count=300`，也会在某一轮挂住，见 [isolated-old-hang-excerpt.txt](isolated-old-hang-excerpt.txt)。

## 根因（测试的调度假设）

用例的设置是完成泵队列容量 1，同一实体上并发 3 个异步完成事务。旧版先等 3 次 Enqueue，再去确认“有一个事务走了队列满的降级路径”。这只在泵已经把第 1 条从队列取走时成立。

如果泵 goroutine 还没取走第 1 条，情况是这样：

1. 第 2 个事务发现队列满，走降级路径，在自己的 worker 里等票据。这是既定行为，见 `commitPipelined` 的注释“队列满则在本 worker 等待”。
2. 同一实体的派发是串行的，第 3 个事务在票据落定前连 Enqueue 都到不了。
3. 旧版等不到第 3 次 Enqueue 就 `Fatalf`。失败路径上又没有落定票据，`StopNest` 永远等下去。

栈与此一致：
- 泵停在 `pipelined_completion.go:197`，已经取出一个条目，正在等它的票据；
- 一个 worker 停在 `execution.go:403`，即降级路径上的 `<-ticket.Done()`；
- 没有第 3 个事务。

## 修法

- 不再等 3 次 Enqueue，改为直接等“降级计数 > 0”，期限 10 秒。两种调度下都会有一个事务降级：泵已取走第 1 条时是第 3 个，没取走时是第 2 个。
- 等到降级后调用 `committer.resolveAllFromNowOn(nil)`：落定已有的票据，之后新 Enqueue 的票据也立即落定。这样第 2 个降级时，第 3 个也能入队并完成。
- AfterCommit 必须按 1、2、3 的顺序执行，这条断言不变。
- 失败路径也会落定票据：`defer committer.resolveAllFromNowOn(...)` 排在 `defer StopNest()` 之后，按后进先出先执行。以后这个用例再失败，只会报错，不会挂住整个包。
- 测试提交器新增 `autoErr` 和 `resolveAllFromNowOn`，见 `pipelined_commit_test.go`。

## 红 / 绿

结果见 [stress-red-green.txt](stress-red-green.txt)。

| 版本 | 6 个进程并行，各 `-count=600` | 结果 |
|---|---|---|
| 旧用例 | `-timeout 3m` | 6 个进程全部挂到超时 |
| 新用例 | `-timeout 9m` | 6 个进程全部 ok，共 3600 轮 |

- 用 `-cpu 1` 跑旧用例 40 轮没有复现，说明它取决于泵 goroutine 有没有及时被调度，靠单核串行触发不了。
- 失败路径的验证：临时把降级等待的期限改成 0，用例 0.65 秒内 FAIL，没有挂住。
- `framework/nest` 整包通过，根包通过。

## 同类用例一并修掉（维护者 2026-11-18：“这种可以直接修改掉”）

本包有 22 条用例用不自动落定票据的提交器（`newPipelinedTestCommitter(false)`，包括嵌入它的 `castBoundaryCommitter`、`releaseFailureCommitter`、`bestEffortPipelinedCommitter` 和 `asyncSetEngine`）。它们只要在落定票据之前失败退出，都会像上面那样把 `StopNest` 挂住。这次没有逐条改，而是在两处统一处理：

- **测试提交器登记**：`newPipelinedTestCommitter` 把每个提交器记进包内名单，`StopNest` 结束时清空名单。
- **`StopNest` 有上限**（`fixtures_test.go`）：
  - 最多等 20 秒；
  - 还没停下，就把名单里所有未决票据以 `errStopNestCleanup` 落定，再等 20 秒；
  - 仍然停不下就 panic；
  - 任何情况下都不会无限等。
- **新增 `TestMain`**：只要有一次 `StopNest` 超时，整个包就判失败。这样“用例自己报通过，但关停卡住”的真问题不会因为票据被替它落定而被掩盖。

验证见 [stopnest-bounded.txt](stopnest-bounded.txt)：

| 情形 | 结果 |
|---|---|
| 旧 `StopNest`，在 `TestAsyncCompletionFreesWorkerDuringDurableWait` 里注入失败 | 挂在 `StopNest` 里，直到 `-timeout 60s` |
| 新 `StopNest`，在上面这条和 `TestPipelinedCommitReleasesLocksBeforeDurable` 里注入失败 | 分别在 20s、25s 报 FAIL，打出 `StopNest: shutdown did not finish…`，`TestMain` 判整包失败 |
| 去掉注入后跑 nest 整包（普通 / `-race`） | 都通过，`StopNest` 超时 0 次 |

注入失败的做法：在等到入队之后、落定票据之前插一行 `t.Fatal`。

其余自定义提交器（`blockingCommitter` 等）不在名单里，由 20 秒加 20 秒的上限兜底，到时直接 panic。
