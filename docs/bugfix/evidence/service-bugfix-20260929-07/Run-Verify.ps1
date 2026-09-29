param(
 [string]$RepositoryRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
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
$utf8=[Text.UTF8Encoding]::new($false)
$module=[regex]::Match((Get-Content (Join-Path $demo 'go.mod') -Raw),'(?m)^module\s+(\S+)').Groups[1].Value
if (-not $module) { throw 'missing module' }
foreach ($rel in @('internal/service/platform/collaborators.go','internal/service/platform/purchase_delivery_test.go','game/purchase/purchase.go')) {
 $template=(Get-Content (Join-Path $root "demo/$rel.tmpl") -Raw).Replace('{{MODULE}}',$module).Replace("`r`n","`n")
 $normalized=Join-Path $out ('template-'+[IO.Path]::GetFileName($rel))
 [IO.File]::WriteAllText($normalized,$template,$utf8)
 & gofmt -w $normalized
 if ($LASTEXITCODE -ne 0) { throw "template does not parse: $rel" }
 $template=(Get-Content $normalized -Raw).Replace("`r`n","`n")
 $consumer=(Get-Content (Join-Path $demo $rel) -Raw).Replace("`r`n","`n")
 if ($template -ne $consumer) { throw "consumer differs from template: $rel" }
}
$saved=@{}
foreach ($key in @('ROOST_REVIEW_REDIS','ROOST_REVIEW_CLUSTER','ROOST_REVIEW_PURCHASE_PREFIX','ROOST_REVIEW_PURCHASE_STAGE')) { $saved[$key]=[Environment]::GetEnvironmentVariable($key) }
$runs=[Collections.Generic.List[object]]::new()
function Run-Probe([string]$name,[string]$overlay,[string]$package) {
 $raw=@(& go test -mod=mod -race -count=1 -timeout=120s -overlay $overlay -run '^TestReview8' -json $package 2>&1 | ForEach-Object { "$_" })
 $code=$LASTEXITCODE; $log=Join-Path $out "$name.jsonl"; [IO.File]::WriteAllLines($log,$raw,$utf8)
 $events=@($raw | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json })
 $names=@($events | Where-Object { $_.Action -eq 'run' -and $_.Test } | Select-Object -ExpandProperty Test -Unique)
 $leaves=@($names | Where-Object { $n=$_; -not @($names | Where-Object { $_.StartsWith("$n/") }).Count })
 $outcomes=@($events | Where-Object { $_.Test -in $leaves -and $_.Action -in @('pass','fail','skip') } | Select-Object Package,Test,Action)
 $runs.Add([pscustomobject]@{name=$name;exit=$code;outcomes=$outcomes;log_sha256=(Get-FileHash $log).Hash;evidence=@($events | Where-Object { $_.Output -match 'resolved_count=' } | Select-Object Test,Output)})
 if ($code -ne 0 -or @($outcomes | Where-Object Action -ne 'pass').Count -or -not $outcomes.Count) { throw "probe $name did not pass; see $log" }
}
try {
 $env:ROOST_REVIEW_REDIS=$RedisAddress; $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
 $env:ROOST_REVIEW_PURCHASE_PREFIX='bugfix7-purchase:'+([guid]::NewGuid().ToString('N'))
 $old=Join-Path $root 'docs/review/evidence/service-review-20260929-08'
 $replace=@{}; $replace[(Join-Path $root 'kit/service/global/activity/review8_overlay_test.go')]=(Join-Path $old 'activity_test.go.txt')
 $overlay=Join-Path $out 'activity-overlay.json'; [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
 Push-Location $root; try { Run-Probe 'activity' $overlay './kit/service/global/activity' } finally { Pop-Location }
 $probe=(Get-Content (Join-Path $old 'purchase_test.go.txt') -Raw).Replace('{{MODULE}}',$module)
 $start=$probe.IndexOf('func (s review8LostGrantReply) HSet('); $end=$probe.IndexOf('func TestReview8PurchaseCatalogRetry')
 if ($start -lt 0 -or $end -le $start) { throw 'old fault seam missing' }
 $seam=@'
func (s review8LostGrantReply) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
 if _, err := s.grantStore.Eval(ctx, script, keys, args...); err != nil { return nil, err }
 return nil, context.DeadlineExceeded
}

'@
 $probe=$probe.Substring(0,$start)+$seam+$probe.Substring($end)
 $path=Join-Path $out 'purchase_test.go'; [IO.File]::WriteAllText($path,$probe,$utf8)
 $replace=@{}; $replace[(Join-Path $demo 'internal/service/platform/review8_overlay_test.go')]=$path
 $overlay=Join-Path $out 'purchase-overlay.json'; [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
 Push-Location $demo
 try {
  foreach ($stage in @('seed','control')) { $env:ROOST_REVIEW_PURCHASE_STAGE=$stage; Run-Probe "purchase-$stage" $overlay './internal/service/platform' }
  $catalog=Get-Content 'game/purchase/purchase.go' -Raw; $needle='ID: "potion_pack", ItemID: 1001, Count: 10'
  if (-not $catalog.Contains($needle)) { throw 'catalog fixture missing' }
  $path=Join-Path $out 'purchase-upgraded.go'; [IO.File]::WriteAllText($path,$catalog.Replace($needle,'ID: "potion_pack", ItemID: 1001, Count: 3'),$utf8)
  $replace[(Join-Path $demo 'game/purchase/purchase.go')]=$path
  $overlay=Join-Path $out 'purchase-upgrade-overlay.json'; [IO.File]::WriteAllText($overlay,(@{Replace=$replace}|ConvertTo-Json),$utf8)
  $env:ROOST_REVIEW_PURCHASE_STAGE='upgrade'; Run-Probe 'purchase-upgrade' $overlay './internal/service/platform'
 } finally { Pop-Location }
 $result=[pscustomobject]@{source_base='bcebb80568559dcf895db0683cb85eee75b9ef53';working_changes=$true;fault_seam='HSet -> Eval after real write; original assertion retained';runs=$runs}
 [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),($result|ConvertTo-Json -Depth 12),$utf8)
 Write-Output "original business assertions passed; probe stages=$($runs.Count)"
} finally { foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key,$saved[$key]) } }
