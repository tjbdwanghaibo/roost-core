param(
    [Parameter(Mandatory=$true)][string]$RepoPath,
    [Parameter(Mandatory=$true)][string]$RedisAddress
)
$ErrorActionPreference = 'Stop'
$reviewRoot = (Resolve-Path -LiteralPath $RepoPath).Path
if (!(Test-Path -LiteralPath (Join-Path $reviewRoot '.git') -PathType Leaf)) {
    throw 'Use a disposable Git worktree, whose .git is a file.'
}
if ([string]::IsNullOrWhiteSpace($RedisAddress)) { throw 'A test Redis address is required.' }
$reviewMapping = @{
    'mail.go.txt' = 'service/mail'
    'account.go.txt' = 'kit/service/account'
    'rank.go.txt' = 'kit/service/rank'
    'platform.go.txt' = 'kit/service/platform'
    'activity.go.txt' = 'kit/service/global/activity'
    'chat.go.txt' = 'kit/service/chat'
}
$reviewCopies = @()
$reviewOldRedis = $env:ROOST_REVIEW_REDIS
$reviewTestExit = 1
foreach ($reviewName in $reviewMapping.Keys) {
    $reviewTarget = Join-Path $reviewRoot ($reviewMapping[$reviewName] + '/zz_review_20260928_test.go')
    if (Test-Path -LiteralPath $reviewTarget) { throw "Preserving existing file: $reviewTarget" }
}
try {
    foreach ($reviewName in $reviewMapping.Keys) {
        $reviewSource = Join-Path $PSScriptRoot $reviewName
        $reviewTarget = Join-Path $reviewRoot ($reviewMapping[$reviewName] + '/zz_review_20260928_test.go')
        Copy-Item -LiteralPath $reviewSource -Destination $reviewTarget
        $reviewCopies += @{ Path=$reviewTarget; Hash=(Get-FileHash -LiteralPath $reviewTarget).Hash }
    }
    $env:ROOST_REVIEW_REDIS = $RedisAddress
    Push-Location $reviewRoot
    try {
        $reviewPackages = @('./service/mail','./kit/service/account','./kit/service/rank',
            './kit/service/platform','./kit/service/global/activity','./kit/service/chat')
        & go test -race @reviewPackages -run '^TestReview' -v -count=1 -timeout 60s
        $reviewTestExit = $LASTEXITCODE
    } finally { Pop-Location }
} finally {
    $env:ROOST_REVIEW_REDIS = $reviewOldRedis
    foreach ($reviewCopy in $reviewCopies) {
        if ((Get-FileHash -LiteralPath $reviewCopy.Path).Hash -eq $reviewCopy.Hash) {
            Remove-Item -LiteralPath $reviewCopy.Path
        } else { Write-Warning "File changed; preserving $($reviewCopy.Path)" }
    }
}
Write-Output "Repro exit=$reviewTestExit (baseline expects 1; after fixes expects 0)."
exit $reviewTestExit
