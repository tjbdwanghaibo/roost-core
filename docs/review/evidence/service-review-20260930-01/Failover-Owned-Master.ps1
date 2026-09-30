param([string]$Prefix='{review12-ha}', [string]$RunID='review12-ha-run')
$ErrorActionPreference = 'Stop'
$cli = 'C:/Users/tjbdw/scoop/shims/redis-cli.exe'
$key = "$($Prefix):run:$RunID"
$slot = [int](& $cli -h 127.0.0.1 -p 16531 CLUSTER KEYSLOT $key)
if ($LASTEXITCODE -ne 0) { throw 'cannot find key slot' }
$nodes = @(& $cli -h 127.0.0.1 -p 16531 CLUSTER NODES)
$master = $null
foreach ($line in $nodes) {
    $parts = $line -split ' '
    if ($parts[2] -notmatch 'master') { continue }
    foreach ($token in $parts | Select-Object -Skip 8) {
        if ($token -match '^(\d+)-(\d+)$' -and $slot -ge [int]$Matches[1] -and $slot -le [int]$Matches[2]) { $master = $parts; break }
    }
}
if (-not $master) { throw "no owner for slot $slot" }
$replica = $null
foreach ($line in $nodes) {
    $parts = $line -split ' '
    if ($parts[2] -match 'slave' -and $parts[3] -eq $master[0]) { $replica = $parts; break }
}
if (-not $replica) { throw "expected one replica for slot $slot" }
$masterPort = [int](($master[1] -split '@')[0] -split ':')[-1]
$replicaPort = [int](($replica[1] -split '@')[0] -split ':')[-1]
$expectedDir = "/cygdrive/d/whb_s/.tmp/service-review12-redis/$masterPort"
$actualDir = @(& $cli -h 127.0.0.1 -p $masterPort --raw CONFIG GET dir)
if ($actualDir.Count -ne 2 -or $actualDir[1] -ne $expectedDir) { throw "master dir mismatch: $actualDir" }
$expectedReplicaDir = "/cygdrive/d/whb_s/.tmp/service-review12-redis/$replicaPort"
$actualReplicaDir = @(& $cli -h 127.0.0.1 -p $replicaPort --raw CONFIG GET dir)
if ($actualReplicaDir.Count -ne 2 -or $actualReplicaDir[1] -ne $expectedReplicaDir) { throw "replica dir mismatch: $actualReplicaDir" }
$waitReply = & $cli -h 127.0.0.1 -p $masterPort WAIT 1 5000
if ($LASTEXITCODE -ne 0 -or [int]$waitReply -lt 1) { throw "seed writes not replicated: $waitReply" }
$processes = @(Get-CimInstance Win32_Process -Filter "name='redis-server.exe'" | Where-Object {
    $_.ExecutablePath -like '*scoop\apps\redis\current\redis-server.exe' -and
    $_.CommandLine.Contains("$expectedDir/redis.conf")
})
if ($processes.Count -ne 1) { throw "expected one owned master server process, got $($processes.Count)" }
$before = @(& $cli -h 127.0.0.1 -p 16531 CLUSTER INFO)
if (-not @($before | Where-Object { $_ -match '^cluster_state:ok' }).Count) { throw 'cluster unhealthy before fault' }
"slot=$slot master=$masterPort replica=$replicaPort waited=$waitReply killed_pid=$($processes[0].ProcessId)"
Stop-Process -Id $processes[0].ProcessId -Force
$healthy = $false
for ($i = 0; $i -lt 120; $i++) {
    Start-Sleep -Milliseconds 500
    $info = @(& $cli -h 127.0.0.1 -p $replicaPort CLUSTER INFO 2>$null)
    $role = @(& $cli -h 127.0.0.1 -p $replicaPort ROLE 2>$null)
    if (@($info | Where-Object { $_ -match '^cluster_state:ok' }).Count -and $role[0] -eq 'master') { $healthy = $true; break }
}
if (-not $healthy) { throw "replica $replicaPort did not become healthy master" }
$socket = [Net.Sockets.TcpClient]::new()
try { $socket.Connect('127.0.0.1', $masterPort); throw "killed master $masterPort is still listening" }
catch [Net.Sockets.SocketException] {} finally { $socket.Dispose() }
"failover healthy after $($i+1) polls; role=$($role[0]); old master port closed"
@(& $cli -h 127.0.0.1 -p $replicaPort CLUSTER INFO) | Select-String 'cluster_state|cluster_slots_assigned|cluster_known_nodes|cluster_size'
