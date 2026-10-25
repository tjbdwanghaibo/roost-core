#!/usr/bin/env python3
"""生成临时 Go overlay：在 nats.go 收包解析后、入订阅队列前记录 Saga 消息。不会修改依赖缓存或生产驱动。"""
import argparse
import hashlib
import json
import shutil
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--repo', type=Path, required=True)
parser.add_argument('--nats-dir', type=Path, required=True)
parser.add_argument('--output', type=Path, required=True)
args = parser.parse_args()
args.output.mkdir(parents=True, exist_ok=False)
lib = (args.nats_dir / 'nats.go').resolve()
fixture = (args.repo / 'framework/saga/performance_real_integration_test.go').resolve()
text = lib.read_text()
needle = '\t// Check for message filters.\n'
if text.count(needle) != 1:
    raise SystemExit('nats.go parser anchor changed; inspect before adapting this diagnostic')
text = text.replace(needle, '''\t// Diagnostic overlay only: after protocol parsing, before subscriber queuing.
\tif RoostDiagnosticReceive != nil { RoostDiagnosticReceive(m) }
''' + needle)
text += '\n// RoostDiagnosticReceive exists only in this diagnostic binary.\nvar RoostDiagnosticReceive func(*Msg)\n'
# Go 禁止 overlay 覆盖模块缓存；复制依赖后通过独立 modfile 替换。
local_nats = args.output.resolve() / 'nats-local'
shutil.copytree(args.nats_dir, local_nats)
lib_out = local_nats / 'nats.go'
lib_out.chmod(0o644)
lib_out.write_text(text)
modfile = args.output.resolve() / 'diagnostic.mod'
modfile.write_text((args.repo / 'go.mod').read_text() + '\nreplace github.com/nats-io/nats.go => ' + json.dumps(str(local_nats)) + '\n')
shutil.copyfile(args.repo / 'go.sum', args.output / 'diagnostic.sum')
text = fixture.read_text()
needle = '\t\thoptrace.Install(recorder)'
if text.count(needle) != 1:
    raise SystemExit('fixture recorder anchor changed')
text = text.replace('import (', 'import (\n gonats "github.com/nats-io/nats.go"', 1)
text = text.replace(needle, needle + '''
        // Only one selected perf test runs in this process. Install before opening NATS;
        // do not mutate this hook while a connection read loop exists.
        gonats.RoostDiagnosticReceive = func(message *gonats.Msg) {
            if !strings.HasPrefix(message.Subject, "perfsaga") { return }
            at := time.Now()
            if strings.Contains(message.Subject, ".command.") {
                var envelope commandEnvelope
                if json.Unmarshal(message.Data, &envelope) == nil && envelope.Command.ID != "" {
                    recorder.Mark("timeline/step.wire/"+envelope.Command.ID, at)
                }
            } else if strings.Contains(message.Subject, ".result.") {
                var envelope completionEnvelope
                if json.Unmarshal(message.Data, &envelope) == nil && envelope.Completion.CommandID != "" {
                    recorder.Mark("timeline/result.wire/"+envelope.Completion.CommandID, at)
                }
            }
        }
''', 1)
(fixture_out := args.output / 'performance_real_integration_test.go').write_text(text)
(args.output / 'overlay.json').write_text(json.dumps({'Replace': {str(fixture): str(fixture_out.resolve())}}, indent=2))
(args.output / 'sources.json').write_text(json.dumps({str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in [lib, fixture]}, indent=2))
print(args.output / 'overlay.json')
