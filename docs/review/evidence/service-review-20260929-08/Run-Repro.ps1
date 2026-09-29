param(
    [string]$RepositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$DemoRoot,
    [Parameter(Mandatory=$true)][string]$RedisAddress,
    [Parameter(Mandatory=$true)][string]$ClusterAddresses,
    [Parameter(Mandatory=$true)][string]$OutputDirectory
)
$ErrorActionPreference='Stop'
$root=(Resolve-Path -LiteralPath $RepositoryRoot).Path
$demo=(Resolve-Path -LiteralPath $DemoRoot).Path
$out=[IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out) | Out-Null
$module=[regex]::Match((Get-Content (Join-Path $demo 'go.mod') -Raw),'(?m)^module\s+(\S+)').Groups[1].Value
if (-not $module) { throw 'Demo module is missing' }
foreach ($relative in @('internal/service/platform/collaborators.go','game/purchase/purchase.go')) {
    $source=(Get-Content (Join-Path $root "demo/$relative.tmpl") -Raw).Replace('{{MODULE}}',$module).Replace("`r`n","`n")
    $consumer=(Get-Content (Join-Path $demo $relative) -Raw).Replace("`r`n","`n")
    if ($source -ne $consumer) { throw "Generated consumer differs from current template: $relative" }
}
$utf8=[Text.UTF8Encoding]::new($false)
$saved=@{}
foreach ($key in @('ROOST_REVIEW_REDIS','ROOST_REVIEW_CLUSTER','ROOST_REVIEW_PURCHASE_PREFIX','ROOST_REVIEW_PURCHASE_STAGE')) {
    $saved[$key]=[Environment]::GetEnvironmentVariable($key)
}
$runs=[Collections.Generic.List[object]]::new()
function Invoke-Probe([string]$name,[string]$overlay,[string]$package,[int]$expectedExit) {
    $raw=@(& go test -mod=mod -race -count=1 -timeout=120s -overlay $overlay -run '^TestReview8' -json $package 2>&1 | ForEach-Object { "$_" })
    $goExit=$LASTEXITCODE
    $log=Join-Path $out "$name.jsonl"
    [IO.File]::WriteAllLines($log,$raw,$utf8)
    $events=@($raw | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json })
    $names=@($events | Where-Object { $_.Action -eq 'run' -and $_.Test } | Select-Object -ExpandProperty Test -Unique)
    $leaf=@($names | Where-Object { $n=$_; -not @($names | Where-Object { $_.StartsWith("$n/") }).Count })
    $outcomes=@($events | Where-Object { $_.Test -in $leaf -and $_.Action -in @('pass','fail','skip') } | Select-Object Package,Test,Action)
    $runs.Add([pscustomobject]@{name=$name;go_exit=$goExit;outcomes=$outcomes;log_sha256=(Get-FileHash $log).Hash;evidence=@($events | Where-Object { $_.Test -and $_.Action -eq 'output' -and $_.Output -match 'aggregation=|resolved_count=|overwrote|CROSSSLOT' } | Select-Object Test,Output)})
    if ($goExit -ne $expectedExit -or @($events | Where-Object { $_.Action -eq 'build-fail' -or $_.Action -eq 'skip' -or $_.Output -match 'WARNING: DATA RACE|panic:' }).Count) {
        throw "Unexpected $name outcome; inspect the log, do not count environment/build failures as bugs"
    }
}
try {
    $env:ROOST_REVIEW_REDIS=$RedisAddress
    $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
    $env:ROOST_REVIEW_PURCHASE_PREFIX='review8:purchase:'+([guid]::NewGuid().ToString('N'))
    $replace=@{}
    $replace[(Join-Path $root 'kit/service/global/activity/review8_overlay_test.go')]=(Join-Path $PSScriptRoot 'activity_test.go.txt')
    $overlay=Join-Path $out 'activity-overlay.json'
    [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
    Push-Location $root
    try { Invoke-Probe 'activity' $overlay './kit/service/global/activity' 1 } finally { Pop-Location }
    $probe=Join-Path $out 'purchase_test.go'
    [IO.File]::WriteAllText($probe,(Get-Content (Join-Path $PSScriptRoot 'purchase_test.go.txt') -Raw).Replace('{{MODULE}}',$module),$utf8)
    $replace=@{}
    $replace[(Join-Path $demo 'internal/service/platform/review8_overlay_test.go')]=$probe
    $overlay=Join-Path $out 'purchase-overlay.json'
    [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
    Push-Location $demo
    try {
        foreach ($stage in @('seed','control')) {
            $env:ROOST_REVIEW_PURCHASE_STAGE=$stage
            Invoke-Probe "purchase-$stage" $overlay './internal/service/platform' 0
        }
        $catalog=Get-Content 'game/purchase/purchase.go' -Raw
        $old='ID: "potion_pack", ItemID: 1001, Count: 10'
        if (-not $catalog.Contains($old)) { throw 'Catalog fixture is missing' }
        $upgraded=Join-Path $out 'purchase-upgraded.go'
        [IO.File]::WriteAllText($upgraded,$catalog.Replace($old,'ID: "potion_pack", ItemID: 1001, Count: 3'),$utf8)
        $replace[(Join-Path $demo 'game/purchase/purchase.go')]=$upgraded
        $overlay=Join-Path $out 'purchase-upgrade-overlay.json'
        [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
        $env:ROOST_REVIEW_PURCHASE_STAGE='upgrade'
        Invoke-Probe 'purchase-upgrade' $overlay './internal/service/platform' 1
    } finally { Pop-Location }
    $outcomes=@($runs | ForEach-Object { $_.outcomes })
    $result=[ordered]@{source_base=(& git -C $root -c "safe.directory=$root" rev-parse HEAD);working_changes=$true;go=(& go version);demo_module=$module;leaf_pass=@($outcomes | Where-Object Action -eq 'pass').Count;leaf_fail=@($outcomes | Where-Object Action -eq 'fail').Count;leaf_skip=@($outcomes | Where-Object Action -eq 'skip').Count;runs=$runs.ToArray()}
    [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),($result|ConvertTo-Json -Depth 10),$utf8)
    if ($result.leaf_pass -ne 3 -or $result.leaf_fail -ne 5 -or $result.leaf_skip -ne 0) { throw 'Unexpected final leaf counts' }
    [pscustomobject]$result | Select-Object source_base,leaf_pass,leaf_fail,leaf_skip | ConvertTo-Json
} finally {
    foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key,$saved[$key]) }
}
