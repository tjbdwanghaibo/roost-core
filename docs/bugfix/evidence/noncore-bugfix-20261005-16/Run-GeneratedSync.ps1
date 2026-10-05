param([string]$RepoRoot='D:/whb_s/cube-core', [string]$GoExe='go', [Parameter(Mandatory=$true)][string]$Scratch)
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Scratch) { throw 'Use a new scratch directory.' }
New-Item -ItemType Directory $Scratch | Out-Null
$scratchPath=(Resolve-Path $Scratch).Path
$repoPath=(Resolve-Path $RepoRoot).Path.Replace('\','/')
Copy-Item (Join-Path $RepoRoot 'codegen/internal/entity/testdata/syncmodes/*.go') $scratchPath
@"
module syncmodes

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => $repoPath
"@ | Set-Content (Join-Path $scratchPath 'go.mod')
$env:GOWORK='off'
$log=Join-Path $PSScriptRoot 'generated-sync.txt'
[IO.File]::WriteAllText($log,'')
Push-Location $RepoRoot
try {
    & $GoExe run ./codegen/cmd/dao -def ./codegen/internal/entity/testdata/syncmodes/def -out $scratchPath -pkg syncmodes -force 2>&1 | Tee-Object -FilePath $log -Append
    if($LASTEXITCODE -ne 0) { throw 'DAO generation failed.' }
    & $GoExe run ./codegen/cmd/entity -dir $scratchPath -force 2>&1 | Tee-Object -FilePath $log -Append
    if($LASTEXITCODE -ne 0) { throw 'Entity generation failed.' }
} finally { Pop-Location }
Push-Location $scratchPath
try {
    & $GoExe test -mod=mod -race -count=1 -v ./... 2>&1 | Tee-Object -FilePath $log -Append
    $testExit=$LASTEXITCODE
    "exit=$testExit" | Set-Content (Join-Path $PSScriptRoot 'generated-sync-exit.txt')
    if($testExit -ne 0) { throw 'Generated consumer test failed.' }
} finally { Pop-Location }
