param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [Parameter(Mandatory=$true)][string]$RedisExecutable,
    [Parameter(Mandatory=$true)][string]$RedisCliExecutable,
    [ValidateSet('Red','Green')][string]$Mode='Green',
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot) {throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$overlayArgs=@()
if($Mode -eq 'Red') {
    $replace=@{}
    foreach($path in @('cache/ref_hmap.go','cache/ref_hmap_test.go','mongo/mongotest/mongotest.go')) {
        $dest=Join-Path $OutputRoot ($path.Replace('/','-'))
        $source=& git -C $RepoRoot -c "safe.directory=$RepoRoot" show "08d18be9608598c042a58b3658aa4735d88f8c7b:$path"
        if($LASTEXITCODE -ne 0) {throw 'Cannot recover red baseline'}
        $source | Set-Content $dest -Encoding utf8NoBOM
        $replace[(Join-Path $RepoRoot $path)]=$dest
    }
    $replace[(Join-Path $RepoRoot 'cache/ref_hmap_contracts_test.go')]=Join-Path $PSScriptRoot 'red-ref-hmap-test.go.txt'
    $replace[(Join-Path $RepoRoot 'mongo/mongotest/pagination_promises_test.go')]=Join-Path $PSScriptRoot 'red-pagination-test.go.txt'
    @{Replace=$replace} | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
    $overlayArgs=@('-overlay',(Join-Path $OutputRoot 'overlay.json'))
}
$listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
$listener.Start(); $port=$listener.LocalEndpoint.Port; $listener.Stop()
@('bind 127.0.0.1',"port $port",'save ""','appendonly no','dir .','logfile redis.log') | Set-Content (Join-Path $OutputRoot 'redis.conf') -Encoding utf8NoBOM
& $RedisExecutable --version | Set-Content (Join-Path $OutputRoot 'redis-version.log') -Encoding utf8NoBOM
$server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
$oldAddr=$env:ROOST_REDIS_TEST_ADDR
Push-Location $RepoRoot
try {
    $ready=$false
    for($attempt=0;$attempt -lt 50;$attempt++) {
        $server.Refresh()
        if($server.HasExited) {throw 'Owned Redis exited before readiness'}
        $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
        if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG') {$ready=$true;break}
        Start-Sleep -Milliseconds 100
    }
    if(!$ready) {throw 'Owned Redis did not become ready'}
    $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
    $packages=@('./cache','./mongo/mongotest')
    $selectArgs=@('-run','^(TestRefHMapContractsRealRedis|TestPaginationPromises)$')
    if($Mode -eq 'Green') {
        $packages=@('./cache','./redis','./redis/driver','./mongo','./mongo/driver','./mongo/mongotest','./migration','./codegen/internal/dao','./kit/redis','./kit/mongo')
        $selectArgs=@()
    }
    & $GoExecutable test -race -count=1 -timeout=120s -json @overlayArgs @selectArgs @packages 2>&1 | Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
    $testExit=$LASTEXITCODE
    $vetExit=$null
    if($Mode -eq 'Green') {
        & $GoExecutable vet @packages 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
        $vetExit=$LASTEXITCODE
    }
    @{mode=$Mode;test_exit=$testExit;vet_exit=$vetExit;port=$port;owned_pid=$server.Id} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if($Mode -eq 'Green' -and ($testExit -ne 0 -or $vetExit -ne 0)) {throw 'Green verification failed'}
    if($Mode -eq 'Green') {
        & (Join-Path $PSScriptRoot 'Run-Consumer.ps1') -GoExecutable $GoExecutable -RepoRoot $RepoRoot -OutputRoot (Join-Path $OutputRoot 'consumer')
    }
} finally {
    $env:ROOST_REDIS_TEST_ADDR=$oldAddr
    $server.Refresh()
    if(!$server.HasExited) {Stop-Process -Id $server.Id -ErrorAction Stop}
    @{owned_pid=$server.Id;exited=$server.WaitForExit(5000)} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM
    Pop-Location
}
