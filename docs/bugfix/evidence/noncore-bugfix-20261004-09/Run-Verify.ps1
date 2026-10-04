param(
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [string]$RedisExecutable,
 [string]$RedisCliExecutable,
 [ValidateSet('Red','Green','Review','Wanted','RemoteIntegration')][string]$Mode='Green',
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$replace=@{}
$packages=@('./mongo/mongotest')
$selectArgs=@()
if($Mode -eq 'Red'){
 $source=& git -C $RepoRoot -c "safe.directory=$RepoRoot" show '3d3b22c9f8398c032c6a2c7a574197dfe1bf59a2:mongo/mongotest/mongotest.go'
 if($LASTEXITCODE -ne 0){throw 'Cannot recover red baseline'}
 $dest=Join-Path $OutputRoot 'mongotest.go';$source | Set-Content $dest -Encoding utf8NoBOM
 $replace[(Join-Path $RepoRoot 'mongo/mongotest/mongotest.go')]=$dest
 foreach($path in @('transaction.go','transaction_promises_test.go','index_bulk_promises_test.go')){$replace[(Join-Path $RepoRoot "mongo/mongotest/$path")]=''}
 $selectArgs=@('-run','^TestMongoBoundaryPromises$')
}elseif($Mode -eq 'Review'){
 $replace[(Join-Path $RepoRoot 'redis/driver/review18_test.go')]=Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261004-18/review18_test.go.txt'
 $packages=@('./redis/driver');$selectArgs=@('-run','^TestReview18RedisLeaseAndPubSub$')
}elseif($Mode -eq 'Wanted'){
 $replace[(Join-Path $RepoRoot 'remoteentity/wanted_acquire_test.go')]=Join-Path $PSScriptRoot '../../../review/evidence/noncore-review-20261004-18/wanted_test.go.txt'
 $packages=@('./remoteentity');$selectArgs=@('-run','^TestWantedAcquireUnknown$')
}elseif($Mode -eq 'RemoteIntegration'){
 $packages=@('./remoteentity');$selectArgs=@('-tags','integration','-run','^TestRealVersionedLock(ReacquiresAfterAcquireReplyLost|ReacquiresAcrossUnknownChain|LateAcquireScript)$')
}else{
 $packages=@('./cache','./redis','./redis/driver','./mongo','./mongo/driver','./mongo/mongotest','./migration','./codegen/internal/dao','./kit/redis','./kit/mongo')
}
$overlayArgs=@()
if($replace.Count -gt 0){
 @{Replace=$replace}|ConvertTo-Json -Depth 5|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
 $overlayArgs=@('-overlay',(Join-Path $OutputRoot 'overlay.json'))
}
$server=$null;$oldAddr=$env:ROOST_REDIS_TEST_ADDR
$oldIT=$env:ROOST_DATAENGINE_IT;$oldITAddr=$env:ROOST_DATAENGINE_IT_REDIS_ADDR
Push-Location $RepoRoot
try{
 if($Mode -ne 'Red'){
  if(!$RedisExecutable -or !$RedisCliExecutable){throw 'Owned Redis executables are required'}
  $listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
  $listener.Start();$port=$listener.LocalEndpoint.Port;$listener.Stop()
  @('bind 127.0.0.1',"port $port",'save ""','appendonly no','dir .','logfile redis.log')|Set-Content (Join-Path $OutputRoot 'redis.conf') -Encoding utf8NoBOM
  & $RedisExecutable --version|Set-Content (Join-Path $OutputRoot 'redis-version.log') -Encoding utf8NoBOM
  $server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
  $ready=$false
  for($attempt=0;$attempt -lt 50;$attempt++){
   $server.Refresh();if($server.HasExited){throw 'Owned Redis exited before readiness'}
   $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
   if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG'){$ready=$true;break}
   Start-Sleep -Milliseconds 100
  }
  if(!$ready){throw 'Owned Redis did not become ready'}
  $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
  if($Mode -eq 'RemoteIntegration'){$env:ROOST_DATAENGINE_IT='1';$env:ROOST_DATAENGINE_IT_REDIS_ADDR=$env:ROOST_REDIS_TEST_ADDR}
 }
 & $GoExecutable test -race -count=1 -timeout=120s -json @overlayArgs @selectArgs @packages 2>&1|Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
 $testExit=$LASTEXITCODE;$vetExit=$null
 if($Mode -eq 'Green'){
  & $GoExecutable vet @packages 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
  $vetExit=$LASTEXITCODE
 }
 @{mode=$Mode;test_exit=$testExit;vet_exit=$vetExit;port=$port;owned_pid=if($server){$server.Id}else{$null}}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
 if($Mode -eq 'Green' -and ($testExit -ne 0 -or $vetExit -ne 0)){throw 'Green verification failed'}
 if($Mode -eq 'Red' -and $testExit -ne 1){throw 'Expected the recorded red behavior'}
 if($Mode -eq 'RemoteIntegration' -and $testExit -ne 0){throw 'Remote integration failed'}
 if($Mode -eq 'Green'){
  & (Join-Path $PSScriptRoot 'Run-Consumer.ps1') -GoExecutable $GoExecutable -RepoRoot $RepoRoot -OutputRoot (Join-Path $OutputRoot 'consumer')
 }
}finally{
 $env:ROOST_REDIS_TEST_ADDR=$oldAddr
 $env:ROOST_DATAENGINE_IT=$oldIT;$env:ROOST_DATAENGINE_IT_REDIS_ADDR=$oldITAddr
 if($server){$server.Refresh();if(!$server.HasExited){Stop-Process -Id $server.Id -ErrorAction Stop};@{owned_pid=$server.Id;exited=$server.WaitForExit(5000)}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM}
 Pop-Location
}
