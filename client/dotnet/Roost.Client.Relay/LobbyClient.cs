using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client.Relay
{
    /// <summary>向 lobby 要的 relay 接入方式。C# SDK 只连 TCP；Ws 给浏览器（TS SDK）。</summary>
    public enum RelayTransport { Tcp, Ws }

    /// <summary>房主设置的房间状态。</summary>
    public enum RoomState { Open, Started }

    /// <summary>lobby 返回的房间（docs/framework/LOBBY.md §2 的 room）。</summary>
    public sealed class RoomInfo
    {
        public string Id { get; internal set; } = "";
        public string Game { get; internal set; } = "";
        public string Mode { get; internal set; } = "";
        public string Name { get; internal set; } = "";
        /// <summary>人数上限，含房主。</summary>
        public int MaxMembers { get; internal set; }
        /// <summary>占着席位的人数（在线、预留期内与房主）。</summary>
        public int Members { get; internal set; }
        public bool HasPassword { get; internal set; }
        public RoomState State { get; internal set; }
        public string Host { get; internal set; } = "";
        public ulong HostEpoch { get; internal set; }
        /// <summary>建房时写的自定义属性（JSON 原文），lobby 不解析；没有时为 null。</summary>
        public string? AttrsJson { get; internal set; }
        public long CreatedAtUnix { get; internal set; }

        internal static RoomInfo From(JsonObject? json)
        {
            if (json == null) throw new InvalidDataException("Lobby reply has no room.");
            return new RoomInfo
            {
                Id = json.String("id") ?? "", Game = json.String("game") ?? "", Mode = json.String("mode") ?? "",
                Name = json.String("name") ?? "", MaxMembers = (int)json.Long("max_members"), Members = (int)json.Long("members"),
                HasPassword = json.Bool("has_password"), State = json.String("state") == "started" ? RoomState.Started : RoomState.Open,
                Host = json.String("host") ?? "", HostEpoch = json.ULong("host_epoch"), AttrsJson = json.RawValue("attrs"),
                CreatedAtUnix = json.Long("created_at_unix"),
            };
        }
    }

    /// <summary>建房、加入的答复：房间、relay 地址与 relay 令牌。令牌寿命很短（缺省 30s），只用来建立连接。</summary>
    public sealed class RoomTicket
    {
        public RoomInfo Room { get; }
        /// <summary>tcp 时是 host:port，ws 时是完整的 ws:// / wss:// URL。</summary>
        public string RelayAddr { get; }
        public string Token { get; }
        /// <summary>relay 令牌的过期时刻（Unix 秒）。</summary>
        public long ExpiresAt { get; }
        public RoomTicket(RoomInfo room, string relayAddr, string token, long expiresAt)
        {
            Room = room; RelayAddr = relayAddr; Token = token; ExpiresAt = expiresAt;
        }
    }

    public sealed class CreateRoomRequest
    {
        public string Game { get; set; } = "";
        public string Mode { get; set; } = "";
        /// <summary>含房主；0 取 lobby 对这个游戏的上限。</summary>
        public int MaxMembers { get; set; }
        public string? Name { get; set; }
        public string? Password { get; set; }
        /// <summary>自定义属性（JSON 原文，不超过 1KB）。</summary>
        public string? AttrsJson { get; set; }
        public RelayTransport Transport { get; set; } = RelayTransport.Tcp;
    }

    /// <summary>加入请求。对已在房间里的人是幂等的（不再查密码、人数和“已开始”），断线重连就走这一条。</summary>
    public sealed class JoinRoomRequest
    {
        public string Game { get; set; } = "";
        public string? Password { get; set; }
        public RelayTransport Transport { get; set; } = RelayTransport.Tcp;
    }

    /// <summary>改状态请求，只有当前房主能调；State 与 AttrsJson 至少写一个。</summary>
    public sealed class SetRoomStateRequest
    {
        public string Game { get; set; } = "";
        public RoomState? State { get; set; }
        public string? AttrsJson { get; set; }
    }

    /// <summary>lobby 的错误名（答复的 error 字段）。</summary>
    public enum LobbyErrorCode
    {
        /// <summary>答复不是 lobby 的 JSON（前置代理的错误页），或是本 SDK 还不认识的错误名。</summary>
        Unknown,
        BadRequest, Unauthorized, GameNotServed, NotHost, WrongPassword, RoomNotFound, RoomFull, RoomStarted,
        TransportUnavailable, TooLarge, NoRelay, Unavailable,
    }

    /// <summary>lobby 答的非 200。</summary>
    public sealed class LobbyException : Exception
    {
        private static readonly Dictionary<string, LobbyErrorCode> Codes = new Dictionary<string, LobbyErrorCode>(StringComparer.Ordinal)
        {
            ["bad_request"] = LobbyErrorCode.BadRequest, ["unauthorized"] = LobbyErrorCode.Unauthorized,
            ["game_not_served"] = LobbyErrorCode.GameNotServed, ["not_host"] = LobbyErrorCode.NotHost,
            ["wrong_password"] = LobbyErrorCode.WrongPassword, ["room_not_found"] = LobbyErrorCode.RoomNotFound,
            ["room_full"] = LobbyErrorCode.RoomFull, ["room_started"] = LobbyErrorCode.RoomStarted,
            ["transport_unavailable"] = LobbyErrorCode.TransportUnavailable, ["too_large"] = LobbyErrorCode.TooLarge,
            ["no_relay"] = LobbyErrorCode.NoRelay, ["unavailable"] = LobbyErrorCode.Unavailable,
        };

        /// <summary>HTTP 状态码。</summary>
        public int Status { get; }
        /// <summary>答复里的错误名原文；不是 lobby 的 JSON 时为 null。</summary>
        public string? ErrorName { get; }
        public LobbyErrorCode Code { get; }
        /// <summary>值得稍后重试：5xx（依赖不可达、没有 relay）。</summary>
        public bool IsTemporary => Status >= 500;

        public LobbyException(int status, string? errorName, string message)
            : base(errorName == null ? $"lobby: HTTP {status}: {message}" : $"lobby: {errorName} (HTTP {status}): {message}")
        {
            Status = status; ErrorName = errorName;
            Code = errorName != null && Codes.TryGetValue(errorName, out var code) ? code : LobbyErrorCode.Unknown;
        }
    }

    /// <summary>
    /// lobby 的 HTTP 客户端（docs/framework/LOBBY.md §2）。每个请求带 Authorization: Bearer gt1...（游戏账号 token）；
    /// 请求的 game 必须等于 token 里的游戏。线程安全。
    /// </summary>
    public sealed class LobbyClient
    {
        private readonly string baseUrl;
        private readonly Func<CancellationToken, Task<string>> gameToken;
        private readonly HttpClient http;

        /// <param name="baseUrl">lobby 的根地址，例如 https://lobby.example.com。</param>
        /// <param name="gameToken">每个请求取一次 gt1；gt1 有寿命，长时间运行的客户端在这里换新的。</param>
        /// <param name="http">null 时新建一个 HttpClient。</param>
        public LobbyClient(string baseUrl, Func<CancellationToken, Task<string>> gameToken, HttpClient? http = null)
        {
            this.baseUrl = (baseUrl ?? throw new ArgumentNullException(nameof(baseUrl))).TrimEnd('/');
            this.gameToken = gameToken ?? throw new ArgumentNullException(nameof(gameToken));
            this.http = http ?? new HttpClient();
        }

        public LobbyClient(string baseUrl, string gameToken, HttpClient? http = null)
            : this(baseUrl, _ => Task.FromResult(gameToken), http) { }

        /// <summary>建房，调用者是房主。拿到的 RoomTicket 交给 RelayClient.ConnectAsync。</summary>
        public async Task<RoomTicket> CreateRoomAsync(CreateRoomRequest request, CancellationToken cancellation = default)
        {
            var body = MiniJson.WriteObject(new Dictionary<string, object?>
            {
                ["game"] = request.Game, ["mode"] = request.Mode, ["max_members"] = request.MaxMembers == 0 ? (object?)null : request.MaxMembers,
                ["name"] = request.Name, ["password"] = request.Password,
                ["attrs"] = request.AttrsJson == null ? null : new RawJson(request.AttrsJson), ["transport"] = TransportName(request.Transport),
            });
            return Ticket(await CallAsync(HttpMethod.Post, "/rooms", body, cancellation).ConfigureAwait(false));
        }

        /// <summary>列出 game / mode 下可加入的房间（未满、未开始、未过期、房主已连上 relay），新建的在前。limit 为 0 取 lobby 的缺省。</summary>
        public async Task<IReadOnlyList<RoomInfo>> ListRoomsAsync(string game, string mode, int limit = 0, CancellationToken cancellation = default)
        {
            var path = "/rooms?game=" + Uri.EscapeDataString(game) + "&mode=" + Uri.EscapeDataString(mode) + (limit > 0 ? "&limit=" + limit : "");
            var reply = await CallAsync(HttpMethod.Get, path, null, cancellation).ConfigureAwait(false);
            var rooms = new List<RoomInfo>();
            foreach (var item in reply.Array("rooms") ?? new List<object?>()) rooms.Add(RoomInfo.From(item as JsonObject));
            return rooms;
        }

        /// <summary>加入房间，或给已在房间里的人（含房主）重新签一张 relay 令牌。</summary>
        public async Task<RoomTicket> JoinRoomAsync(string roomId, JoinRoomRequest request, CancellationToken cancellation = default)
        {
            var body = MiniJson.WriteObject(new Dictionary<string, object?>
            {
                ["game"] = request.Game, ["password"] = request.Password, ["transport"] = TransportName(request.Transport),
            });
            return Ticket(await CallAsync(HttpMethod.Post, RoomPath(roomId, "join"), body, cancellation).ConfigureAwait(false));
        }

        /// <summary>离开房间：让出席位，lobby 再让 relay 断开这个账号（Closed{left}）。房主离开由 relay 走换 host。</summary>
        public async Task<RoomInfo> LeaveRoomAsync(string roomId, string game, CancellationToken cancellation = default)
        {
            var body = MiniJson.WriteObject(new Dictionary<string, object?> { ["game"] = game });
            return RoomInfo.From((await CallAsync(HttpMethod.Post, RoomPath(roomId, "leave"), body, cancellation).ConfigureAwait(false)).Object("room"));
        }

        /// <summary>改房间状态或自定义属性，只有当前房主能调（否则 LobbyErrorCode.NotHost）。</summary>
        public async Task<RoomInfo> SetRoomStateAsync(string roomId, SetRoomStateRequest request, CancellationToken cancellation = default)
        {
            var body = MiniJson.WriteObject(new Dictionary<string, object?>
            {
                ["game"] = request.Game, ["state"] = request.State == null ? null : (request.State == RoomState.Started ? "started" : "open"),
                ["attrs"] = request.AttrsJson == null ? null : new RawJson(request.AttrsJson),
            });
            return RoomInfo.From((await CallAsync(HttpMethod.Post, RoomPath(roomId, "state"), body, cancellation).ConfigureAwait(false)).Object("room"));
        }

        private static string RoomPath(string roomId, string action) => "/rooms/" + Uri.EscapeDataString(roomId) + "/" + action;

        private static string TransportName(RelayTransport transport) => transport == RelayTransport.Ws ? "ws" : "tcp";

        private static RoomTicket Ticket(JsonObject reply) =>
            new RoomTicket(RoomInfo.From(reply.Object("room")), reply.String("relay_addr") ?? "", reply.String("token") ?? "", reply.Long("expires_at"));

        /// <summary>发一个请求：带上 gt1；200 解成 JSON 对象，其余答复换成 LobbyException。</summary>
        private async Task<JsonObject> CallAsync(HttpMethod method, string path, string? body, CancellationToken cancellation)
        {
            var token = await gameToken(cancellation).ConfigureAwait(false);
            using var request = new HttpRequestMessage(method, baseUrl + path);
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
            if (body != null) request.Content = new StringContent(body, Encoding.UTF8, "application/json");
            using var response = await http.SendAsync(request, cancellation).ConfigureAwait(false);
            var text = await response.Content.ReadAsStringAsync().ConfigureAwait(false);
            int status = (int)response.StatusCode;
            if (status != 200)
            {
                JsonObject? error = null;
                try { error = MiniJson.Parse(text) as JsonObject; } catch (InvalidDataException) { }
                var name = error?.String("error");
                if (string.IsNullOrEmpty(name)) throw new LobbyException(status, null, text.Trim());
                throw new LobbyException(status, name, error!.String("message") ?? "");
            }
            return MiniJson.Parse(text) as JsonObject ?? throw new InvalidDataException("Lobby reply is not a JSON object.");
        }
    }
}
