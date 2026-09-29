param(
    [string]$Repository = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [ValidateSet('memory', 'redis')][string]$Backend = 'memory',
    [string]$RedisAddress = '',
    [string]$OutputDirectory = (Join-Path ([IO.Path]::GetTempPath()) ('roost-review4-' + [Guid]::NewGuid().ToString('N')))
)
$ErrorActionPreference = 'Stop'
if ($Backend -eq 'redis' -and !$RedisAddress) { throw 'Supply an isolated test Redis address; never use production Redis.' }
$reviewRepo = (Resolve-Path -LiteralPath $Repository).Path
$reviewEvidence = $PSScriptRoot
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$reviewOverlay = @{
    Replace = @{
        (Join-Path $reviewRepo 'kit/service/platform/review4_overlay_test.go') = (Join-Path $reviewEvidence 'platform_review_test.go.txt')
        (Join-Path $reviewRepo 'service/mail/review4_overlay_test.go') = (Join-Path $reviewEvidence 'mail_review_test.go.txt')
        (Join-Path $reviewRepo 'service/match/review4_overlay_test.go') = (Join-Path $reviewEvidence 'match_review_test.go.txt')
    }
}
$reviewOverlayPath = Join-Path $OutputDirectory 'overlay.json'
$reviewOverlay | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $reviewOverlayPath
$reviewEnvNames = @('ROOST_REVIEW3_BACKEND', 'ROOST_REVIEW4_BACKEND', 'ROOST_REVIEW_REDIS')
$reviewSavedEnv = @{}
foreach ($name in $reviewEnvNames) { $reviewSavedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Push-Location -LiteralPath $reviewRepo
try {
    $env:ROOST_REVIEW3_BACKEND = $Backend # Reuses the current Mail backend fixture.
    $env:ROOST_REVIEW4_BACKEND = $Backend
    $env:ROOST_REVIEW_REDIS = $RedisAddress
    $reviewLog = Join-Path $OutputDirectory 'repro.jsonl'
    & go test -race -count 1 -timeout 120s -overlay $reviewOverlayPath -run '^TestReview4' -json ./kit/service/platform ./service/mail ./service/match > $reviewLog
    $reviewExit = $LASTEXITCODE
    $reviewEvents = @(Get-Content -LiteralPath $reviewLog | ForEach-Object { $_ | ConvertFrom-Json })
    $reviewSummary = [ordered]@{
        backend = $Backend
        go_exit = $reviewExit
        pass_events = @($reviewEvents | Where-Object { $_.Test -and $_.Action -eq 'pass' }).Count
        fail_events = @($reviewEvents | Where-Object { $_.Test -and $_.Action -eq 'fail' }).Count
        skip_events = @($reviewEvents | Where-Object { $_.Test -and $_.Action -eq 'skip' }).Count
        log = $reviewLog
    }
    $reviewSummary | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $OutputDirectory 'summary.json')
    $reviewSummary
} finally {
    Pop-Location
    foreach ($name in $reviewEnvNames) { [Environment]::SetEnvironmentVariable($name, $reviewSavedEnv[$name], 'Process') }
}
