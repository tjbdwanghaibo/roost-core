param(
    [string]$RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [ValidateSet('memory','redis')][string]$Backend = 'memory',
    [Parameter(Mandatory=$true)][string]$RedisAddress,
    [Parameter(Mandatory=$true)][string]$OutputDirectory
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path -LiteralPath $RepositoryRoot).Path
$out = [IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out) | Out-Null
$map = @{
    'service/match'='match'; 'kit/service/rank'='rank';
    'kit/service/global/activity'='activity'; 'service/session'='session';
    'kit/service/chat'='chat'; 'kit/service/global'='global';
    'kit/service/account'='account'
}
$replace = @{}
foreach ($package in $map.Keys) {
    $replace[(Join-Path $root "$package/review5_overlay_test.go")] =
        (Join-Path $PSScriptRoot "$($map[$package])_test.go.txt")
}
$overlay = Join-Path $out 'overlay.json'
[IO.File]::WriteAllText($overlay, (@{Replace=$replace} | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
$saved = @{}
foreach ($key in @('ROOST_REVIEW5_BACKEND','ROOST_REVIEW3_BACKEND','ROOST_REVIEW_REDIS')) {
    $saved[$key] = [Environment]::GetEnvironmentVariable($key)
}
Push-Location $root
try {
    $env:ROOST_REVIEW5_BACKEND=$Backend
    $env:ROOST_REVIEW3_BACKEND=$Backend
    $env:ROOST_REVIEW_REDIS=$RedisAddress
    $packages = @('service/match','kit/service/rank','kit/service/global/activity','service/session','kit/service/chat','kit/service/global','kit/service/account') | ForEach-Object { "./$_" }
    $raw = @(& go test -race -count=1 -timeout=120s -overlay $overlay -run '^TestReview5' -json @packages 2>&1 | ForEach-Object { "$_" })
    $goExit=$LASTEXITCODE
    $log=Join-Path $out 'repro.jsonl'
    [IO.File]::WriteAllLines($log,$raw,[Text.UTF8Encoding]::new($false))
    $events=@($raw | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json })
    $runs=@($events | Where-Object { $_.Action -eq 'run' -and $_.Test } | Select-Object -ExpandProperty Test -Unique)
    $leaf=@($runs | Where-Object { $name=$_; -not @($runs | Where-Object { $_.StartsWith("$name/") }).Count })
    $outcomes=@($events | Where-Object { $_.Test -in $leaf -and $_.Action -in @('pass','fail','skip') } | Select-Object Package,Test,Action)
    $result=[pscustomobject][ordered]@{
        source_head=(& git -c "safe.directory=$root" rev-parse HEAD);
        backend=$Backend; redis=$RedisAddress; go_exit=$goExit;
        leaf_pass=@($outcomes | Where-Object Action -eq 'pass').Count;
        leaf_fail=@($outcomes | Where-Object Action -eq 'fail').Count;
        leaf_skip=@($outcomes | Where-Object Action -eq 'skip').Count;
        log_sha256=(Get-FileHash -LiteralPath $log -Algorithm SHA256).Hash;
        outcomes=$outcomes;
        failure_output=@($events | Where-Object { $_.Action -eq 'output' -and $_.Test -and ($_.OutputType -eq 'error' -or $_.Output -match 'panic|overflow|expired admission|expired late|pruned=') } | Select-Object Package,Test,Output)
    }
    $json=$result | ConvertTo-Json -Depth 8
    [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),$json,[Text.UTF8Encoding]::new($false))
    $result | Select-Object source_head,backend,go_exit,leaf_pass,leaf_fail,leaf_skip | ConvertTo-Json
    if ($goExit -ne 1 -or $result.leaf_fail -ne 12 -or $result.leaf_pass -ne 9 -or $result.leaf_skip -ne 0 -or @($events | Where-Object Action -eq 'build-fail').Count) {
        throw 'Unexpected evidence outcome; inspect raw log for compile/environment failure or changed behavior.'
    }
} finally {
    Pop-Location
    foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key,$saved[$key]) }
}
