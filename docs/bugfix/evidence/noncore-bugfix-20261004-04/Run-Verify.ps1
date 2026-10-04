param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[string]$OutputRoot=(Join-Path $env:TEMP 'roost-n03-fix4'))
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force $OutputRoot | Out-Null
Push-Location $RepoRoot
try {
    & $GoExecutable test -race -count=1 -timeout=120s -json ./bus ./nats ./nats/driver ./servicerpc ./kit/nats ./worker 2>&1 | Set-Content (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) { throw 'RPC regression failed' }
    & $GoExecutable vet ./bus ./nats ./nats/driver ./servicerpc ./kit/nats ./worker 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) { throw 'RPC vet failed' }
    $overlay=@{Replace=@{}}
    foreach ($pair in @(@('bus','bus'),@('nats/driver','nats_driver'),@('servicerpc','servicerpc'))) {
        $overlay.Replace[(Join-Path $RepoRoot "$($pair[0])/noncore_review6_test.go")]=Join-Path $RepoRoot "docs/review/evidence/noncore-review-20261004-06/$($pair[1])_review_test.go.txt"
    }
    $overlayPath=Join-Path $OutputRoot 'overlay.json'
    $overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
    & $GoExecutable test -race -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview6' ./bus ./nats/driver ./servicerpc 2>&1 | Set-Content (Join-Path $OutputRoot 'original.jsonl') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) { throw 'Original review counterexample remains red' }
} finally { Pop-Location }
