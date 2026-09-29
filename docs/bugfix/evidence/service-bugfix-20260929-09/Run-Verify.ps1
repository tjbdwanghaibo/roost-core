param(
 [string]$RepositoryRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$RedisAddress,
 [Parameter(Mandatory=$true)][string]$ClusterAddresses,
 [Parameter(Mandatory=$true)][string]$OutputDirectory,
 [switch]$AllServices
)
$ErrorActionPreference='Stop'
$root=(Resolve-Path -LiteralPath $RepositoryRoot).Path
$out=[IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out)|Out-Null
$utf8=[Text.UTF8Encoding]::new($false)
$saved=@{}
foreach($key in @('REDIS_ADDR','ROOST_REDIS_TEST_ADDR','ROOST_REVIEW_REDIS','ROOST_REVIEW_CLUSTER')){$saved[$key]=[Environment]::GetEnvironmentVariable($key)}
function RunTests([string]$name,[string[]]$goArguments,[int]$expectedSkips) {
 $raw=@(& go @goArguments 2>&1|ForEach-Object{"$_"})
 $code=$LASTEXITCODE
 $log=Join-Path $out "$name.jsonl"
 [IO.File]::WriteAllLines($log,$raw,$utf8)
 $events=@($raw|Where-Object{$_.StartsWith('{')}|ForEach-Object{$_|ConvertFrom-Json})
 $skips=@($events|Where-Object{$_.Test -and $_.Action -eq 'skip'})
 $failures=@($events|Where-Object{$_.Action -in @('fail','build-fail') -or $_.FailedBuild})
 if($code -ne 0 -or $failures.Count -or $skips.Count -ne $expectedSkips -or -not @($events|Where-Object{$_.Test -and $_.Action -eq 'pass'}).Count){throw "unexpected result: $name exit=$code skips=$($skips.Count)"}
 # The three optional physical-network tests are the only allowed skips.
 foreach($skip in $skips){if($skip.Package -ne 'github.com/tjbdwanghaibo/roost-core/redis/driver' -or $skip.Test -notin @('TestToxicRedisDroppedReleaseReplyLeavesTheLockUncertainUntilReconciled','TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried','TestToxicRedisLatencyKeepsAcquireWithinItsDeadline')){throw "unexpected skipped test: $($skip.Test)"}}
 Write-Output "$name passed; test skips=$($skips.Count); log SHA256=$((Get-FileHash $log).Hash)"
}
Push-Location $root
try {
 $env:REDIS_ADDR=$RedisAddress
 $env:ROOST_REDIS_TEST_ADDR=$RedisAddress
 $env:ROOST_REVIEW_REDIS=$RedisAddress
 $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
 & (Join-Path $root 'docs/review/evidence/service-review-20260929-10/Run-Review.ps1') -RepositoryRoot $root -RedisAddress $RedisAddress -ClusterAddresses $ClusterAddresses -OutputDirectory (Join-Path $out 'original') -ExpectFixed
 RunTests 'pipeline-final' @('test','-tags','integration','-race','-count=2','-timeout=120s','-json','-run','^(TestIntegrationPipeline|TestPipelineExec)','./redis/driver') 0
 if($AllServices) {
  # Match the existing toxicRedis environment contract, never accept arbitrary skips.
  $expected=3
  if($env:ROOST_DATAENGINE_IT -eq '1' -and $env:ROOST_DATAENGINE_IT_TOXIPROXY_URL -and $env:ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR){$expected=0}
  RunTests 'all-services' @('test','-tags','integration','-race','-count=1','-timeout=180s','-json','./versionstore','./cache','./redis/driver','./kit/mods','./kit/service/...','./service/...') $expected
 }
} finally {Pop-Location;foreach($key in $saved.Keys){[Environment]::SetEnvironmentVariable($key,$saved[$key])}}
