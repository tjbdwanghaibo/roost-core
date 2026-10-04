param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [Parameter(Mandatory=$true)][string]$RedisExecutable,
    [Parameter(Mandatory=$true)][string]$RedisCliExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'; $env:GOWORK='off'
if(Test-Path -LiteralPath $OutputRoot) {throw 'Choose a new OutputRoot'}
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
$listener.Start();$port=$listener.LocalEndpoint.Port;$listener.Stop()
@('bind 127.0.0.1',"port $port",'save ""','appendonly no','dir .','logfile redis.log') | Set-Content (Join-Path $OutputRoot 'redis.conf') -Encoding utf8NoBOM
& $RedisExecutable --version | Set-Content (Join-Path $OutputRoot 'redis-version.log') -Encoding utf8NoBOM
$server=Start-Process -FilePath $RedisExecutable -ArgumentList @('redis.conf') -WorkingDirectory $OutputRoot -PassThru -WindowStyle Hidden
$oldAddr=$env:ROOST_REDIS_TEST_ADDR
Push-Location $RepoRoot
try {
    $ready=$false
    for($attempt=0;$attempt -lt 50;$attempt++) {
        $server.Refresh();if($server.HasExited){throw 'Owned Redis exited'}
        $reply=& $RedisCliExecutable -h 127.0.0.1 -p $port ping 2>$null
        if($LASTEXITCODE -eq 0 -and $reply -eq 'PONG'){$ready=$true;break}
        Start-Sleep -Milliseconds 100
    }
    if(!$ready){throw 'Redis readiness failed'}
    $env:ROOST_REDIS_TEST_ADDR="127.0.0.1:$port"
    $replace=@{}
    $replace[(Join-Path $RepoRoot 'cache/review14_unknown_test.go')]=Join-Path $PSScriptRoot 'refhmap_unknown_test.go.txt'
    $replace[(Join-Path $RepoRoot 'mongo/mongotest/review14_contracts_test.go')]=Join-Path $PSScriptRoot 'mongotest_contracts_test.go.txt'
    @{Replace=$replace}|ConvertTo-Json -Depth 5|Set-Content (Join-Path $OutputRoot 'overlay.json') -Encoding utf8NoBOM
    & $GoExecutable test -race -count=1 -timeout=90s -json -overlay (Join-Path $OutputRoot 'overlay.json') -run '^(TestReview14UnknownRefHMapWrite|TestReview14MongoContracts)$' ./cache ./mongo/mongotest 2>&1 | Set-Content (Join-Path $OutputRoot 'review.jsonl') -Encoding utf8NoBOM
    @{review_exit=$LASTEXITCODE;port=$port;owned_pid=$server.Id}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
} finally {
    $env:ROOST_REDIS_TEST_ADDR=$oldAddr
    $server.Refresh();if(!$server.HasExited){Stop-Process -Id $server.Id -ErrorAction Stop}
    @{owned_pid=$server.Id;exited=$server.WaitForExit(5000)}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'cleanup.json') -Encoding utf8NoBOM
    Pop-Location
}
