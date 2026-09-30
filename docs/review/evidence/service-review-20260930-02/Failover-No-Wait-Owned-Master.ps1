param([string]$Prefix='{review13-ha-owner-1}', [string]$RunID='review13-ha-run-1')
$ErrorActionPreference = 'Stop'
$cli = 'C:/Users/tjbdw/scoop/shims/redis-cli.exe'
$key = "$($Prefix):run:$RunID"
$slot = [int](& $cli -h 127.0.0.1 -p 16631 CLUSTER KEYSLOT $key)
if ($LASTEXITCODE -ne 0) { throw 'cannot find key slot' }
$nodes = @(& $cli -h 127.0.0.1 -p 16631 CLUSTER NODES)
$master = $null
foreach ($line in $nodes) {
    $parts = $line -split ' '
    if ($parts[2] -notmatch 'master') { continue }
    foreach ($token in $parts | Select-Object -Skip 8) {
        if ($token -match '^(\d+)-(\d+)$' -and $slot -ge [int]$Matches[1] -and $slot -le [int]$Matches[2]) { $master = $parts; break }
    }
}
if (-not $master) { throw "no master for slot $slot" }
$replica = $null
foreach ($line in $nodes) {
    $parts = $line -split ' '
    if ($parts[2] -match 'slave' -and $parts[3] -eq $master[0]) { $replica = $parts; break }
}
if (-not $replica) { throw "no replica for slot $slot" }
$masterPort = [int](($master[1] -split '@')[0] -split ':')[-1]
$replicaPort = [int](($replica[1] -split '@')[0] -split ':')[-1]
foreach ($port in @($masterPort, $replicaPort)) {
    $expected = "/cygdrive/d/whb_s/.tmp/service-review13-redis/$port"
    $actual = @(& $cli -h 127.0.0.1 -p $port --raw CONFIG GET dir)
    if ($actual.Count -ne 2 -or $actual[1] -ne $expected) { throw "port $port is not an owned review13 Redis: $actual" }
}
$expectedMasterDir = "/cygdrive/d/whb_s/.tmp/service-review13-redis/$masterPort"
$servers = @(Get-CimInstance Win32_Process -Filter "name='redis-server.exe'" | Where-Object {
    $_.ExecutablePath -like '*scoop\shims\redis-server.exe' -and $_.CommandLine.Contains("$expectedMasterDir/redis.conf")
})
if ($servers.Count -ne 1) { throw "expected one owned master, got $($servers.Count)" }
$before = @(& $cli -h 127.0.0.1 -p 16631 CLUSTER INFO)
if (-not @($before | Where-Object { $_ -match '^cluster_state:ok' }).Count) { throw 'cluster unhealthy before fault' }
"slot=$slot master=$masterPort replica=$replicaPort killed_pid=$($servers[0].ProcessId) WAIT_not_called=true"
Stop-Process -Id $servers[0].ProcessId -Force
$healthy = $false
for ($i = 0; $i -lt 120; $i++) {
    Start-Sleep -Milliseconds 500
    $info = @(& $cli -h 127.0.0.1 -p $replicaPort CLUSTER INFO 2>$null)
    $role = @(& $cli -h 127.0.0.1 -p $replicaPort ROLE 2>$null)
    if (@($info | Where-Object { $_ -match '^cluster_state:ok' }).Count -and $role[0] -eq 'master') { $healthy = $true; break }
}
if (-not $healthy) { throw "replica $replicaPort did not become healthy master" }
"healthy_after_polls=$($i+1) role=$($role[0])"
@(& $cli -h 127.0.0.1 -p $replicaPort CLUSTER INFO) | Select-String 'cluster_state|cluster_slots_assigned|cluster_known_nodes|cluster_size'
