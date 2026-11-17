// 演示用的房主规则（纯 JS 模块，实现 HostRules）：每个槽位一个计数器，成员点一下 +1；每一步推进 tick，
// 有变化或每 20 步广播一次全量状态（msgId 2）。不碰网络、计时器与 DOM，可以在 Worker 里跑。
const encoder = new TextEncoder();
const decoder = new TextDecoder();

export const MSG_CLICK = 1;
export const MSG_STATE = 2;

export class CounterRules {
  constructor() {
    this.state = { tick: 0, counts: {}, restoredFrom: "new" };
    this.dirty = true;
  }
  restore(bytes) {
    this.state = bytes ? { ...JSON.parse(decoder.decode(bytes)), restoredFrom: "state" } : { tick: 0, counts: {}, restoredFrom: "new" };
    this.dirty = true;
  }
  input(slot, msgId) {
    if (msgId !== MSG_CLICK) return;
    this.state.counts[slot] = (this.state.counts[slot] ?? 0) + 1;
    this.dirty = true;
  }
  step() {
    this.state.tick++;
    if (!this.dirty && this.state.tick % 20 !== 0) return [];
    this.dirty = false;
    return [{ target: 0, msgId: MSG_STATE, payload: encoder.encode(JSON.stringify(this.state)) }];
  }
  snapshot() {
    return encoder.encode(JSON.stringify(this.state));
  }
}
