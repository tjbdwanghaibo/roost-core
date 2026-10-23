#!/usr/bin/env python3
"""只读 macOS 秒级资源采样。累计计数保留原值，分析时求差；不将磁盘带宽当延迟。"""
import argparse
import datetime
import json
import plistlib
import subprocess
import threading
import time
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--output', type=Path, required=True)
parser.add_argument('--seconds', type=int, default=115)
args = parser.parse_args()
if not 1 <= args.seconds <= 600:
    parser.error('seconds must be 1..600')
args.output.mkdir(parents=True, exist_ok=True)

def command(*argv):
    return subprocess.check_output(argv, text=True, timeout=4).strip()

def stamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()

# iostat 第一行数值是开机至今均值，分析时丢弃；其后是1秒间隔。
def disk_cpu():
    with (args.output / 'iostat.jsonl').open('w') as out:
        proc = subprocess.Popen(['iostat', '-w', '1', '-c', str(args.seconds)], stdout=subprocess.PIPE, text=True)
        for line in proc.stdout:
            out.write(json.dumps({'at': stamp(), 'line': line.strip()})+'\n')
            out.flush()
        if proc.wait():
            raise RuntimeError('iostat failed')

thread = threading.Thread(target=disk_cpu)
thread.start()
start = time.monotonic()
with (args.output / 'resources.jsonl').open('w') as out:
    for tick in range(args.seconds):
        delay = start + tick - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        row = {'at': stamp(), 'tick': tick}
        try:
            row['vm'] = command('vm_stat')
            row['processes'] = command('ps', '-axo', 'pid,time,rss,comm')
            row['network'] = command('netstat', '-ibn')
            tree = plistlib.loads(subprocess.check_output(['ioreg', '-a', '-r', '-c', 'IOBlockStorageDriver'], timeout=4))
            row['disks'] = [{'name': d.get('IORegistryEntryName'), 'id': d.get('IORegistryEntryID'), 'statistics': d.get('Statistics')} for d in tree if d.get('Statistics')]
            if tick % 5 == 0:
                row['pressure'] = command('memory_pressure', '-Q')
                row['swap'] = command('sysctl', 'vm.swapusage')
        except Exception as exc:
            row['error'] = str(exc)
        row['sample_seconds'] = time.monotonic() - start - tick
        out.write(json.dumps(row)+'\n')
        out.flush()
thread.join()
