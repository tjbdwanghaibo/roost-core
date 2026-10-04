param(
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Use a new output directory'}
New-Item -ItemType Directory -Path (Join-Path $OutputRoot 'db/def') -Force|Out-Null
Copy-Item (Join-Path $PSScriptRoot 'consumer-definition.go.txt') (Join-Path $OutputRoot 'db/def/record.go')
Copy-Item (Join-Path $PSScriptRoot 'consumer-test.go.txt') (Join-Path $OutputRoot 'db/consumer_test.go')
Copy-Item (Join-Path $PSScriptRoot 'aggregate-test.go.txt') (Join-Path $OutputRoot 'db/aggregate_test.go')
@('module example.com/roostreview20','','go 1.27.0','','require github.com/tjbdwanghaibo/roost-core v0.0.0','',"replace github.com/tjbdwanghaibo/roost-core => `"$($RepoRoot.Replace('\','/'))`"")|Set-Content (Join-Path $OutputRoot 'go.mod') -Encoding utf8NoBOM
$dao=Join-Path $OutputRoot 'dao.exe'
Push-Location $RepoRoot
try {
 & $GoExecutable build -o $dao ./codegen/cmd/dao
 if($LASTEXITCODE -ne 0){throw 'DAO CLI build failed'}
 & $dao -def (Join-Path $OutputRoot 'db/def') -out (Join-Path $OutputRoot 'db') -pkg db 2>&1|Set-Content (Join-Path $OutputRoot 'generate.log') -Encoding utf8NoBOM
 if($LASTEXITCODE -ne 0){throw 'DAO generation failed'}
}finally{Pop-Location}
Push-Location $OutputRoot
try {
 & $GoExecutable test -mod=mod -race -count=1 -timeout=60s -json ./db 2>&1|Set-Content 'consumer.jsonl' -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE
 & $GoExecutable vet -mod=mod ./db 2>&1|Set-Content 'vet.log' -Encoding utf8NoBOM
 $vetExit=$LASTEXITCODE
 @{test_exit=$testExit;vet_exit=$vetExit;source_head=(& git -C $RepoRoot -c "safe.directory=$RepoRoot" rev-parse HEAD)}|ConvertTo-Json|Set-Content 'exits.json' -Encoding utf8NoBOM
}finally{Pop-Location}
# Review 故意保留行为反例，不将编译/基础设施失败误认成产品红。
if($testExit -ne 0){throw 'Consumer behavior validation failed; inspect consumer.jsonl'}
if($vetExit -ne 0){throw 'Consumer compile/vet failed'}
$events=@(Get-Content (Join-Path $OutputRoot 'consumer.jsonl')|ForEach-Object{ConvertFrom-Json $_})
if(@($events|Where-Object{$_.Action -eq 'build-fail' -or $_.Output -match 'WARNING: DATA RACE|panic:|test timed out'}).Count){throw 'Infrastructure/race/panic/timeout is not behavior evidence'}
