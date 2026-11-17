#nullable enable
using System;
using System.Net;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using UnityEngine.Networking;

namespace Roost.RelaySmoke
{
    /// <summary>
    /// 让 LobbyClient 的 HttpClient 经 UnityWebRequest 发请求：WebGL 上 HttpClient 自带的网络实现不可用（浏览器沙箱里没有套接字，
    /// 只能经 fetch / XHR，Unity 里就是 UnityWebRequest）。用法：new LobbyClient(url, gt1, new HttpClient(new UnityWebRequestHandler())
    /// { Timeout = Timeout.InfiniteTimeSpan })。SDK 不带它（参考写法），编辑器里也能用（本冒烟在编辑器里就走它）。
    ///
    /// UnityWebRequest 只能在主线程上用，而 SDK 可能在后台线程上调 lobby（断线重连）：请求一律 Post 到构造时所在线程的
    /// SynchronizationContext（Unity 主线程），完成后把答复交回。4xx / 5xx 照常返回答复（LobbyClient 要读 lobby 的错误 JSON），
    /// 只有连不上等网络错误抛 HttpRequestException。
    /// </summary>
    public sealed class UnityWebRequestHandler : HttpMessageHandler
    {
        private readonly SynchronizationContext main;

        /// <summary>在 Unity 主线程上构造。</summary>
        public UnityWebRequestHandler()
        {
            main = SynchronizationContext.Current ?? throw new InvalidOperationException("Create UnityWebRequestHandler on the Unity main thread.");
        }

        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellation)
        {
            byte[]? body = request.Content == null ? null : await request.Content.ReadAsByteArrayAsync().ConfigureAwait(false);
            string? contentType = request.Content?.Headers.ContentType?.ToString();
            var done = new TaskCompletionSource<HttpResponseMessage>();
            main.Post(_ => Start(request, body, contentType, done, cancellation), null);
            return await done.Task.ConfigureAwait(false);
        }

        private void Start(HttpRequestMessage request, byte[]? body, string? contentType, TaskCompletionSource<HttpResponseMessage> done, CancellationToken cancellation)
        {
            UnityWebRequest web;
            try
            {
                web = new UnityWebRequest(request.RequestUri!.ToString(), request.Method.Method) { downloadHandler = new DownloadHandlerBuffer() };
                if (body != null)
                {
                    web.uploadHandler = new UploadHandlerRaw(body);
                    if (contentType != null) web.SetRequestHeader("Content-Type", contentType);
                }
                foreach (var header in request.Headers) web.SetRequestHeader(header.Key, string.Join(", ", header.Value));
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
                    {
                        done.TrySetException(new HttpRequestException($"{request.Method} {request.RequestUri}: {web.error}"));
                        return;
                    }
                    var response = new HttpResponseMessage((HttpStatusCode)web.responseCode)
                    {
                        Content = new ByteArrayContent(web.downloadHandler.data ?? Array.Empty<byte>()),
                        RequestMessage = request,
                    };
                    done.TrySetResult(response);
                }
                catch (Exception error) { done.TrySetException(error); }
                finally { web.Dispose(); }
            };
        }
    }
}
