import sys,json,collections,statistics
rows=collections.defaultdict(list)
for line in open(sys.argv[1]):
    label,r,js=line.split(' ',2); d=json.loads(js)
    rows[(d['Scene'],d['Rooms'],label)].append(d)
for k in sorted(rows):
    v=rows[k]
    f=lambda key:[x[key] for x in v]
    fmt=lambda a:"%.2f[%s]"%(statistics.median(a),",".join("%.2f"%x for x in a))
    print(k[0],k[1],k[2],"MB/s",fmt(f('AllocationMBps')),"allocs/s",fmt([x/1000 for x in f('MallocsPerSecond')]),"k; KB/frame",fmt(f('AllocKBPerFrame')),"allocs/frame",fmt(f('AllocsPerFrame')),"GCs",f('GCs'),"Hz",fmt(f('MeanPhysicsHz')),"deliv",fmt(f('DeliveredHzPerClient')),"pollP99",fmt(f('PollP99ms')))
