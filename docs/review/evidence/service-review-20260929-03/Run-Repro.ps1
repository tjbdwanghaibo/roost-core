param(
    [Parameter(Mandatory=$true)][string]$RepoPath,
    [Parameter(Mandatory=$true)][string]$RedisAddress,
    [ValidateSet('all','memory','redis')][string]$Mode = 'all'
)
$ErrorActionPreference = 'Stop'
$reviewRoot = (Resolve-Path -LiteralPath $RepoPath).Path
if (!(Test-Path -LiteralPath (Join-Path $reviewRoot 'go.mod'))) { throw 'RepoPath must be the Core module root.' }
if ([string]::IsNullOrWhiteSpace($RedisAddress)) { throw 'Provide an isolated test Redis; this script does not start or stop Redis.' }
$reviewMapping = @{
    'kit/service/account/review3_overlay_test.go' = 'account_review_test.go.txt'
    'kit/service/directory/review3_overlay_test.go' = 'directory_review_test.go.txt'
    'service/mail/review3_overlay_test.go' = 'mail_review_test.go.txt'
    'service/session/review3_overlay_test.go' = 'session_review_test.go.txt'
}
$reviewReplace = @{}
foreach ($reviewTarget in $reviewMapping.Keys) {
    $reviewPath = Join-Path $reviewRoot $reviewTarget
    if (Test-Path -LiteralPath $reviewPath) { throw "Preserving existing overlay target: $reviewPath" }
    $reviewReplace[$reviewPath] = Join-Path $PSScriptRoot $reviewMapping[$reviewTarget]
}
$reviewOverlayPath = Join-Path ([IO.Path]::GetTempPath()) ('roost-service-review3-' + [guid]::NewGuid().ToString('N') + '.json')
$reviewOldRedis = $env:ROOST_REVIEW_REDIS
$reviewOldBackend = $env:ROOST_REVIEW3_BACKEND
$reviewExit = 0
try {
    $reviewJSON = @{ Replace=$reviewReplace } | ConvertTo-Json -Depth 4
    [IO.File]::WriteAllText($reviewOverlayPath, $reviewJSON, [Text.UTF8Encoding]::new($false))
    $env:ROOST_REVIEW_REDIS = $RedisAddress
    Push-Location $reviewRoot
    try {
        if ($Mode -in @('all','memory')) {
            $env:ROOST_REVIEW3_BACKEND = 'memory'
            & go test -race -count 1 -timeout 2m -overlay $reviewOverlayPath ./kit/service/account ./kit/service/directory ./service/mail ./service/session -run '^TestReview3' -v
            if ($LASTEXITCODE -ne 0) { $reviewExit=$LASTEXITCODE }
        }
        if ($Mode -in @('all','redis')) {
            $env:ROOST_REVIEW3_BACKEND = 'redis'
            & go test -race -count 1 -timeout 2m -overlay $reviewOverlayPath ./kit/service/account ./service/mail -run '^TestReview3' -v
            if ($LASTEXITCODE -ne 0) { $reviewExit=$LASTEXITCODE }
        }
    } finally { Pop-Location }
} finally {
    $env:ROOST_REVIEW_REDIS = $reviewOldRedis
    $env:ROOST_REVIEW3_BACKEND = $reviewOldBackend
    if (Test-Path -LiteralPath $reviewOverlayPath -PathType Leaf) { Remove-Item -LiteralPath $reviewOverlayPath }
}
Write-Output "Review3 exit=$reviewExit. At 83c04243 defect assertions fail; controls/observations pass. Inspect names and diagnostics; arbitrary failures do not confirm a defect."
exit $reviewExit
