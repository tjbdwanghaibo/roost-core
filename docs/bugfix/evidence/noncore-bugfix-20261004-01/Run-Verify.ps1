param(
    [Parameter(Mandatory = $true)][string]$GoExecutable,
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot = (Join-Path $env:TEMP 'roost-noncore-bugfix-20261004-01')
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$oldEvidence = Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261003-01'
$overlay = @{ Replace = @{} }
foreach ($package in @('app','httpclient','httpserver')) {
    $overlay.Replace[(Join-Path $RepoRoot "$package/noncore_review_test.go")] = Join-Path $oldEvidence "${package}_review_test.go.txt"
}
$overlayPath = Join-Path $OutputRoot 'original-review-overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestNoncore' ./app ./httpclient ./httpserver 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'original-review-green.jsonl') -Encoding utf8NoBOM
    $overlayExit = $LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./app/... ./httpclient ./httpserver ./lifecycle ./manager ./health ./security ./admin ./kit/manager ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $regressionExit = $LASTEXITCODE
    & $GoExecutable vet ./app/... ./httpclient ./httpserver ./lifecycle ./manager ./health ./security ./admin ./kit/manager ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit = $LASTEXITCODE
    [ordered]@{ original_review_exit=$overlayExit;regression_exit=$regressionExit;vet_exit=$vetExit } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $OutputRoot 'verify-exits.json') -Encoding utf8NoBOM
    if ($overlayExit -ne 0 -or $regressionExit -ne 0 -or $vetExit -ne 0) { throw 'Bugfix verification failed; inspect logs.' }
} finally { Pop-Location }
