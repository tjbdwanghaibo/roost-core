param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [ValidateSet('Red','Green')][string]$Mode='Green',
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [Parameter(Mandatory=$true)][string]$OutputRoot
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
Push-Location $RepoRoot
try {
    if($Mode -eq 'Red') {
        $overlay=@{Replace=@{}}
        foreach($path in @('cache/read_through.go','cache/layered.go','cache/local.go','cache/grouped_local.go','cache/redis_raw.go','cache/redis_hash.go')) {
            $snapshot=Join-Path $OutputRoot ($path.Replace('/','_'))
            & git -c "safe.directory=$RepoRoot" show "1502f97372139a238e73850ff7337b9454146f9e:$path" | Set-Content $snapshot -Encoding utf8NoBOM
            if($LASTEXITCODE -ne 0) {throw "Cannot read original source: $path"}
            $overlay.Replace[(Join-Path $RepoRoot $path)]=$snapshot
        }
        $overlay.Replace[(Join-Path $RepoRoot 'cache/admission_promises_test.go')]=Join-Path $PSScriptRoot 'red-original-test.go.txt'
        $overlayPath=Join-Path $OutputRoot 'overlay.json'
        $overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
        & $GoExecutable test -race -count=1 -timeout=120s -json -overlay $overlayPath -run '^(TestCacheAdmissionFatalRemotePolicy|TestCacheAdmissionStaleWriteAcrossStores|TestCacheAdmissionBackfillAdmission)$' ./cache 2>&1 | Set-Content (Join-Path $OutputRoot 'red.jsonl') -Encoding utf8NoBOM
        $testExit=$LASTEXITCODE
        @{test_exit=$testExit;expected_behavior_exit=1} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
        if($testExit -ne 1) {throw "Red must fail on behavior; exit=$testExit"}
    } else {
        $packages=@('./cache','./redis','./redis/driver','./kit/redis','./codegen/internal/dao')
        & $GoExecutable test -race -count=1 -timeout=120s -json @packages 2>&1 | Set-Content (Join-Path $OutputRoot 'green.jsonl') -Encoding utf8NoBOM
        $testExit=$LASTEXITCODE
        & $GoExecutable vet @packages 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
        $vetExit=$LASTEXITCODE
        @{test_exit=$testExit;vet_exit=$vetExit} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
        if($testExit -ne 0 -or $vetExit -ne 0) {throw 'Green verification failed'}
    }
} finally {Pop-Location}
