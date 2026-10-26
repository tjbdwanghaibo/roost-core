#!/usr/bin/env python3
"""20 independent controlling TCP clients; CPU/resource accounting covers server only."""
import argparse
import json
import math
import os
import platform
import socket
import struct
import subprocess
import threading
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


def receive(conn, stats, stop, radius, margin):
    states = {}
    baseline = None
    epoch = None
    ever_seen = set()
    seq = 0
    try:
        while not stop.is_set():
            try:
                header = exact(conn, 16)
            except EOFError:
                break
            magic, version, flags, msg, _, size = struct.unpack('>2sBBIII', header)
            if (magic, version, flags, msg) != (b'RS', 2, 3, 0x50485901) or size > 1048576:
                raise RuntimeError('invalid RSv2 header')
            data = exact(conn, size)
            magic, version, full, current_epoch, current_seq, tick, deletes, groups = struct.unpack_from('<2sBBQQQII', data)
            if magic != b'RC' or version != 1 or full not in (0, 1) or current_seq != seq + 1:
                raise RuntimeError('invalid chunk sequence')
            if epoch is not None and (epoch != current_epoch or tick <= stats['last_tick']):
                raise RuntimeError('non-monotonic stream')
            old_ids = set(states)
            if full:
                states.clear()
            epoch, seq = current_epoch, current_seq
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
                    values = struct.unpack_from('<13fB', data, offset)
                    if not all(math.isfinite(v) for v in values[:13]) or values[-1] > 1:
                        raise RuntimeError('non-finite body')
                    position = (values[0] + 25*x, values[1], values[2] + 25*z)
                    if position[1] < -.1 or abs(position[0]) > 25.1 or abs(position[2]) > 25.1:
                        raise RuntimeError(f'body escaped terrain: tick={tick}, id={entity_id}, position={position}')
                    states[entity_id] = position
                    offset += length
            own = stats['slot'] + 50
            if offset != len(data) or own not in states or not set(states).issubset(range(1, 72)):
                raise RuntimeError('invalid AOI membership')
            if radius == 0 and len(states) != 71:
                raise RuntimeError('incomplete full-scene view')
            if radius > 0:
                center = states[own]
                if any(math.dist(p, center) > radius+margin+.001 for p in states.values()):
                    raise RuntimeError('object outside AOI exit radius')
            new_ids = set(states)
            entering = new_ids - old_ids
            stats['entered_objects'] += len(entering)
            stats['left_objects'] += len(old_ids - new_ids)
            stats['reentered_objects'] += len(entering & ever_seen)
            ever_seen.update(new_ids)
            stats['visible_object_samples'] += len(states)
            stats['min_visible_objects'] = min(stats['min_visible_objects'], len(states))
            stats['max_visible_objects'] = max(stats['max_visible_objects'], len(states))
            if baseline is None:
                baseline = dict(states)
            own = stats['slot'] + 50
            stats['min_own_height_m'] = min(stats['min_own_height_m'], states[own][1])
            stats['max_own_height_m'] = max(stats['max_own_height_m'], states[own][1])
            stats['max_own_vertical_travel_m'] = max(stats['max_own_vertical_travel_m'], abs(states[own][1]-baseline[own][1]))
            stats['max_own_displacement_m'] = max(stats['max_own_displacement_m'], math.dist(states[own], baseline[own]))
            if 71 in states:
                baseline.setdefault(71, states[71])
                stats['big_ball_displacement_m'] = max(stats['big_ball_displacement_m'], math.dist(states[71], baseline[71]))
            stats['packets'] += 1
            stats['received_bytes'] += size + 16
            stats['last_tick'] = tick
    except Exception as error:
        stats['receive_error'] = str(error)
    finally:
        stop.set()


def control(conn, stats, stop):
    seq = 0
    deadline = time.monotonic()
    angle = (stats['slot'] - .5) * 2 * math.pi / 20
    try:
        while not stop.is_set():
            seq += 1
            x, z = -math.cos(angle), -math.sin(angle)
            phase = ((seq-1)//60) % 4
            if phase == 1:
                x, z = -z, x
            elif phase == 2:
                x, z = -x, -z
            elif phase == 3:
                x, z = z, -x
            gain = 8 + (stats['slot']-1) % 3 * 2
            conn.sendall(struct.pack('>Iff', seq, x*gain, z*gain))
            stats['inputs_sent'] += 1
            stats['sent_bytes'] += 12
            deadline += .05
            delay = deadline - time.monotonic()
            if delay < 0:
                stats['late_inputs'] += 1
                deadline = time.monotonic()
            stop.wait(max(0, delay))
    except OSError as error:
        if not stop.is_set():
            stats['send_error'] = str(error)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', default='artifacts/physics-viewer')
    parser.add_argument('--seconds', type=int, default=60)
    parser.add_argument('--hz', type=int, choices=(20, 50), default=20)
    parser.add_argument('--aoi-radius', type=float, default=12)
    parser.add_argument('--aoi-margin', type=float, default=2)
    parser.add_argument('--port', type=int, default=19092)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    if args.seconds < 10:
        parser.error('use at least 10 seconds')
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    connections, threads, results, stops = [], [], [], []
    with (output/'host.log').open('w') as log:
        started = time.monotonic()
        child = subprocess.Popen([args.server, '-scenario', 'crowd', '-listen', f'127.0.0.1:{args.port}',
                                  '-send-hz', str(args.hz), '-aoi-radius', str(args.aoi_radius), '-aoi-margin', str(args.aoi_margin), '-duration', f'{args.seconds}s'], stdout=log, stderr=subprocess.STDOUT)
        try:
            for slot in range(1, 21):
                while True:
                    try:
                        conn = socket.create_connection(('127.0.0.1', args.port), timeout=3)
                        break
                    except ConnectionRefusedError:
                        if time.monotonic()-started > 5:
                            raise
                        time.sleep(.02)
                conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
                conn.sendall(struct.pack('>I', slot))
                connections.append(conn)
                stats = dict(slot=slot, packets=0, received_bytes=0, inputs_sent=0, sent_bytes=4,
                             last_tick=0, late_inputs=0, max_own_displacement_m=0, big_ball_displacement_m=0,
                             min_own_height_m=float("inf"), max_own_height_m=float("-inf"), max_own_vertical_travel_m=0,
                             visible_object_samples=0, min_visible_objects=71, max_visible_objects=0,
                             entered_objects=0, left_objects=0, reentered_objects=0)
                results.append(stats)
                stop = threading.Event()
                stops.append(stop)
                for function in (receive, control):
                    arguments = (conn, stats, stop, args.aoi_radius, args.aoi_margin) if function is receive else (conn, stats, stop)
                    thread = threading.Thread(target=function, args=arguments, daemon=True)
                    threads.append(thread)
                    thread.start()
            _, status, usage = os.wait4(child.pid, 0)
            child.returncode = os.waitstatus_to_exitcode(status)
            elapsed = time.monotonic()-started
            for thread in threads:
                thread.join(timeout=5)
            if any(thread.is_alive() for thread in threads):
                raise RuntimeError('client did not drain')
        finally:
            for stop in stops:
                stop.set()
            for conn in connections:
                conn.close()
            if child.returncode is None:
                child.terminate()
                child.wait(timeout=5)
    (output/'clients.json').write_text(json.dumps(results, indent=2)+'\n')
    # A normal close may race the final input send; retain its error separately.
    failures = [r for r in results if 'receive_error' in r or r['last_tick'] < (args.seconds-3)*45 or r['max_own_displacement_m'] < 1]
    report = dict(platform=platform.platform(), logical_cpus=os.cpu_count(), clients=len(results), dynamic_bodies=71,
                  physics_hz=50, input_hz_per_client=20, send_hz=args.hz, wall_seconds=elapsed,
                  aoi_radius=args.aoi_radius, aoi_margin=args.aoi_margin,
                  average_visible_objects=sum(r['visible_object_samples'] for r in results)/sum(r['packets'] for r in results),
                  min_visible_objects=min(r['min_visible_objects'] for r in results),
                  max_visible_objects=max(r['max_visible_objects'] for r in results),
                  entered_objects=sum(r['entered_objects'] for r in results),
                  left_objects=sum(r['left_objects'] for r in results),
                  reentered_objects=sum(r['reentered_objects'] for r in results),
                  cpu_one_core_percent=100*(usage.ru_utime+usage.ru_stime)/elapsed,
                  cpu_user_seconds=usage.ru_utime, cpu_system_seconds=usage.ru_stime,
                  max_rss_mib=usage.ru_maxrss/(1048576 if platform.system()=='Darwin' else 1024),
                  received_bytes=sum(r['received_bytes'] for r in results), sent_bytes=sum(r['sent_bytes'] for r in results),
                  inputs_sent=sum(r['inputs_sent'] for r in results), late_inputs=sum(r['late_inputs'] for r in results),
                  min_client_tick=min(r['last_tick'] for r in results), max_client_tick=max(r['last_tick'] for r in results),
                  min_controlled_ball_displacement_m=min(r['max_own_displacement_m'] for r in results),
                  players_with_vertical_travel_over_1m=sum(r['max_own_vertical_travel_m']>1 for r in results),
                  max_player_height_m=max(r['max_own_height_m'] for r in results),
                  min_player_height_m=min(r['min_own_height_m'] for r in results),
                  big_ball_displacement_m=max(r['big_ball_displacement_m'] for r in results), failed_clients=len(failures))
    report['downlink_mbit_per_second']=report['received_bytes']*8/elapsed/1e6
    report['uplink_kbyte_per_second']=report['sent_bytes']/elapsed/1000
    report['per_client_downlink_kbyte_per_second']=report['received_bytes']/elapsed/20/1000
    (output/'result.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))
    if child.returncode != 0 or failures:
        raise RuntimeError(f'validation failed: server={child.returncode}, clients={len(failures)}')


if __name__ == '__main__':
    main()
