param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [Parameter(Mandatory=$true)][string]$RedisExecutable,
    [Parameter(Mandatory=$true)][string]$RedisCliExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
$listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
$listener.Start()
$port=$listener.LocalEndpoint.Port
$listener.Stop()
$config=Join-Path $OutputRoot 'redis.conf'
@("bind 127.0.0.1","port $port",'save ""','appendonly no','dir .','logfile redis.log') | Set-Content $config -Encoding utf8NoBOM
& $RedisExecutable --version | Set-Content (Join-Path $OutputRoot 'redis-version.log') -Encoding utf8NoBOM
$server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
$oldAddr=$env:ROOST_REDIS_TEST_ADDR
$cleanup=$false
Push-Location $RepoRoot
try {
    $ready=$false
    for($attempt=0;$attempt -lt 50;$attempt++) {
        $server.Refresh()
        if($server.HasExited) { throw 'Owned Redis exited before readiness' }
        $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
        if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG') {$ready=$true;break}
        Start-Sleep -Milliseconds 100
    }
    if(!$ready) { throw 'Owned Redis did not become ready' }
    $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
    & $GoExecutable test -race -count=1 -timeout=90s -json -run '^(TestCompareAndDeleteOnlyRemovesTheValueItWasShownIntegration|TestIndexIsMaintainedInTheSameWriteIntegration|TestIndexScorePrecisionSurvivesLuaIntegration|TestDistLockExpiryAndReacquireIntegration|TestMGet.*)$' ./redis ./redis/driver 2>&1 | Set-Content (Join-Path $OutputRoot 'integration.jsonl') -Encoding utf8NoBOM
    $integrationExit=$LASTEXITCODE
    $overlay=@{Replace=@{}}
    $overlay.Replace[(Join-Path $RepoRoot 'cache/review10_redis_test.go')]=Join-Path $PSScriptRoot 'cache_redis_review_test.go.txt'
    $overlay.Replace[(Join-Path $RepoRoot 'cache/review12_refhmap_test.go')]=Join-Path $PSScriptRoot 'refhmap_review_test.go.txt'
    $overlayPath=Join-Path $OutputRoot 'overlay.json'
    $overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
    & $GoExecutable test -race -overlay $overlayPath -count=1 -timeout=45s -json -run '^(TestReview10RedisStaleContract|TestReview12RefHMapRealRedis)$' ./cache 2>&1 | Set-Content (Join-Path $OutputRoot 'cache-redis.jsonl') -Encoding utf8NoBOM
    $reviewExit=$LASTEXITCODE
    @{integration_exit=$integrationExit;review_exit=$reviewExit;port=$port;pid=$server.Id} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if($integrationExit -ne 0) { throw 'Redis integration failed' }
} finally {
    $env:ROOST_REDIS_TEST_ADDR=$oldAddr
    $server.Refresh()
    if(!$server.HasExited) { Stop-Process -Id $server.Id -ErrorAction Stop }
    $cleanup=$server.WaitForExit(5000)
    @{owned_pid=$server.Id;exited=$cleanup} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM
    Pop-Location
}
