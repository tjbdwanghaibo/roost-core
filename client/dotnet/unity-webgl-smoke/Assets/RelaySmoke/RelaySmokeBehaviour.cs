#nullable enable
using System;
using System.Collections.Generic;
using Roost.Client.Relay;
using UnityEngine;

namespace Roost.RelaySmoke
{
    /// <summary>
    /// 播放器（WebGL 或桌面）里的冒烟入口：Start 读设置并开始，Update 每帧驱动（先浏览器桥接、再 Pump），OnGUI 显示日志。
    /// 设置来源：WebGL 页面 URL 的查询参数（?lobby=/lobby&amp;transport=ws&amp;hold=40），桌面播放器的命令行（-roostLobby、
    /// -roostTransport、-roostHold）。WebGL 上缺省用 .jslib 的浏览器 WebSocket、lobby 走同源的 /lobby（serve.mjs 代理）。
    /// </summary>
    public sealed class RelaySmokeBehaviour : MonoBehaviour
    {
        private readonly List<string> lines = new List<string>();
        private RelaySmoke? smoke;
        private Vector2 scroll;

        private void Start()
        {
            var config = new RelaySmokeConfig();
#if UNITY_WEBGL && !UNITY_EDITOR
            config.BrowserWebSocket = true;
            var page = new Uri(Application.absoluteURL);
            config.LobbyUrl = new Uri(page, Query(page.Query, "lobby") ?? "/lobby").ToString().TrimEnd('/');
            if (Query(page.Query, "transport") == "tcp") config.Transport = RelayTransport.Tcp; // WebGL 上连不了 TCP：只为看清错误
            if (float.TryParse(Query(page.Query, "hold"), out float hold)) config.HoldSeconds = hold;
#else
            var args = Environment.GetCommandLineArgs();
            for (int i = 0; i + 1 < args.Length; i++)
            {
                if (args[i] == "-roostLobby") config.LobbyUrl = args[i + 1];
                if (args[i] == "-roostTransport") config.Transport = args[i + 1] == "tcp" ? RelayTransport.Tcp : RelayTransport.Ws;
                if (args[i] == "-roostHold" && float.TryParse(args[i + 1], out float hold)) config.HoldSeconds = hold;
            }
#endif
            smoke = new RelaySmoke(config, Write, () => Time.realtimeSinceStartupAsDouble);
            _ = smoke.RunAsync();
        }

        private void Update() => smoke?.Tick();

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
