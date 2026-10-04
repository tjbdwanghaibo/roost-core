param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[string]$OutputRoot=(Join-Path $env:TEMP 'roost-n03-review8'))
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$overlay=@{Replace=@{}}
$overlay.Replace[(Join-Path $RepoRoot 'etcd/review8_test.go')]=Join-Path $PSScriptRoot 'etcd_review_test.go.txt'
$overlay.Replace[(Join-Path $RepoRoot 'etcd/driver/review8_test.go')]=Join-Path $PSScriptRoot 'driver_review_test.go.txt'
$overlayPath=Join-Path $OutputRoot 'overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -race -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview8' ./etcd ./etcd/driver 2>&1 | Set-Content (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
    $reviewExit=$LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./etcd ./etcd/driver ./kit/etcd 2>&1 | Set-Content (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $regressionExit=$LASTEXITCODE
    & $GoExecutable vet ./etcd ./etcd/driver ./kit/etcd 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    @{review_exit=$reviewExit;regression_exit=$regressionExit;vet_exit=$vetExit} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if ($regressionExit -ne 0 -or $vetExit -ne 0) { throw 'Existing etcd regression/vet failed' }
} finally { Pop-Location }
