"""Summarize one game's Saga delivery histograms, after minus before. No live queries.

Usage: python3 split-waits.py /path/to/one/demo-robot-run
Percentiles are bucket bounds, never differences of percentiles.
"""
import json
import math
from pathlib import Path
import re
import sys

SERIES = re.compile(r'^saga_consumer_(delivery_age|append_to_callback|callback_to_worker)_(count|sum_nanos|bucket)\{([^}]+)\} ([0-9]+)$')
LABEL = re.compile(r'(\w+)="([^"\\]*)"')


def read_metrics(path):
    result = {}
    for line in path.read_text().splitlines():
        match = SERIES.fullmatch(line)
        if not match:
            continue
        name, field, raw, value = match.groups()
        parts = LABEL.findall(raw)
        if ','.join(f'{k}="{v}"' for k, v in parts) != raw:
            raise ValueError(f'unsupported label encoding in demo snapshot: {raw}')
        labels = dict(parts)
        want = {'stage', 'delivery'} | ({'topic'} if name != 'delivery_age' else set()) | ({'le'} if field == 'bucket' else set())
        if set(labels) != want or len(labels) != len(parts):
            raise ValueError(f'unexpected labels: {raw}')
        key = (labels['stage'], labels['delivery'], labels.get('topic', ''), name)
        fields = result.setdefault(key, {})
        label = labels['le'] if field == 'bucket' else field
        if label in fields:
            raise ValueError(f'duplicate {key}/{label}')
        fields[label] = int(value)
    return result


def delta(before, after):
    if before.keys() - after.keys():
        raise ValueError('series vanished between snapshots')
    result = {}
    for key, fields in after.items():
        old = before.get(key, {})
        if old and old.keys() != fields.keys():
            raise ValueError(f'bucket layout changed: {key}')
        inc = {k: v - old.get(k, 0) for k, v in fields.items()}
        if min(inc.values()) < 0:
            raise ValueError(f'counter reset: {key}')
        n = inc.pop('count')
        total = inc.pop('sum_nanos')
        buckets = sorted((float(k), v) for k, v in inc.items())
        if inc['+Inf'] != n or any(a[1] > b[1] for a,b in zip(buckets, buckets[1:])):
            raise ValueError(f'inconsistent histogram: {key}')
        if n:
            bound = next(k for k,v in buckets if v >= (95*n+99)//100)
            result[key] = dict(n=n, sum_ns=total, mean_ms=round(total/n/1e6,4),
                               p95_le_ms=round(bound*1000) if math.isfinite(bound) else '+Inf',
                               gt_4096ms=n-inc['4.096'])
    return result


def summarize(path):
    series = delta(read_metrics(path/'game-metrics-before.txt'), read_metrics(path/'game-metrics-after.txt'))
    topics, matched, totals = {}, {}, []
    for (stage, delivery, topic, name), values in series.items():
        if name == 'delivery_age':
            continue
        topics.setdefault((stage,delivery,topic), {})[name] = values
    if not topics:
        raise ValueError('no sampled split histograms')
    rows = []
    for (stage,delivery,topic), pair in sorted(topics.items()):
        a, b = pair['append_to_callback'], pair['callback_to_worker']
        if a['n'] != b['n']:
            raise ValueError(f'unpaired sample counts: {stage}/{topic}/{delivery}')
        group = matched.setdefault((stage,delivery), [0,0]); group[0] += a['n']; group[1] += a['sum_ns'] + b['sum_ns']
        rows.append(dict(stage=stage, delivery=delivery, topic=topic, **pair))
    for (stage, delivery, _, name), values in sorted(series.items()):
        if name == 'delivery_age':
            n, total = matched.get((stage,delivery), (0,0))
            totals.append(dict(stage=stage, delivery=delivery, age=values, paired_n=n,
                               # Broker age uses wall time, local duration uses monotonic time.
                               # Missing/future stamps at callback may exclude pairs; don't force equality.
                               age_minus_pair_sum_ns=values['sum_ns']-total))
    return {'scope':'one game process; cumulative after minus before', 'pairs':rows, 'totals':totals}


if __name__ == '__main__':
    print(json.dumps(summarize(Path(sys.argv[1])), indent=2, allow_nan=False))
