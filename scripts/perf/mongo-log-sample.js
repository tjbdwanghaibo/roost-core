// mongosh --nodb，只读采样私有副本集。凭据从环境读取，不写入结果。
const initial = new Mongo(process.env.ROOST_DATAENGINE_IT_MONGO_URI);
const hello = initial.getDB('admin').runCommand({hello: 1});
const nodes = hello.hosts.map(host => ({host, admin: new Mongo('mongodb://' + host + '/?directConnection=true').getDB('admin')}));
for (let tick = 0; tick < 115; tick++) {
    const begin = Date.now();
    const row = {at: new Date().toISOString(), tick, primary: hello.primary, nodes: []};
    for (const node of nodes) {
        const status = node.admin.runCommand({serverStatus: 1});
        if (status.ok !== 1) throw new Error('serverStatus failed: ' + node.host);
        const log = {};
        for (const [key, value] of Object.entries(status.wiredTiger.log)) {
            if (/sync|write operations|bytes written|yield time/.test(key)) log[key] = Number(value);
        }
        row.nodes.push({host: node.host, pid: Number(status.pid), log, queue: status.globalLock.currentQueue,
            flowControlMicros: Number(status.flowControl?.timeAcquiringMicros || 0)});
    }
    print(JSON.stringify(row));
    sleep(Math.max(0, 1000 - (Date.now() - begin)));
}
