import subprocess, sys, json
root='/Users/whb/roost/roost-core-wt-hbbatch'
muts=json.load(open(sys.argv[1]))
out=open(sys.argv[2],'a')
for m in muts:
    p=root+'/'+m['file']
    orig=open(p).read()
    assert orig.count(m['old'])==1, (m['name'], orig.count(m['old']))
    open(p,'w').write(orig.replace(m['old'],m['new']))
    try:
        r=subprocess.run(['go','test','-count=1','-timeout',m.get('timeout','60s'),'-run',m['test'],m['pkg']],cwd=root,capture_output=True,text=True,env={**__import__('os').environ,'GOWORK':'off'})
        lines=[l for l in (r.stdout+r.stderr).splitlines() if ('FAIL' in l or 'panic:' in l or '_test.go:' in l or l.startswith('ok')) and 'kcp' not in l][:6]
        status='RED' if r.returncode!=0 else 'GREEN(!)'
        out.write(f"## {m['name']}\nmutation: {m['desc']}\ntest: {m['test']}\nresult: {status}\n"+"\n".join(lines)+"\n\n")
        print(m['name'],status)
    finally:
        open(p,'w').write(orig)
