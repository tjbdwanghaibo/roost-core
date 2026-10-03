param(
    [Parameter(Mandatory = $true)][string]$GoExecutable,
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot = (Join-Path $env:TEMP 'roost-noncore-review-20261003-01')
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$overlay = @{ Replace = @{} }
$overlay.Replace[(Join-Path $RepoRoot 'app/noncore_review_test.go')] = Join-Path $PSScriptRoot 'app_review_test.go.txt'
$overlay.Replace[(Join-Path $RepoRoot 'httpclient/noncore_review_test.go')] = Join-Path $PSScriptRoot 'httpclient_review_test.go.txt'
$overlay.Replace[(Join-Path $RepoRoot 'httpserver/noncore_review_test.go')] = Join-Path $PSScriptRoot 'httpserver_review_test.go.txt'
$overlayPath = Join-Path $OutputRoot 'overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -overlay $overlayPath -count=1 -timeout=60s -v -run '^TestNoncore' ./app ./httpclient ./httpserver 2>&1 |
        Tee-Object -FilePath (Join-Path $OutputRoot 'repro.log')
    $reproExit = $LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./app ./httpclient ./httpserver ./lifecycle ./manager ./health ./security 2>&1 |
        Tee-Object -FilePath (Join-Path $OutputRoot 'regression.jsonl') | Out-Null
    $regressionExit = $LASTEXITCODE
    & $GoExecutable vet ./app ./httpclient ./httpserver ./lifecycle ./manager ./health ./security 2>&1 |
        Tee-Object -FilePath (Join-Path $OutputRoot 'vet.log')
    $vetExit = $LASTEXITCODE
    [ordered]@{ repro_exit = $reproExit; regression_exit = $regressionExit; vet_exit = $vetExit } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if ($regressionExit -ne 0 -or $vetExit -ne 0) { throw 'Existing regression or vet failed; inspect logs.' }
} finally { Pop-Location }
