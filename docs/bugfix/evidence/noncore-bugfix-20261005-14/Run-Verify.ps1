param([string]$RepoRoot='D:/whb_s/cube-core', [string]$GoExe='go', [string]$Scratch=(Join-Path $env:TEMP ('roost-nc35-36-'+[guid]::NewGuid().ToString('N'))), [switch]$IncludeAppSingleton, [string]$ResultPrefix='')
$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force $Scratch | Out-Null
$evidence=$PSScriptRoot
$checks=@(
    @{name='race'; args=@('test','-race','-count=1','-json','./cache','./remoteentity','./sync/syncbus/...','./ownerroute','./kit/remoteentity','./entity')},
    @{name='root'; args=@('test','-count=1','-json','.')},
    @{name='build'; args=@('build','./...')},
    @{name='vet'; args=@('vet','./cache','./remoteentity','./sync/syncbus/...','./ownerroute','./kit/remoteentity','./entity')},
    @{name='glsvet'; args=@('run','./cmd/glsvet','./nest','./entity','./dataengine/engine','./sync/entitysync')}
)
if($IncludeAppSingleton) {
    foreach($check in $checks) {
        if($check.name -in @('race','vet')) { $check.args+=@('./app','./kit/nest','./kit/redis') }
    }
}
$results=@()
Push-Location $RepoRoot
try {
    $env:GOWORK='off'
    foreach($check in $checks) {
        $log=Join-Path $Scratch ($check.name+'.log')
        $goArgs=$check.args
        # 不将无输出等同于失败或成功：退出码独立保存，日志显式建立。
        [IO.File]::WriteAllText($log,'')
        & $GoExe @goArgs 2>&1 | Add-Content $log
        $checkExit=$LASTEXITCODE
        $passed=@{}; $skipped=@(); $failed=@()
        if($check.name -in @('race','root')) {
            Get-Content $log | ForEach-Object {
                try { $event=$_ | ConvertFrom-Json -ErrorAction Stop } catch { return }
                if($event.Test) {
                    $identity=$event.Package+'::'+$event.Test
                    if($event.Action -eq 'pass') { $passed[$identity]=$true }
                    if($event.Action -eq 'skip') { $skipped+=$identity }
                    if($event.Action -eq 'fail') { $failed+=$identity }
                }
            }
        }
        $leafCount=0
        foreach($testName in $passed.Keys) {
            $prefix=$testName+'/'
            if(!($passed.Keys | Where-Object { $_.StartsWith($prefix) } | Select-Object -First 1)) { $leafCount++ }
        }
        $result=[pscustomobject]@{check=$check.name;command='go '+($goArgs -join ' ');exit=$checkExit;passedTestEvents=$passed.Count;passedLeaves=$leafCount;skipped=$skipped;failed=$failed}
        $results+=$result
        $result | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $evidence ($ResultPrefix+$check.name+'-summary.json'))
        $result | ConvertTo-Json -Depth 5 -Compress
        if($checkExit -ne 0) { Get-Content $log -Tail 80; break }
    }
    $results | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $evidence ($ResultPrefix+'checks.json'))
    if($results.Count -ne $checks.Count -or ($results | Where-Object exit -ne 0)) { throw 'Verification failed; inspect checks.json and scratch logs.' }
} finally { Pop-Location }
