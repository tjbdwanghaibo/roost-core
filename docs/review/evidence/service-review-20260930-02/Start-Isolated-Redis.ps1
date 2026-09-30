$ErrorActionPreference = 'Stop'
$root = 'D:/whb_s/.tmp/service-review13-redis'
$cli = 'C:/Users/tjbdw/scoop/shims/redis-cli.exe'
$server = 'C:/Users/tjbdw/scoop/shims/redis-server.exe'
$ports = @(16630, 16631, 16632, 16633, 16634, 16635, 16636)
$utf8 = [Text.UTF8Encoding]::new($false)
foreach ($port in $ports) {
    $socket = [Net.Sockets.TcpClient]::new()
    try { $socket.Connect('127.0.0.1', $port); throw "port $port already in use" }
    catch [Net.Sockets.SocketException] {} finally { $socket.Dispose() }
    $dir = Join-Path $root "$port"
    [IO.Directory]::CreateDirectory($dir) | Out-Null
    $cygwinDir = "/cygdrive/d/whb_s/.tmp/service-review13-redis/$port"
    $config = "port $port`nbind 127.0.0.1`nprotected-mode yes`ndir $cygwinDir`nsave `"`"`nappendonly yes`nappendfsync everysec`n"
    if ($port -ne 16630) { $config += "cluster-enabled yes`ncluster-config-file nodes.conf`ncluster-node-timeout 2000`ncluster-announce-ip 127.0.0.1`n" }
    $file = Join-Path $dir 'redis.conf'
    [IO.File]::WriteAllText($file, $config, $utf8)
    $process = Start-Process -FilePath $server -ArgumentList "$cygwinDir/redis.conf" -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $dir 'stdout.log') -RedirectStandardError (Join-Path $dir 'stderr.log')
    $ready = $false
    for ($n = 0; $n -lt 80; $n++) {
        Start-Sleep -Milliseconds 250
        $reply = & $cli -h 127.0.0.1 -p $port PING 2>$null
        if ($LASTEXITCODE -eq 0 -and $reply -eq 'PONG') { $ready = $true; break }
    }
    if (-not $ready) { throw "Redis $port did not start; see $dir/stderr.log" }
    $observed = @(& $cli -h 127.0.0.1 -p $port --raw CONFIG GET dir)
    if ($observed.Count -ne 2 -or $observed[1] -ne $cygwinDir) { throw "Redis $port has wrong dir: $observed" }
    "ready $port pid=$($process.Id) dir=$cygwinDir"
}
$addresses = @(16631..16636 | ForEach-Object { "127.0.0.1:$_" })
$out = @(& $cli --cluster create @addresses --cluster-replicas 1 --cluster-yes 2>&1)
if ($LASTEXITCODE -ne 0) { $out; throw 'cluster create failed' }
$out | Select-Object -Last 12
$info = & $cli -h 127.0.0.1 -p 16631 CLUSTER INFO
$info | Select-String 'cluster_state|cluster_slots_assigned|cluster_size|cluster_known_nodes'
