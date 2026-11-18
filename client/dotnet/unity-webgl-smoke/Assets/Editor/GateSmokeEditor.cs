#nullable enable
using System;
using Roost.GateSmoke;
using UnityEditor;
using UnityEditor.Build.Reporting;
using UnityEditor.SceneManagement;
using UnityEngine;

/// <summary>
/// 连玩家接入冒烟的批处理入口（见 README“连玩家接入（GateSmoke）”）：
/// - RunInEditor：编辑器里直接跑（SDK 自带的 ClientWebSocket，PumpDriven 打开，由 EditorApplication.update 驱动 Pump），结束时以
///   0 / 1 退出。环境变量 ROOST_SMOKE_WS（ws:// 地址）、ROOST_SMOKE_HOLD（秒）。不要加 -quit。
/// - BuildWebGL：生成 GateSmoke 场景并构建 WebGL 开发版到 Build/GateWebGL（不压缩，便于本地静态服务）。
/// </summary>
public static class GateSmokeEditor
{
    private const string ScenePath = "Assets/GateSmoke.unity";
    private static GateSmoke? smoke;
    private static double deadline;

    public static void RunInEditor()
    {
        var config = new GateSmokeConfig { BrowserWebSocket = false, PumpDriven = true };
        config.Url = Environment.GetEnvironmentVariable("ROOST_SMOKE_WS") ?? config.Url;
        if (float.TryParse(Environment.GetEnvironmentVariable("ROOST_SMOKE_HOLD"), out float hold)) config.HoldSeconds = hold;
        smoke = new GateSmoke(config, line => Debug.Log(line), () => EditorApplication.timeSinceStartup);
        deadline = EditorApplication.timeSinceStartup + config.HoldSeconds + 60;
        EditorApplication.update += Tick;
        _ = smoke.RunAsync();
    }

    private static void Tick()
    {
        if (smoke == null) return;
        smoke.Client.Pump();
        smoke.Tick();
        if (smoke.Finished || EditorApplication.timeSinceStartup > deadline)
        {
            EditorApplication.update -= Tick;
            bool passed = smoke.Finished && smoke.Passed;
            if (!smoke.Finished) Debug.Log("SMOKE FAIL editor run did not finish before its deadline");
            smoke.Client.Dispose();
            smoke = null;
            EditorApplication.Exit(passed ? 0 : 1);
        }
    }

    public static void BuildWebGL()
    {
        var scene = EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        var camera = new GameObject("Camera").AddComponent<Camera>();
        camera.clearFlags = CameraClearFlags.SolidColor;
        camera.backgroundColor = Color.black;
        new GameObject("Gate Smoke").AddComponent<GateSmokeBehaviour>();
        EditorSceneManager.SaveScene(scene, ScenePath);
        PlayerSettings.productName = "RoostGateSmoke";
        PlayerSettings.WebGL.compressionFormat = WebGLCompressionFormat.Disabled;
        PlayerSettings.WebGL.exceptionSupport = WebGLExceptionSupport.FullWithStacktrace;
        PlayerSettings.runInBackground = true; // 页面在后台时 Unity 仍跑循环（浏览器自己的限流另说）
        var report = BuildPipeline.BuildPlayer(new BuildPlayerOptions
        {
            scenes = new[] { ScenePath }, locationPathName = "Build/GateWebGL", target = BuildTarget.WebGL, options = BuildOptions.Development,
        });
        if (report.summary.result != BuildResult.Succeeded) throw new Exception($"WebGL build failed: {report.summary.result}");
        Debug.Log("SMOKE built WebGL at Build/GateWebGL");
    }
}
