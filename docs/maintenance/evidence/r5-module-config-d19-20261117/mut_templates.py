"""模板侧的变异：四个模块各把“读配置”的那一行换回常量，跑一次端到端用例（四个运行时场景应各自变红），然后恢复。"""
import os, shutil, subprocess, sys
BK = '/private/tmp/claude-502/-Users-whb-roost/3919db58-1604-4ead-83ca-606db85f4c89/scratchpad/r5/bak_'
os.chdir('/Users/whb/roost/roost-core/.claude/worktrees/agent-ac148c8f254c0ab00')
mutations = [
    ('modules/profile/files/game/components/profile/profile.go.tmpl', '	perLevel := ExpPerLevel()', '	perLevel := int64(100)'),
    ('modules/stamina/files/game/components/stamina/rules.go.tmpl', '		Max:              config.Max,', '		Max:              100,'),
    ('modules/leaderboard/files/game/modules/leaderboard/leaderboard_board.go.tmpl', 'svcrank.Board{ID: CurrentConfig().BoardID,', 'svcrank.Board{ID: "total",'),
    ('modules/mailbox/files/game/modules/mailbox/mailbox.go.tmpl', '	return int64(CurrentConfig().LedgerPruneGrace / time.Second)', '	return 600 + 0*int64(time.Second)'),
]
for path, a, b in mutations:
    shutil.copy(path, BK + path.replace('/', '__'))
    s = open(path).read()
    assert a in s, (path, a)
    open(path, 'w').write(s.replace(a, b, 1))
    print('=== mutation:', path, repr(a), '->', repr(b))
try:
    out = subprocess.run(['go', 'test', '-count=1', '-timeout', '30m', '-run', '^TestModuleConfigIsWrittenCheckedAndLoaded$', './codegen/internal/roost/'],
                         env=dict(os.environ, GOWORK='off'), capture_output=True, text=True)
    for line in (out.stdout + out.stderr).splitlines():
        if 'TestR5Config' in line or line.startswith(('ok', 'FAIL', '--- FAIL')) or 'runtime config scenarios passed' in line or '_test.go:' in line:
            print(line[:300])
finally:
    for path, _, _ in mutations:
        shutil.move(BK + path.replace('/', '__'), path)
