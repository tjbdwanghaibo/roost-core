# -*- coding: utf-8 -*-
# 在 tree-new（HEAD + 本次改动的干净副本）里逐个做变异：改一处关键代码，跑对应用例，记录结果，再还原。
import subprocess, sys, os

T = '/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/b1117-client-export/tree-new'
out_path = sys.argv[1]

mutations = [
    ('M1 --check 不认领导出目录（snapshotGeneratedFor 去掉 owns）',
     'codegen/internal/roost/generate.go',
     ' && !manifest.ClientExport.owns(rel) {\n\t\t\treturn nil\n\t\t}\n\t\t// Newlines',
     ' {\n\t\t\treturn nil\n\t\t}\n\t\t// Newlines',
     './codegen/internal/roost/', 'TestGenerateClientExport$'),
    ('M2 不核对 protoc 版本',
     'codegen/internal/roost/client_export.go',
     'if strings.TrimPrefix(got, "libprotoc ") != want {',
     'if false && strings.TrimPrefix(got, "libprotoc ") != want {',
     './codegen/internal/roost/', 'TestClientExportProtocVersion$'),
    ('M3 暂存提交不认领导出目录（planStagedProjectCommit 两处去掉 owns）',
     'codegen/internal/roost/project.go',
     ' && !manifest.ClientExport.owns(rel) {',
     ' {',
     './codegen/internal/roost/', 'TestClientExportTransactionalCommitAndRetirement$'),
    ('M4 doctor 不比 protocol/def 的哈希',
     'codegen/internal/roost/client_export.go',
     'if current.ProtocolDef != manifest.Sources.ProtocolDef {',
     'if false && current.ProtocolDef != manifest.Sources.ProtocolDef {',
     './codegen/internal/roost/', 'TestGenerateClientExport$'),
    ('M5 client sync 不删上次写过、这次没有的文件',
     'codegen/internal/roost/client_sync.go',
     '\t\tif _, err := os.Stat(filepath.Join(to, name)); err == nil {\n\t\t\tremoved = append(removed, name)',
     '\t\tif _, err := os.Stat(filepath.Join(to, name)); err == nil && false {\n\t\t\tremoved = append(removed, name)',
     './codegen/internal/roost/', 'TestClientSync$'),
    ('M6 没声明 client_export 也加 client-export 生成器',
     'codegen/internal/roost/generate.go',
     '\tif m.ClientExport != nil {\n\t\tgenerators = append(',
     '\tif true {\n\t\tgenerators = append(',
     './codegen/internal/roost/', 'TestUndeclaredClientExportChangesNothing$'),
    ('M7 导出清单丢掉应答',
     'codegen/internal/protocol/client_export.go',
     '\t\tif message.Resp != "" {\n\t\t\tmessages = append(',
     '\t\tif false && message.Resp != "" {\n\t\t\tmessages = append(',
     './codegen/internal/protocol/', 'TestExportClientIDsMatchesCSharpOutput$'),
    ('M8 ErrorCodes.cs 说明不做 XML 转义',
     'codegen/internal/errcode/csharp.go',
     '\ttext = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\\r\\n", " ", "\\n", " ", "\\r", " ").Replace(text)',
     '\ttext = strings.NewReplacer("\\r\\n", " ", "\\n", " ", "\\r", " ").Replace(text)',
     './codegen/internal/errcode/', 'TestGenerateCSharp$'),
]

env = dict(os.environ, GOWORK='off')
with open(out_path, 'w', encoding='utf-8') as out:
    for title, rel, old, new, pkg, run in mutations:
        path = os.path.join(T, rel)
        original = open(path, encoding='utf-8').read()
        count = original.count(old)
        if count == 0:
            sys.exit('%s: pattern not found' % title)
        open(path, 'w', encoding='utf-8').write(original.replace(old, new))
        try:
            proc = subprocess.run(['go', 'test', '-count=1', '-run', run, pkg], cwd=T, env=env, capture_output=True, text=True)
        finally:
            open(path, 'w', encoding='utf-8').write(original)
        lines = [l for l in (proc.stdout + proc.stderr).splitlines() if l.strip()]
        detail = [l for l in lines if '_test.go:' in l][:3] or lines[-3:]
        verdict = 'RED（抓到）' if proc.returncode != 0 else 'GREEN（没抓到）'
        out.write('## %s\n文件：%s（替换 %d 处）\n命令：GOWORK=off go test -count=1 -run %r %s\n结果：%s\n' % (title, rel, count, run, pkg, verdict))
        for l in detail:
            out.write('    ' + l.strip()[:400] + '\n')
        out.write('\n')
        print(title, verdict, flush=True)
    # 还原后同一组用例全绿
    proc = subprocess.run(['go', 'test', '-count=1', '-run', 'ClientExport|ClientSync|SDKGoogle|UndeclaredClientExport|ExportClientIDs|GenerateCSharp',
                           './codegen/internal/roost/', './codegen/internal/protocol/', './codegen/internal/errcode/'], cwd=T, env=env, capture_output=True, text=True)
    out.write('## 还原后\n结果：%s\n' % ('全部通过' if proc.returncode == 0 else '失败'))
    for l in (proc.stdout + proc.stderr).splitlines()[-4:]:
        out.write('    ' + l + '\n')
    print('restored', proc.returncode)
