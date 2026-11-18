// Gate 与 relay 共用的 RS v2 帧层：16 字节大端包头 [R][S][版本 2][flags][msgId 4B][seq 4B][载荷长度 4B]，后接载荷。
// 两边的 flags 规则不同（Gate：client/wire Header.Validate；relay：Header.ValidateRelay），由调用方传入校验函数；
// 这里只管包头读写与按字节流拼帧。

export const HEADER_SIZE = 16;
export const VERSION = 2;
/** 单帧载荷上限的缺省值，与服务端 max_payload_bytes 缺省（1 MiB）一致。 */
export const DEFAULT_MAX_PAYLOAD = 1 << 20;

/** 拆开的一帧：包头字段原样，载荷是独立副本（不与接收缓冲共用内存）。 */
export interface RawFrame {
  flags: number;
  msgId: number;
  seq: number;
  payload: Uint8Array;
}

/**
 * 包头校验：在等载荷之前调用（坏包头不会让读者先申请或等待一个巨大的载荷）。不合规时抛错，错误原样交给 push 的调用者。
 * size 是包头里写的载荷长度。
 */
export type HeaderCheck = (flags: number, msgId: number, size: number) => void;

/** 写一帧：包头 + 载荷。调用方先做自己的规则校验。 */
export function encodeRaw(flags: number, msgId: number, seq: number, payload: Uint8Array): Uint8Array {
  const data = new Uint8Array(HEADER_SIZE + payload.length);
  const view = new DataView(data.buffer);
  data[0] = 0x52; // R
  data[1] = 0x53; // S
  data[2] = VERSION;
  data[3] = flags;
  view.setUint32(4, msgId >>> 0);
  view.setUint32(8, seq >>> 0);
  view.setUint32(12, payload.length);
  data.set(payload, HEADER_SIZE);
  return data;
}

/**
 * 字节流 → 帧。不依赖消息边界：一条 WebSocket 消息里几帧、一帧拆在几条消息里都能解（TCP 与 WebSocket 同一套解码）。
 * badMagic 造“不是 RS v2 包头”的错误，check 校验 flags / msgId / 长度。push 抛错后读者不再可用（调用方应断开连接）。
 */
export class RawFrameReader {
  private buffer: Uint8Array = new Uint8Array(0);

  constructor(
    private readonly check: HeaderCheck,
    private readonly badMagic: () => Error,
  ) {}

  /** 还没凑成一帧的字节数。 */
  get buffered(): number {
    return this.buffer.length;
  }

  push(chunk: Uint8Array): RawFrame[] {
    if (this.buffer.length === 0) {
      this.buffer = chunk;
    } else {
      const joined = new Uint8Array(this.buffer.length + chunk.length);
      joined.set(this.buffer);
      joined.set(chunk, this.buffer.length);
      this.buffer = joined;
    }
    const frames: RawFrame[] = [];
    while (this.buffer.length >= HEADER_SIZE) {
      const data = this.buffer;
      if (data[0] !== 0x52 || data[1] !== 0x53 || data[2] !== VERSION) {
        throw this.badMagic();
      }
      const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
      const flags = data[3];
      const msgId = view.getUint32(4);
      const size = view.getUint32(12);
      this.check(flags, msgId, size); // 在等载荷之前检查
      if (data.length < HEADER_SIZE + size) {
        break;
      }
      frames.push({ flags, msgId, seq: view.getUint32(8), payload: data.slice(HEADER_SIZE, HEADER_SIZE + size) });
      this.buffer = data.subarray(HEADER_SIZE + size);
    }
    return frames;
  }
}

/**
 * UTF-8 编码，不依赖 TextEncoder（以后的小游戏环境不一定有，方案 §10“不需要决定、但要知道的风险”）。孤立的代理项按
 * U+FFFD 编码，与 TextEncoder 相同。
 */
export function utf8Encode(text: string): Uint8Array {
  const out: number[] = [];
  for (let i = 0; i < text.length; i++) {
    let code = text.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff && i + 1 < text.length) {
      const low = text.charCodeAt(i + 1);
      if (low >= 0xdc00 && low <= 0xdfff) {
        code = 0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00);
        i++;
      }
    }
    if (code >= 0xd800 && code <= 0xdfff) code = 0xfffd;
    if (code < 0x80) {
      out.push(code);
    } else if (code < 0x800) {
      out.push(0xc0 | (code >> 6), 0x80 | (code & 0x3f));
    } else if (code < 0x10000) {
      out.push(0xe0 | (code >> 12), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
    } else {
      out.push(0xf0 | (code >> 18), 0x80 | ((code >> 12) & 0x3f), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f));
    }
  }
  return Uint8Array.from(out);
}
