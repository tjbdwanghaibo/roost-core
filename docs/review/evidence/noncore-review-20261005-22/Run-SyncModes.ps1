param(
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot
)
# scripts/test-sync-modes-generated.sh 的 Windows 等价流程，保留生成产物和日志。
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Use a new output directory'}
New-Item -ItemType Directory -Path $OutputRoot -Force|Out-Null
Copy-Item (Join-Path $RepoRoot 'codegen/internal/entity/testdata/syncmodes/*.go') $OutputRoot
@('module syncmodes','','go 1.27.0','','require github.com/tjbdwanghaibo/roost-core v0.0.0','',"replace github.com/tjbdwanghaibo/roost-core => `"$($RepoRoot.Replace('\','/'))`"")|Set-Content (Join-Path $OutputRoot 'go.mod') -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
 & $GoExecutable run ./codegen/cmd/dao -def ./codegen/internal/entity/testdata/syncmodes/def -out $OutputRoot -pkg syncmodes -force 2>&1|Set-Content (Join-Path $OutputRoot 'dao.log') -Encoding utf8NoBOM
 if($LASTEXITCODE -ne 0){throw 'DAO generation failed'}
 & $GoExecutable run ./codegen/cmd/entity -dir $OutputRoot -force 2>&1|Set-Content (Join-Path $OutputRoot 'entity.log') -Encoding utf8NoBOM
 if($LASTEXITCODE -ne 0){throw 'Entity generation failed'}
}finally{Pop-Location}
Push-Location $OutputRoot
try {
 & $GoExecutable test -mod=mod -race -count=1 -timeout=90s -json ./... 2>&1|Set-Content 'sync-modes.jsonl' -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE
 & $GoExecutable vet -mod=mod ./... 2>&1|Set-Content 'vet.log' -Encoding utf8NoBOM
 $vetExit=$LASTEXITCODE
 @{test_exit=$testExit;vet_exit=$vetExit}|ConvertTo-Json|Set-Content 'exits.json' -Encoding utf8NoBOM
}finally{Pop-Location}
if($testExit -ne 0 -or $vetExit -ne 0){throw 'Generated sync-modes consumer failed'}
