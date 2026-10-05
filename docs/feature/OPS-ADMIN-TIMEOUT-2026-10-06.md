# Ops admin 命令期限与写超时（N02 O1，2026-10-06）

来由：[N02 观察 O1](../review/REVIEW-2026-10-05-noncore-n02.md)（N01 / Kit Ops 同行留项）。没有既有承诺被打破，不占 RR；维护者在 N01 留项里要求一并处理。

## 当前问题

`/admin/execute` 把请求 ctx 原样交给命令，没有期限；Ops 的 HTTP 写超时固定为 httpserver 默认的 15s。超过 15s 的命令照常执行完，但回复写不出去，客户端只看到 `EOF`——命令执行了没有、执行到哪一步，运维都不知道，重试可能重复执行（GM 命令有 trace_id 幂等，DLQ requeue / purge 等没有）。

## 方案

| 项 | 内容 |
| --- | --- |
| 配置 | 新键 `ops.admin_timeout`（时长，可选，缺省 10s，写了就必须为正；登记进 `app` 的 `frameworkDurationKeys`，严格读取）。不改生成模板：缺省值对现有工程生效，需要时手写。 |
| 命令期限 | `/admin/execute` 用 `context.WithTimeout(r.Context(), ops.admin_timeout)` 执行命令。 |
| 写超时 | Ops server 的写超时 = max(15s, `admin_timeout` + 5s)，配合 ctx 的命令到期返回后，回复一定写得出去。 |
| 回复 | 命令因期限返回 `context.DeadlineExceeded` 时回 **504**，`message` 写明 “did not finish within ops.admin_timeout … its effects are unknown”；其他错误仍是 400。 |
| 不配合 ctx 的命令 | Ops 杀不掉它；它跑过写超时后回复写不出去，客户端看到传输错误——同样是“结果未知”，不等于没执行。文档写明，不为此把命令放进另一个 goroutine（那样 Ops 的停止就等不到它，违反三步停机）。 |

改动面：`kit/ops/ops_mod.go`、`app/config_validation.go`（登记键）、`kit/README.md`、`docs/USER_GUIDE.md`。公开 API、wire、生成形状不变。

## 兼容

- 原来能跑超过 10s 的、配合 ctx 的命令（大批量 DLQ requeue 等）现在在 10s 处被取消并回 504；需要更长时把 `ops.admin_timeout` 调大（写超时随之变长）。
- 不配合 ctx 的命令行为不变。

## 验证

`kit/ops/admin_deadline_promises_test.go`：命令看到的期限 ≤ `ops.admin_timeout`、到期回 504。修前（命令没有期限）：

```
--- FAIL: TestOpsAdminCommandRunsUnderTheConfiguredDeadline (0.00s)
    admin_deadline_promises_test.go:57: admin command deadline remaining = -1ns, want ≤ ops.admin_timeout (100ms); -1 means the command ran without a deadline
```

`TestOpsStartServesOnTheBoundAddress` 断言写超时 ≥ 命令期限 + 5s。`go test -race -count=3 ./kit/ops/`、`./app/` 通过。

## 实施状态

已实施（提交见 [本轮记录](../review/REVIEW-2026-10-06-n01b.md)）。
