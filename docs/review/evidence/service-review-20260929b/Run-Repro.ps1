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
    'account.go.txt' = 'kit/service/account'
    'global.go.txt' = 'kit/service/global'
    'session.go.txt' = 'service/session'
    'activity.go.txt' = 'kit/service/global/activity'
    'mail.go.txt' = 'service/mail'
}
$reviewCopies = @()
$reviewOldRedis = $env:ROOST_REVIEW_REDIS
$reviewTestExit = 1
foreach ($reviewName in $reviewMapping.Keys) {
    $reviewTarget = Join-Path $reviewRoot ($reviewMapping[$reviewName] + '/zz_review_20260929b_test.go')
    if (Test-Path -LiteralPath $reviewTarget) { throw "Preserving existing file: $reviewTarget" }
}
try {
    foreach ($reviewName in $reviewMapping.Keys) {
        $reviewSource = Join-Path $PSScriptRoot $reviewName
        $reviewTarget = Join-Path $reviewRoot ($reviewMapping[$reviewName] + '/zz_review_20260929b_test.go')
        Copy-Item -LiteralPath $reviewSource -Destination $reviewTarget
        $reviewCopies += @{ Path=$reviewTarget; Hash=(Get-FileHash -LiteralPath $reviewTarget).Hash }
    }
    $env:ROOST_REVIEW_REDIS = $RedisAddress
    Push-Location $reviewRoot
    try {
        $reviewPackages = @('./service/session','./service/mail','./kit/service/account',
            './kit/service/global','./kit/service/global/activity')
        & go test -race @reviewPackages -run '^TestReview' -v -count=1 -timeout 60s
        $reviewTestExit = $LASTEXITCODE
    } finally { Pop-Location }
} finally {
    $env:ROOST_REVIEW_REDIS = $reviewOldRedis
    foreach ($reviewCopy in $reviewCopies) {
        if ((Test-Path -LiteralPath $reviewCopy.Path) -and
            (Get-FileHash -LiteralPath $reviewCopy.Path).Hash -eq $reviewCopy.Hash) {
            Remove-Item -LiteralPath $reviewCopy.Path
        } else { Write-Warning "File changed or absent; preserving $($reviewCopy.Path)" }
    }
}
Write-Output "Repro exit=$reviewTestExit (baseline expects 1; defect assertions after fixes expect 0; observation is contract-dependent)."
exit $reviewTestExit
