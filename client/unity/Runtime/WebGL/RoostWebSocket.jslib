// Unity WebGL 上浏览器 WebSocket 的 JS 一侧（Roost.Unity 包，维护者 2026-11-18 定方案 B；C# 一侧见 ../BrowserWebSocket.cs，
// 连玩家接入与开房间 relay 共用）。不从 JS 回调 C#：收到的二进制消息排成字节队列，C# 每帧轮询（RoostWsReceive / RoostWsState）。
// 状态码：0 连接中、1 已连上、2 已关闭、3 出错（与 BrowserWebSocket 的常量一致）。
// 两边的服务端每条消息恰好一帧，但这里按字节流交出，不依赖这一点；文本消息当协议错误，关掉连接。
var RoostWebSocket = {
  $RoostWs: { next: 1, sockets: {} },

  RoostWsOpen: function (urlPtr) {
    var url = UTF8ToString(urlPtr);
    var id = RoostWs.next++;
    var entry = { ws: null, state: 0, chunks: [], head: 0 };
    RoostWs.sockets[id] = entry;
    try {
      entry.ws = new WebSocket(url);
    } catch (e) {
      entry.state = 3;
      return id;
    }
    entry.ws.binaryType = "arraybuffer";
    entry.ws.onopen = function () { if (entry.state === 0) entry.state = 1; };
    entry.ws.onmessage = function (event) {
      if (event.data instanceof ArrayBuffer) {
        if (event.data.byteLength > 0) entry.chunks.push(new Uint8Array(event.data));
      } else {
        entry.state = 3;
        entry.ws.close();
      }
    };
    entry.ws.onerror = function () { entry.state = 3; };
    entry.ws.onclose = function () { if (entry.state !== 3) entry.state = 2; };
    return id;
  },

  RoostWsState: function (id) {
    var entry = RoostWs.sockets[id];
    return entry ? entry.state : 2;
  },

  // 发一整帧（一条二进制消息）。返回 1 成功，0 没连上或已关闭。
  RoostWsSend: function (id, ptr, length) {
    var entry = RoostWs.sockets[id];
    if (!entry || entry.state !== 1) return 0;
    try {
      entry.ws.send(HEAPU8.slice(ptr, ptr + length));
      return 1;
    } catch (e) {
      return 0;
    }
  },

  // 把排着的字节拷进 C# 的 buffer，至多 length 个，返回拷了几个（0 表示现在没有）。
  RoostWsReceive: function (id, ptr, length) {
    var entry = RoostWs.sockets[id];
    if (!entry) return 0;
    var copied = 0;
    while (copied < length && entry.chunks.length > 0) {
      var chunk = entry.chunks[0];
      var take = Math.min(length - copied, chunk.length - entry.head);
      HEAPU8.set(chunk.subarray(entry.head, entry.head + take), ptr + copied);
      copied += take;
      entry.head += take;
      if (entry.head === chunk.length) {
        entry.chunks.shift();
        entry.head = 0;
      }
    }
    return copied;
  },

  // 还没发出去的字节数（WebSocket.bufferedAmount）。
  RoostWsBuffered: function (id) {
    var entry = RoostWs.sockets[id];
    return entry && entry.ws ? entry.ws.bufferedAmount : 0;
  },

  RoostWsClose: function (id) {
    var entry = RoostWs.sockets[id];
    if (!entry) return;
    delete RoostWs.sockets[id];
    try { if (entry.ws) entry.ws.close(); } catch (e) {}
  },
};

autoAddDeps(RoostWebSocket, "$RoostWs");
mergeInto(LibraryManager.library, RoostWebSocket);
