# WAL 刷盘指标修正对照

基线 `435f431e`，修后仅将计时移到 `fsyncSegment`，持久策略未变。macOS / Apple M5 / Go 1.27.3，默认 GOMAXPROCS（10），同机前后各 10 次、交替先后顺序，测量期间没有并跑本轮其他负载。

分别在修前、修后用 `GOWORK=off go test -c -o /tmp/wal.test ./framework/nestwal` 编译，然后运行：

```sh
/tmp/wal.test -test.run='^$' -test.bench='^BenchmarkWALAppend(AsyncParallel|StrictParallel)$' -test.benchmem -test.benchtime=1s -test.count=1
benchstat before.txt after.txt
```

Async 121.9→123.9µs（p=0.105），Strict 473.4→466.9µs（p=0.393），未发现显著差异；allocs 均为 6。不能据此宣称吞吐提升或内存节省。旧指标漏记的红测与修后结果见 red.txt / green.txt。
