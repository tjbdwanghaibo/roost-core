param([string]$RepoRoot='D:/whb_s/cube-core',[string]$GoExe='go',[string]$Scratch=(Join-Path $env:TEMP ('roost-nc39-40-'+[guid]::NewGuid().ToString('N'))))
$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force $Scratch | Out-Null
$packages=@('./saga','./kit/saga','./dataengine/engine','./kit/dataengine','./nats','./nats/driver','./servicemetrics','./kit/service/servicemetrics','./mongo/mongotest')
$checks=@(
    @{name='race';args=@('test','-race','-count=1','-json')+$packages},
    @{name='root';args=@('test','-count=1','-json','.')},
    @{name='build';args=@('build','./...')},
    @{name='vet';args=@('vet')+$packages},
    @{name='glsvet';args=@('run','./cmd/glsvet','./nest','./entity','./dataengine/engine','./sync/entitysync')}
)
$results=@()
Push-Location $RepoRoot
try {
    $env:GOWORK='off'
    foreach($check in $checks) {
        $log=Join-Path $Scratch ($check.name+'.jsonl')
        $goArgs=$check.args
        & $GoExe @goArgs 2>&1 | Set-Content $log
        $result=[pscustomobject]@{check=$check.name;command='go '+($goArgs -join ' ');exit=$LASTEXITCODE;log=$log}
        $results+=$result
        $result | ConvertTo-Json -Compress
        $results | ConvertTo-Json -Depth 4 | Set-Content (Join-Path $PSScriptRoot 'checks.json')
        if($result.exit -ne 0) { Get-Content $log -Tail 50; throw ('Verification failed: '+$check.name) }
    }
} finally {Pop-Location}
