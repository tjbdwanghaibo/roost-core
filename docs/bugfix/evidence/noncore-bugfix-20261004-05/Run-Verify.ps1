param(
    [Parameter(Mandatory=$true)][string]$GoExecutable,
    [ValidateSet('Red','Green')][string]$Mode='Green',
    [string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,
    [string]$OutputRoot=(Join-Path $env:TEMP 'roost-etcd-lifetime-verify')
)
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot | Out-Null
Push-Location $RepoRoot
try {
    $args=@('test','-race','-count=1','-timeout=120s','-json')
    if ($Mode -eq 'Red') {
        $baseline='3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956'
        $overlay=@{Replace=@{}}
        foreach ($path in @('etcd/driver/election.go','etcd/driver/election_test.go','etcd/driver/guards_promises_test.go','etcd/watch_callback.go','etcd/watcher.go')) {
            $snapshot=Join-Path $OutputRoot ($path.Replace('/','_')+'.txt')
            $source=& git -c "safe.directory=$RepoRoot" show "${baseline}:$path"
            if ($LASTEXITCODE -ne 0) { throw "Cannot read baseline $path" }
            $source | Set-Content $snapshot -Encoding utf8NoBOM
            $overlay.Replace[(Join-Path $RepoRoot $path)]=$snapshot
        }
        $overlay.Replace[(Join-Path $RepoRoot 'etcd/driver/election_lifetime_promises_test.go')]=Join-Path $PSScriptRoot 'red-driver-test.go.txt'
        $overlay.Replace[(Join-Path $RepoRoot 'etcd/watch_lifetime_promises_test.go')]=Join-Path $PSScriptRoot 'red-etcd-test.go.txt'
        $overlayPath=Join-Path $OutputRoot 'red-overlay.json'
        $overlay | ConvertTo-Json -Depth 5 | Set-Content $overlayPath -Encoding utf8NoBOM
        $args+=@('-overlay',$overlayPath,'-run','^TestEtcdLifetime')
    }
    $args+=@('./etcd','./etcd/driver','./kit/etcd')
    & $GoExecutable @args 2>&1 | Set-Content (Join-Path $OutputRoot 'tests.jsonl') -Encoding utf8NoBOM
    $testExit=$LASTEXITCODE
    $vetExit=$null
    if ($Mode -eq 'Green') {
        & $GoExecutable vet ./etcd ./etcd/driver ./kit/etcd 2>&1 | Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
        $vetExit=$LASTEXITCODE
    }
    @{mode=$Mode;test_exit=$testExit;vet_exit=$vetExit} | ConvertTo-Json | Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if ($Mode -eq 'Green' -and ($testExit -ne 0 -or $vetExit -ne 0)) { throw 'Lifecycle verification failed' }
    if ($Mode -eq 'Red' -and $testExit -eq 0) { throw 'Baseline counterexamples unexpectedly passed' }
} finally { Pop-Location }
