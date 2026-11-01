using System;
using System.Threading;
using Roost.Native;
using UnityEditor;
using UnityEditor.Build;
using UnityEditor.Build.Reporting;
using UnityEditor.SceneManagement;
using UnityEngine;
public static class UnityAcceptanceBuild
{
    public static void Run()
    {
        // 先在真正的 Editor 进程加载 native 插件，构建成功不等于插件可加载。
        using (var client = new NativeClient())
        {
            client.Connect("not-an-ip", 1, "ticket");
            bool closed = false;
            var deadline = DateTime.UtcNow.AddSeconds(3);
            while (DateTime.UtcNow < deadline)
            {
                if (client.Poll(out var value))
                {
                    closed = value.Type == NativeClient.EventType.Closed;
                    break;
                }
                Thread.Sleep(1);
            }
            if (!closed)
                throw new Exception("Editor native ABI/worker check failed");
        }
        Debug.Log("PASS Editor native ABI/worker");
        var scene = EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
        new GameObject("Native Session").AddComponent<Roost.Unity.Samples.NativeSession>();
        EditorSceneManager.SaveScene(scene, "Assets/NativeSession.unity");
        bool il2cpp = Environment.GetEnvironmentVariable("ROOST_UNITY_BACKEND") == "il2cpp";
        PlayerSettings.productName = "RoostNative";
        PlayerSettings.SetScriptingBackend(NamedBuildTarget.Standalone,
                                           il2cpp ? ScriptingImplementation.IL2CPP : ScriptingImplementation.Mono2x);
        // 与包内 ARM64 插件一致，不能生成缺失 x64 插件的 Universal Player。
        EditorUserBuildSettings.SetPlatformSettings("OSXUniversal", "Architecture", "ARM64");
        var report = BuildPipeline.BuildPlayer(
            new BuildPlayerOptions { scenes = new[] { "Assets/NativeSession.unity" },
                                     locationPathName = "Build/RoostNative-" + (il2cpp ? "il2cpp" : "mono") + ".app",
                                     target = BuildTarget.StandaloneOSX, options = BuildOptions.Development });
        if (report.summary.result != BuildResult.Succeeded)
            throw new Exception("Unity player build failed");
    }
}
