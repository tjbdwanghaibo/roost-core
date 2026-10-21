import sys,collections,re
c=collections.Counter()
lines=open(sys.argv[1]).read().split('\n')
i=0;n=len(lines)
want=sys.argv[2] if len(sys.argv)>2 else 'Reason="sync"'
while i<n:
    l=lines[i]
    if 'Running->Waiting' in l and want in l:
        frames=[]
        j=i+1
        while j<n and not lines[j].startswith('M='):
            s=lines[j].strip()
            if '@ 0x' in s: frames.append(s.split(' @')[0])
            if s.startswith('Stack='): break
            j+=1
        user=[f for f in frames if not re.match(r'(runtime|internal/sync|sync)\.',f)]
        key=' <- '.join(user[:3])
        c[key]+=1
        i=j
    else: i+=1
for k,v in c.most_common(12): print(v,k)
