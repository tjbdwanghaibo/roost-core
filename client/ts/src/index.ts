// Roost 开房间场景（房主的客户端就是 host）的 TS SDK：lobby 的 HTTP 调用（LobbyClient）与 relay 的 WebSocket 连接
// （RelayClient）。方案 FEATURE-2026-11-16-lobby-relay §4.7.5；帧格式与 Go 的 client/wire 相同（client/spec/relay-packets.json）。
export * from "./codec.js";
export * from "./lobby.js";
export * from "./compress.js";
export { RelayClient, RelayClosedError, RelayClientClosedError, SnapshotTooSoonError } from "./client.js";
export type {
  Disconnect,
  HostChange,
  HostRole,
  HostTakeover,
  ReconnectPolicy,
  RelayClientOptions,
  SentSnapshot,
  SnapshotOptions,
  WebSocketLike,
} from "./client.js";
export * from "./web.js";
