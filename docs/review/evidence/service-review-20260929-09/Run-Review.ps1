param(
 [string]$RepositoryRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$RedisAddress,
 [Parameter(Mandatory=$true)][string]$ClusterAddresses,
 [Parameter(Mandatory=$true)][string]$OutputDirectory,
 [switch]$RPCOnly
)
$ErrorActionPreference='Stop'
$root=(Resolve-Path -LiteralPath $RepositoryRoot).Path
$out=[IO.Path]::GetFullPath($OutputDirectory); [IO.Directory]::CreateDirectory($out)|Out-Null
$utf8=[Text.UTF8Encoding]::new($false)
$saved=@{}; foreach($key in @('ROOST_REVIEW_REDIS','ROOST_REVIEW_CLUSTER')){$saved[$key]=[Environment]::GetEnvironmentVariable($key)}
$runs=[Collections.Generic.List[object]]::new()
function Probe([string]$name,[string]$package,[string]$pattern,[int]$expected) {
 $raw=@(& go test -count=1 -timeout=120s -overlay (Join-Path $out 'overlay.json') -run $pattern -json $package 2>&1|ForEach-Object{"$_"})
 $code=$LASTEXITCODE; $log=Join-Path $out "$name.jsonl"; [IO.File]::WriteAllLines($log,$raw,$utf8)
 $events=@($raw|Where-Object{$_.StartsWith('{')}|ForEach-Object{$_|ConvertFrom-Json})
 $names=@($events|Where-Object{$_.Action -eq 'run' -and $_.Test}|Select-Object -ExpandProperty Test -Unique)
 $leaves=@($names|Where-Object{$n=$_;-not @($names|Where-Object{$_.StartsWith("$n/")}).Count})
 $outcomes=@($events|Where-Object{$_.Test -in $leaves -and $_.Action -in @('pass','fail','skip')}|Select-Object Package,Test,Action)
 $runs.Add([pscustomobject]@{name=$name;exit=$code;outcomes=$outcomes;log_sha256=(Get-FileHash $log).Hash;evidence=@($events|Where-Object{$_.Output -match 'COST|batch_error='}|Select-Object Test,Output)})
 if($code -ne $expected -or @($events|Where-Object{$_.Action -eq 'build-fail' -or $_.FailedBuild}).Count -or @($outcomes|Where-Object Action -eq 'skip').Count -or -not $outcomes.Count){throw "unexpected result: $name"}
 if($name -eq 'mail-cluster') {
  if(@($outcomes|Where-Object{$_.Test -like '*/tagged_false' -and $_.Action -eq 'fail'}).Count -ne 1 -or @($outcomes|Where-Object{$_.Test -like '*/tagged_true' -and $_.Action -eq 'pass'}).Count -ne 1){throw 'mail counterexample/control missing'}
 } elseif(@($outcomes|Where-Object Action -ne 'pass').Count){throw "nonpassing control: $name"}
}
Push-Location $root
try {
 $env:ROOST_REVIEW_REDIS=$RedisAddress; $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
 $map=@{}; $map[(Join-Path $root 'kit/service/mail/review9_overlay_test.go')]=(Join-Path $PSScriptRoot 'mail_test.go.txt'); $map[(Join-Path $root 'service/match/review9_overlay_test.go')]=(Join-Path $PSScriptRoot 'match_test.go.txt')
 [IO.File]::WriteAllText((Join-Path $out 'overlay.json'),(@{Replace=$map}|ConvertTo-Json),$utf8)
 if(-not $RPCOnly) {
 Probe 'mail-cluster' './kit/service/mail' '^TestReview9MailClusterPage$' 1
 Probe 'mail-pipeline-candidate' './kit/service/mail' '^TestReview9MailClusterPipelineCandidate$' 0
 # Serial, without race/profile; wait for unrelated CPU work before invoking.
 Probe 'match-cost' './service/match' '^TestReview9Match' 0
 [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),(@{source_base='bcebb80568559dcf895db0683cb85eee75b9ef53';working_changes=$true;runs=$runs}|ConvertTo-Json -Depth 12),$utf8)
 }
 $binary=Join-Path $out 'servicerpc.exe'; & go build -o $binary ./codegen/cmd/servicerpc; if($LASTEXITCODE -ne 0){throw 'generator build failed'}
 $checks=[Collections.Generic.List[object]]::new()
 foreach($dir in @('kit/service/account','kit/service/chat','kit/service/global','kit/service/global/activity','kit/service/platform','kit/service/rank')){
  Push-Location (Join-Path $root $dir)
  try { $result=@(& $binary -dir . -check 2>&1|ForEach-Object{"$_"}); $code=$LASTEXITCODE } finally { Pop-Location }
  $checks.Add(@{dir=$dir;exit=$code;output=$result}); if($code -ne 0){throw "rpc check: $dir $result"}
 }
 foreach($name in @('mail','match','session')){
  $dir="service/$name"; Push-Location (Join-Path $root $dir)
  try { $result=@(& $binary -dir . -emit transport -check 2>&1|ForEach-Object{"$_"}); $code=$LASTEXITCODE } finally { Pop-Location }
  $checks.Add(@{dir=$dir;emit='transport';exit=$code;output=$result}); if($code -ne 0){throw "transport check: $name $result"}
  $target="kit/service/$name"; Push-Location (Join-Path $root $target)
  try { $result=@(& $binary -dir "github.com/tjbdwanghaibo/roost-core/service/$name" -emit assembly -out . -check 2>&1|ForEach-Object{"$_"}); $code=$LASTEXITCODE } finally { Pop-Location }
  $checks.Add(@{dir=$dir;out=$target;emit='assembly';exit=$code;output=$result}); if($code -ne 0){throw "assembly check: $name $result"}
 }
 [IO.File]::WriteAllText((Join-Path $out 'RPC-CHECKS.json'),($checks|ConvertTo-Json -Depth 8),$utf8)
 Write-Output "review probes complete; RPC checks=$($checks.Count)"
} finally {Pop-Location;foreach($key in $saved.Keys){[Environment]::SetEnvironmentVariable($key,$saved[$key])}}
