// lobby 的 HTTP 客户端（docs/framework/LOBBY.md §2）。全部 JSON，每个请求带 Authorization: Bearer gt1...（游戏账号 token），
// 请求里的 game 必须等于 token 里的游戏。浏览器跨域调用要 lobby 的 cors_origins 放行游戏页面的 Origin。

/** 向 lobby 要的 relay 接入方式：浏览器用 ws（relay_addr 是完整的 ws:// / wss:// URL）。 */
export type Transport = "tcp" | "ws";

/** 房主设置的房间状态。 */
export type RoomState = "open" | "started";

/** lobby 返回的房间。 */
export interface Room {
  id: string;
  game: string;
  mode: string;
  name: string;
  /** 人数上限，含房主。 */
  maxMembers: number;
  /** 占着席位的人数（在线、预留期内与房主）。 */
  members: number;
  hasPassword: boolean;
  state: RoomState;
  host: string;
  hostEpoch: number;
  /** 建房时写的自定义属性，lobby 原样存、原样给。 */
  attrs?: unknown;
  createdAtUnix: number;
}

/** 建房、加入的答复：房间、relay 地址与 relay 令牌（寿命很短，缺省 30s，只用来建立连接）。 */
export interface Ticket {
  room: Room;
  relayAddr: string;
  token: string;
  /** relay 令牌的过期时刻（Unix 秒）。 */
  expiresAt: number;
}

export interface CreateRoomRequest {
  game: string;
  mode: string;
  /** 含房主；不写取 lobby 对这个游戏的上限。 */
  maxMembers?: number;
  name?: string;
  password?: string;
  /** 自定义属性（序列化后不超过 1KB）。 */
  attrs?: unknown;
  /** 缺省 ws。 */
  transport?: Transport;
}

/** 加入请求。对已在房间里的人是幂等的（不再查密码、人数和“已开始”），断线重连就走这一条。 */
export interface JoinRoomRequest {
  game: string;
  password?: string;
  /** 缺省 ws。 */
  transport?: Transport;
}

/** 改状态请求，只有当前房主能调；state 与 attrs 至少写一个。 */
export interface SetRoomStateRequest {
  game: string;
  state?: RoomState;
  attrs?: unknown;
}

/** lobby 的错误名（答复的 error 字段）。 */
export type LobbyErrorCode =
  | "bad_request"
  | "unauthorized"
  | "game_not_served"
  | "not_host"
  | "wrong_password"
  | "room_not_found"
  | "room_full"
  | "room_started"
  | "transport_unavailable"
  | "too_large"
  | "no_relay"
  | "unavailable";

/** lobby 答的非 200。code 为空表示答复不是 lobby 的 JSON（前置代理的错误页）。 */
export class LobbyError extends Error {
  constructor(
    readonly status: number,
    readonly code: LobbyErrorCode | "",
    message: string,
  ) {
    super(code ? `lobby: ${code} (HTTP ${status}): ${message}` : `lobby: HTTP ${status}: ${message}`);
    this.name = "LobbyError";
  }

  /** 值得稍后重试：5xx（依赖不可达、没有 relay）。 */
  get temporary(): boolean {
    return this.status >= 500;
  }
}

/** 返回当前的 gt1；每个请求调用一次，gt1 过期前在这里换新的。 */
export type GameTokenSource = string | (() => string | Promise<string>);

interface RoomJSON {
  id: string;
  game: string;
  mode: string;
  name: string;
  max_members: number;
  members: number;
  has_password: boolean;
  state: RoomState;
  host: string;
  host_epoch: number;
  attrs?: unknown;
  created_at_unix: number;
}

interface TicketJSON {
  room: RoomJSON;
  relay_addr?: string;
  token?: string;
  expires_at?: number;
}

function room(json: RoomJSON): Room {
  return {
    id: json.id,
    game: json.game,
    mode: json.mode,
    name: json.name,
    maxMembers: json.max_members,
    members: json.members,
    hasPassword: json.has_password,
    state: json.state,
    host: json.host,
    hostEpoch: json.host_epoch,
    attrs: json.attrs,
    createdAtUnix: json.created_at_unix,
  };
}

function ticket(json: TicketJSON): Ticket {
  return { room: room(json.room), relayAddr: json.relay_addr ?? "", token: json.token ?? "", expiresAt: json.expires_at ?? 0 };
}

export class LobbyClient {
  private readonly baseUrl: string;

  /**
   * @param baseUrl lobby 的根地址，例如 https://lobby.example.com
   * @param gameToken gt1，或每个请求取一次 gt1 的函数
   * @param fetchImpl 缺省用全局 fetch（浏览器与 node 18+ 都有）
   */
  constructor(
    baseUrl: string,
    private readonly gameToken: GameTokenSource,
    private readonly fetchImpl: typeof fetch = (...args) => globalThis.fetch(...args),
  ) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
  }

  /** 建房，调用者是房主。拿到的 Ticket 交给 RelayClient.connect。 */
  async createRoom(req: CreateRoomRequest): Promise<Ticket> {
    return ticket(
      await this.call<TicketJSON>("POST", "/rooms", {
        game: req.game,
        mode: req.mode,
        max_members: req.maxMembers,
        name: req.name,
        password: req.password,
        attrs: req.attrs,
        transport: req.transport ?? "ws",
      }),
    );
  }

  /** 列出 game / mode 下可加入的房间（未满、未开始、未过期、房主已连上 relay），新建的在前。 */
  async listRooms(game: string, mode: string, limit = 0): Promise<Room[]> {
    const query = new URLSearchParams({ game, mode });
    if (limit > 0) query.set("limit", String(limit));
    const reply = await this.call<{ rooms: RoomJSON[] | null }>("GET", `/rooms?${query.toString()}`);
    return (reply.rooms ?? []).map(room);
  }

  /** 加入房间，或给已在房间里的人（含房主）重新签一张 relay 令牌。 */
  async joinRoom(roomId: string, req: JoinRoomRequest): Promise<Ticket> {
    return ticket(
      await this.call<TicketJSON>("POST", `/rooms/${encodeURIComponent(roomId)}/join`, {
        game: req.game,
        password: req.password,
        transport: req.transport ?? "ws",
      }),
    );
  }

  /** 离开房间：让出席位，lobby 再让 relay 断开这个账号（Closed{left}）。房主离开由 relay 走换 host。 */
  async leaveRoom(roomId: string, game: string): Promise<Room> {
    return room((await this.call<TicketJSON>("POST", `/rooms/${encodeURIComponent(roomId)}/leave`, { game })).room);
  }

  /** 改房间状态或自定义属性，只有当前房主能调（否则 not_host）。 */
  async setRoomState(roomId: string, req: SetRoomStateRequest): Promise<Room> {
    return room(
      (await this.call<TicketJSON>("POST", `/rooms/${encodeURIComponent(roomId)}/state`, { game: req.game, state: req.state, attrs: req.attrs })).room,
    );
  }

  private async call<T>(method: string, path: string, body?: object): Promise<T> {
    const token = typeof this.gameToken === "string" ? this.gameToken : await this.gameToken();
    const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const response = await this.fetchImpl(this.baseUrl + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await response.text();
    if (response.status !== 200) {
      let error: { error?: string; message?: string } | undefined;
      try {
        error = JSON.parse(text);
      } catch {
        error = undefined;
      }
      if (!error || typeof error.error !== "string" || error.error === "") {
        throw new LobbyError(response.status, "", text.trim());
      }
      throw new LobbyError(response.status, error.error as LobbyErrorCode, error.message ?? "");
    }
    return JSON.parse(text) as T;
  }
}
