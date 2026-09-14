# U-0199:SubmitInput 先按客户端给的帧号索引身份环,再校验

**仓库 / 位置**:roost-core `lockstep/sequencer.go`,`SubmitInput` / `slot`(U-0193 引入,U-0197 改成环后放大)。
**来源**:用户复审本方法时提出"觉得有点问题",本会话定位。**没有 RR 编号**——不是 review agent 登记的。
**修复单元**:U-0199(C8)。**定位文档**:TROUBLESHOOTING T-93。

## 问题

两条同因,都是"先去重、后校验"的顺序问题:

**P1 — 32 位平台上客户端可触发的 panic。** 环的定位是 `int(original) % replayWindowSize`,而 `original` 是
`uint32` 且完全由客户端控制(`Room.SubmitInput` 只判 `closed`,帧号原样透传)。`int` 为 32 位的平台上:

```
64 位 int:  int(4000000000) % 129 = 121
32 位 int:  int32(4000000000) = -294967296, % 129 = -24
```

`ring[-24]` → index out of range。Room 是单 goroutine 驱动的,panic 会带走整个房间。
`GOARCH=386 go build ./lockstep` 是通过的,该平台并未被排除。

**P2 — 顺序本身。** 身份查找连同环的惰性分配(每座位 129 槽 ≈ 1.5KB)排在窗口检查之前,于是一个必然被
`ErrFrameTooEarly` 拒绝的垃圾帧号照样先按它索引了环。实测:超大帧号被拒绝之后 `len(s.accepted[1]) == 129`。

## 根因

U-0193 把"身份优先"写成了字面意义上的第一步,但身份表是**按客户端给的数寻址**的结构——寻址在校验之前,
既是资源问题也是越界问题(C8:入参校验与使用对同一个值的判据不同)。

## 方案选择

- **只把 `int(...)` 换成宽度安全的取模。** 解决 P1,不解决"未经校验就分配环"。
- **在 `slot()` 里加范围判断。** 把校验责任推到底层,调用点仍然会拿非法值去调它。
- **先校验再去重,并且取模在 `FrameID` 域内做(采用)。** 身份查找挪到窗口检查之后;`replaySlotIndex`
  用 `original % FrameID(replayWindowSize)`,对任意 uint32 都落在 `[0, 129)`,与 `int` 的位宽无关。

  **等价性证明**(挪动不会漏掉命中):任何被记住的 `original` 在写入时都满足 `original <= next+window`
  ——显式提交是 `original == folded_frame <= next+window`,迟到提交是 `original < next <= next+window`。
  `next` 只增不减,`window` 构造后固定,所以这个不等式在任何更晚的时刻仍然成立;反过来,窗口检查会拒绝的
  `original`(即 `original > next+window`)不可能有身份记录。

## 改动

`lockstep/sequencer.go`:`SubmitInput` 里 `original` / 折叠 / 窗口检查前移,身份查找与 `remembered` 后移;
新增 `replaySlotIndex`,`slot` 改用它;删掉 `slot != nil` 死判断(`slot()` 从不返回 nil)。

## 证明

`lockstep/submit_validation_promises_test.go`:
- `TestSubmitInputPromiseRejectedFrameNeverTouchesTheIdentityRing`:超大帧号必须以 `ErrFrameTooEarly` 被拒且
  `s.accepted[1] == nil`;随后一次合法提交建立身份、重传仍然幂等(证明挪动没有破坏去重)。
- `TestReplaySlotIndexPromiseStaysInRangeForEveryFrameID`:0 / 1 / 128 / 129 / 130 / 2^16 / 4e9 / 2^32-1 的下标
  都在 `[0, replayWindowSize)`,且等于 `FrameID` 域的取模。

修前红:`a refused submission indexed the identity ring with the client's frame id before validating it: ring len=129`。
修后 lockstep 含 `-race` 全绿,core 全仓 `-race` 全绿,`GOARCH=386 GOOS=linux go build ./lockstep` 通过。

## 未做 / 边界

- **P1 的行为红只存在于 `int` 为 32 位的平台,本机跑不了 386 二进制**,只验证了它能编译。仓库里现在的
  `TestReplaySlotIndexPromiseStaysInRangeForEveryFrameID` 在 amd64/arm64 上修前修后都是绿的——它钉的是
  "下标与 int 位宽无关"这条不变量,不是一条能在本机变红的行为断言。真正在本机红的是 P2 的顺序测试,
  而顺序修好之后 `slot()` 只会收到已经通过 `original <= next+window` 的值,P1 在实践中也不再可达;
  取模改到 `FrameID` 域是与位宽无关的第二道保险。
- 三仓 CI 没有 32 位矩阵,本轮也没有加。
- 用户同时提出的第三点——返回值区分不出"payload 被存下"与"被丢弃"(重复身份 / 输给同帧已有输入),
  三种都是 `(frame, nil)`——是公开 API 的语义扩展,本单元没有做。
