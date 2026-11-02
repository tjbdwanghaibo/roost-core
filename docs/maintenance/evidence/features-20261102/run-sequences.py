"""Replay the bounded functional sequence probe against an explicit core checkout."""
import pathlib
import shutil
import subprocess
import sys
import tempfile
root = pathlib.Path(sys.argv[1]).resolve()
probe = pathlib.Path(__file__).with_name('review_sequences_test.go.txt')
with tempfile.TemporaryDirectory(prefix='core-feature-review-') as directory:
    work = pathlib.Path(directory)
    (work / 'gen').mkdir()
    (work / 'rangecontract').mkdir()
    for source in (root / 'codegen/internal/dao/testdata/golden').glob('*.go'):
        if not source.name.endswith('_redis_dao.go'):
            shutil.copyfile(source, work / 'gen' / source.name)
    for source in (root / 'codegen/internal/dao/testdata/runtime').glob('*_test.go'):
        (work / 'gen' / source.name).write_text(source.read_text().replace('//go:build daoruntime\n', ''))
    shutil.copyfile(root / 'internal/rangecontract/rangecontract.go', work / 'rangecontract/rangecontract.go')
    shutil.copyfile(probe, work / 'gen/review_sequences_test.go')
    (work / 'go.mod').write_text('module daogoldenruntime\n\ngo 1.27.0\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\n')
    subprocess.run(['go', 'mod', 'edit', '-replace', 'github.com/tjbdwanghaibo/roost-core=' + str(root)], cwd=work, check=True)
    subprocess.run(['go', 'test', '-mod=mod', '-race', './gen', '-run', '^TestReviewTrackedSequences$', '-count=1'], cwd=work, check=True)
