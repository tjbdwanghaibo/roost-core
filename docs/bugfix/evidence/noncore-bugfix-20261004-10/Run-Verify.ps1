param(
 [Parameter(Mandatory=$true)][string]$GoExecutable,
 [Parameter(Mandatory=$true)][string]$RedisExecutable,
 [Parameter(Mandatory=$true)][string]$RedisCliExecutable,
 [ValidateSet('Red','Green','Review','Consumer')][string]$Mode='Review',
 [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
 [Parameter(Mandatory=$true)][string]$OutputRoot,
 [string]$ReviewSource=(Join-Path $PSScriptRoot '../../../review/evidence/noncore-review-20261004-19/review19_merged_test.go.txt')
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot){throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$packages=@('./cache');$select=@();$overlay=@()
if($Mode -eq 'Red'){
 $select=@('-run','^TestRefHMapSchemaRegistryPromises/(set_registry_changed|delete_registry_changed|set_sequential_cleanup|delete_sequential_cleanup|same_layout_last_writer|registry_read_error)$')
 $replace=@{}
 foreach($path in @('cache/ref_hmap.go','cache/ref_hmap_test.go')){
  $oldSource=& git -C $RepoRoot -c "safe.directory=$RepoRoot" show "e7027a65c8a3b7649837a61c50a29d999ba32636:$path"
  if($LASTEXITCODE -ne 0){throw 'Cannot recover red baseline'}
  $dest=Join-Path $OutputRoot ([IO.Path]::GetFileName($path));$oldSource|Set-Content $dest -Encoding utf8NoBOM
  $replace[(Join-Path $RepoRoot $path)]=$dest
 }
 $replace[(Join-Path $RepoRoot 'cache/ref_hmap_schema_promises_test.go')]=Join-Path $PSScriptRoot 'red_test.go.txt'
 @{Replace=$replace}|ConvertTo-Json -Depth 4|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
 $overlay=@('-overlay',(Join-Path $OutputRoot 'overlay.json'))
}
if($Mode -eq 'Review'){
 @{Replace=@{(Join-Path $RepoRoot 'cache/review19_test.go')=$ReviewSource}}|ConvertTo-Json -Depth 4|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
 $overlay=@('-overlay',(Join-Path $OutputRoot 'overlay.json'));$select=@('-run','^TestReview19RefHMap$')
}
if($Mode -eq 'Green'){$packages=@('./cache','./redis','./redis/driver','./migration','./codegen/internal/dao','./kit/redis')}
$old=$env:ROOST_REDIS_TEST_ADDR;$server=$null
Push-Location $RepoRoot
try{
 $listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0);$listener.Start();$port=$listener.LocalEndpoint.Port;$listener.Stop()
 @('bind 127.0.0.1',"port $port",'save ""','appendonly no','dir .','logfile redis.log')|Set-Content (Join-Path $OutputRoot 'redis.conf') -Encoding utf8NoBOM
 & $RedisExecutable --version|Set-Content (Join-Path $OutputRoot 'redis-version.log') -Encoding utf8NoBOM
 $server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
 $ready=$false
 for($attempt=0;$attempt -lt 50;$attempt++){
  $server.Refresh();if($server.HasExited){throw 'Owned Redis exited before readiness'}
  $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
  if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG'){$ready=$true;break};Start-Sleep -Milliseconds 100
 }
 if(!$ready){throw 'Owned Redis not ready'}
 $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
 if($Mode -eq 'Consumer'){
  & (Join-Path $PSScriptRoot 'Run-Consumer.ps1') -GoExecutable $GoExecutable -RepoRoot $RepoRoot -OutputRoot (Join-Path $OutputRoot 'consumer')
  $consumerExit=Get-Content (Join-Path $OutputRoot 'consumer/exits.json') -Raw|ConvertFrom-Json
  $testExit=$consumerExit.test_exit;$vetExit=$consumerExit.vet_exit
 }else{
  & $GoExecutable test -race -count=1 -timeout=120s -json @overlay @select @packages 2>&1|Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
  $testExit=$LASTEXITCODE;$vetExit=$null
 }
 if($Mode -eq 'Green'){& $GoExecutable vet @packages 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM;$vetExit=$LASTEXITCODE}
 @{mode=$Mode;test_exit=$testExit;vet_exit=$vetExit;port=$port;owned_pid=$server.Id}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
 if($Mode -eq 'Red' -and $testExit -ne 1){throw 'Expected behavior red'}
 if($Mode -ne 'Red' -and ($testExit -ne 0 -or ($Mode -eq 'Green' -and $vetExit -ne 0))){throw 'Verification failed'}
}finally{
 $env:ROOST_REDIS_TEST_ADDR=$old
 if($server){$server.Refresh();if(!$server.HasExited){Stop-Process -Id $server.Id -ErrorAction Stop};@{owned_pid=$server.Id;exited=$server.WaitForExit(5000)}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM}
 Pop-Location
}
