"""Read pinned git blobs, run an isolated source replay, and compare the committed trace.

No source checkout or project files are modified. No network, Go packages to install,
SSR business handlers, target decision import, or Go workspace required.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

HERE = Path(__file__).resolve().parent
DATA = HERE.parent
COMMIT = '854fe4f7d8492fe7e42fb143515b9cc814d5d99f'
PKG = 'kingdom/modules/ai_engine'
CORE = ('action builder compose_node condition conf_data engine errors event_checker '
        'event_data event leaf_node node parser root_node root run_data selector sequence').split()
SOURCES = ([f'{PKG}/ai_engine.go', 'kingdom/utils/editor_parser/parser.go', 'kingdom/utils/simple_timer/timer.go'] +
           [f'{PKG}/internal/{f}.go' for f in CORE])


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('ssr', type=Path, help='root of SSR git checkout (containing ssr/go.mod)')
    p.add_argument('work', type=Path, help='new private directory; must not exist')
    p.add_argument('--capture', action='store_true', help='capture only, rather than comparing trace')
    opts = p.parse_args()
    work = opts.work.resolve()
    # Avoid placing original private sources in the deliverable or original repository.
    if work.is_relative_to(opts.ssr.resolve()) or work.is_relative_to(HERE.parents[5]):
        p.error('work must be outside both code repositories')
    work.mkdir(parents=True, exist_ok=False)
    def blob(path):
        return subprocess.check_output(['git', '-C', opts.ssr.resolve(), 'show', COMMIT + ':ssr/' + path])
    module = re.search(rb'^module\s+(\S+)', blob('go.mod'), re.M)[1].decode()
    wlog = re.search(rb'"([^"]+/misc/wlog)"', blob(PKG + '/ai_engine.go'))[1].decode()
    root = work / 'src' / module
    def put(path, content):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content if isinstance(content, bytes) else content.encode())
    manifest = {}
    for name in SOURCES:
        content = blob(name)
        put(root / name, content)
        manifest[name] = hashlib.sha256(content).hexdigest()
    put(work / 'source-sha256.json', json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    if manifest != json.loads((HERE / 'source-sha256.json').read_bytes()):
        raise SystemExit('source blob SHA-256 mismatch')
    # Only peripheral imports not exercising decisions in this fixture. Unchanged engine,
    # arg parsing, event filter/comparators, action-limit logic and timer implementations.
    put(root / 'kingdom/cfg/names.go', '''package cfg
func LevelTemplateActionKey(uint32) string { return "synthetic" }
func LevelTemplateConditionKey(uint32) string { return "synthetic" }
func LevelTemplateEventKey(uint32) string { return "synthetic" }
''')
    put(work / 'src' / wlog / 'log.go', '''package wlog
import "fmt"
type LogLevel int
func Errorf(format string, args ...interface{}) { panic(fmt.Sprintf(format,args...)) }
func Errorw(msg string, args ...interface{}) { panic(msg) }
''')
    # Used solely to populate/lookup comparator display names, never the comparator itself.
    put(work / 'src/github.com/modern-go/reflect2/display.go', '''package reflect2
import "reflect"
func RTypeOf(v interface{}) uintptr { return reflect.ValueOf(v).Pointer() }
''')
    # Use the original structs and their JSON tags; alias only to prevent recursion.
    # This is a fixture decoder, not a substitute for easyjson's general input semantics.
    put(root / PKG / 'internal/oracle_decode.go', '''package internal
import "encoding/json"
func(e *EditorConf) UnmarshalJSON(data []byte)error{
 type wire EditorConf
 return json.Unmarshal(data, (*wire)(e))
}
''')
    put(root / PKG / 'cmd/oracle/main.go', (HERE / 'main.go.in').read_text().replace('SRCMOD', module).replace('WLOG', wlog))
    env = dict(os.environ, GO111MODULE='off', GOWORK='off', GOPROXY='off', GOSUMDB='off', GOTOOLCHAIN='local',
               GOMAXPROCS='2', GOFLAGS='-p=2', CGO_ENABLED='0', GOENV='off', GOTELEMETRY='off', GOPATH=str(work),
               GOCACHE=str(work / 'cache'), GOTMPDIR=str(work / 'tmp'))
    (work / 'tmp').mkdir()
    run = subprocess.run(['go', 'run', './' + PKG + '/cmd/oracle', DATA / 'synthetic.editor.json', DATA / 'inputs.json'],
                         cwd=root, env=env, check=False, capture_output=True)
    put(work / 'stderr.log', run.stderr)
    if run.returncode:
        raise SystemExit('source replay failed; see ' + str(work / 'stderr.log'))
    put(work / 'trace.json', run.stdout)
    if not opts.capture and json.loads(run.stdout) != json.loads((DATA / 'source-trace.json').read_bytes()):
        raise SystemExit('source trace mismatch; captured trace: ' + str(work / 'trace.json'))
    print(f'{COMMIT}: {len(manifest)} pinned blobs; {len(json.loads(run.stdout))} transitions; {work}/trace.json')


if __name__ == '__main__':
    main()
