#nullable enable
using System;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client.Relay;
using UnityEngine.Networking;

namespace Roost.RelaySmoke
{
    /// <summary>
    /// LobbyClient 的 HTTP 经 UnityWebRequest 发（ILobbyHttp，WebGL A1）：WebGL 上 HttpClient 自带的网络不可用（浏览器沙箱里没有套接字，
    /// 只能经 fetch / XHR，Unity 里就是 UnityWebRequest）。SDK 不带它（参考写法），编辑器与其他平台上也能用。
    /// 用法：new LobbyClient(url, gt2, new UnityWebRequestLobbyHttp(), pumpDriven: true)（WebGL 上与 RelayClientOptions.PumpDriven 一起开）。
    ///
    /// UnityWebRequest 只能在主线程上用：在主线程上调时直接发；SDK 不是 PumpDriven 时可能在后台线程上调 lobby（断线重连），
    /// 那时投到构造时所在线程的 SynchronizationContext（Unity 主线程）再发。答复在主线程的 completed 回调里交回（任务在主线程上完成）。
    /// 4xx / 5xx 照常返回答复（LobbyClient 要读 lobby 的错误 JSON），只有连不上等网络错误抛异常。
    /// </summary>
    public sealed class UnityWebRequestLobbyHttp : ILobbyHttp
    {
        private readonly SynchronizationContext main;

        /// <summary>在 Unity 主线程上构造。</summary>
        public UnityWebRequestLobbyHttp()
        {
            main = SynchronizationContext.Current ?? throw new InvalidOperationException("Create UnityWebRequestLobbyHttp on the Unity main thread.");
        }

        public Task<LobbyHttpResponse> SendAsync(string method, string url, string bearer, string? jsonBody, CancellationToken cancellation)
        {
            var done = new TaskCompletionSource<LobbyHttpResponse>();
            if (SynchronizationContext.Current == main) Start(method, url, bearer, jsonBody, done, cancellation);
            else main.Post(_ => Start(method, url, bearer, jsonBody, done, cancellation), null);
            return done.Task;
        }

        private void Start(string method, string url, string bearer, string? jsonBody, TaskCompletionSource<LobbyHttpResponse> done, CancellationToken cancellation)
        {
            UnityWebRequest web;
            try
            {
                web = new UnityWebRequest(url, method) { downloadHandler = new DownloadHandlerBuffer() };
                web.SetRequestHeader("Authorization", "Bearer " + bearer);
                if (jsonBody != null)
                {
                    web.uploadHandler = new UploadHandlerRaw(Encoding.UTF8.GetBytes(jsonBody));
                    web.SetRequestHeader("Content-Type", "application/json");
                }
            }
            catch (Exception error)
            {
                done.TrySetException(error);
                return;
            }
            // Abort 也只能在主线程上调：取消可能来自别的线程，投回主线程。
            var registration = cancellation.Register(() => main.Post(_ => { try { web.Abort(); } catch (Exception) { } }, null));
            web.SendWebRequest().completed += _ =>
            {
                registration.Dispose();
                try
                {
                    if (web.result == UnityWebRequest.Result.ConnectionError || web.result == UnityWebRequest.Result.DataProcessingError)
                        done.TrySetException(new System.Net.Http.HttpRequestException($"{method} {url}: {web.error}"));
                    else
                        done.TrySetResult(new LobbyHttpResponse((int)web.responseCode, web.downloadHandler.text ?? ""));
                }
                catch (Exception error) { done.TrySetException(error); }
                finally { web.Dispose(); }
            };
        }
    }
}
