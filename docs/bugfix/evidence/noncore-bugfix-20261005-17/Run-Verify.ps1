param([string]$Scratch='D:/whb_s/.tmp/nc41-42-verify-new')
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Scratch){throw 'Use a new, isolated scratch directory'}
New-Item -ItemType Directory -Path $Scratch|Out-Null
$repo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../..'))
$env:GOWORK='off'
$checks=@()
Push-Location -LiteralPath $repo
try {
    foreach($check in @(
        @{name='race';args=@('test','-race','-count=1','-json','./saga','./kit/saga','./kit/service/account','./kit/service/chat','./kit/service/global/...','./servicemetrics','./kit/service/servicemetrics')},
        @{name='root';args=@('test','-count=1','-json','.')},
        @{name='build';args=@('build','./...')},
        @{name='vet';args=@('vet','./saga','./kit/saga','./kit/service/account','./kit/service/chat','./kit/service/global/...','./servicemetrics','./kit/service/servicemetrics')}
    )) {
        $argsForGo=$check.args
        & go @argsForGo > (Join-Path $Scratch ($check.name+'.jsonl')) 2>&1
        $code=$LASTEXITCODE
        $checks+=@{name=$check.name;command=('go '+($argsForGo -join ' '));exit=$code}
        $checks|ConvertTo-Json -Depth 4|Set-Content (Join-Path $Scratch 'checks.json')
        if($code -ne 0){throw "Check failed: $($check.name), exit $code"}
    }
} finally {Pop-Location}
