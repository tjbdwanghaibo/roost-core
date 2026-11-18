#nullable enable
using System;
using System.Collections.Generic;
using Roost.Unity;
using UnityEngine;

namespace Roost.GateSmoke
{
    /// <summary>
    /// 播放器（WebGL 或桌面）里的连玩家接入冒烟入口。网络由托管包的 RoostClientDriver 每帧驱动——WebGL 上
    /// Attach(client, BrowserGateTransport.PollAll)，即游戏照抄的接法；本组件的 LateUpdate 只做判定，OnGUI 显示日志。
    /// 设置来源：WebGL 页面 URL 的查询参数（?ws=ws://127.0.0.1:7080/ws&amp;hold=100，serve-gate.mjs 自动补上），桌面播放器的命令行
    /// （-roostWs、-roostHold）。WebGL 上用浏览器 WebSocket 且必开 PumpDriven。
    /// </summary>
    public sealed class GateSmokeBehaviour : MonoBehaviour
    {
        private readonly List<string> lines = new List<string>();
        private GateSmoke? smoke;
        private Vector2 scroll;

        private void Start()
        {
            var config = new GateSmokeConfig();
#if UNITY_WEBGL && !UNITY_EDITOR
            config.BrowserWebSocket = true;
            config.PumpDriven = true;
            var page = new Uri(Application.absoluteURL);
            config.Url = Query(page.Query, "ws") ?? config.Url;
            if (float.TryParse(Query(page.Query, "hold"), out float hold)) config.HoldSeconds = hold;
#else
            var args = Environment.GetCommandLineArgs();
            for (int i = 0; i + 1 < args.Length; i++)
            {
                if (args[i] == "-roostWs") config.Url = args[i + 1];
                if (args[i] == "-roostHold" && float.TryParse(args[i + 1], out float hold)) config.HoldSeconds = hold;
            }
#endif
            smoke = new GateSmoke(config, Write, () => Time.realtimeSinceStartupAsDouble);
            var driver = gameObject.AddComponent<RoostClientDriver>();
#if UNITY_WEBGL && !UNITY_EDITOR
            driver.Attach(smoke.Client, BrowserGateTransport.PollAll); // 每帧先收浏览器的字节，再 Pump
#else
            driver.Attach(smoke.Client);
#endif
            _ = smoke.RunAsync();
        }

        private void LateUpdate() => smoke?.Tick();

        private void OnApplicationPause(bool paused) => Write("SMOKE application paused: " + paused);

        private void OnApplicationFocus(bool focused) => Write("SMOKE application focus: " + focused);

        private void Write(string line)
        {
            Debug.Log(line);
            lock (lines) lines.Add(line);
        }

        private void OnGUI()
        {
            string[] snapshot;
            lock (lines) snapshot = lines.ToArray();
            scroll = GUILayout.BeginScrollView(scroll, GUILayout.Width(Screen.width), GUILayout.Height(Screen.height));
            foreach (var line in snapshot) GUILayout.Label(line);
            GUILayout.EndScrollView();
        }

        private static string? Query(string query, string key)
        {
            foreach (var pair in query.TrimStart('?').Split('&'))
            {
                int eq = pair.IndexOf('=');
                if (eq > 0 && pair.Substring(0, eq) == key) return Uri.UnescapeDataString(pair.Substring(eq + 1));
            }
            return null;
        }
    }
}
