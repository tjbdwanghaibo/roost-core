$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..\..')).Path
$base = Join-Path ([System.IO.Path]::GetTempPath()) ('roost-codegen-review03-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $base | Out-Null
if (-not $env:GOCACHE) {
    $env:GOCACHE = Join-Path $base 'go-cache'
}
$env:GOTMPDIR = Join-Path $base 'go-tmp'
New-Item -ItemType Directory -Force -Path $env:GOTMPDIR | Out-Null
Write-Output "FIXTURE=$base"
function Invoke-Go {
    param([string[]]$Arguments)
    & go @Arguments | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') exited $LASTEXITCODE"
    }
}
Push-Location $repo
try {
    $att = Join-Path $base 'attribute'
    New-Item -ItemType Directory -Force -Path $att | Out-Null
    $attSource = Join-Path $att 'profile.go'
    @'
package attribute
//roost:attribute index=1 max=4
type PlayerProfile struct { HP int64; dirtyMask uint64 }
'@ | Set-Content -LiteralPath $attSource
    Invoke-Go -Arguments @('run', './codegen/cmd/attribute', '-dir', $att)
    $attOutput = Join-Path $att 'gen_player_profile_attribute.go'
    (Get-Content -LiteralPath $attSource -Raw).Replace('roost:attribute', 'retired attribute') | Set-Content -LiteralPath $attSource
    Invoke-Go -Arguments @('run', './codegen/cmd/attribute', '-dir', $att)
    Write-Output "ATTRIBUTE_OLD_EXISTS=$(Test-Path -LiteralPath $attOutput)"

    $evt = Join-Path $base 'event'
    $defs = Join-Path $evt 'def'
    $out = Join-Path $evt 'out'
    New-Item -ItemType Directory -Force -Path $defs, $out | Out-Null
    $evtSource = Join-Path $defs 'events.go'
    "package def`ntype EventPing struct { ID int64 }`n" | Set-Content -LiteralPath $evtSource
    Invoke-Go -Arguments @('run', './codegen/cmd/eventgen', '-def', $defs, '-out', $out, '-eventpkg', 'example.com/game/event')
    "package def`n" | Set-Content -LiteralPath $evtSource
    Invoke-Go -Arguments @('run', './codegen/cmd/eventgen', '-def', $defs, '-out', $out, '-eventpkg', 'example.com/game/event')
    $old = @(Get-ChildItem -LiteralPath $out -Filter '*_gen.go' -File)
    Write-Output "EVENT_OLD_COUNT=$($old.Count)"

    $web = Join-Path $base 'webroute'
    New-Item -ItemType Directory -Force -Path $web | Out-Null
    $webSource = Join-Path $web 'handler.go'
    @'
package web
import "context"
type Service struct{}
type Request struct { Name string }
type Response struct { Name string }
//roost:web method=POST path=/ping body=json
func handlePing(ctx context.Context, svc *Service, request Request) (Response,error) { return Response{},nil }
'@ | Set-Content -LiteralPath $webSource
    Invoke-Go -Arguments @('run', './codegen/cmd/webroute', '-dir', $web)
    (Get-Content -LiteralPath $webSource -Raw).Replace('roost:web', 'retired web') | Set-Content -LiteralPath $webSource
    Invoke-Go -Arguments @('run', './codegen/cmd/webroute', '-dir', $web)
    $webOld = @(Get-ChildItem -LiteralPath $web -Filter '*web*gen.go' -File)
    Write-Output "WEB_OLD=$($webOld.Name -join ',')"

    $table = Join-Path $base 'tablegen'
    $schema = Join-Path $table 'configs\schema'
    $csvDir = Join-Path $table 'configs\table'
    $jsonDir = Join-Path $table 'configs\data'
    New-Item -ItemType Directory -Force -Path $schema, $csvDir, $jsonDir | Out-Null
    "module example.com/game`n" | Set-Content -LiteralPath (Join-Path $table 'go.mod')
    $tableSource = Join-Path $schema 'monster.go'
    @'
package schema
//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct { ID int32 `csv:"id" json:"id"` }
'@ | Set-Content -LiteralPath $tableSource
    "id`n1`n" | Set-Content -LiteralPath (Join-Path $csvDir 'monster.csv')
    Invoke-Go -Arguments @('run', './codegen/cmd/tablegen', '-meta', $schema, '-csv', $csvDir, '-json', $jsonDir, '-force')
    "package schema`n" | Set-Content -LiteralPath $tableSource
    Invoke-Go -Arguments @('run', './codegen/cmd/tablegen', '-meta', $schema, '-csv', $csvDir, '-json', $jsonDir, '-force')
    Write-Output "TABLE_JSON_OLD_EXISTS=$(Test-Path -LiteralPath (Join-Path $jsonDir 'monster.json'))"

    $errorsDir = Join-Path $base 'errcode'
    New-Item -ItemType Directory -Force -Path $errorsDir | Out-Null
    @'
package game
// retired: var ErrGhost = errcode.Define(500999, "ghost", "removed")
'@ | Set-Content -LiteralPath (Join-Path $errorsDir 'errors.go')
    $errorsCSV = Join-Path $errorsDir 'errcode.csv'
    Invoke-Go -Arguments @('run', './codegen/cmd/errcode', '-root', $errorsDir, '-out', $errorsCSV)
    $ghost = Select-String -LiteralPath $errorsCSV -Pattern '500999'
    Write-Output "ERRCODE_COMMENT_GHOST=$($null -ne $ghost)"
}
finally {
    Pop-Location
}
