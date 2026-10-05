param([string]$RepoRoot='D:/whb_s/cube-core',[string]$GoExe='go',[string]$Baseline='cb11be90',[string]$Scratch=(Join-Path $env:TEMP ('roost-nc39-40-red-'+[guid]::NewGuid().ToString('N'))))
$ErrorActionPreference='Stop'
New-Item -ItemType Directory -Force $Scratch | Out-Null
Push-Location $RepoRoot
try {
    $replacement=@{}
    foreach($path in @('saga/engine.go','saga/record.go','saga/mongo_store.go','saga/nest_completion_consumer.go')) {
        $original=& git show ($Baseline+':'+$path)
        if($LASTEXITCODE -ne 0){throw "Cannot read baseline $path"}
        $target=Join-Path $Scratch ([IO.Path]::GetFileName($path))
        [IO.File]::WriteAllText($target,(($original -join "`n")+"`n"),[Text.UTF8Encoding]::new($false))
        $replacement[(Join-Path $RepoRoot $path)]=$target
    }
    # 两个新增控制文件引用新 Record 字段，旧实现复现只编译原 API 反例，不把编译失败算红。
    $empty=Join-Path $Scratch 'empty_test.go'
    [IO.File]::WriteAllText($empty,"package saga`n",[Text.UTF8Encoding]::new($false))
    foreach($path in @('saga/start_identity_compatibility_promises_test.go','saga/transaction_cancel_review_test.go')) {
        $replacement[(Join-Path $RepoRoot $path)]=$empty
    }
    $overlay=Join-Path $Scratch 'overlay.json'
    @{Replace=$replacement}|ConvertTo-Json -Depth 4|Set-Content $overlay
    $env:GOWORK='off'
    $log=Join-Path $Scratch 'overlay-red.jsonl'
    & $GoExe test -overlay $overlay -count=1 -json -run 'TestStartIdentitySurvivesProgressAndResume|TestNestCompletionRejectsForeignSagaRouteBeforeMutation' ./saga 2>&1|Set-Content $log
    [pscustomobject]@{baseline=$Baseline;exit=$LASTEXITCODE;log=$log;overlay=$overlay}|ConvertTo-Json|Set-Content (Join-Path $PSScriptRoot 'overlay-result.json')
} finally {Pop-Location}
