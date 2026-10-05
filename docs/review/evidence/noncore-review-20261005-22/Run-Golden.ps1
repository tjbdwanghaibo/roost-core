param(
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot,
 [string]$Run='.'
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Use a new output directory'}
New-Item -ItemType Directory -Path $OutputRoot -Force|Out-Null
# 与 canonical codegen/scripts/dao-golden-runtime.sh 一致，Redis 文本金样不属于此消费者。
Get-ChildItem (Join-Path $RepoRoot 'codegen/internal/dao/testdata/golden') -Filter '*.go'|Where-Object {$_.Name -notlike '*_redis_dao.go'}|Copy-Item -Destination $OutputRoot
Copy-Item (Join-Path $RepoRoot 'codegen/internal/dao/testdata/runtime/*.go') $OutputRoot
@('module example.com/roostgolden22','','go 1.27.0','','require github.com/tjbdwanghaibo/roost-core v0.0.0','',"replace github.com/tjbdwanghaibo/roost-core => `"$($RepoRoot.Replace('\','/'))`"")|Set-Content (Join-Path $OutputRoot 'go.mod') -Encoding utf8NoBOM
Push-Location $OutputRoot
try {
 & $GoExecutable test -mod=mod -tags daoruntime -race -count=1 -timeout=90s -json -run $Run . 2>&1|Set-Content 'runtime.jsonl' -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE
 & $GoExecutable vet -mod=mod -tags daoruntime . 2>&1|Set-Content 'vet.log' -Encoding utf8NoBOM
 $vetExit=$LASTEXITCODE
 @{test_exit=$testExit;vet_exit=$vetExit}|ConvertTo-Json|Set-Content 'exits.json' -Encoding utf8NoBOM
}finally{Pop-Location}
if($testExit -ne 0){throw 'Golden behavior validation failed; inspect runtime.jsonl'}
if($vetExit -ne 0){throw 'Golden vet failed'}
