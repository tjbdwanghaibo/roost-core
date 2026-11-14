"""Materialize a retention-only copy of the hash-pinned M12 entry, then run it."""
import difflib,hashlib,json,os,pathlib,shlex
root=pathlib.Path(os.environ['ROOST_PERF_REPO']).resolve()
source=root/'scripts/perf/demo-robot.sh'
original=source.read_text()
expected='19e37b8de92ae98e062ee45151ac1ef4e7cca408e0d18682ccbb7d5ea3c5b77d'
actual=hashlib.sha256(source.read_bytes()).hexdigest()
if actual != expected:
 raise SystemExit('M12 entry changed; audit cleanup patch before use: '+actual)
cleanup = '''  # Formal M12 retains this new project's data and sources; stop only owned processes.
  if [[ -d "$work_dir/$project" ]]; then
    (
      cd "$work_dir/$project" || exit 1
      sh deploy/dev/run.sh stop
      stop_rc=$?
      mkdir -p "$out/logs"
      cp .dev/*.log "$out/logs/" 2>/dev/null
      exit "$stop_rc"
    ) >"$out/cleanup.log" 2>&1 || rc=1
  fi
  if [[ -n "${etcd_pid:-}" ]]; then
    kill "$etcd_pid" 2>/dev/null
    wait "$etcd_pid" 2>/dev/null
  fi
'''
changes=[
 ('repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"', 'repo_dir='+shlex.quote(str(root))),
 ('  drill_cleanup >"$out/cleanup.log" 2>&1\n', cleanup),
 ('bash "$repo_dir/scripts/mirror-local.sh" clean', 'bash "$repo_dir/scripts/mirror-local.sh" down')
]

rendered=original
for before,after in changes:
 if rendered.count(before)!=1: raise SystemExit('M12 cleanup boundary changed')
 rendered=rendered.replace(before,after)
out=pathlib.Path(os.environ['DEMO_ROBOT_OUT'])
if out.exists() or not out.is_absolute(): raise SystemExit('M12 needs a new absolute output')
entry=out.parent/(out.name+'.entry.sh')
with entry.open('x') as handle: handle.write(rendered)
entry.with_suffix('.source.sh').write_text(original)
entry.with_suffix('.diff').write_text(''.join(difflib.unified_diff(original.splitlines(True),rendered.splitlines(True),fromfile=str(source),tofile=str(entry))))
entry.with_suffix('.json').write_text(json.dumps({'source':str(source),'source_sha256':actual,'entry':str(entry),'entry_sha256':hashlib.sha256(rendered.encode()).hexdigest(),'changes':'repo location; cleanup only; workload and assertions unchanged'},indent=2)+'\n')
os.execvp('bash',['bash',str(entry)])
