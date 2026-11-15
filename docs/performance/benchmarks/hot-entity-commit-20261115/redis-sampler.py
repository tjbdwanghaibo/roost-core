# 在 M-15 复现期间采 Redis：每 50ms 一次 PING 往返（新连接之外的长连接），每秒一次 INFO（cpu、commandstats）。
# 用法：python3 redis-sampler.py host port out.tsv stopfile
import socket, sys, time, os
host, port, out, stop = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
s = socket.create_connection((host, port))
def cmd(parts):
    s.sendall(('*%d\r\n' % len(parts) + ''.join('$%d\r\n%s\r\n' % (len(p), p) for p in parts)).encode())
    buf = b''
    while True:
        buf += s.recv(1 << 20)
        if buf.startswith(b'+') and buf.endswith(b'\r\n'):
            return buf
        if buf.startswith(b'$'):
            n = int(buf[1:buf.index(b'\r\n')])
            if len(buf) >= buf.index(b'\r\n') + 2 + n + 2:
                return buf
with open(out, 'w') as f:
    last_info = 0
    while not os.path.exists(stop):
        t = time.time()
        cmd(['PING'])
        rtt = time.time() - t
        f.write('ping\t%.3f\t%.6f\n' % (t, rtt))
        if t - last_info >= 1:
            last_info = t
            info = cmd(['INFO', 'all']).decode(errors='replace')
            keep = [l for l in info.split('\r\n') if l.startswith(('used_cpu_sys:', 'used_cpu_user:', 'cmdstat_eval', 'cmdstat_evalsha', 'cmdstat_get', 'cmdstat_set', 'instantaneous_ops_per_sec', 'connected_clients'))]
            f.write('info\t%.3f\t%s\n' % (t, ' '.join(keep)))
        f.flush()
        time.sleep(0.05)
