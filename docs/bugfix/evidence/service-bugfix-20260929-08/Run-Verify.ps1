param(
 [string]$RepositoryRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$RedisAddress,
 [Parameter(Mandatory=$true)][string]$ClusterAddresses,
 [Parameter(Mandatory=$true)][string]$OutputDirectory
)
$ErrorActionPreference='Stop'
$root=(Resolve-Path -LiteralPath $RepositoryRoot).Path
$out=[IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($out)|Out-Null
$utf8=[Text.UTF8Encoding]::new($false)
$saved=@{}
foreach($name in @('REDIS_ADDR','ROOST_REDIS_TEST_ADDR','ROOST_REVIEW_CLUSTER')) { $saved[$name]=[Environment]::GetEnvironmentVariable($name) }
$runs=[Collections.Generic.List[object]]::new()
Push-Location $root
try {
 $env:REDIS_ADDR=$RedisAddress; $env:ROOST_REDIS_TEST_ADDR=$RedisAddress; $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
 $map=@{}
 $map[(Join-Path $root 'kit/service/mail/review9_overlay_test.go')]=(Join-Path $root 'docs/review/evidence/service-review-20260929-09/mail_test.go.txt')
 $overlay=Join-Path $out 'overlay.json'
 [IO.File]::WriteAllText($overlay,(@{Replace=$map}|ConvertTo-Json),$utf8)
 foreach($run in @(
  @{name='original-mail';args=@('test','-count=1','-json','-overlay',$overlay,'-run','^TestReview9MailClusterPage$','./kit/service/mail')},
  @{name='formal-mail';args=@('test','-tags','integration','-race','-count=2','-timeout=120s','-json','./service/mail','./kit/service/mail')}
 )) {
  $raw=@(& go @($run.args) 2>&1|ForEach-Object{"$_"}); $code=$LASTEXITCODE
  $log=Join-Path $out ($run.name+'.jsonl'); [IO.File]::WriteAllLines($log,$raw,$utf8)
  $events=@($raw|Where-Object{$_.StartsWith('{')}|ForEach-Object{$_|ConvertFrom-Json})
  $bad=@($events|Where-Object{$_.Action -in @('fail','skip','build-fail') -or $_.FailedBuild})
  $runs.Add(@{name=$run.name;exit=$code;log_sha256=(Get-FileHash $log).Hash;pass_events=@($events|Where-Object{$_.Test -and $_.Action -eq 'pass'}).Count;bad_events=$bad.Count})
  if($code -ne 0 -or $bad.Count) { throw "verification failed: $($run.name)" }
 }
 [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),(@{runs=$runs}|ConvertTo-Json -Depth 8),$utf8)
 $runs|ConvertTo-Json -Depth 8
} finally {
 Pop-Location
 foreach($name in $saved.Keys) { [Environment]::SetEnvironmentVariable($name,$saved[$name]) }
}
