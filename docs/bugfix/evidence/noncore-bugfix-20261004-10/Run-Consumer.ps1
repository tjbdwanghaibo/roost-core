param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot) {throw 'Choose a new OutputRoot for this consumer run'}
New-Item -ItemType Directory -Path (Join-Path $OutputRoot 'db/def') -Force | Out-Null
Copy-Item (Join-Path $PSScriptRoot 'consumer-definition.go.txt') (Join-Path $OutputRoot 'db/def/record.go')
Copy-Item (Join-Path $PSScriptRoot 'consumer-record.go.txt') (Join-Path $OutputRoot 'db/record.go')
Copy-Item (Join-Path $PSScriptRoot 'consumer-test.go.txt') (Join-Path $OutputRoot 'db/consumer_test.go')
@('module example.com/roostrefconsumer','', 'go 1.27.0','', 'require github.com/tjbdwanghaibo/roost-core v0.0.0', '', "replace github.com/tjbdwanghaibo/roost-core => `"$($RepoRoot.Replace('\','/'))`"") | Set-Content (Join-Path $OutputRoot 'go.mod') -Encoding utf8NoBOM
$dao=Join-Path $OutputRoot 'dao.exe'
Push-Location $RepoRoot
try {
    & $GoExecutable build -o $dao ./codegen/cmd/dao
    if($LASTEXITCODE -ne 0) {throw 'DAO CLI build failed'}
    & $dao -def (Join-Path $OutputRoot 'db/def') -out (Join-Path $OutputRoot 'db') -pkg db 2>&1 | Set-Content (Join-Path $OutputRoot 'generate.log') -Encoding utf8NoBOM
    if($LASTEXITCODE -ne 0) {throw 'DAO generation failed'}
} finally {Pop-Location}
Push-Location $OutputRoot
try {
    & $GoExecutable test -mod=mod -race -count=1 -timeout=90s -json ./db 2>&1 | Set-Content 'consumer.jsonl' -Encoding utf8NoBOM
    $testExit=$LASTEXITCODE
    & $GoExecutable vet -mod=mod ./db 2>&1 | Set-Content 'vet.log' -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    @{test_exit=$testExit;vet_exit=$vetExit} | ConvertTo-Json | Set-Content 'exits.json' -Encoding utf8NoBOM
    if($testExit -ne 0 -or $vetExit -ne 0) {throw 'Generated consumer failed'}
} finally {Pop-Location}
