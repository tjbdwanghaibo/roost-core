// 不连真实对端的测试替身：假的 relay 连接（WebSocketLike）与假的 lobby（fetch）。
import {
  FrameReader,
  LobbyClient,
  RelayChannel,
  RelayOp,
  encodeFrame,
  encodeWelcome,
  type RelayFrame,
  type WebSocketLike,
  type Welcome,
} from "../src/index.js";

/**
 * 一条假的 relay 连接：打开后收到令牌首帧就按 welcome 回 Welcome（或按 closeWith 回 Closed 并断开）；之后测试用 push
 * 推帧给客户端，用 frames / waitFor 看客户端发了什么（已解码，不含令牌）。
 */
export class FakeRelay implements WebSocketLike {
  binaryType = "arraybuffer";
  readyState = 0;
  readonly frames: RelayFrame[] = [];
  token = "";
  onopen: ((event: unknown) => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: unknown) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  private readonly reader = new FrameReader(1 << 24);
  private waiters: (() => void)[] = [];

  constructor(
    private readonly welcome: (Omit<Welcome, "epoch"> & { epoch?: bigint }) | undefined,
    private readonly closeWith?: number,
  ) {
    setTimeout(() => {
      this.readyState = 1;
      this.onopen?.({});
    }, 0);
  }

  send(data: Uint8Array): void {
    for (const frame of this.reader.push(data.slice())) {
      if (frame.channel === RelayChannel.None) {
        this.token = new TextDecoder().decode(frame.payload);
        if (this.closeWith !== undefined) {
          this.push(control(RelayOp.Closed, 0, Uint8Array.of(this.closeWith)));
          queueMicrotask(() => this.close());
        } else if (this.welcome) {
          this.push(control(RelayOp.Welcome, this.welcome.slot, encodeWelcome({ ...this.welcome, epoch: this.welcome.epoch ?? 0n })));
        }
        continue;
      }
      this.frames.push(frame);
      for (const waiter of this.waiters.splice(0)) waiter();
    }
  }

  /** relay → 客户端：一帧一条消息。 */
  push(frame: RelayFrame): void {
    const data = encodeFrame(frame, 1 << 24);
    this.onmessage?.({ data: data.slice().buffer });
  }

  /** 与浏览器一样，onclose 在之后的任务里触发（不在 close 调用里同步触发）。 */
  close(): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    setTimeout(() => this.onclose?.({}), 0);
  }

  /** 客户端发出的某个操作码的控制帧。 */
  controls(op: number): RelayFrame[] {
    return this.frames.filter((f) => f.channel === RelayChannel.Control && f.msgId === op);
  }

  /** 等到 cond 成立（每收到一帧检查一次）。 */
  async waitFor(what: string, cond: () => boolean, timeoutMs = 3000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (!cond()) {
      const left = deadline - Date.now();
      if (left <= 0) throw new Error(`timed out waiting for ${what}`);
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, Math.min(left, 50));
        this.waiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
  }
}

export function control(op: number, slot: number, payload: Uint8Array = new Uint8Array(0)): RelayFrame {
  return { channel: RelayChannel.Control, slot, msgId: op, flags: 0, payload };
}

export const TICKET = { room: { id: "r1" }, relayAddr: "ws://fake/relay", token: "rt1.fake" };

/** 假的 lobby：createRoom / joinRoom 返回 ws://fake 的票据；joinStatus 不为 200 时 join 答这个错误。 */
export class FakeLobby {
  joins = 0;
  joinError: { status: number; code: string } | undefined;
  readonly client: LobbyClient;

  constructor(readonly roomId = "r1") {
    this.client = new LobbyClient("http://lobby.fake", "gt1.fake", async (input) => {
      const url = String(input);
      if (url.endsWith("/join")) {
        this.joins++;
        if (this.joinError) {
          return new Response(JSON.stringify({ error: this.joinError.code, message: "no" }), { status: this.joinError.status });
        }
      }
      return new Response(
        JSON.stringify({
          room: { id: this.roomId, game: "party", mode: "m", name: "", max_members: 4, members: 1, has_password: false, state: "open", host: "a1", host_epoch: 0, created_at_unix: 0 },
          relay_addr: "ws://fake/relay",
          token: `rt1.${this.joins}`,
          expires_at: 0,
        }),
        { status: 200 },
      );
    });
  }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
