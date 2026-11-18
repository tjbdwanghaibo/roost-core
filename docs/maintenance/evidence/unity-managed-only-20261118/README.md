# Unity 托管路线打包只要 .NET SDK（2026-11-18）

基线 main `e4450e39`。来源：roost-genre skill 重写时发现 `client/unity/README` 说托管路线“不需要 C++ 构建环境”，但唯一打包入口 `scripts/client/build-unity.py` 一开头就要求 cmake（还有 protoc）。

## 修复

`build-unity.py` 加 `--managed-only`：不查 / 不跑 cmake、不要 protoc，不建 native 插件、不生成原生示例（同时从包的 `package.json` 去掉 samples），只 `dotnet build` `Roost.Client` 并复制。原生绑定的 C# 源码（`Runtime/Native`）照带，因为 `RoostNativeDriver` 引用它；没有插件时只在真用原生路线时找不到库。`build-info.json` 的 `target` 写 `managed`、`native_source_sha256` 为 null。与 `--merge-plugins-from` 互斥。

## 红绿

两次都把 PATH 限成 `/usr/bin:/bin:/usr/local/share/dotnet`（没有 cmake、protoc）：

- 修前（基线版本的脚本，不带新参数）：`error: cmake required in PATH`，退出 2 —— [red-before.txt](red-before.txt)。
- 修后 `--managed-only`：退出 0，包内文件与 build-info 见 [green-build.txt](green-build.txt)。
- 新建空 Unity 工程，`Packages/manifest.json` 以 `file:` 引用这个包，加一个 `AddComponent<RoostClientDriver>()` 的业务脚本，Unity 6000.3.12f1 `-batchmode -nographics -quit` 导入：正常退出、`error CS` 为 0，生成 `Roost.Unity.dll`、`Roost.Unity.Editor.dll`、`Assembly-CSharp.dll` —— [unity-import.txt](unity-import.txt)。

## 没验证

只在 macOS ARM64 的 Editor 里编译过；没有进 Play 连服务器、没打 Player / IL2CPP（托管路线运行时与原生包里的同一个 `Roost.Client.dll`，那部分的已有证据见 KNOWN-LIMITS“客户端”）。原生路线的构建没有改动，没有重跑。
