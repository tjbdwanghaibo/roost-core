param([string]$Scratch='D:/whb_s/.tmp/nc41-42-repro-new')
$ErrorActionPreference='Stop'
$repo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../..'))
if(Test-Path -LiteralPath $Scratch){throw 'Use a new, isolated scratch directory'}
New-Item -ItemType Directory -Path $Scratch|Out-Null
$replace=@{}
foreach($path in @('saga/mongo_store.go','kit/service/global/activity/service.go')) {
    $original=Join-Path $Scratch ($path.Replace('/','_'))
    $lines=git -C $repo show ('1272671557c57d6abc97b99471335e3fe9c3f299:'+$path)
    if($LASTEXITCODE -ne 0){throw "Cannot read baseline: $path"}
    [IO.File]::WriteAllText($original,($lines -join "`n")+"`n",[Text.UTF8Encoding]::new($false))
    $replace[(Join-Path $repo $path)]=$original
}
$overlay=Join-Path $Scratch 'overlay.json'
@{Replace=$replace}|ConvertTo-Json -Depth 4|Set-Content $overlay
$env:GOWORK='off'
Push-Location -LiteralPath $repo
try {
    go test -overlay $overlay -count=1 -json -run 'TestOutboxClaimRechecksDueAfterAnotherPublisher|TestOpenActivityRejectsMalformedPersistedIntentBeforeCreate|TestOpeningSweepRejectsInvalidOrForeignGroupBeforeSideEffects' ./saga ./kit/service/global/activity > (Join-Path $Scratch 'overlay-red.jsonl')
    $result=$LASTEXITCODE
    @{baseline='1272671557c57d6abc97b99471335e3fe9c3f299';exit=$result;overlay=$overlay}|ConvertTo-Json|Set-Content (Join-Path $Scratch 'result.json')
} finally {Pop-Location}
exit $result
