param(
    [string]$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$GoExe = 'go',
    [string]$WorkRoot = (Join-Path ([IO.Path]::GetTempPath()) ('roost-codegen-review-05-' + [Guid]::NewGuid().ToString('N')))
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$utf8 = [Text.UTF8Encoding]::new($false)
function Write-Utf8([string]$Path, [string]$Body) {
    [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($Path)) | Out-Null
    [IO.File]::WriteAllText($Path, $Body, $utf8)
}
function Run-Go([string]$Directory, [string[]]$GoArgs) {
    Push-Location $Directory
    try {
        $lines = @(& $GoExe @GoArgs 2>&1)
        return @{ exit = $LASTEXITCODE; output = ($lines -join "`n") }
    } finally { Pop-Location }
}
[IO.Directory]::CreateDirectory($WorkRoot) | Out-Null
$oldGoWork = $env:GOWORK
$env:GOWORK = 'off'
try {
    $head = (& git -c "safe.directory=$($RepoRoot.Replace('\','/'))" -C $RepoRoot rev-parse HEAD).Trim()
    $binary = Join-Path $WorkRoot 'cfggen.exe'
    $build = Run-Go $RepoRoot @('build', '-o', $binary, './codegen/cmd/cfggen')
    Write-Utf8 (Join-Path $WorkRoot 'build.log') $build.output
    if ($build.exit -ne 0) { throw "cfggen build failed: $($build.output)" }
    $base = @'
package: cfg
tables:
  - name: monster
    key: id
    fields:
      - { name: id, type: int32 }
      - { name: camp, type: int32, index: false }
'@
    $namespace = @'
package: cfg
beans:
  - name: RegisterConfigData
    fields:
      - { name: value, type: int32 }
tables:
  - name: monster
    key: id
    fields:
      - { name: id, type: int32 }
'@
    $complex = @'
package: cfg
groups: { names: [c, s], target: [s] }
beans:
  - name: Reward
    fields:
      - { name: value, type: uint64 }
      - { name: icon, type: string, group: c }
  - name: Bundle
    fields:
      - { name: reward, type: Reward }
      - { name: children, type: "[]Bundle" }
tables:
  - name: monster
    key: id
    fields:
      - { name: id, type: int32 }
      - { name: camp, type: int32, index: true }
      - { name: flag, type: bool, index: true }
      - { name: name, type: string, index: true }
      - { name: bundle, type: Bundle }
      - { name: model, type: string, group: c }
  - name: ui
    key: id
    group: c
    fields:
      - { name: id, type: int32 }
globals:
  - name: world
    fields:
      - { name: width, type: int32 }
'@
    $cases = @(
        @{ name = 'RR12-index-false'; meta = $base; diagnostic = '"strconv" imported and not used' },
        @{ name = 'RR13-register-name'; meta = $namespace; diagnostic = 'RegisterConfigData redeclared' },
        @{ name = 'RR13-import-name'; meta = $namespace.Replace('name: RegisterConfigData', 'name: configdata'); diagnostic = 'configdata already declared' },
        @{ name = 'control-index-true'; meta = $base.Replace('index: false', 'index: true'); diagnostic = '' },
        @{ name = 'control-index-absent'; meta = $base.Replace(', index: false', ''); diagnostic = '' },
        @{ name = 'control-complex-server'; meta = $complex; diagnostic = '' }
    )
    $results = @()
    foreach ($case in $cases) {
        $root = Join-Path $WorkRoot $case.name
        Write-Utf8 (Join-Path $root 'schema.yaml') $case.meta
        Write-Utf8 (Join-Path $root 'go.mod') "module example.com/reviewconsumer`n`ngo 1.27.0`n`nrequire github.com/tjbdwanghaibo/roost-core v1.18.0`n`nreplace github.com/tjbdwanghaibo/roost-core => $($RepoRoot.Replace('\','/'))`n"
        $cli = @(& $binary '-meta' (Join-Path $root 'schema.yaml') '-out' (Join-Path $root 'cfg') 2>&1)
        $cliExit = $LASTEXITCODE
        Write-Utf8 (Join-Path $root 'generation.log') ($cli -join "`n")
        if ($cliExit -ne 0) { throw "generation unexpectedly failed for $($case.name): $cli" }
        $tidy = Run-Go $root @('mod', 'tidy')
        Write-Utf8 (Join-Path $root 'tidy.log') $tidy.output
        if ($tidy.exit -ne 0) { throw "dependency setup failed: $($tidy.output)" }
        $compile = Run-Go $root @('test', './...', '-count=1')
        Write-Utf8 (Join-Path $root 'compile.log') $compile.output
        if ($case.diagnostic) {
            if ($compile.exit -eq 0 -or !$compile.output.Contains($case.diagnostic)) { throw "expected red evidence missing for $($case.name): $($compile.output)" }
        } elseif ($compile.exit -ne 0) { throw "control failed: $($case.name): $($compile.output)" }
        if ($case.name -eq 'control-complex-server') {
            $generated = [IO.File]::ReadAllText((Join-Path $root 'cfg/cfg_gen.go'))
            if ($generated.Contains('type UiCfg') -or $generated.Contains('json:"model"') -or $generated.Contains('json:"icon"')) { throw 'server group filtering failed' }
        }
        $results += @{ name = $case.name; generation_exit = $cliExit; compile_exit = $compile.exit; output = $compile.output }
    }
    $virtual = Join-Path $RepoRoot 'codegen/internal/roost/review_codegen_05_test.go'
    $overlay = @{ Replace = @{ $virtual = (Join-Path $PSScriptRoot 'dependencies_test.go.txt') } }
    $overlayPath = Join-Path $WorkRoot 'overlay.json'
    Write-Utf8 $overlayPath ($overlay | ConvertTo-Json -Depth 5)
    $deps = Run-Go $RepoRoot @('test', '-overlay', $overlayPath, './codegen/internal/roost', '-run', '^TestReviewRR14DependencyConsolidationCommitsRelatedFiles$', '-v', '-count=1')
    Write-Utf8 (Join-Path $WorkRoot 'RR14.log') $deps.output
    if ($deps.exit -eq 0 -or !$deps.output.Contains('RR14: reported success') -or !$deps.output.Contains('RR14: consolidation dropped legacy policies')) { throw "RR14 not reproduced: $($deps.output)" }
    $failure = Run-Go $RepoRoot @('test', '-overlay', $overlayPath, './codegen/internal/roost', '-run', '^TestReviewDependencyConsolidationFailurePreservesRoot$', '-v', '-count=1')
    Write-Utf8 (Join-Path $WorkRoot 'failure-control.log') $failure.output
    if ($failure.exit -ne 0) { throw "dependency failure control failed: $($failure.output)" }
    $results += @{ name = 'RR14-consolidation'; exit = $deps.exit; output = $deps.output }
    $results += @{ name = 'control-resolver-failure'; exit = $failure.exit; output = $failure.output }
    Write-Utf8 (Join-Path $WorkRoot 'RESULTS.json') (@{ source_head = $head; cases = $results } | ConvertTo-Json -Depth 6)
    Write-Output "Review evidence complete: $WorkRoot/RESULTS.json"
} finally { $env:GOWORK = $oldGoWork }
