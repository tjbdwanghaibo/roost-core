import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
const sdk = (file) => pathToFileURL(resolve(process.argv[2], file)).href;
const { Connection, ConnectionState } = await import(sdk("dist/gate/index.js"));
const { FakeClock, settle } = await import(sdk("dist-test/test/gate-fake.js"));
for (const immediate of [false, true]) {
  const timers = new FakeClock();
  const connection = new Connection({ webSocketUrl: "ws://localhost/ws", timers, connectTimeoutMs: 5000 });
  let calls = 0, aborted = false, outcome = "pending";
  if (immediate) connection.on("stateChanged", state => {
    if (state === ConnectionState.Connecting) connection.disconnect();
  });
  const result = connection.connect(async signal => {
    calls++;
    aborted = signal.aborted;
    signal.addEventListener("abort", () => { aborted = true; }, { once: true });
    return new Promise(() => {});
  }).then(() => { outcome = "connected"; }, err => { outcome = err.name; });
  if (!immediate) connection.disconnect();
  await settle();
  console.log(JSON.stringify({ immediate, time: timers.now(), state: ConnectionState[connection.state], calls, aborted, outcome }));
  await timers.advance(5000);
  await result;
  console.log(JSON.stringify({ immediate, time: timers.now(), aborted, outcome }));
}
