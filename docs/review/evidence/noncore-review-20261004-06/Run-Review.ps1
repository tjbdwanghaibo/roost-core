param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[string]$OutputRoot=(Join-Path $env:TEMP 'roost-n03-review6'))
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot|Out-Null
$overlay=@{Replace=@{}}
foreach($pair in @(@('bus','bus'),@('nats/driver','nats_driver'),@('servicerpc','servicerpc'))){$overlay.Replace[(Join-Path $RepoRoot "$($pair[0])/noncore_review6_test.go")]=Join-Path $PSScriptRoot "$($pair[1])_review_test.go.txt"}
$overlayPath=Join-Path $OutputRoot 'overlay.json';$overlay|ConvertTo-Json -Depth 5|Set-Content $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -race -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview6' ./bus ./nats/driver ./servicerpc 2>&1|Set-Content (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
    $reviewExit=$LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./bus ./nats ./nats/driver ./servicerpc ./kit/nats ./worker 2>&1|Set-Content (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $regressionExit=$LASTEXITCODE
    & $GoExecutable vet ./bus ./nats ./nats/driver ./servicerpc ./kit/nats ./worker 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    @{review_exit=$reviewExit;regression_exit=$regressionExit;vet_exit=$vetExit}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if($regressionExit -ne 0 -or $vetExit -ne 0){throw 'Existing N03 regression/vet failed'}
} finally {Pop-Location}
