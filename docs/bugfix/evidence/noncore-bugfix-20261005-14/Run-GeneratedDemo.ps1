param([string]$RepoRoot='D:/whb_s/cube-core', [string]$GoExe='go', [Parameter(Mandatory=$true)][string]$Scratch, [string]$GenprotoVersion='')
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Scratch) { throw 'Use a new scratch directory.' }
New-Item -ItemType Directory $Scratch | Out-Null
$repoPath=(Resolve-Path $RepoRoot).Path.Replace('\','/')
$output=Join-Path $Scratch 'nc24demo'
$env:GOWORK='off'
Push-Location $RepoRoot
try {
    & $GoExe run ./codegen/cmd/roost project new nc24demo -module example.com/nc24demo -out $output -template game-demo -skip-deps 2>&1 | Set-Content (Join-Path $Scratch 'generate.log')
    if($LASTEXITCODE -ne 0) { throw 'Game-demo generation failed.' }
} finally { Pop-Location }
Push-Location $output
try {
    & $GoExe mod edit "-replace=github.com/tjbdwanghaibo/roost-core=$repoPath"
    if($LASTEXITCODE -ne 0) { throw 'Local core replacement failed.' }
    # 历史B35的依赖整理须显式选择；缺省保留原始生成依赖，不能悄悄掩盖失败。
    if($GenprotoVersion) {
        & $GoExe get "google.golang.org/genproto@$GenprotoVersion" 2>&1 | Set-Content (Join-Path $Scratch 'deps-recovery.log')
        if($LASTEXITCODE -ne 0) { throw 'Explicit genproto dependency alignment failed.' }
    }
    $checks=@(
        @{name='consumer';args=@('test','-mod=mod','-race','-count=1','-json','./internal/service/game/...','./game/controllers/player/...')},
        @{name='build';args=@('build','-mod=mod','./...')},
        @{name='vet';args=@('vet','-mod=mod','./...')}
    )
    $results=@()
    foreach($check in $checks) {
        $log=Join-Path $Scratch ($check.name+'.log')
        [IO.File]::WriteAllText($log,'')
        $goArgs=$check.args
        & $GoExe @goArgs 2>&1 | Add-Content $log
        $checkExit=$LASTEXITCODE
        $passed=@{}; $skips=@(); $failed=@()
        if($check.name -eq 'consumer') {
            foreach($line in Get-Content $log) {
                try { $event=$line | ConvertFrom-Json -ErrorAction Stop } catch { continue }
                if(!$event.Test) { continue }
                $id=$event.Package+'::'+$event.Test
                if($event.Action -eq 'pass') { $passed[$id]=$true }
                if($event.Action -eq 'skip') { $skips+=$id }
                if($event.Action -eq 'fail') { $failed+=$id }
            }
        }
        $leaves=@($passed.Keys | Where-Object { $prefix=$_+'/'; !($passed.Keys | Where-Object { $_.StartsWith($prefix) } | Select-Object -First 1) }).Count
        $result=[pscustomobject]@{name=$check.name;command='go '+($goArgs -join ' ');exit=$checkExit;passedLeaves=$leaves;passedEvents=$passed.Count;skipped=$skips;failed=$failed}
        $results+=$result
        $result | ConvertTo-Json -Depth 6 -Compress
        $results | ConvertTo-Json -Depth 6 | Set-Content (Join-Path $PSScriptRoot 'generated-demo-checks.json')
        if($checkExit -ne 0) { Get-Content $log -Tail 60; throw 'Generated demo verification failed.' }
    }
} finally { Pop-Location }
