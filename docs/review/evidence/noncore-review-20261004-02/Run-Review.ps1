param(
    [Parameter(Mandatory = $true)][string]$GoExecutable,
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot = (Join-Path $env:TEMP 'roost-noncore-review-20261004-02')
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$overlay = @{ Replace = @{} }
foreach ($package in @('manager','admin','lifecycle')) {
    $overlay.Replace[(Join-Path $RepoRoot "$package/noncore_review2_test.go")] = Join-Path $PSScriptRoot "${package}_review_test.go.txt"
}
$overlay.Replace[(Join-Path $RepoRoot 'kit/ops/noncore_review2_test.go')] = Join-Path $PSScriptRoot 'ops_review_test.go.txt'
$overlayPath = Join-Path $OutputRoot 'overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview2' ./manager ./admin ./lifecycle ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
    $reviewExit = $LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./manager ./admin ./lifecycle ./health ./kit/manager ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $regressionExit = $LASTEXITCODE
    & $GoExecutable vet ./manager ./admin ./lifecycle ./health ./kit/manager ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit = $LASTEXITCODE
    [ordered]@{ review_exit=$reviewExit;regression_exit=$regressionExit;vet_exit=$vetExit } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if ($regressionExit -ne 0 -or $vetExit -ne 0) { throw 'Existing regression or vet failed; inspect logs.' }
} finally { Pop-Location }
