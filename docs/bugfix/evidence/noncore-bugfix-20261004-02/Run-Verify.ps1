param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot=(Join-Path $env:TEMP 'roost-noncore-fix-20261004-02')
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$oldEvidence=Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261004-02'
$overlay=@{Replace=@{}}
foreach ($package in @('manager','admin','lifecycle')) {
    $overlay.Replace[(Join-Path $RepoRoot "$package/noncore_review2_test.go")]=Join-Path $oldEvidence "${package}_review_test.go.txt"
}
$overlay.Replace[(Join-Path $RepoRoot 'kit/ops/noncore_review2_test.go')]=Join-Path $oldEvidence 'ops_review_test.go.txt'
$overlayPath=Join-Path $OutputRoot 'original-overlay.json'
$overlay | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview2' ./manager ./admin ./lifecycle ./kit/ops 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'original-green.jsonl') -Encoding utf8NoBOM
    $originalExit=$LASTEXITCODE
    & $GoExecutable test -race -count=1 -timeout=120s -json ./manager ./admin ./kit/ops ./kit/manager ./lifecycle ./health ./app/... ./httpserver ./httpclient ./security 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $regressionExit=$LASTEXITCODE
    & $GoExecutable vet ./manager ./admin ./kit/ops ./kit/manager ./lifecycle ./health ./app/... ./httpserver ./httpclient ./security 2>&1 |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    [ordered]@{original_exit=$originalExit;regression_exit=$regressionExit;vet_exit=$vetExit} | ConvertTo-Json |
        Set-Content -LiteralPath (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if ($originalExit -ne 0 -or $regressionExit -ne 0 -or $vetExit -ne 0) { throw 'Fix verification failed; inspect logs.' }
} finally { Pop-Location }
