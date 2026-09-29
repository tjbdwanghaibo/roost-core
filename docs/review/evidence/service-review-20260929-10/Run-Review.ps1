param(
 [string]$RepositoryRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$RedisAddress,
 [Parameter(Mandatory=$true)][string]$ClusterAddresses,
 [Parameter(Mandatory=$true)][string]$OutputDirectory,
 [switch]$ExpectFixed
)
$ErrorActionPreference='Stop'
$root=(Resolve-Path -LiteralPath $RepositoryRoot).Path
$out=[IO.Path]::GetFullPath($OutputDirectory); [IO.Directory]::CreateDirectory($out)|Out-Null
$utf8=[Text.UTF8Encoding]::new($false)
$saved=@{}; foreach($key in @('REDIS_ADDR','ROOST_REVIEW_CLUSTER')) { $saved[$key]=[Environment]::GetEnvironmentVariable($key) }
Push-Location $root
try {
 $env:REDIS_ADDR=$RedisAddress; $env:ROOST_REVIEW_CLUSTER=$ClusterAddresses
 $map=@{}; $map[(Join-Path $root 'redis/driver/review10_overlay_test.go')]=(Join-Path $PSScriptRoot 'pipeline_test.go.txt')
 $overlay=Join-Path $out 'overlay.json'; [IO.File]::WriteAllText($overlay,(@{Replace=$map}|ConvertTo-Json),$utf8)
 $raw=@(& go test -count=1 -timeout=120s -json -overlay $overlay -run '^TestReview10Pipeline' ./redis/driver 2>&1|ForEach-Object{"$_"})
 $code=$LASTEXITCODE; $log=Join-Path $out 'pipeline.jsonl'; [IO.File]::WriteAllLines($log,$raw,$utf8)
 $events=@($raw|Where-Object{$_.StartsWith('{')}|ForEach-Object{$_|ConvertFrom-Json})
 $names=@($events|Where-Object{$_.Action -eq 'run' -and $_.Test}|Select-Object -ExpandProperty Test -Unique)
 $leaves=@($names|Where-Object{$n=$_; -not @($names|Where-Object{$_.StartsWith("$n/")}).Count})
 $outcomes=@($events|Where-Object{$_.Test -in $leaves -and $_.Action -in @('pass','fail','skip')}|Select-Object Test,Action)
 $result=@{exit=$code;expect_fixed=[bool]$ExpectFixed;outcomes=$outcomes;log_sha256=(Get-FileHash $log).Hash;evidence=@($events|Where-Object{$_.Output -match 'Exec='}|Select-Object Test,Output)}
 [IO.File]::WriteAllText((Join-Path $out 'RESULTS.json'),($result|ConvertTo-Json -Depth 8),$utf8)
 $expected=1; if($ExpectFixed){$expected=0}
 if($code -ne $expected -or @($events|Where-Object{$_.Action -eq 'build-fail' -or $_.FailedBuild}).Count -or $outcomes.Count -ne 3 -or @($outcomes|Where-Object Action -eq 'skip').Count){throw 'environment/build/unexpected outcome'}
 if($ExpectFixed){if(@($outcomes|Where-Object Action -ne 'pass').Count){throw 'fix not established'}}
 elseif(@($outcomes|Where-Object{$_.Test -like '*/cluster_*' -and $_.Action -eq 'fail'}).Count -ne 2 -or @($outcomes|Where-Object{$_.Test -eq 'TestReview10PipelineReuseAndDiscard' -and $_.Action -eq 'pass'}).Count -ne 1){throw 'counterexamples/positive control not established'}
 $result|ConvertTo-Json -Depth 8
} finally { Pop-Location; foreach($key in $saved.Keys){[Environment]::SetEnvironmentVariable($key,$saved[$key])} }
