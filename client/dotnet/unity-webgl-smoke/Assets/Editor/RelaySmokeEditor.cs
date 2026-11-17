#nullable enable
using System;
using Roost.Client.Relay;
using Roost.RelaySmoke;
using UnityEditor;
using UnityEditor.Build.Reporting;
using UnityEditor.SceneManagement;
using UnityEngine;

/// <summary>
/// 冒烟工程的批处理入口（见 README）：
/// - RunInEditor：在编辑器里直接跑一遍（SDK 自带的 ClientWebSocket / TCP，lobby 走 UnityWebRequest），由 EditorApplication.update 驱动，
///   结束时以 0 / 1 退出。设置取环境变量 ROOST_SMOKE_LOBBY、ROOST_SMOKE_TRANSPORT（ws / tcp）、ROOST_SMOKE_HOLD（秒）、
///   ROOST_SMOKE_PUMP_DRIVEN（1 打开 RelayClientOptions.PumpDriven）。
///   不要加 -quit（要等 update 循环跑完）。
/// - BuildWebGL：生成场景并构建 WebGL 开发版到 Build/WebGL（要装 WebGL 模块；不压缩，便于本地静态服务）。
/// - BuildMac：同一场景构建 macOS 播放器（Mono）到 Build/RelaySmoke.app，用来在真正的播放器循环里跑（命令行参数见 RelaySmokeBehaviour）。
/// </summary>
public static class RelaySmokeEditor
{
    private const string ScenePath = "Assets/RelaySmoke.unity";
    private static RelaySmoke? smoke;
    private static double deadline;

    public static void RunInEditor()
    {
        var config = new RelaySmokeConfig();
        config.LobbyUrl = Environment.GetEnvironmentVariable("ROOST_SMOKE_LOBBY") ?? config.LobbyUrl;
        config.Transport = Environment.GetEnvironmentVariable("ROOST_SMOKE_TRANSPORT") == "tcp" ? RelayTransport.Tcp : RelayTransport.Ws;
        if (float.TryParse(Environment.GetEnvironmentVariable("ROOST_SMOKE_HOLD"), out float hold)) config.HoldSeconds = hold;
        config.PumpDriven = Environment.GetEnvironmentVariable("ROOST_SMOKE_PUMP_DRIVEN") == "1";
        smoke = new RelaySmoke(config, line => Debug.Log(line), () => EditorApplication.timeSinceStartup);
        deadline = EditorApplication.timeSinceStartup + config.HoldSeconds + 120;
        EditorApplication.update += Tick;
        _ = smoke.RunAsync();
    }

    private static void Tick()
    {
        if (smoke == null) return;
        smoke.Tick();
        if (smoke.Finished || EditorApplication.timeSinceStartup > deadline)
        {
            EditorApplication.update -= Tick;
            bool passed = smoke.Finished && smoke.Passed;
            if (!smoke.Finished) Debug.Log("SMOKE FAIL editor run did not finish before its deadline");
            smoke = null;
            EditorApplication.Exit(passed ? 0 : 1);
        }
    }

    public static void BuildWebGL()
    {
        CreateScene();
        PlayerSettings.productName = "RoostRelaySmoke";
        PlayerSettings.WebGL.compressionFormat = WebGLCompressionFormat.Disabled;
        PlayerSettings.WebGL.exceptionSupport = WebGLExceptionSupport.FullWithStacktrace;
        Build(BuildTarget.WebGL, "Build/WebGL");
    }

    public static void BuildMac()
    {
        CreateScene();
        PlayerSettings.productName = "RoostRelaySmoke";
        PlayerSettings.SetScriptingBackend(UnityEditor.Build.NamedBuildTarget.Standalone, ScriptingImplementation.Mono2x);
        Build(BuildTarget.StandaloneOSX, "Build/RelaySmoke.app");
    }

    private static void CreateScene()
    {
        var scene = EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        var camera = new GameObject("Camera").AddComponent<Camera>();
        camera.clearFlags = CameraClearFlags.SolidColor;
        camera.backgroundColor = Color.black;
        new GameObject("Relay Smoke").AddComponent<RelaySmokeBehaviour>();
        EditorSceneManager.SaveScene(scene, ScenePath);
    }

    private static void Build(BuildTarget target, string location)
    {
        var report = BuildPipeline.BuildPlayer(new BuildPlayerOptions
        {
            scenes = new[] { ScenePath }, locationPathName = location, target = target, options = BuildOptions.Development,
        });
        if (report.summary.result != BuildResult.Succeeded) throw new Exception($"{target} build failed: {report.summary.result}");
        Debug.Log($"SMOKE built {target} at {location}");
    }
}
