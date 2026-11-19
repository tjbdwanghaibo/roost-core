import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

const fromSDK = (file) => pathToFileURL(resolve(process.argv[2], file));
const { ConnectionState, GameClient } = await import(fromSDK('dist/gate/index.js'));
const { FakeClock, ScriptedNetwork, settle } = await import(fromSDK('dist-test/test/gate-fake.js'));

for (const trigger of ['write', 'peer-close']) {
  for (const mode of ['raw-ok', 'raw-throws', 'game-client-throws']) {
    const clock = new FakeClock();
    const network = new ScriptedNetwork();
    const client = new GameClient({
      webSocketUrl: 'ws://localhost/ws', transportFactory: network.factory,
      timers: clock, heartbeatIntervalMs: 0, requestTimeoutMs: 100, autoReconnect: false,
    });
    const connection = client.connection;
    await connection.connect('t');
    const body = new Uint8Array();
    const appErrors = [];
    client.on('error', (error) => appErrors.push(error.message));
    let first = 0;
    const callback = () => {
      first++;
      if (mode !== 'raw-ok') throw new Error('view disposed');
    };
    if (mode === 'game-client-throws') {
      client.request(5, 6, body, (payload) => payload, callback, callback);
    } else {
      connection.beginRequest(5, 6, body, callback);
    }
    let second = 'pending';
    connection.requestAsync(7, 8, body).then(() => { second = 'success'; }, e => { second = e.name; });
    let escaped;
    try {
      if (trigger === 'write') {
        network.last.send = () => { throw new Error('local write failed'); };
        connection.notify(9, body);
      } else {
        network.last.drop();
      }
    } catch (error) { escaped = error.message; }
    await settle();
    await clock.advance(1000);
    console.log(JSON.stringify({ trigger, mode, first, second, state: ConnectionState[connection.state],
      transportClosed: network.last.closedByClient, escaped, appErrors, time: clock.now() }));
  }
}
