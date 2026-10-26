#!/usr/bin/env python3
"""Measure one native server, one receiving client; no Unity editor CPU is included.

CPU is wait4 user+system time for the server (all Go/native threads), divided by
process wall time. 100% means one core. Bytes are RSv2 application bytes, not
Ethernet/TCP overhead. Run against a prebuilt executable to exclude compilation.
"""
import argparse
import json
import os
import platform
import socket
import struct
import subprocess
import time
from pathlib import Path


def exact(conn, size):
    data = bytearray()
    while len(data) < size:
        part = conn.recv(size - len(data))
        if not part:
            if data:
                raise RuntimeError('truncated packet')
            raise EOFError
        data.extend(part)
    return data


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', required=True)
    parser.add_argument('--seconds', type=int, default=60)
    parser.add_argument('--hz', type=int, choices=(20, 50), default=20)
    parser.add_argument('--port', type=int, default=19092)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    if args.seconds < 10:
        parser.error('use at least 10 seconds')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    states = {}
    packets = total = last_tick = last_seq = 0
    epoch = None
    first = last = None
    buckets = {}
    with (output / 'host.log').open('w') as log:
        started = time.monotonic()
        child = subprocess.Popen([args.server, '-listen', f'127.0.0.1:{args.port}',
                                  '-send-hz', str(args.hz), '-duration', f'{args.seconds}s'],
                                 stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = started + 5
            while True:
                try:
                    conn = socket.create_connection(('127.0.0.1', args.port), timeout=2)
                    break
                except ConnectionRefusedError:
                    if time.monotonic() > deadline:
                        raise
                    time.sleep(.02)
            with conn:
                while True:
                    try:
                        header = exact(conn, 16)
                    except EOFError:
                        break
                    magic, version, flags, msg, _, size = struct.unpack('>2sBBIII', header)
                    if (magic, version, flags, msg) != (b'RS', 2, 3, 0x50485901) or size > 1048576:
                        raise RuntimeError('invalid RSv2 header')
                    data = exact(conn, size)
                    magic, version, full, current_epoch, seq, tick, deletes, groups = struct.unpack_from('<2sBBQQQII', data)
                    if magic != b'RC' or version != 1 or seq != last_seq + 1:
                        raise RuntimeError('invalid chunk sequence')
                    if epoch is not None and (epoch != current_epoch or tick <= last_tick):
                        raise RuntimeError('non-monotonic stream')
                    if full:
                        states.clear()
                    epoch, last_seq, last_tick = current_epoch, seq, tick
                    offset = 36
                    for _ in range(deletes):
                        entity_id, = struct.unpack_from('<Q', data, offset)
                        del states[entity_id]
                        offset += 8
                    for _ in range(groups):
                        x, z, count = struct.unpack_from('<iiI', data, offset)
                        offset += 12
                        for _ in range(count):
                            entity_id, length = struct.unpack_from('<QH', data, offset)
                            offset += 10
                            if length != 70 or offset + length > len(data):
                                raise RuntimeError('invalid body payload')
                            states[entity_id] = (x, z, data[offset:offset + length])
                            offset += length
                    if offset != len(data) or set(states) != set(range(1, 22)):
                        raise RuntimeError('invalid complete world')
                    now = time.monotonic()
                    if first is None:
                        first = now
                    last = now
                    packets += 1
                    total += size + 16
                    bucket = int(now - first)
                    buckets[bucket] = buckets.get(bucket, 0) + size + 16
            _, status, usage = os.wait4(child.pid, 0)
            child.returncode = os.waitstatus_to_exitcode(status)
            elapsed = time.monotonic() - started
            if child.returncode != 0 or packets < 2:
                raise RuntimeError(f'server failed: {child.returncode}')
        finally:
            if child.returncode is None:
                child.terminate()
                child.wait(timeout=5)
    stream_seconds = last - first
    result = dict(platform=platform.platform(), machine=platform.machine(), logical_cpus=os.cpu_count(),
                  physics_hz=50, send_hz=args.hz, bodies=21, clients=1,
                  wall_seconds=elapsed, stream_seconds=stream_seconds,
                  cpu_user_seconds=usage.ru_utime, cpu_system_seconds=usage.ru_stime,
                  cpu_one_core_percent=100 * (usage.ru_utime + usage.ru_stime) / elapsed,
                  max_rss_mib=usage.ru_maxrss / (1048576 if platform.system() == 'Darwin' else 1024),
                  packets=packets, last_tick=last_tick, received_bytes=total,
                  application_bytes_per_second=total / stream_seconds,
                  peak_one_second_bytes=max(buckets.values()),
                  application_mbit_per_second=total * 8 / stream_seconds / 1e6)
    (output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
