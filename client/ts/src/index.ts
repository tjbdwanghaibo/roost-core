// @roost/client（维护者 2026-11-18 D1：合成一个包，原 @roost/relay-client 改名并入，不做兼容）。子路径：
//   - @roost/client/gate：连玩家接入（嵌入 TCP 或独立 Gate 的 WebSocket 监听）的 GameClient / Connection；
//   - @roost/client/relay：开房间的 LobbyClient / RelayClient；
//   - @roost/client/web：Web 端房主辅助 WebHost；
//   - @roost/client/protobuf：按 protobuf-es 生成的类型收发（peer 依赖 @bufbuild/protobuf）。根路径不导出它：只用 relay 或
//     gate 的工程不必装运行库，根路径保持运行时零依赖。
// 两边同名的帧层常量（HEADER_SIZE、VERSION、DEFAULT_MAX_PAYLOAD）来自同一份 src/wire/header.ts，由 relay 一侧导出。
export * from "./relay/index.js";
export * from "./web/index.js";
export * from "./gate/index.js";
