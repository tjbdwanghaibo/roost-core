param([Parameter(Mandatory=$true)][string]$GoExecutable,[string]$RepoRoot=(Resolve-Path (Join-Path $PSScriptRoot '../../../..')).Path,[string]$OutputRoot=(Join-Path $env:TEMP 'roost-request-bugfix-verify'))
$ErrorActionPreference='Stop'
$env:GOWORK='off'
New-Item -ItemType Directory -Force -Path $OutputRoot|Out-Null
$overlay=@{Replace=@{}}
foreach($package in @('security','gateway','webroute')){$overlay.Replace[(Join-Path $RepoRoot "$package/noncore_review4_test.go")]=Join-Path $RepoRoot "docs/review/evidence/noncore-review-20261004-04/${package}_review_test.go.txt"}
$overlayPath=Join-Path $OutputRoot 'overlay.json';$overlay|ConvertTo-Json -Depth 5|Set-Content $overlayPath -Encoding utf8NoBOM
Push-Location $RepoRoot
try {
    & $GoExecutable test -overlay $overlayPath -count=1 -timeout=60s -json -run '^TestReview4' ./security ./gateway ./webroute 2>&1|Set-Content (Join-Path $OutputRoot 'original.jsonl') -Encoding utf8NoBOM
    $originalExit=$LASTEXITCODE
    $supportedArgs=@();if(!(Get-Command sh -ErrorAction SilentlyContinue)){$supportedArgs=@('-skip','^TestDeployScriptsCarryNoKnownShellcheckFindings$')}
    & $GoExecutable test -race -count=1 -timeout=240s -json @supportedArgs ./security ./gateway ./webroute ./httpserver ./httpclient ./codegen/... 2>&1|Set-Content (Join-Path $OutputRoot 'regression.jsonl') -Encoding utf8NoBOM
    $raceExit=$LASTEXITCODE
    & $GoExecutable vet ./security ./gateway ./webroute ./httpserver ./httpclient ./codegen/... 2>&1|Set-Content (Join-Path $OutputRoot 'vet.log') -Encoding utf8NoBOM
    $vetExit=$LASTEXITCODE
    @{original_exit=$originalExit;race_exit=$raceExit;vet_exit=$vetExit;excluded=$supportedArgs}|ConvertTo-Json|Set-Content (Join-Path $OutputRoot 'exits.json') -Encoding utf8NoBOM
    if($originalExit -ne 0 -or $raceExit -ne 0 -or $vetExit -ne 0){throw 'Verification failed; inspect preserved logs'}
} finally {Pop-Location}
