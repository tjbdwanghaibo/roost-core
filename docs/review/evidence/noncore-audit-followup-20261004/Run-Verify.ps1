param(
 [Parameter(Mandatory=$true)][ValidateSet('Red','Green')][string]$Mode,
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [Parameter(Mandatory=$true)][string]$RedisExecutable,
 [Parameter(Mandatory=$true)][string]$RedisCliExecutable,
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $OutputRoot){throw 'Use a new output directory'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$env:GOWORK='off'
$server=$null;$oldAddr=$env:ROOST_REDIS_TEST_ADDR
Push-Location $RepoRoot
try {
 $overlay=@();$packages=@('./cache','./mongo/mongotest')
 $select='^(TestLayeredExpiredLocalCopyDoesNotVetoAuthority.*|TestReadThroughLoaderFillRefusalIsNotReadFailure|TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis|TestRefHMapGetTreatsAMissingReferencedHashAsMiss|TestUniqueIndexMissingFieldPromises)$'
 if($Mode -eq 'Red'){
  # Old implementation only; keep the same post-audit regressions. The NC-30
  # file references an API absent at v1.19.0 and is outside this audit scope.
  $replace=@{}
  foreach($path in @('cache/layered.go','cache/read_through.go','cache/ref_hmap.go','cache/ref_hmap_test.go','mongo/mongotest/mongotest.go')){
   $source=& git -C $RepoRoot -c "safe.directory=$RepoRoot" show "74e1ba39:$path"
   if($LASTEXITCODE -ne 0){throw "Cannot read historical source $path"}
   $target=Join-Path $OutputRoot ($path.Replace('/','_'))
   $source | Set-Content $target -Encoding utf8NoBOM
   $replace[(Join-Path $RepoRoot $path)]=$target
  }
  $empty=Join-Path $OutputRoot 'out-of-scope.go'
  'package cache' | Set-Content $empty -Encoding utf8NoBOM
  $replace[(Join-Path $RepoRoot 'cache/ref_hmap_schema_promises_test.go')]=$empty
  @{Replace=$replace}|ConvertTo-Json -Depth 4|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
  $overlay=@('-overlay',(Join-Path $OutputRoot 'overlay.json'))
 }else{
  $packages+=@('./etcd/driver','./kit/nats')
  $select=$select.TrimEnd('$').TrimEnd(')')+'|TestElectionRevoke.*|TestNatsModStopRetry.*)$'
 }
 $listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
 $listener.Start();$port=$listener.LocalEndpoint.Port;$listener.Stop()
 $config=Join-Path $OutputRoot 'redis.conf'
 @('bind 127.0.0.1',"port $port",'save ""','appendonly no','dir .','logfile redis.log')|Set-Content $config -Encoding utf8NoBOM
 $server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
 $ready=$false
 for($attempt=0;$attempt -lt 50;$attempt++){
  $server.Refresh();if($server.HasExited){throw 'Redis failed to start'}
  $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
  if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG'){$ready=$true;break}
  Start-Sleep -Milliseconds 100
 }
 if(!$ready){throw 'Redis not ready'}
 $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
 & $GoExecutable test -race -count=1 -timeout=90s -json @overlay -run $select @packages 2>&1|Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE
 @{mode=$Mode;test_exit=$testExit;historical_source='74e1ba39';current_source=(& git -c "safe.directory=$RepoRoot" rev-parse HEAD);selector=$select}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
 if($Mode -eq 'Green'){
  & $GoExecutable vet @packages 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
  $vetExit=$LASTEXITCODE
  & $GoExecutable test -count=1 -timeout=90s -json . 2>&1|Set-Content (Join-Path $OutputRoot 'root.jsonl') -Encoding utf8NoBOM
  @{vet_exit=$vetExit;root_exit=$LASTEXITCODE}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'checks.json') -Encoding utf8NoBOM
  if($vetExit -ne 0 -or $LASTEXITCODE -ne 0){throw 'Checks failed'}
 }
 $events=@(Get-Content (Join-Path $OutputRoot 'tests.jsonl')|ForEach-Object{$_|ConvertFrom-Json})
 if(@($events|Where-Object{$_.Action -eq 'build-fail' -or $_.Output -match '\[build failed\]|WARNING: DATA RACE|panic:|test timed out'}).Count){throw 'Infrastructure, race, panic or timeout is not the promised red'}
 if($Mode -eq 'Red' -and $testExit -ne 1){throw 'Expected historical behavior failures'}
 if($Mode -eq 'Green' -and $testExit -ne 0){throw 'Current regressions failed'}
}finally{
 $env:ROOST_REDIS_TEST_ADDR=$oldAddr
 if($server){$server.Refresh();if(!$server.HasExited){Stop-Process -Id $server.Id};@{owned_pid=$server.Id;exited=$server.WaitForExit(5000)}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM}
 Pop-Location
}
