// 演示页：WebHost + Worker 里的规则。lobby 经 serve.mjs 同源代理在 /lobby 下（含测试用的 /lobby/harness/game-token）。
// 刷新页面：账号存在 sessionStorage，载入时 resume()——宽限期内回来仍是房主，规则从存档恢复。
import { LobbyClient, WebHost, WorkerRunner, isMobileUserAgent, RelayReason } from "../dist/index.js";
import { MSG_CLICK, MSG_STATE } from "./rules.js";

const $ = (id) => document.getElementById(id);
const log = (text) => {
  const line = `${new Date().toLocaleTimeString()} ${text}\n`;
  $("log").textContent = line + $("log").textContent;
};
const lobbyBase = `${location.origin}/lobby`;
const accountKey = "roost.demo.account";
$("account").value = sessionStorage.getItem(accountKey) ?? `web-${Math.random().toString(36).slice(2, 7)}`;

let webHost;
let lastTickAt = 0;

async function open() {
  if (webHost) return webHost;
  const account = $("account").value.trim();
  sessionStorage.setItem(accountKey, account);
  const reply = await fetch(`${lobbyBase}/harness/game-token`, { method: "POST", body: JSON.stringify({ account }) });
  const { token } = await reply.json();
  const worker = new Worker(new URL("./rules.worker.js", import.meta.url), { type: "module" });
  const runner = new WorkerRunner(worker);
  runner.onError = (message) => log(`规则出错：${message}`);
  webHost = new WebHost({
    lobby: new LobbyClient(lobbyBase, token),
    account,
    runner,
    relay: { game: "party", reconnect: { maxAttempts: 5 }, snapshotIntervalMs: 3000 },
    hiddenHandoffMs: isMobileUserAgent(navigator.userAgent) ? 0 : 5000,
  });
  const c = webHost.client;
  webHost.onFrame((f) => {
    if (f.msgId !== MSG_STATE) return;
    const state = JSON.parse(new TextDecoder().decode(f.payload));
    $("tick").textContent = state.tick;
    $("from").textContent = state.restoredFrom;
    $("sender").textContent = f.slot;
    lastTickAt = performance.now();
    $("counts").innerHTML = "<tr><th>槽位</th><th>次数</th></tr>" + Object.entries(state.counts).map(([s, n]) => `<tr><td>${s}</td><td>${n}</td></tr>`).join("");
  });
  webHost.onHiddenHandoff((target) => log(target === undefined ? "页面隐藏：没有可转交的成员，留在原地" : `页面隐藏：已转交给槽位 ${target}`));
  c.onMemberJoined((m) => log(`成员上线 槽位 ${m.slot} ${m.account}`));
  c.onMemberLeft((m) => log(`成员下线 槽位 ${m.slot} ${m.account}（${RelayReason[m.reason]}）`));
  // 这些都是业务自己的回调（日志）：WebHost 走 SDK 的内部监听，这里注册不会把它顶掉
  c.onYouAreHost((t) => {
    const text = `epoch ${t.epoch}，来源 ${["无", "交接数据", "快照"][t.source]}，快照 seq ${t.snapshotSeq}，年龄 ${t.snapshotAgeMs}ms`;
    $("takeover").textContent = text;
    log(`YouAreHost ${text}`);
  });
  c.onYouAreMember((e) => log(`YouAreMember epoch ${e}`));
  c.onHostChanging((e) => log(`HostChanging epoch ${e}`));
  c.onHostChanged((h) => log(`HostChanged epoch ${h.epoch}：新房主槽位 ${h.slot} ${h.account}${h.self ? "（自己）" : ""}`));
  c.onHostResumed((r) => log(`HostResumed epoch ${r.epoch}：房主 ${r.slot} 回来了`));
  c.onRejected((r) => log(`Rejected 操作码 ${r.op} 原因 ${r.code} seq ${r.seq}`));
  c.onSnapshotSent((s) => {
    $("lastSnapshot").textContent = `seq ${s.seq}，${new Date().toLocaleTimeString()}`;
    log(`快照 seq ${s.seq}，${s.data.length} 字节（压缩后）`);
  });
  c.onDisconnected((d) => log(`断开：${RelayReason[d.reason]}${d.reconnecting ? "，重连中" : ""}`));
  c.onReconnected((w) => log(`重连成功：槽位 ${w.slot}${w.host ? "（房主）" : ""}`));
  setInterval(render, 250);
  return webHost;
}

function render() {
  const c = webHost.client;
  $("status").textContent = c.connected ? `已连接 房间 ${webHost.room}` : "未连接";
  $("role").textContent = c.hostRole;
  $("role").className = c.hostRole;
  $("slot").textContent = c.welcome.slot;
  $("hostSlot").textContent = c.welcome.hostSlot;
  $("epoch").textContent = c.epoch;
  $("seq").textContent = c.snapshotSeq;
  $("members").textContent = [...c.onlineMembers].map(([s, a]) => `${s}:${a}`).join(", ") || "-";
  if (webHost.room) $("room").value = webHost.room;
  $("running").textContent = webHost["runner"].running ? "在跑" : "停着";
  $("tickAge").textContent = lastTickAt ? `${((performance.now() - lastTickAt) / 1000).toFixed(1)}s 前更新` : "-";
  $("visibility").textContent = document.visibilityState;
}

// 调试：有的浏览器面板里后台标签页仍报 visible，用按钮覆盖 visibilityState 并派发事件，走“隐藏满 5s 让出房主”这条路
function simulateVisibility(state) {
  if (state === undefined) delete document.visibilityState;
  else Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
  document.dispatchEvent(new Event("visibilitychange"));
  log(`模拟可见性：${document.visibilityState}`);
}
$("hide").onclick = () => simulateVisibility("hidden");
$("show").onclick = () => simulateVisibility(undefined);

const guard = (fn) => async () => {
  try {
    await fn();
  } catch (error) {
    log(`失败：${error}`);
  }
};
$("create").onclick = guard(async () => {
  const w = await (await open()).create({ game: "party", mode: "demo", maxMembers: 8 });
  log(`建房成功，槽位 ${w.slot}`);
});
$("join").onclick = guard(async () => {
  const w = await (await open()).join($("room").value.trim());
  log(`加入成功，槽位 ${w.slot}${w.host ? "（房主）" : ""}`);
});
$("list").onclick = guard(async () => {
  const account = $("account").value.trim();
  const reply = await fetch(`${lobbyBase}/harness/game-token`, { method: "POST", body: JSON.stringify({ account }) });
  const rooms = await new LobbyClient(lobbyBase, (await reply.json()).token).listRooms("party", "demo");
  $("rooms").innerHTML = rooms.map((r) => `<div>${r.id} 房主 ${r.host} ${r.members}/${r.maxMembers}</div>`).join("") || "（没有）";
});
$("click").onclick = () => webHost?.input(MSG_CLICK, new Uint8Array(0));
$("snapshot").onclick = guard(async () => log(`手动快照 seq ${await webHost.client.pushSnapshot()}`));
$("transfer").onclick = guard(async () => log(`转交给槽位：${await webHost.handOff(Number($("target").value))}`));
$("handoff").onclick = guard(async () => log(`让出：${await webHost.handOff()}`));
$("leave").onclick = guard(async () => {
  await webHost.leave();
  log("已离开");
});

// 刷新后：同一个标签页有存档就回去
if (sessionStorage.getItem("roost.relay.host")) {
  guard(async () => {
    const w = await (await open()).resume();
    if (w) log(`刷新后回到房间 ${webHost.room}：槽位 ${w.slot}${w.host ? "，仍是房主" : "，已是成员"}`);
  })();
}
