// @roost/client/gate：连玩家接入（嵌入 TCP 或独立 Gate 的 WebSocket 监听）。方案 FEATURE-2026-11-18-ts-gate-client；
// 能力与 C# 托管客户端 Roost.Client 对齐（第 1 轮：连上、握手含连接代号、心跳、超时、重连、请求应答、推送、校时）。
export * from "./packet.js";
export * from "./auth.js";
export * from "./transport.js";
export * from "./clock.js";
export * from "./options.js";
export { Connection, type ConnectionEvents } from "./connection.js";
export { GameClient, type GameClientEvents } from "./game-client.js";
export type { Completion } from "./session.js";
export type { WebSocketLike } from "../wire/websocket.js";
