#!/usr/bin/env python3
"""汇总Mongo同步累计计数及sample栈；样本驻留占比不是CPU利用率或请求延迟。"""
import argparse
import datetime as dt
import json
import re
from pathlib import Path

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('directory',type=Path)
parser.add_argument('--output',type=Path,required=True)
a=parser.parse_args();p=a.directory
stamp=lambda x:dt.datetime.fromisoformat(x.replace('Z','+00:00')).timestamp()
rows=[json.loads(s) for s in (p/'mongo-status.jsonl').read_text().splitlines()]
gaps=[stamp(y['at'])-stamp(x['at']) for x,y in zip(rows,rows[1:])]
if not gaps or min(gaps)<=0 or max(gaps)>3:
    raise ValueError('clock/sampling gap: cannot align wall-clock and monotonic probe buckets')
phases={k:stamp(v) for k,v in [line.split() for line in (p/'phases.txt').read_text().splitlines()]}
probe=json.loads((p/'probe.json').read_text());begin=stamp(probe['StartedAt'])
windows={'before':(begin+3,phases['mongo_start']-2),'mixed':(phases['mongo_start']+5,phases['mongo_end']-5),'after':(phases['mongo_end']+5,begin+probe['Seconds']-2)}
result={'Windows':windows,'Stages':{},'Stacks':{}}
for name,(lo,hi) in windows.items():
    selected=[r for r in rows if lo<=stamp(r['at'])<=hi]
    if len(selected)<2:raise ValueError('insufficient samples')
    first,last=selected[0],selected[-1];elapsed=stamp(last['at'])-stamp(first['at'])
    nodes=[]
    for n0,n1 in zip(first['nodes'],last['nodes']):
        if n0['pid']!=n1['pid']:raise ValueError('node restarted')
        count=n1['log']['log sync operations']-n0['log']['log sync operations']
        micros=n1['log']['log sync time duration (usecs)']-n0['log']['log sync time duration (usecs)']
        nodes.append({'host':n1['host'],'pid':n1['pid'],'SyncOperations':count,'SyncPerSecond':count/elapsed,'SyncMeanMS':micros/count/1000 if count else None,'SyncSecondsPerWallSecond':micros/1e6/elapsed,'FlowControlMicros':n1['flowControlMicros']-n0['flowControlMicros']})
    secs=[s for s in probe['PerSecond'] if lo<=begin+s['Second'] and begin+s['Second']+1<=hi]
    count=sum(s['Count'] for s in secs)
    result['Stages'][name]={'MongoElapsedSeconds':elapsed,'Nodes':nodes,'MessageCount':count,'DeliveryMeanMS':sum(s['Count']*s['Delivery']['MeanMS'] for s in secs)/count,'PubAckMeanMS':sum(s['Count']*s['PubAck']['MeanMS'] for s in secs)/count}
for phase in ['baseline','mixed']:
    for name in ['nats-1','nats-2','nats-3','mongo-1','mongo-2','mongo-3']:
        text=(p/f'{phase}-{name}.sample.txt').read_text()
        summary={}
        if name.startswith('nats'):
            ticks=int(re.search(r'^\s+(\d+) Thread_',text,re.M).group(1))
            graph=text.split('Total number in stack')[0]
            count=sum(int(n) for n in re.findall(r'(\d+) pwrite\s+\(in libsystem_kernel.dylib\)',graph))
            summary={'SampleTicks':ticks,'PwriteThreadSamples':count,'MeanThreadsInPwrite':count/ticks}
        else:
            match=re.search(r'^\s+(\d+) Thread_[^\n]*: JournalFlusher\n(.*?)(?=^\s+\d+ Thread_|^Total number)',text,re.M|re.S)
            if not match:raise ValueError('missing JournalFlusher')
            total=int(match.group(1));leaves=[int(x) for x in re.findall(r'(\d+) __fcntl\s+\(in libsystem_kernel.dylib\)',match.group(2))]
            summary={'JournalThreadSamples':total,'FcntlSamples':sum(leaves),'FcntlFraction':sum(leaves)/total}
        result['Stacks'][phase+'-'+name]=summary
a.output.write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result,indent=2))
