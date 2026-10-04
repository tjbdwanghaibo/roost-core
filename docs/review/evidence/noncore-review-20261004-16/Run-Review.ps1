param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[Parameter(Mandatory=$true)][string]$OutputRoot)
$ErrorActionPreference='Stop'
if(Test-Path $OutputRoot){throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$replace=@{}
$replace[(Join-Path $RepoRoot 'mongo/mongotest/review16_test.go')]=Join-Path $PSScriptRoot 'review16_test.go.txt'
@{Replace=$replace}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
Push-Location $RepoRoot
try{
 $env:GOWORK='off'
 & $GoExecutable test -race -count=1 -timeout=90s -json -overlay (Join-Path $OutputRoot 'overlay.json') -run '^TestReview16MongoBoundaries$' ./mongo/mongotest 2>&1 | Set-Content (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
 @{test_exit=$LASTEXITCODE}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
}finally{Pop-Location}
