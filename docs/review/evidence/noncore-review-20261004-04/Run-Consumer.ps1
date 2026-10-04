param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot=(Join-Path $env:TEMP 'roost-webroute-consumer-20261004-04')
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if (Test-Path -LiteralPath $OutputRoot) { throw 'Use a new empty OutputRoot; existing consumer evidence is preserved.' }
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$cli=Join-Path $OutputRoot 'webroute.exe'
Push-Location $RepoRoot
try {
    & $GoExecutable build -o $cli ./codegen/cmd/webroute
    if ($LASTEXITCODE -ne 0) { throw 'CLI build failed' }
} finally { Pop-Location }
$results=@()
foreach ($scenario in @(@{name='normal';path='/generated/json';mode='normal'},@{name='missing_brace';path='/broken/{id';mode='invalid'},@{name='bad_regexp';path='/broken/{id:[}';mode='invalid'},@{name='wildcard_tail';path='/broken/*/tail';mode='invalid'})) {
    $consumer=Join-Path $OutputRoot $scenario.name
    New-Item -ItemType Directory -Path $consumer | Out-Null
    $repoSlash=$RepoRoot.Replace('\','/')
    @"
module example.com/roost-webroute-consumer

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => "$repoSlash"
"@ | Set-Content -LiteralPath (Join-Path $consumer 'go.mod') -Encoding utf8NoBOM
    (Get-Content -LiteralPath (Join-Path $PSScriptRoot 'handlers.go.txt') -Raw).Replace('__ROOST_PATH__',$scenario.path) |
        Set-Content -LiteralPath (Join-Path $consumer 'handlers.go') -Encoding utf8NoBOM
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'consumer_test.go.txt') -Destination (Join-Path $consumer 'consumer_test.go')
    & $cli -dir $consumer 2>&1 | Set-Content -LiteralPath (Join-Path $consumer 'generation.log') -Encoding utf8NoBOM
    $generationExit=$LASTEXITCODE
    if ($generationExit -ne 0) { throw "Generation failed: $($scenario.name)" }
    Push-Location $consumer
    try {
        & $GoExecutable test -mod=mod -count=1 -timeout=60s -json ./... -args -review-mode $scenario.mode 2>&1 |
            Set-Content -LiteralPath (Join-Path $consumer 'consumer.jsonl') -Encoding utf8NoBOM
        $consumerExit=$LASTEXITCODE
    } finally { Pop-Location }
    $results += [ordered]@{scenario=$scenario.name;generation_exit=$generationExit;consumer_exit=$consumerExit}
    if ($scenario.mode -eq 'normal') {
        if ($consumerExit -ne 0) { throw 'Normal generated HTTP consumer failed' }
        (Get-Content -LiteralPath (Join-Path $consumer 'handlers.go') -Raw).Replace('//roost:web','//retired:web') |
            Set-Content -LiteralPath (Join-Path $consumer 'handlers.go') -Encoding utf8NoBOM
        & $cli -dir $consumer 2>&1 | Set-Content -LiteralPath (Join-Path $consumer 'retirement.log') -Encoding utf8NoBOM
        $retirementExit=$LASTEXITCODE
        if ($retirementExit -ne 0 -or (Test-Path -LiteralPath (Join-Path $consumer 'webroute_gen.go'))) { throw 'Generated routes were not retired' }
        Push-Location $consumer
        try {
            & $GoExecutable test -mod=mod -count=1 -timeout=60s -json ./... -args -review-mode retired 2>&1 |
                Set-Content -LiteralPath (Join-Path $consumer 'retired.jsonl') -Encoding utf8NoBOM
            $retiredExit=$LASTEXITCODE
        } finally { Pop-Location }
        $results += [ordered]@{scenario='retired';generation_exit=$retirementExit;consumer_exit=$retiredExit}
        if ($retiredExit -ne 0) { throw 'Retired routes consumer failed' }
    }
}
$results | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
