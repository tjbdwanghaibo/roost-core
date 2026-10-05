param([string]$RepoRoot='D:/whb_s/cube-core',[string]$GoExe='go',[string]$Baseline='af2f67fbb75e34110f59e4603e0046eedf7e358d',[string]$Scratch=(Join-Path $env:TEMP ('roost-nc37-38-red-'+[guid]::NewGuid().ToString('N'))))
$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force $Scratch | Out-Null
Push-Location $RepoRoot
try {
    $replacement=@{}
    foreach($relative in @('saga/assembly.go','saga/mongo_store.go')) {
        $original=& git show ($Baseline+':'+$relative)
        if($LASTEXITCODE -ne 0){throw "Cannot read baseline $relative"}
        $target=Join-Path $Scratch ([IO.Path]::GetFileName($relative))
        [IO.File]::WriteAllText($target,(($original -join "`n")+"`n"),[Text.UTF8Encoding]::new($false))
        $replacement[(Join-Path $RepoRoot $relative)]=$target
    }
    $overlay=Join-Path $Scratch 'overlay.json'
    @{Replace=$replacement}|ConvertTo-Json -Depth 5|Set-Content $overlay
    $env:GOWORK='off'
    $log=Join-Path $Scratch 'overlay-red.jsonl'
    & $GoExe test -overlay $overlay -count=1 -json -run 'TestAssemblyHealthIncludesEveryRequiredConsumer|TestAssemblyNativeConsumerClosureIsVisibleAfterFormalStart|TestSagaModHealthDetectsEveryConsumerAndRecovers|TestMongoResumePersistsGenerationAndAcceptsFreshCompletion' ./saga ./kit/saga 2>&1 | Set-Content $log
    $reproExit=$LASTEXITCODE
    [pscustomobject]@{baseline=$Baseline;exit=$reproExit;log=$log;overlay=$overlay}|ConvertTo-Json|Set-Content (Join-Path $PSScriptRoot 'overlay-result.json')
    $reproExit
    # 这里保留预期红，不把编译失败或超时当作行为失败；需另核对叶子与失败文本。
} finally {Pop-Location}
