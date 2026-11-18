// @roost/client/protobuf：按 protobuf-es v2 生成的 Schema 收发（对应 C# Roost.Client.Codecs 的 ProtobufExtensions；维护者
// 2026-11-18 D3）。peer 依赖 @bufbuild/protobuf，版本要与生成 *_pb.ts 的 protoc-gen-es 相同（导出目录 manifest.json 的
// protobuf_es，client/ts/package.json 锁定同一版本）；游戏工程自己装这一版，SDK 与 *_pb.ts 共用同一份运行库。
// 64 位整数是 bigint（D5）：精确，业务算术写 BigInt(…) 或 n 后缀。registerDao（自身视图同步）在第 2 轮。

import { create, fromBinary, toBinary, type DescMessage, type MessageInitShape, type MessageShape } from "@bufbuild/protobuf";
import type { Connection } from "../gate/connection.js";
import type { GameClient } from "../gate/game-client.js";

function encode<Req extends DescMessage>(schema: Req, message: MessageInitShape<Req>): Uint8Array {
  return toBinary(schema, create(schema, message));
}

/**
 * 回调形式的请求：请求按 reqSchema 编码（可以传普通对象或已建好的消息），应答按 resSchema 解码后交给 received。顺序保证与
 * GameClient.request 相同（与同一条消息里的推送按线上顺序）。
 */
export function request<Req extends DescMessage, Res extends DescMessage>(
  client: GameClient,
  requestId: number,
  responseId: number,
  reqSchema: Req,
  message: MessageInitShape<Req>,
  resSchema: Res,
  received: (response: MessageShape<Res>) => void,
  failed: (error: Error) => void,
  signal?: AbortSignal,
): void {
  client.request(requestId, responseId, encode(reqSchema, message), (payload) => fromBinary(resSchema, payload), received, failed, signal);
}

/** Promise 形式（续行在微任务里，晚于同一条消息里的推送回调）。 */
export async function requestAsync<Req extends DescMessage, Res extends DescMessage>(
  connection: Connection,
  requestId: number,
  responseId: number,
  reqSchema: Req,
  message: MessageInitShape<Req>,
  resSchema: Res,
  signal?: AbortSignal,
): Promise<MessageShape<Res>> {
  const packet = await connection.requestAsync(requestId, responseId, encode(reqSchema, message), signal);
  return fromBinary(resSchema, packet.payload);
}

/** 订阅推送，载荷按 schema 解码；解码失败交给 GameClient 的 "error"。返回取消函数。 */
export function subscribe<S extends DescMessage>(client: GameClient, pushId: number, schema: S, received: (value: MessageShape<S>) => void): () => void {
  return client.subscribe(pushId, (payload) => fromBinary(schema, payload), received);
}

/** 只写出、不等应答的业务消息。 */
export function notify<S extends DescMessage>(connection: Connection, messageId: number, schema: S, message: MessageInitShape<S>): void {
  connection.notify(messageId, encode(schema, message));
}
