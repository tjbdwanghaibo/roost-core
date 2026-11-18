/**
 * 浏览器 WebSocket 与 node 的 ws 包共有的那部分（relay 与 gate 都用它）：SDK 只用这些成员，测试里用 ws 包或假的实现代替
 * 浏览器的全局 WebSocket。
 */
export interface WebSocketLike {
  binaryType: string;
  readonly readyState: number;
  send(data: Uint8Array): void;
  close(code?: number, reason?: string): void;
  onopen: ((event: unknown) => void) | null;
  onmessage: ((event: { data: unknown }) => void) | null;
  onclose: ((event: unknown) => void) | null;
  onerror: ((event: unknown) => void) | null;
}

/** WebSocket.OPEN。 */
export const WEBSOCKET_OPEN = 1;
