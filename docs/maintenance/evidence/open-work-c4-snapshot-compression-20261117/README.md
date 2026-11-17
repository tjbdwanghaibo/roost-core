# 方案：快照压缩要不要提供 zstd（交接 C4，2026-11-17）

状态：**方案，未实施**。要不要提供由维护者定；提供就要加依赖，需要批准。源码行号按 main 的 `742ccafc`。

## 现状

- 方案原文：“压缩在房主客户端做，SDK 提供压缩辅助（LZ4 或 zstd，由游戏选）。relay 不压缩、不解压”（`docs/maintenance/FEATURE-2026-11-16-lobby-relay.md:189`）。
- 实际：三份 SDK 缺省都用各语言自带的原始 deflate，压缩方式是一个可以替换的接口。
  - Go：`Compressor` 接口两个方法（`client/relay/host.go:101-107`），缺省 `Deflate`（`:109-110`），`NoCompression`（`:113`）；`Options.Compressor` 为空时用 `Deflate`（`client/relay/client.go:156-158、185-186`）。
  - C#：`IRelayCompressor`（`client/dotnet/Roost.Client.Relay/RelayCompressor.cs:11-16`），自带 `Deflate` 与 `None`。
  - TS：`Compressor`（`client/ts/src/compress.ts:9-14`），缺省用浏览器的 `CompressionStream('deflate-raw')`，node 里退到 `node:zlib`（同文件开头的说明）。
- 这个差异已经登记（`docs/maintenance/KNOWN-LIMITS.md:577`）。
- **快照里没有“用的是哪种压缩”的标识。** 快照载荷是 `[seq 8 字节][快照字节]`（`client/wire/relay.go:325-330`），relay 不解析。所以同一个房间里所有客户端必须用同一种压缩；混用时新房主解压失败，拿不到状态（`client/relay/host.go:54` 的注释）。
- 依赖情况：
  - Go：标准库没有公开的 zstd 包（`$GOROOT/src/compress` 下只有 bzip2、flate、gzip、lzw、zlib）。根模块的 `go.mod:39` 已经有 `github.com/klauspost/compress`，但标着 `// indirect`，是别的依赖带进来的；SDK 直接用它，它就变成直接依赖。
  - C#（`netstandard2.1`）与浏览器：自带的压缩里没有 zstd，要用第三方库。**这两条是我对平台的一般了解，没有在这个仓库里验证。**

## 选项

| | 做法 | 代价 |
| --- | --- | --- |
| A（推荐） | 维持 deflate 作缺省，SDK 不带 zstd。把方案原文那一句改成和实际一致，并在三份 README 里各给一段“怎么接入自己的 zstd / LZ4”的示例 | 没有新依赖。要用 zstd 的游戏自己带库 |
| B | 提供 zstd，但做成三个可选的附加包（Go 单独一个子模块、C# 单独一个工程、TS 一个可选依赖），核心 SDK 仍然零依赖 | 三个第三方依赖都要批准；要有跨三端的互通用例；Unity 下第三方 C# 库能不能用要在 Unity 里验证 |
| C | 把 zstd 直接做进核心 SDK，作为缺省 | 核心 SDK 不再零依赖；改缺省会让新旧客户端在同一个房间里互不兼容 |

另外有一项和上面无关、可以单独做的改进：

| | 做法 | 代价 |
| --- | --- | --- |
| D | SDK 在压缩后的字节前加 1 个字节的“压缩方式”标识（SDK 之间的约定，relay 仍然不解析）。混用时新房主能给出明确的错误，而不是解压出一堆乱码或静默失败 | 快照字节的格式变了，三份 SDK 同时改；金样里和快照内容有关的条目要更新 |

## 推荐

选 A。理由：

- deflate 三端都自带、互通，已经有互通用例。
- zstd 的好处是压得更快、压缩率更高，对“每秒最多一份、几十到几百 KB”的快照，收益有多大**没有测过**。先用真实的游戏状态量一下 deflate 的耗时和大小，如果不构成问题，就没有理由加三个依赖。
- 接口已经留好，真有需要的游戏今天就能自己接。

D 建议做，但优先级低；它解决的是“配错了很难查”的问题。

## 影响面

- 选 A：只改文档（方案原文一句、三份 README、KNOWN-LIMITS 那一条的措辞）。
- 选 B：三个新包、各自的依赖声明、互通用例、发布流程里多三个产物。
- 选 D：`client/relay/host.go`、C# 与 TS 对应的压缩封装、金样、RELAY.md。

## 测试

- 选 A：没有代码改动；README 里的示例代码要能编译（放进各自的测试里编一遍）。
- 选 B：三端互通——每一端压缩的结果，另外两端都能解开且字节相同；解压上限（16 MiB）对 zstd 同样生效（构造一个解压炸弹，先红后绿）。
- 选 D：标识不认识时返回明确的错误；标识对、内容坏时返回解压错误。变异：去掉标识检查 → 混用的用例变红。

## 需要维护者定的点

1. A、B、C 选哪个。
2. 选 B 的话，三个具体的第三方库各用哪个，逐个批准。
3. D 做不做。
4. 是否先量一次 deflate 在真实状态上的耗时和大小，再做决定。

## 尚未确认

- C# 与浏览器“没有自带 zstd”是一般了解，没有在这里验证。
- deflate 与 zstd 在真实游戏状态上的差距没有测过。
- 三份 SDK 各自解压失败时的具体行为只读了 Go 的注释，C# 与 TS 没有逐行核对。
