param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$overlay=@{Replace=@{}}
$overlay.Replace[(Join-Path $RepoRoot 'mongo/mongotest/review12_test.go')]=Join-Path $PSScriptRoot 'mongotest_review_test.go.txt'
$overlayPath=Join-Path $OutputRoot 'overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -race -count=1 -timeout=60s -json -overlay $overlayPath -run '^TestReview12PaginationParity$' ./mongo/mongotest 2>&1 | Set-Content (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
    $reviewExit=$LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./mongo ./mongo/driver ./mongo/mongotest ./migration ./kit/mongo 2>&1 | Set-Content (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $testExit=$LASTEXITCODE
    & $GoExecutable vet ./mongo ./mongo/driver ./mongo/mongotest ./migration ./kit/mongo 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    @{review_exit=$reviewExit;test_exit=$testExit;vet_exit=$vetExit} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if($testExit -ne 0 -or $vetExit -ne 0) {throw 'Existing regression failed'}
} finally {Pop-Location}
