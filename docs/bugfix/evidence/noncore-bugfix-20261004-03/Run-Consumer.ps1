param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot=(Join-Path $env:TEMP 'roost-webroute-fixed-consumers')
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
if (Test-Path -LiteralPath $OutputRoot) { throw 'Use a new empty OutputRoot; existing evidence is preserved.' }
New-Item -ItemType Directory -Path $OutputRoot | Out-Null
$oldEvidence=Join-Path $RepoRoot 'docs/review/evidence/noncore-review-20261004-04'
$cli=Join-Path $OutputRoot 'webroute.exe'
Push-Location $RepoRoot
try { & $GoExecutable build -o $cli ./codegen/cmd/webroute; if($LASTEXITCODE -ne 0){throw 'CLI build failed'} } finally { Pop-Location }
$results=@()
foreach($scenario in @(@{name='normal';path='/generated/json'},@{name='missing_brace';path='/broken/{id'},@{name='bad_regexp';path='/broken/{id:[}'},@{name='wildcard_tail';path='/broken/*/tail'})) {
    $consumer=Join-Path $OutputRoot $scenario.name
    New-Item -ItemType Directory -Path $consumer|Out-Null
    $repoSlash=$RepoRoot.Replace('\','/')
    @"
module example.com/roost-webroute-consumer

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v0.0.0
replace github.com/tjbdwanghaibo/roost-core => "$repoSlash"
"@ | Set-Content (Join-Path $consumer 'go.mod') -Encoding utf8NoBOM
    (Get-Content (Join-Path $oldEvidence 'handlers.go.txt') -Raw).Replace('__ROOST_PATH__',$scenario.path)|Set-Content (Join-Path $consumer 'handlers.go') -Encoding utf8NoBOM
    Copy-Item -LiteralPath (Join-Path $oldEvidence 'consumer_test.go.txt') -Destination (Join-Path $consumer 'consumer_test.go')
    & $cli -dir $consumer 2>&1|Set-Content (Join-Path $consumer 'generation.log') -Encoding utf8NoBOM
    $generationExit=$LASTEXITCODE
    $mode='normal'
    if($scenario.name -eq 'normal') { if($generationExit -ne 0){throw 'Normal generation failed'} }
    else {
        $message=Get-Content (Join-Path $consumer 'generation.log') -Raw
        if($generationExit -eq 0 -or !$message.Contains('handleJSON') -or !$message.Contains($scenario.path) -or (Test-Path (Join-Path $consumer 'webroute_gen.go'))){throw 'Invalid pattern was not rejected before output'}
        # 旧已生成消费者仍须由运行期拒绝，不依赖重新生成才能获得保护。
        (Get-Content (Join-Path $oldEvidence 'generated-invalid.go.txt') -Raw).Replace('/broken/{id',$scenario.path)|Set-Content (Join-Path $consumer 'webroute_gen.go') -Encoding utf8NoBOM
        $mode='invalid'
    }
    Push-Location $consumer
    try { & $GoExecutable test -mod=mod -count=1 -timeout=60s -json ./... -args -review-mode $mode 2>&1|Set-Content consumer.jsonl -Encoding utf8NoBOM; $consumerExit=$LASTEXITCODE } finally { Pop-Location }
    $results += [ordered]@{scenario=$scenario.name;generation_exit=$generationExit;consumer_exit=$consumerExit}
    if($consumerExit -ne 0){throw "Consumer failed: $($scenario.name)"}
    if($mode -eq 'normal') {
        (Get-Content (Join-Path $consumer 'handlers.go') -Raw).Replace('//roost:web','//retired:web')|Set-Content (Join-Path $consumer 'handlers.go') -Encoding utf8NoBOM
        & $cli -dir $consumer 2>&1|Set-Content (Join-Path $consumer 'retirement.log') -Encoding utf8NoBOM
        if($LASTEXITCODE -ne 0 -or (Test-Path (Join-Path $consumer 'webroute_gen.go'))){throw 'Retirement failed'}
        Push-Location $consumer
        try { & $GoExecutable test -mod=mod -count=1 -timeout=60s -json ./... -args -review-mode retired 2>&1|Set-Content retired.jsonl -Encoding utf8NoBOM; $retiredExit=$LASTEXITCODE } finally { Pop-Location }
        $results += [ordered]@{scenario='retired';generation_exit=0;consumer_exit=$retiredExit}
        if($retiredExit -ne 0){throw 'Retired HTTP routes failed'}
    }
}
$results|ConvertTo-Json -Depth 5|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
