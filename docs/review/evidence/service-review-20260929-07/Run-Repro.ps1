param(
    [string]$RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$RedisAddress,
    [Parameter(Mandatory=$true)][string]$ClusterAddresses,
    [Parameter(Mandatory=$true)][string]$OutputDirectory
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path -LiteralPath $RepositoryRoot).Path
$out = [IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out) | Out-Null
$replace = @{}
foreach ($package in @('platform','rank')) {
    $replace[(Join-Path $root "kit/service/$package/review7_overlay_test.go")] =
        (Join-Path $PSScriptRoot "$package`_test.go.txt")
}
$replace[(Join-Path $root 'service/match/review7_overlay_test.go')] =
    (Join-Path $PSScriptRoot 'match_test.go.txt')
$overlay = Join-Path $out 'overlay.json'
[IO.File]::WriteAllText($overlay, (@{Replace=$replace} | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
$saved = @{}
foreach ($key in @('ROOST_REDIS_TEST_ADDR','ROOST_REVIEW_REDIS','ROOST_REVIEW_CLUSTER')) {
    $saved[$key] = [Environment]::GetEnvironmentVariable($key)
}
Push-Location $root
try {
    $env:ROOST_REDIS_TEST_ADDR=$RedisAddress
    $env:ROOST_REVIEW_REDIS=$RedisAddress
    $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
    $raw = @(& go test -race -count=1 -timeout=120s -overlay $overlay -run '^TestReview7' -json ./kit/service/platform ./kit/service/rank ./service/match 2>&1 | ForEach-Object { "$_" })
    $goExit=$LASTEXITCODE
    $log=Join-Path $out 'repro.jsonl'
    [IO.File]::WriteAllLines($log,$raw,[Text.UTF8Encoding]::new($false))
    $events=@($raw | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json })
    $runs=@($events | Where-Object { $_.Action -eq 'run' -and $_.Test } | Select-Object -ExpandProperty Test -Unique)
    $leaf=@($runs | Where-Object { $name=$_; -not @($runs | Where-Object { $_.StartsWith("$name/") }).Count })
    $outcomes=@($events | Where-Object { $_.Test -in $leaf -and $_.Action -in @('pass','fail','skip') } | Select-Object Package,Test,Action)
    $result=[pscustomobject][ordered]@{
        source_head=(& git -c "safe.directory=$root" rev-parse HEAD)
        go=(& go version)
        redis=$RedisAddress
        cluster=$ClusterAddresses
        go_exit=$goExit
        leaf_pass=@($outcomes | Where-Object Action -eq 'pass').Count
        leaf_fail=@($outcomes | Where-Object Action -eq 'fail').Count
        leaf_skip=@($outcomes | Where-Object Action -eq 'skip').Count
        build_fail=@($events | Where-Object Action -eq 'build-fail').Count
        log_sha256=(Get-FileHash -LiteralPath $log -Algorithm SHA256).Hash
        outcomes=$outcomes
        evidence_output=@($events | Where-Object { $_.Action -eq 'output' -and $_.Test -and $_.Output -match 'CROSSSLOT|pending=|lost its retry|waiting=|DATA RACE|panic' } | Select-Object Package,Test,Output)
    }
    [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),($result | ConvertTo-Json -Depth 8),[Text.UTF8Encoding]::new($false))
    $result | Select-Object source_head,go_exit,leaf_pass,leaf_fail,leaf_skip,build_fail | ConvertTo-Json
    if ($goExit -ne 1 -or $result.leaf_fail -ne 6 -or $result.leaf_pass -ne 9 -or $result.leaf_skip -ne 0 -or $result.build_fail -ne 0 -or @($events | Where-Object { $_.Output -match 'WARNING: DATA RACE|panic:' }).Count) {
        throw 'Unexpected evidence outcome; inspect the raw log. Changed behavior, environment failure and compilation errors are not confirmed bugs.'
    }
} finally {
    Pop-Location
    foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key,$saved[$key]) }
}
